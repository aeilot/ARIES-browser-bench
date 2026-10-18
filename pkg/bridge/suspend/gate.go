// Package suspend freezes a sandbox while no tool call is using it.
//
// A Gate counts the tool calls in flight for one sandbox. When the count drops
// to zero it waits a grace period, then pauses the sandbox; the next call
// unpauses it before proceeding. Every pause and unpause is recorded. A failed
// pause or unpause disables the gate for the rest of the occurrence, leaving
// the sandbox running, so suspension never fails a task.
package suspend

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"
)

// Suspender pauses and resumes one sandbox's processes.
type Suspender interface {
	Pause(context.Context) error
	Unpause(context.Context) error
}

// Event is one line of suspend.jsonl.
type Event struct {
	Time  time.Time `json:"time"`
	Event string    `json:"event"` // pause, unpause or disabled
	// Reason is idle (grace period elapsed), call (a tool call arrived) or
	// release (bridge access is closing before evaluation).
	Reason string `json:"reason"`
	// LatencyMS is how long the pause or unpause request took.
	LatencyMS float64 `json:"latency_ms"`
	// PausedMS is, for an unpause, how long the sandbox had been paused.
	PausedMS float64 `json:"paused_ms,omitempty"`
	Error    string  `json:"error,omitempty"`
}

// Options configures a Gate. With Pause false the gate only counts calls,
// which isolates the cost of routing calls through the bridge.
type Options struct {
	Suspender Suspender
	Pause     bool
	Grace     time.Duration
	Record    io.Writer
	// OperationTimeout bounds one pause or unpause request.
	OperationTimeout time.Duration
	now              func() time.Time
}

type Gate struct {
	mu       sync.Mutex
	options  Options
	enabled  bool
	closed   bool
	inflight int
	paused   bool
	pausedAt time.Time
	timer    *time.Timer
	// generation invalidates a pending grace timer whenever a call starts.
	generation uint64
	encoder    *json.Encoder
	recordErr  error
}

func New(options Options) (*Gate, error) {
	if options.Pause && options.Suspender == nil {
		return nil, errors.New("suspend gate requires a suspender to pause")
	}
	if options.Grace < 0 {
		return nil, errors.New("suspend grace period must not be negative")
	}
	if options.OperationTimeout <= 0 {
		options.OperationTimeout = 30 * time.Second
	}
	if options.now == nil {
		options.now = time.Now
	}
	g := &Gate{options: options, enabled: options.Pause}
	if options.Record != nil {
		g.encoder = json.NewEncoder(options.Record)
	}
	return g, nil
}

// Acquire marks a call as started, unpausing the sandbox first if needed.
// It never fails the call: an unpause error disables the gate.
func (g *Gate) Acquire(ctx context.Context) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.inflight++
	g.generation++
	if g.timer != nil {
		g.timer.Stop()
		g.timer = nil
	}
	if g.paused {
		g.unpause(ctx, "call")
	}
}

// Release marks a call as finished and, once none remain, schedules a pause.
func (g *Gate) Release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.inflight > 0 {
		g.inflight--
	}
	if g.inflight > 0 || !g.enabled || g.closed || g.paused {
		return
	}
	g.generation++
	generation := g.generation
	// The pause runs on the timer's goroutine even with no grace, so that a
	// finished call returns to its caller without waiting for the runtime.
	g.timer = time.AfterFunc(g.options.Grace, func() { g.expire(generation) })
}

func (g *Gate) expire(generation uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if generation != g.generation || g.inflight > 0 || !g.enabled || g.closed || g.paused {
		return
	}
	g.timer = nil
	ctx, cancel := context.WithTimeout(context.Background(), g.options.OperationTimeout)
	defer cancel()
	started := g.options.now()
	err := g.options.Suspender.Pause(ctx)
	event := Event{Time: started, Event: "pause", Reason: "idle", LatencyMS: ms(g.options.now().Sub(started))}
	if err != nil {
		event.Error = err.Error()
		g.record(event)
		g.disable(started)
		return
	}
	g.paused = true
	g.pausedAt = g.options.now()
	g.record(event)
}

// unpause requires g.mu.
func (g *Gate) unpause(ctx context.Context, reason string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), g.options.OperationTimeout)
	defer cancel()
	started := g.options.now()
	err := g.options.Suspender.Unpause(ctx)
	event := Event{Time: started, Event: "unpause", Reason: reason, LatencyMS: ms(g.options.now().Sub(started)), PausedMS: ms(started.Sub(g.pausedAt))}
	g.paused = false
	if err != nil {
		event.Error = err.Error()
		g.record(event)
		g.disable(started)
		return err
	}
	g.record(event)
	return nil
}

func (g *Gate) disable(at time.Time) {
	if g.enabled {
		g.enabled = false
		g.record(Event{Time: at, Event: "disabled", Reason: "error"})
	}
}

// Close stops scheduling pauses and leaves the sandbox running, as evaluation
// needs it. It reports an unpause failure and any error writing the record.
func (g *Gate) Close(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return g.recordErr
	}
	g.closed = true
	g.generation++
	if g.timer != nil {
		g.timer.Stop()
		g.timer = nil
	}
	var err error
	if g.paused {
		err = g.unpause(ctx, "release")
	}
	return errors.Join(err, g.recordErr)
}

func (g *Gate) record(event Event) {
	if g.encoder == nil || g.recordErr != nil {
		return
	}
	g.recordErr = g.encoder.Encode(event)
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
