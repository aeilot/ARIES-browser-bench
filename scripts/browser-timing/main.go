// browser-timing splits each browserbench task's time between waiting on
// remote sites and local browser work. It joins the browser's request
// timing (evaluation/browser-network.jsonl) with the harness's tool and model
// spans; only Hermes writes the spans it reads (harness/telemetry/otel-spans.jsonl).
//
// Usage: go run ./scripts/browser-timing [-json] runs/<run-id>
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/hyscale-lab/aries/pkg/benchmark/browserbench"
)

// taskTiming is one task's line of output.
type taskTiming struct {
	TaskID string `json:"task_id"`
	browserbench.TimeSplit
	Requests        int     `json:"requests"`
	NetworkActiveMs float64 `json:"network_active_ms"`
}

func main() {
	asJSON := flag.Bool("json", false, "print JSON instead of a table")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: browser-timing [-json] runs/<run-id>")
		os.Exit(2)
	}
	timings, err := collect(flag.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *asJSON {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(timings); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	writer := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(writer, "task\tbrowser calls\tcall s\tremote wait s\tlocal s\tnet outside calls s\tmodel s\tspan s\trequests\t")
	for _, timing := range timings {
		fmt.Fprintf(writer, "%s\t%d\t%.1f\t%.1f\t%.1f\t%.1f\t%.1f\t%.1f\t%d\t\n", shortID(timing.TaskID), timing.BrowserCalls,
			timing.BrowserCallMs/1000, timing.RemoteWaitMs/1000, timing.LocalMs/1000, timing.NetworkOutsideCallsMs/1000,
			timing.ModelMs/1000, timing.SpanMs/1000, timing.Requests)
	}
	if err := writer.Flush(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func collect(runDir string) ([]taskTiming, error) {
	entries, err := os.ReadDir(runDir)
	if err != nil {
		return nil, err
	}
	var timings []taskTiming
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		taskDir := filepath.Join(runDir, entry.Name())
		network, err := os.Open(filepath.Join(taskDir, "evaluation", "browser-network.jsonl"))
		if errors.Is(err, fs.ErrNotExist) {
			continue // not a browserbench task, or its browser loaded nothing
		}
		if err != nil {
			return nil, err
		}
		summary, intervals, err := browserbench.ParseNetworkTiming(network)
		network.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", entry.Name(), err)
		}
		spans, err := readSpans(filepath.Join(taskDir, "harness", "telemetry", "otel-spans.jsonl"))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", entry.Name(), err)
		}
		split := browserbench.SplitTime(spans, intervals, isBrowserCall, isModelCall)
		timings = append(timings, taskTiming{TaskID: entry.Name(), TimeSplit: split, Requests: summary.Requests, NetworkActiveMs: summary.NetworkActiveMs})
	}
	sort.Slice(timings, func(i, j int) bool { return timings[i].TaskID < timings[j].TaskID })
	return timings, nil
}

// isBrowserCall selects Hermes's MCP client spans for the browser's tools,
// which time each request to the in-sandbox server.
func isBrowserCall(name string) bool {
	return strings.HasPrefix(name, "MCP send tools/call browser_")
}

// isModelCall selects Hermes's model API spans.
func isModelCall(name string) bool { return strings.HasPrefix(name, "api.") }

func readSpans(path string) ([]browserbench.Span, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read Hermes spans (only Hermes records them): %w", err)
	}
	defer file.Close()
	var spans []browserbench.Span
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var span struct {
			Name  string `json:"name"`
			Start int64  `json:"start_time_unix_nano"`
			End   int64  `json:"end_time_unix_nano"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &span); err != nil || span.Start <= 0 {
			continue
		}
		spans = append(spans, browserbench.Span{Name: span.Name, Interval: browserbench.Interval{
			StartMs: float64(span.Start) / 1e6, EndMs: float64(span.End) / 1e6,
		}})
	}
	return spans, scanner.Err()
}

func shortID(taskID string) string {
	if len(taskID) > 12 {
		return taskID[:8] + "…" + taskID[len(taskID)-4:]
	}
	return taskID
}
