package suspend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeSuspender struct {
	mu                sync.Mutex
	calls             []string
	pauseErr, unpause error
	paused            chan struct{}
}

func newFake() *fakeSuspender { return &fakeSuspender{paused: make(chan struct{}, 16)} }

func (f *fakeSuspender) Pause(context.Context) error {
	f.mu.Lock()
	f.calls = append(f.calls, "pause")
	err := f.pauseErr
	f.mu.Unlock()
	f.paused <- struct{}{}
	return err
}

func (f *fakeSuspender) Unpause(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "unpause")
	return f.unpause
}

func (f *fakeSuspender) history() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, ",")
}

func waitPause(t *testing.T, f *fakeSuspender) {
	t.Helper()
	select {
	case <-f.paused:
	case <-time.After(2 * time.Second):
		t.Fatal("sandbox was not paused")
	}
}

func events(t *testing.T, record *bytes.Buffer) []Event {
	t.Helper()
	var out []Event
	for _, line := range strings.Split(strings.TrimSpace(record.String()), "\n") {
		if line == "" {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

// syncBuffer guards the record, which the timer goroutine also writes.
type syncBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}

func TestGatePausesWhenIdleAndUnpausesForNextCall(t *testing.T) {
	f := newFake()
	record := &syncBuffer{}
	g, err := New(Options{Suspender: f, Pause: true, Grace: 10 * time.Millisecond, Record: record})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	g.Acquire(ctx)
	g.Release()
	waitPause(t, f)
	g.Acquire(ctx)
	g.Release()
	waitPause(t, f)
	if err := g.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.history(); got != "pause,unpause,pause,unpause" {
		t.Fatalf("calls = %s", got)
	}
	got := events(t, &record.Buffer)
	reasons := make([]string, len(got))
	for i, e := range got {
		reasons[i] = e.Event + ":" + e.Reason
	}
	if strings.Join(reasons, ",") != "pause:idle,unpause:call,pause:idle,unpause:release" {
		t.Fatalf("events = %v", reasons)
	}
	if got[1].PausedMS <= 0 {
		t.Fatalf("unpause should report how long the sandbox was paused: %+v", got[1])
	}
}

func TestGateCallDuringGraceCancelsPause(t *testing.T) {
	f := newFake()
	g, _ := New(Options{Suspender: f, Pause: true, Grace: 50 * time.Millisecond})
	ctx := context.Background()
	g.Acquire(ctx)
	g.Release()
	time.Sleep(10 * time.Millisecond)
	g.Acquire(ctx) // arrives within the grace period
	time.Sleep(100 * time.Millisecond)
	if got := f.history(); got != "" {
		t.Fatalf("a call within the grace period must keep the sandbox running, calls = %s", got)
	}
	g.Release()
	waitPause(t, f)
	g.Close(ctx)
}

func TestGateWaitsForAllConcurrentCalls(t *testing.T) {
	f := newFake()
	g, _ := New(Options{Suspender: f, Pause: true})
	ctx := context.Background()
	g.Acquire(ctx)
	g.Acquire(ctx)
	g.Release()
	time.Sleep(30 * time.Millisecond)
	if got := f.history(); got != "" {
		t.Fatalf("paused with a call still in flight, calls = %s", got)
	}
	g.Release()
	waitPause(t, f)
	g.Close(ctx)
}

func TestGatePauseFailureDisablesSuspension(t *testing.T) {
	f := newFake()
	f.pauseErr = errors.New("pause refused")
	record := &syncBuffer{}
	g, _ := New(Options{Suspender: f, Pause: true, Record: record})
	ctx := context.Background()
	g.Acquire(ctx)
	g.Release()
	waitPause(t, f)
	g.Acquire(ctx)
	g.Release()
	time.Sleep(30 * time.Millisecond)
	if err := g.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.history(); got != "pause" {
		t.Fatalf("a failed pause must disable the gate, calls = %s", got)
	}
	got := events(t, &record.Buffer)
	if len(got) != 2 || got[0].Error == "" || got[1].Event != "disabled" {
		t.Fatalf("events = %+v", got)
	}
}

func TestGateCloseStopsPendingPause(t *testing.T) {
	f := newFake()
	g, _ := New(Options{Suspender: f, Pause: true, Grace: 30 * time.Millisecond})
	ctx := context.Background()
	g.Acquire(ctx)
	g.Release()
	if err := g.Close(ctx); err != nil {
		t.Fatal(err)
	}
	g.Acquire(ctx)
	g.Release()
	time.Sleep(80 * time.Millisecond)
	if got := f.history(); got != "" {
		t.Fatalf("a closed gate must leave the sandbox running, calls = %s", got)
	}
}

func TestGateCloseReportsUnpauseFailure(t *testing.T) {
	f := newFake()
	g, _ := New(Options{Suspender: f, Pause: true})
	ctx := context.Background()
	g.Acquire(ctx)
	g.Release()
	waitPause(t, f)
	f.mu.Lock()
	f.unpause = errors.New("unpause refused")
	f.mu.Unlock()
	if err := g.Close(ctx); err == nil {
		t.Fatal("Close must report that the sandbox could not be resumed")
	}
}

func TestGateWithoutPauseOnlyCounts(t *testing.T) {
	g, err := New(Options{Pause: false})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	g.Acquire(ctx)
	g.Release()
	time.Sleep(10 * time.Millisecond)
	if err := g.Close(ctx); err != nil {
		t.Fatal(err)
	}
}
