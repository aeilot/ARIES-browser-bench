package browserbench

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
)

var (
	numberTaskID = strings.Repeat("a", 64)
	listTaskID   = strings.Repeat("b", 64)
)

func fixtureData(t *testing.T) []byte {
	t.Helper()
	var lines []string
	for _, r := range []row{
		{ID: numberTaskID, Set: "dev", Task: "How many moons does Mars have?", Answer: "2", Difficulty: "Medium"},
		{ID: listTaskID, Set: "dev", Task: "Name two primary colors.", Answer: "red\nblue", Difficulty: "Hard"},
	} {
		encoded, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(encoded))
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// writeFixture installs fixture data at the pinned location under a new root.
func writeFixture(t *testing.T) (string, Dataset) {
	t.Helper()
	data := fixtureData(t)
	dataset := Dataset{RepositoryURL: "https://example.invalid/datasets/ab", Revision: strings.Repeat("1", 40), File: "dev.jsonl", SHA256: digest(data)}
	root := t.TempDir()
	path := dataset.Path(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return root, dataset
}

func baseOptions(t *testing.T) Options {
	root, dataset := writeFixture(t)
	return Options{
		Root: root, TaskIDs: []string{numberTaskID}, OutputDir: t.TempDir(), Dataset: dataset,
		Environment: core.Environment{Image: "mcr.microsoft.com/playwright/mcp:v0.0.83", Workdir: "/home/node"},
	}
}

func TestNewRejectsInvalidOptions(t *testing.T) {
	for name, mutate := range map[string]func(*Options){
		"short task ID":      func(o *Options) { o.TaskIDs = []string{"abc"} },
		"uppercase task ID":  func(o *Options) { o.TaskIDs = []string{strings.Repeat("A", 64)} },
		"duplicate task ID":  func(o *Options) { o.TaskIDs = []string{numberTaskID, numberTaskID} },
		"execution mismatch": func(o *Options) { o.ExecutionTaskIDs = []string{numberTaskID + "-1"} },
		"low port":           func(o *Options) { o.MCPPort = 80 },
		"origin separator":   func(o *Options) { o.BlockedOrigins = []string{"a.com;b.com"} },
		"missing image":      func(o *Options) { o.Environment.Image = "" },
		"missing pin":        func(o *Options) { o.Dataset.SHA256 = "" },
	} {
		t.Run(name, func(t *testing.T) {
			options := baseOptions(t)
			mutate(&options)
			if _, err := New(options); err == nil {
				t.Fatal("New accepted invalid options")
			}
		})
	}
}

func TestTasksDeclareBrowserServiceAndNetwork(t *testing.T) {
	options := baseOptions(t)
	options.ExecutionTaskIDs = []string{numberTaskID + "-001"}
	benchmark, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := benchmark.Tasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].ID != numberTaskID+"-001" {
		t.Fatalf("tasks = %#v", tasks)
	}
	task := tasks[0]
	if !strings.Contains(task.Instruction, "How many moons does Mars have?") || !strings.Contains(task.Instruction, answerPath) || strings.Contains(task.Instruction, "{question}") {
		t.Fatalf("instruction = %q", task.Instruction)
	}
	if strings.Contains(task.Instruction, "\n2\n") {
		t.Fatal("instruction leaks the gold answer")
	}
	if task.Timeout != defaultTaskTimeout {
		t.Fatalf("timeout = %v", task.Timeout)
	}
	if !task.Environment.AllowNetwork || task.NoSandboxTools {
		t.Fatalf("environment = %#v, no_sandbox_tools = %v", task.Environment, task.NoSandboxTools)
	}
	want := []core.TaskMCPService{{Name: "playwright", Port: DefaultMCPPort, Path: "/mcp", Transport: "streamable-http", TimeoutSeconds: defaultMCPCallTimeoutSeconds}}
	if !reflect.DeepEqual(task.Environment.Services.MCP, want) {
		t.Fatalf("MCP services = %#v", task.Environment.Services.MCP)
	}
}

func TestTasksRejectsAlteredDatasetAndUnknownTask(t *testing.T) {
	options := baseOptions(t)
	if err := os.WriteFile(options.Dataset.Path(options.Root), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	benchmark, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := benchmark.Tasks(context.Background()); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("altered dataset error = %v", err)
	}

	options = baseOptions(t)
	options.TaskIDs = []string{strings.Repeat("c", 64)}
	benchmark, err = New(options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := benchmark.Tasks(context.Background()); err == nil || !strings.Contains(err.Error(), "is not in") {
		t.Fatalf("unknown task error = %v", err)
	}
}

// sandboxFake records commands and serves downloads from an in-memory
// filesystem; a path absent from files is runner.ErrNotFound.
type sandboxFake struct {
	commands []core.Command
	results  []core.CommandResult
	files    map[string]string
	limits   map[string]int64
	uploads  map[string]string // destination -> content and mode
}

func (s *sandboxFake) Connectivity() core.HarnessConnectivity { return core.HarnessConnectivity{} }

func (s *sandboxFake) Exec(_ context.Context, command core.Command) (core.CommandResult, error) {
	index := len(s.commands)
	s.commands = append(s.commands, command)
	if index < len(s.results) {
		return s.results[index], nil
	}
	return core.CommandResult{}, nil
}

func (s *sandboxFake) Upload(_ context.Context, source, destination string) error {
	content, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if s.uploads == nil {
		s.uploads = map[string]string{}
	}
	s.uploads[destination] = fmt.Sprintf("%v %s", info.Mode().Perm(), content)
	return nil
}

func (s *sandboxFake) Download(_ context.Context, source, destination string) error {
	content, ok := s.files[source]
	if !ok {
		return fmt.Errorf("download %s: %w", source, runner.ErrNotFound)
	}
	return os.WriteFile(destination, []byte(content), 0o600)
}

func (s *sandboxFake) DownloadLimit(ctx context.Context, source, destination string, limit int64) error {
	if s.limits == nil {
		s.limits = map[string]int64{}
	}
	s.limits[source] = limit
	return s.Download(ctx, source, destination)
}

func TestPrepareSandboxClearsAnswerThenStartsServer(t *testing.T) {
	options := baseOptions(t)
	options.BlockedOrigins = []string{"https://github.com", "huggingface.co"}
	benchmark, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	sandbox := &sandboxFake{}
	if err := benchmark.PrepareSandbox(context.Background(), core.Task{ID: numberTaskID}, sandbox); err != nil {
		t.Fatal(err)
	}
	if len(sandbox.commands) != 4 {
		t.Fatalf("commands = %#v", sandbox.commands)
	}
	if remove := sandbox.commands[0]; remove.Path != "/bin/rm" || !reflect.DeepEqual(remove.Args, []string{"-rf", "--", answerPath, browserDir, networkTimingScript}) {
		t.Fatalf("remove = %#v", remove)
	}
	if probe := sandbox.commands[1]; probe.Path != "/bin/sh" || probe.Args[len(probe.Args)-1] != answerPath {
		t.Fatalf("absence probe = %#v", probe)
	}
	start := sandbox.commands[2]
	wantStart := []string{"-c", mcpStartScript, "aries-browser-start", browserDir,
		"node", mcpEntrypoint, "--headless", "--browser", "chromium", "--no-sandbox", "--isolated",
		"--host", "0.0.0.0", "--port", "8931", "--allowed-hosts", "*", "--output-dir", browserDir,
		"--blocked-origins", "https://github.com;huggingface.co"}
	if start.Path != "/bin/sh" || !reflect.DeepEqual(start.Args, wantStart) {
		t.Fatalf("start = %#v", start)
	}
	if ready := sandbox.commands[3]; ready.Path != "/bin/bash" || ready.Args[len(ready.Args)-1] != "8931" {
		t.Fatalf("readiness probe = %#v", ready)
	}
	if len(sandbox.uploads) != 0 {
		t.Fatalf("uploads without network timing = %v", sandbox.uploads)
	}
}

func TestPrepareSandboxInstallsNetworkTiming(t *testing.T) {
	options := baseOptions(t)
	options.NetworkTiming = true
	benchmark, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	sandbox := &sandboxFake{}
	if err := benchmark.PrepareSandbox(context.Background(), core.Task{ID: numberTaskID}, sandbox); err != nil {
		t.Fatal(err)
	}
	uploaded := sandbox.uploads[networkTimingScript]
	if !strings.HasPrefix(uploaded, "-rw-r--r-- ") || !strings.Contains(uploaded, "requestfinished") {
		t.Fatalf("network timing upload = %.80q", uploaded)
	}
	if own := sandbox.commands[2]; own.Path != "/bin/chown" || own.User != "0:0" || own.Args[len(own.Args)-1] != networkTimingScript {
		t.Fatalf("ownership command = %#v", own)
	}
	start := sandbox.commands[3].Args
	if start[len(start)-2] != "--init-page" || start[len(start)-1] != networkTimingScript {
		t.Fatalf("start = %#v", start)
	}
}

func TestPrepareSandboxReportsServerThatNeverListens(t *testing.T) {
	previousAttempts, previousDelay := mcpReadyAttempts, mcpReadyDelay
	mcpReadyAttempts, mcpReadyDelay = 3, 0
	t.Cleanup(func() { mcpReadyAttempts, mcpReadyDelay = previousAttempts, previousDelay })

	benchmark, err := New(baseOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	failed := core.CommandResult{ExitCode: 1}
	sandbox := &sandboxFake{results: []core.CommandResult{{}, {}, {}, failed, failed, failed, {Stdout: "Error: cannot find module /app/cli.js\n"}}}
	err = benchmark.PrepareSandbox(context.Background(), core.Task{ID: numberTaskID}, sandbox)
	if err == nil || !strings.Contains(err.Error(), "did not listen") || !strings.Contains(err.Error(), "cannot find module") {
		t.Fatalf("error = %v", err)
	}
}

func loadedBenchmark(t *testing.T, taskID string) (*Benchmark, string) {
	t.Helper()
	options := baseOptions(t)
	options.TaskIDs = []string{taskID}
	benchmark, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := benchmark.Tasks(context.Background()); err != nil {
		t.Fatal(err)
	}
	return benchmark, options.OutputDir
}

func readScore(t *testing.T, outputDir, taskID string) scoreRecord {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(outputDir, taskID, "evaluation", "score.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record scoreRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	return record
}

func TestEvaluateScoresAnswerAgainstGold(t *testing.T) {
	for name, c := range map[string]struct {
		taskID, answer string
		score, reward  float64
		status         string
	}{
		"exact number":  {numberTaskID, "2\n", 1, 1, core.StatusSucceeded},
		"wrong number":  {numberTaskID, "3", 1 - 0.4054651081081644, 0, core.StatusFailed},
		"list as array": {listTaskID, `["blue", "red"]`, 1, 1, core.StatusSucceeded},
		"partial list":  {listTaskID, `["red"]`, 0.5, 0, core.StatusFailed},
	} {
		t.Run(name, func(t *testing.T) {
			benchmark, outputDir := loadedBenchmark(t, c.taskID)
			sandbox := &sandboxFake{files: map[string]string{answerPath: c.answer, browserDir + "/mcp.log": "listening\n"}}
			evaluation, err := benchmark.Evaluate(context.Background(), core.Task{ID: c.taskID}, sandbox, nil)
			if err != nil {
				t.Fatal(err)
			}
			if diff := evaluation.Score - c.score; diff > 1e-12 || diff < -1e-12 || evaluation.Reward != c.reward || evaluation.Status != c.status || evaluation.VerifierStatus != c.status {
				t.Fatalf("evaluation = %#v", evaluation)
			}
			if sandbox.limits[answerPath] != maxAnswerBytes || sandbox.limits[browserDir+"/mcp.log"] != maxBrowserLogBytes {
				t.Fatalf("download limits = %v", sandbox.limits)
			}
			record := readScore(t, outputDir, c.taskID)
			if record.Prediction != c.answer || record.TaskID != c.taskID || record.Gold == "" {
				t.Fatalf("score record = %#v", record)
			}
			if len(evaluation.LogPaths) != 3 {
				t.Fatalf("log paths = %v", evaluation.LogPaths)
			}
			if _, err := os.Stat(filepath.Join(outputDir, c.taskID, "evaluation", "network-summary.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("summary without network timing: %v", err)
			}
		})
	}
}

func TestEvaluateSummarizesNetworkTiming(t *testing.T) {
	options := baseOptions(t)
	options.NetworkTiming = true
	benchmark, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := benchmark.Tasks(context.Background()); err != nil {
		t.Fatal(err)
	}
	records := `{"type":"request","page":1,"resource_type":"document","start_ms":1000,"end_ms":1300,"timing":{"response_end":250},"status":200,"sizes":{"response_headers":100,"response_body":900}}` + "\n"
	sandbox := &sandboxFake{files: map[string]string{answerPath: "2", networkTimingFile: records}}
	evaluation, err := benchmark.Evaluate(context.Background(), core.Task{ID: numberTaskID}, sandbox, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sandbox.limits[networkTimingFile] != maxNetworkTimingBytes {
		t.Fatalf("download limits = %v", sandbox.limits)
	}
	data, err := os.ReadFile(filepath.Join(options.OutputDir, numberTaskID, "evaluation", "network-summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var summary NetworkSummary
	if err := json.Unmarshal(data, &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Requests != 1 || summary.ResponseBytes != 1000 || summary.NetworkActiveMs != 250 || evaluation.Score != 1 {
		t.Fatalf("summary = %+v, evaluation = %+v", summary, evaluation)
	}

	// No page loaded: no records, no summary, and no error.
	benchmark2, _ := loadedBenchmark(t, numberTaskID)
	benchmark2.networkTiming = true
	if _, err := benchmark2.Evaluate(context.Background(), core.Task{ID: numberTaskID}, &sandboxFake{files: map[string]string{answerPath: "2"}}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestEvaluateMissingAnswerScoresZero(t *testing.T) {
	benchmark, outputDir := loadedBenchmark(t, numberTaskID)
	evaluation, err := benchmark.Evaluate(context.Background(), core.Task{ID: numberTaskID}, &sandboxFake{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if evaluation.Score != 0 || evaluation.Reward != 0 || evaluation.Status != core.StatusFailed || !strings.Contains(evaluation.Error, answerPath) {
		t.Fatalf("evaluation = %#v", evaluation)
	}
	if record := readScore(t, outputDir, numberTaskID); record.ScorerError == "" {
		t.Fatalf("score record = %#v", record)
	}
}

func TestEvaluateReportsUpstreamScorerFailures(t *testing.T) {
	for name, answer := range map[string]string{"raises": "null", "infinite": "-Infinity"} {
		t.Run(name, func(t *testing.T) {
			benchmark, outputDir := loadedBenchmark(t, numberTaskID)
			sandbox := &sandboxFake{files: map[string]string{answerPath: answer}}
			evaluation, err := benchmark.Evaluate(context.Background(), core.Task{ID: numberTaskID}, sandbox, nil)
			if err != nil {
				t.Fatal(err)
			}
			if evaluation.Score != 0 || evaluation.Status != core.StatusFailed || evaluation.Error == "" {
				t.Fatalf("evaluation = %#v", evaluation)
			}
			if record := readScore(t, outputDir, numberTaskID); record.ScorerError == "" || record.Accuracy != 0 {
				t.Fatalf("score record = %#v", record)
			}
		})
	}
}

func TestEvaluateRequiresLoadedTask(t *testing.T) {
	benchmark, err := New(baseOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := benchmark.Evaluate(context.Background(), core.Task{ID: numberTaskID}, &sandboxFake{}, nil); err == nil {
		t.Fatal("Evaluate scored a task Tasks never loaded")
	}
}

func TestSetupDownloadsVerifiesAndReuses(t *testing.T) {
	data := fixtureData(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/datasets/ab/resolve/"+strings.Repeat("1", 40)+"/dev.jsonl" {
			http.NotFound(w, r)
			return
		}
		w.Write(data)
	}))
	defer server.Close()
	dataset := Dataset{RepositoryURL: server.URL + "/datasets/ab", Revision: strings.Repeat("1", 40), File: "dev.jsonl", SHA256: digest(data)}
	root := filepath.Join(t.TempDir(), "cache")
	if err := Setup(context.Background(), root, dataset, server.Client()); err != nil {
		t.Fatal(err)
	}
	if err := Setup(context.Background(), root, dataset, server.Client()); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want one download then reuse", requests)
	}

	wrong := dataset
	wrong.SHA256 = strings.Repeat("0", 64)
	if err := Setup(context.Background(), filepath.Join(t.TempDir(), "cache"), wrong, server.Client()); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("mismatched download error = %v", err)
	}
	if err := Setup(context.Background(), root, wrong, server.Client()); err == nil {
		t.Fatal("Setup accepted an installed file that does not match the pin")
	}
}
