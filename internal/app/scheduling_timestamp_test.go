package app

import (
	"context"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
)

func TestSchedulingTimestampIncludesCapacityWait(t *testing.T) {
	// The first worker blocks capacity. A second occurrence must retain the
	// admission intent timestamp preceding that capacity wait.
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	done := make(chan core.RunResult, 1)
	go func() {
		r, err := runProfile(context.Background(), "fixture", "run", []string{"task", "task"}, 1, 0, nil, func(_ context.Context, o taskOccurrence) (core.RunResult, error) {
			started := time.Now()
			if o.executionID == "task-001" {
				close(firstStarted)
				<-releaseFirst
			}
			return core.RunResult{Tasks: []core.TaskResult{{TaskID: o.executionID, StartedAt: started}}}, nil
		})
		if err != nil {
			t.Errorf("run: %v", err)
		}
		done <- r
	}()
	<-firstStarted
	time.Sleep(20 * time.Millisecond)
	close(releaseFirst)
	r := <-done
	if len(r.Tasks) != 2 {
		t.Fatal("missing scheduled occurrence")
	}
	for _, task := range r.Tasks {
		if task.ScheduledAt.IsZero() || task.StartedAt.Before(task.ScheduledAt) {
			t.Fatalf("invalid schedule timestamps %+v", task)
		}
	}
	if wait := r.Tasks[1].StartedAt.Sub(r.Tasks[1].ScheduledAt); wait < 10*time.Millisecond {
		t.Fatalf("capacity wait absent: %v", wait)
	}
}
