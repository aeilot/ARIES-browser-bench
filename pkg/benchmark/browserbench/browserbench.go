// Package browserbench runs browser-use tasks against a browser that lives
// in the task sandbox. The sandbox image is Playwright MCP's; preparation
// starts its server, the harness reaches it as an MCP server over the run
// network, and websites stay on the public internet, so the sandbox carries
// the browser's work and nothing else. Tasks come from the AssistantBench
// dev split, the only split with public answers, and are scored host-side
// with AssistantBench's own scorer (see score.go).
package browserbench

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "embed"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
)

const (
	// DefaultMCPPort is the in-sandbox port of the Playwright MCP server.
	DefaultMCPPort = 8931
	// MCPServerName is the name the harness registers the browser under. It
	// must not be a harness toolset's name: Hermes aliases an MCP server's
	// toolset to the server name, so "browser" would collide with Hermes's
	// own browser toolset, which ARIES disables.
	MCPServerName = "playwright"
	// defaultMCPCallTimeoutSeconds bounds one browser tool call at the
	// harness; Playwright MCP's own navigation timeout is 60 seconds.
	defaultMCPCallTimeoutSeconds = 120
	// defaultTaskTimeout bounds one agent run when the profile sets no
	// agent timeout override; AssistantBench tasks take minutes of browsing.
	defaultTaskTimeout = 30 * time.Minute
	// answerPath is where the agent writes its final answer. It is outside
	// the workdir so it exists whatever the image's layout.
	answerPath = "/tmp/aries-answer.txt"
	// browserDir holds the MCP server's log and output files.
	browserDir = "/tmp/aries-browser"
	// mcpEntrypoint is Playwright MCP's CLI in its official image.
	mcpEntrypoint = "/app/cli.js"
	// networkTimingScript is where preparation places network_timing.js,
	// outside browserDir so that the browser's output cannot replace it.
	networkTimingScript = "/tmp/aries-network-timing.js"
	// networkTimingFile is where that script writes its records.
	networkTimingFile = browserDir + "/network.jsonl"
)

//go:embed network_timing.js
var networkTimingSource []byte

// taskPromptTemplate wraps each AssistantBench question. Upstream, a
// BrowserGym agent gets the bare question, has only browser actions, and is
// scored on its final chat message. ARIES's harness also has a shell, which
// writes the scored answer file, so the template confines research to the
// browser and adds where to write the answer and the shapes question_scorer
// parses (see score.go).
const taskPromptTemplate = "" +
	"Answer the question below by browsing the web with the browser tools of the\n" +
	"MCP server named `" + MCPServerName + "` (navigate, snapshot, click, type, and so on);\n" +
	"the browser runs in this Linux container and reaches the public internet.\n" +
	"Find the answer only by browsing with these tools: do not fetch pages or call\n" +
	"web APIs from the shell or from scripts. Use the shell only to write the answer.\n" +
	"\n" +
	"## Question\n" +
	"\n" +
	"{question}\n" +
	"\n" +
	"## Answer\n" +
	"\n" +
	"When you have the answer, write it to `" + answerPath + "` by invoking a shell\n" +
	"command, then reply with a single line: `DONE`. A reply that only states the\n" +
	"answer, without the file, is scored as no answer.\n" +
	"\n" +
	"- Write only the answer, without explanation.\n" +
	"- For a number, write only the number, without units or currency symbols.\n" +
	"- For several items, write a JSON array of strings, such as [\"first\", \"second\"].\n"

// Options selects AssistantBench dev tasks from one pinned dataset file.
type Options struct {
	Root    string
	TaskIDs []string
	// ExecutionTaskIDs optionally renames each TaskIDs entry to a distinct
	// per-occurrence ID ("<task>-001"); it defaults to TaskIDs.
	ExecutionTaskIDs []string
	OutputDir        string
	Dataset          Dataset
	TaskTimeout      time.Duration
	Environment      core.Environment
	MCPPort          int
	MCPCallTimeout   int
	BlockedOrigins   []string
	// NetworkTiming records the browser's per-request and page-load timing
	// in the sandbox (network_timing.js) for evaluation to summarize.
	NetworkTiming bool
}

// Dataset pins one AssistantBench file by revision and SHA-256.
type Dataset struct {
	RepositoryURL string
	Revision      string
	File          string
	SHA256        string
}

// Path is the dataset file's location under root.
func (d Dataset) Path(root string) string {
	return filepath.Join(root, "assistantbench", d.Revision, d.File)
}

// Benchmark loads AssistantBench tasks and keeps their gold answers
// host-side for Evaluate.
type Benchmark struct {
	root             string
	taskIDs          []string
	executionTaskIDs []string
	outputDir        string
	dataset          Dataset
	taskTimeout      time.Duration
	environment      core.Environment
	mcpPort          int
	blockedOrigins   []string
	networkTiming    bool

	mu      sync.RWMutex
	logical map[string]string // execution ID -> AssistantBench ID
}

var _ runner.Benchmark = (*Benchmark)(nil)

func New(options Options) (*Benchmark, error) {
	if strings.TrimSpace(options.Root) == "" {
		return nil, errors.New("browserbench root is required")
	}
	if len(options.TaskIDs) == 0 {
		return nil, errors.New("browserbench task IDs are required")
	}
	if strings.TrimSpace(options.OutputDir) == "" {
		return nil, errors.New("browserbench output directory is required")
	}
	if options.Dataset.Revision == "" || options.Dataset.File == "" || options.Dataset.SHA256 == "" {
		return nil, errors.New("browserbench dataset pin is required")
	}
	if strings.TrimSpace(options.Environment.Image) == "" {
		return nil, errors.New("browserbench environment image is required")
	}
	seen := make(map[string]struct{}, len(options.TaskIDs))
	for _, id := range options.TaskIDs {
		if !validTaskID(id) {
			return nil, fmt.Errorf("invalid browserbench task ID %q: want a 64-character lowercase hex AssistantBench ID", id)
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, fmt.Errorf("duplicate browserbench task ID %q", id)
		}
		seen[id] = struct{}{}
	}
	executionTaskIDs := options.ExecutionTaskIDs
	if executionTaskIDs == nil {
		executionTaskIDs = options.TaskIDs
	} else if len(executionTaskIDs) != len(options.TaskIDs) {
		return nil, errors.New("browserbench execution task IDs must match task IDs")
	} else {
		seenExecution := make(map[string]struct{}, len(executionTaskIDs))
		for index, id := range executionTaskIDs {
			if !safeExecutionTaskID(options.TaskIDs[index], id) {
				return nil, fmt.Errorf("invalid browserbench execution task ID %q", id)
			}
			if _, duplicate := seenExecution[id]; duplicate {
				return nil, fmt.Errorf("duplicate browserbench execution task ID %q", id)
			}
			seenExecution[id] = struct{}{}
		}
	}
	port := options.MCPPort
	if port == 0 {
		port = DefaultMCPPort
	}
	if port < 1024 || port > 65535 {
		return nil, errors.New("browserbench MCP port must be between 1024 and 65535")
	}
	callTimeout := options.MCPCallTimeout
	if callTimeout == 0 {
		callTimeout = defaultMCPCallTimeoutSeconds
	}
	if callTimeout < 0 {
		return nil, errors.New("browserbench MCP call timeout must be positive")
	}
	for _, origin := range options.BlockedOrigins {
		if strings.TrimSpace(origin) == "" || strings.ContainsAny(origin, "; \t\n\x00") {
			return nil, fmt.Errorf("invalid browserbench blocked origin %q", origin)
		}
	}

	taskTimeout := options.TaskTimeout
	if taskTimeout == 0 {
		taskTimeout = defaultTaskTimeout
	}
	if taskTimeout < 0 {
		return nil, errors.New("browserbench task timeout must be positive")
	}

	environment := options.Environment
	// The websites are the public internet.
	environment.AllowNetwork = true
	environment.Services.MCP = []core.TaskMCPService{{
		Name: MCPServerName, Port: port, Path: "/mcp", Transport: "streamable-http", TimeoutSeconds: callTimeout,
	}}
	return &Benchmark{
		root:             filepath.Clean(options.Root),
		taskIDs:          slices.Clone(options.TaskIDs),
		executionTaskIDs: slices.Clone(executionTaskIDs),
		outputDir:        filepath.Clean(options.OutputDir),
		dataset:          options.Dataset,
		taskTimeout:      taskTimeout,
		environment:      environment,
		mcpPort:          port,
		blockedOrigins:   slices.Clone(options.BlockedOrigins),
		networkTiming:    options.NetworkTiming,
	}, nil
}

// row is one line of an AssistantBench JSONL file.
type row struct {
	ID         string `json:"id"`
	Set        string `json:"set"`
	Task       string `json:"task"`
	Answer     string `json:"answer"`
	Difficulty string `json:"difficulty"`
}

func (b *Benchmark) Tasks(ctx context.Context) ([]core.Task, error) {
	rows, err := loadRows(b.dataset.Path(b.root), b.dataset.SHA256)
	if err != nil {
		return nil, err
	}
	tasks := make([]core.Task, 0, len(b.taskIDs))
	logical := make(map[string]string, len(b.taskIDs))
	for index, id := range b.taskIDs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		task, ok := rows[id]
		if !ok {
			return nil, fmt.Errorf("browserbench task %q is not in %s", id, b.dataset.File)
		}
		executionID := b.executionTaskIDs[index]
		logical[executionID] = id
		tasks = append(tasks, core.Task{
			ID:          executionID,
			Instruction: strings.ReplaceAll(taskPromptTemplate, "{question}", task.Task),
			Timeout:     b.taskTimeout,
			Environment: b.environment,
		})
	}
	b.mu.Lock()
	b.logical = logical
	b.mu.Unlock()
	return tasks, nil
}

// loadRows reads the pinned file after checking its digest. Every row must
// carry a question and a gold answer: only the dev split publishes answers.
func loadRows(path, wantSHA256 string) (map[string]row, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read AssistantBench file (run aries setup first): %w", err)
	}
	digest := sha256.Sum256(data)
	if got := hex.EncodeToString(digest[:]); got != wantSHA256 {
		return nil, fmt.Errorf("AssistantBench file %s has SHA-256 %s; want pinned %s", path, got, wantSHA256)
	}
	rows := map[string]row{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		var parsed row
		if err := json.Unmarshal([]byte(text), &parsed); err != nil {
			return nil, fmt.Errorf("parse AssistantBench line %d: %w", line, err)
		}
		if !validTaskID(parsed.ID) || strings.TrimSpace(parsed.Task) == "" || strings.TrimSpace(parsed.Answer) == "" {
			return nil, fmt.Errorf("AssistantBench line %d lacks an ID, question, or gold answer", line)
		}
		if _, duplicate := rows[parsed.ID]; duplicate {
			return nil, fmt.Errorf("AssistantBench line %d repeats ID %s", line, parsed.ID)
		}
		rows[parsed.ID] = parsed
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read AssistantBench file: %w", err)
	}
	return rows, nil
}

func validTaskID(id string) bool {
	if len(id) != 64 || strings.ToLower(id) != id {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

// safeExecutionTaskID requires id to be logicalID, a "-", and a positive
// decimal occurrence index of at least three digits.
func safeExecutionTaskID(logicalID, id string) bool {
	if len(id) > 149 || !strings.HasPrefix(id, logicalID+"-") {
		return false
	}
	suffix := strings.TrimPrefix(id, logicalID+"-")
	if len(suffix) < 3 {
		return false
	}
	index, err := strconv.ParseUint(suffix, 10, 64)
	return err == nil && index > 0
}
