package hermes

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	native "github.com/hyscale-lab/aries/pkg/harness/hermes/gateway"
	"github.com/sirupsen/logrus"
)

const gatewayPort = 8642
const gatewayKeyPath = stagedRoot + "/gateway.key"

type gatewayExecution struct {
	client  *native.Client
	mu      sync.Mutex
	runID   string
	cancel  context.CancelFunc
	stopped bool
}

func newGatewayExecution(address string, token []byte) (*gatewayExecution, error) {
	client, err := native.New(address, string(token))
	if err != nil {
		return nil, err
	}
	return &gatewayExecution{client: client}, nil
}

// Cancellation is advisory. Deployment removal remains the termination proof.
func (execution *gatewayExecution) cancelRun(ctx context.Context) {
	execution.mu.Lock()
	execution.stopped = true
	if execution.cancel != nil {
		execution.cancel()
	}
	id := execution.runID
	execution.mu.Unlock()
	if id != "" {
		cancelCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_ = execution.client.Stop(cancelCtx, id)
		cancel()
	}
}

// stopUnsettled is the StopStatus of a stopped run that did not reach a native
// terminal state within the wait.
const stopUnsettled = "unsettled"

// runOutcome records the native service state and the submission-to-observation
// boundary. It makes no claim about CLI exit codes or upstream compute time.
// StopStatus and StopWaitMS are set only when ARIES stopped a run it had not
// seen end (deadline or cancellation): the native state Hermes reached after
// the stop, or stopUnsettled, and how long ARIES waited for it. EndedAt and
// DurationMS still mark the deadline, not the end of that wait.
type runOutcome struct {
	Status     string `json:"status"`
	EndReason  string `json:"end_reason"`
	RunID      string `json:"run_id,omitempty"`
	SessionID  string `json:"session_id"`
	StartedAt  string `json:"started_at"`
	EndedAt    string `json:"ended_at"`
	DurationMS int64  `json:"duration_ms"`
	StopStatus string `json:"stop_status,omitempty"`
	StopWaitMS int64  `json:"stop_wait_ms,omitempty"`
}

func (manager *Manager) runGateway(ctx context.Context, active *session, instruction string) (string, runOutcome, error) {
	started := time.Now()
	outcome := runOutcome{Status: string(core.StatusFailed), EndReason: "request_error", SessionID: "aries-" + active.AttemptID, StartedAt: started.UTC().Format(time.RFC3339Nano)}
	execution := active.gateway
	runCtx, cancel := context.WithTimeout(ctx, active.AgentTimeout)
	defer cancel()
	execution.mu.Lock()
	if execution.stopped {
		execution.mu.Unlock()
		outcome.EndReason = "stopped"
		outcome.EndedAt = time.Now().UTC().Format(time.RFC3339Nano)
		return "", outcome, errors.New("Hermes Gateway is stopped")
	}
	execution.cancel = cancel
	execution.mu.Unlock()
	id, err := execution.client.Submit(runCtx, instruction, outcome.SessionID)
	var result native.Run
	if err == nil {
		outcome.RunID = id
		execution.mu.Lock()
		execution.runID = id
		execution.mu.Unlock()
		result, err = execution.client.Wait(runCtx, id, 100*time.Millisecond)
		if err == nil {
			outcome.EndReason = result.Status
			if result.SessionID != "" && result.SessionID != outcome.SessionID {
				outcome.EndReason = "session_mismatch"
				err = errors.New("Hermes Gateway returned a different task session")
			} else if result.Status == "completed" {
				outcome.Status = string(core.StatusSucceeded)
			} else {
				if result.Status == "cancelled" || result.Status == "interrupted" {
					outcome.Status = string(core.StatusCanceled)
				}
				err = fmt.Errorf("Hermes Gateway run %s: %s", result.Status, result.Error)
				if outcome.Status == string(core.StatusCanceled) {
					err = errors.Join(context.Canceled, err)
				}
			}
		}
	}
	if result.Status == "" && errors.Is(err, context.DeadlineExceeded) {
		outcome.Status, outcome.EndReason = string(core.StatusCanceled), "deadline_exceeded"
	}
	if result.Status == "" && errors.Is(err, context.Canceled) {
		outcome.Status, outcome.EndReason = string(core.StatusCanceled), "canceled"
	}
	if err != nil {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		execution.cancelRun(cleanupCtx)
		cleanupCancel()
	}
	ended := time.Now()
	outcome.EndedAt = ended.UTC().Format(time.RFC3339Nano)
	outcome.DurationMS = ended.Sub(started).Milliseconds()
	if err != nil && result.Status == "" && outcome.RunID != "" {
		outcome.StopStatus = manager.settleStoppedRun(ctx, active, outcome.RunID)
		outcome.StopWaitMS = time.Since(ended).Milliseconds()
	}
	return result.Output, outcome, err
}

// settleStoppedRun waits, at most the cleanup timeout, for a run ARIES asked
// to stop to reach a native terminal state. Hermes marks the run cancelled
// only after run_conversation returns, and by then on_session_end has ended
// the turn's root span (hermes_otel's "agent" span, marked
// hermes.session.interrupted) and committed it to the span store. Dumping
// spans before that loses the root, leaving every span of the turn pointing at
// a parent the dump does not contain. It returns the terminal status, or
// stopUnsettled when Hermes did not settle in time.
func (manager *Manager) settleStoppedRun(ctx context.Context, active *session, runID string) string {
	waitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), manager.runtime.Options.CleanupTimeout)
	defer cancel()
	run, err := active.gateway.client.Wait(waitCtx, runID, 100*time.Millisecond)
	if err != nil {
		manager.runtime.Options.Logger.WithContext(ctx).WithFields(logrus.Fields{"task_id": active.TaskID, "run_id": runID}).WithError(err).Warn("stopped Hermes run did not settle; spans of its open turn are missing")
		return stopUnsettled
	}
	return run.Status
}
