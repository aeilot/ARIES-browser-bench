package runner

import (
	"context"
	"fmt"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/sirupsen/logrus"
)

// phaseRecorder observes the task's existing sequential lifecycle. Each
// predecessor is the phase this task actually completed before the next began;
// no process-global state or timing-based dependency inference is involved.
func (r *Runner) phaseRecorder(ctx context.Context, taskID string) func(string) func(error) {
	previous := ""
	return func(phase string) func(error) {
		started := time.Now()
		id := fmt.Sprintf("%s/%s/%d", taskID, phase, started.UnixNano())
		entry := r.logger.WithContext(context.WithoutCancel(ctx)).WithFields(logrus.Fields{
			"task_id": taskID, "phase": phase, "phase_id": id, "depends_on": previous,
		})
		entry.WithTime(started).WithField("event", "start").Info("task phase")
		return func(err error) {
			ended := time.Now()
			status := core.StatusSucceeded
			if err != nil {
				status = failureStatus(err)
			}
			entry.WithTime(ended).WithFields(logrus.Fields{"event": "end", "status": status, "duration_ms": float64(ended.Sub(started)) / float64(time.Millisecond)}).Info("task phase")
			previous = id
		}
	}
}
