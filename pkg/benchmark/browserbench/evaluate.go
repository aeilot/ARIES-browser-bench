package browserbench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
)

const (
	// maxAnswerBytes bounds the answer download; answers are short.
	maxAnswerBytes = 64 << 10
	// maxBrowserLogBytes bounds the MCP server log kept as evidence.
	maxBrowserLogBytes = 16 << 20
)

// scoreRecord is evaluation/score.json.
type scoreRecord struct {
	TaskID      string  `json:"assistantbench_id"`
	Prediction  string  `json:"prediction"`
	Gold        string  `json:"gold"`
	Accuracy    float64 `json:"accuracy"`
	ScorerError string  `json:"scorer_error,omitempty"`
}

// Evaluate reads the agent's answer from the live sandbox after both
// isolation gates and scores it host-side against the gold answer with
// AssistantBench's question_scorer. Score is that accuracy, with partial
// credit in [0, 1]; Reward is 1, and Status succeeded, only for a fully
// correct answer. A missing answer scores 0. The browser server's log is
// kept as task evidence whatever the outcome.
func (b *Benchmark) Evaluate(ctx context.Context, task core.Task, sandbox runner.Sandbox, _ runner.EvaluationSandboxes) (core.Evaluation, error) {
	started := time.Now()
	evaluation := core.Evaluation{Status: core.StatusFailed, VerifierStatus: core.StatusFailed}
	finish := func(err error) (core.Evaluation, error) {
		evaluation.Duration = time.Since(started)
		if err != nil {
			evaluation.Error = err.Error()
		}
		return evaluation, err
	}
	if sandbox == nil {
		return finish(errors.New("browserbench evaluator requires a live sandbox"))
	}
	b.mu.RLock()
	id, ok := b.logical[task.ID]
	b.mu.RUnlock()
	if !ok {
		return finish(fmt.Errorf("browserbench task %q was not loaded by Tasks", task.ID))
	}
	rows, err := loadRows(b.dataset.Path(b.root), b.dataset.SHA256)
	if err != nil {
		return finish(fmt.Errorf("reverify AssistantBench file before evaluation: %w", err))
	}
	gold := rows[id].Answer

	artifactDir := filepath.Join(b.outputDir, task.ID, "evaluation")
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		return finish(fmt.Errorf("create evaluator artifact directory: %w", err))
	}
	answerArtifact := filepath.Join(artifactDir, "answer.txt")
	scoreArtifact := filepath.Join(artifactDir, "score.json")
	logArtifact := filepath.Join(artifactDir, "browser-mcp.log")
	for _, path := range []string{answerArtifact, scoreArtifact, logArtifact} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return finish(fmt.Errorf("remove stale evaluator artifact %q: %w", path, err))
		}
	}
	if err := download(ctx, sandbox, browserDir+"/mcp.log", logArtifact, maxBrowserLogBytes); err == nil {
		evaluation.LogPaths = append(evaluation.LogPaths, logArtifact)
	} else if !errors.Is(err, runner.ErrNotFound) {
		return finish(fmt.Errorf("collect Playwright MCP log: %w", err))
	}

	record := scoreRecord{TaskID: id, Gold: gold}
	switch err := download(ctx, sandbox, answerPath, answerArtifact, maxAnswerBytes); {
	case errors.Is(err, runner.ErrNotFound):
		// No answer is a task outcome, not an evaluation failure.
		record.ScorerError = "agent did not write " + answerPath
		evaluation.Error = record.ScorerError
	case err != nil:
		return finish(fmt.Errorf("download answer: %w", err))
	default:
		evaluation.LogPaths = append(evaluation.LogPaths, answerArtifact)
		answer, err := os.ReadFile(answerArtifact)
		if err != nil {
			return finish(fmt.Errorf("read downloaded answer: %w", err))
		}
		record.Prediction = string(answer)
		accuracy, err := scoreAnswer(record.Prediction, gold)
		switch {
		case err != nil:
			record.ScorerError = err.Error()
			evaluation.Error = record.ScorerError
		case math.IsNaN(accuracy) || math.IsInf(accuracy, 0):
			// The Python scorer can return these (an answer of -Infinity
			// against a positive number scores inf); they are not scores.
			record.ScorerError = fmt.Sprintf("upstream AssistantBench scorer returned %s", pyFloatRepr(accuracy))
			evaluation.Error = record.ScorerError
		default:
			record.Accuracy = accuracy
		}
	}
	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return finish(fmt.Errorf("encode score artifact: %w", err))
	}
	if err := os.WriteFile(scoreArtifact, append(encoded, '\n'), 0o600); err != nil {
		return finish(fmt.Errorf("write score artifact: %w", err))
	}
	evaluation.LogPaths = append(evaluation.LogPaths, scoreArtifact)
	evaluation.Score = record.Accuracy
	if record.Accuracy == 1 {
		evaluation.Reward = 1
		evaluation.Status = core.StatusSucceeded
		evaluation.VerifierStatus = core.StatusSucceeded
	}
	evaluation.Duration = time.Since(started)
	return evaluation, nil
}

// download prefers a bounded download when the sandbox offers one.
func download(ctx context.Context, sandbox runner.Sandbox, source, destination string, limit int64) error {
	if limited, ok := sandbox.(runner.LimitedDownloader); ok {
		return limited.DownloadLimit(ctx, source, destination, limit)
	}
	return sandbox.Download(ctx, source, destination)
}
