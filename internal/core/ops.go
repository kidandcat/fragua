package core

import (
	"sync"
	"time"
)

// OpTracker records the long-running operation in flight (route, auto-place,
// compact) so the UI can show progress and ask for a cancel. One at a time:
// the project is single-writer, and every long op holds the write lock.
type OpTracker struct {
	mu        sync.Mutex
	op        string
	started   time.Time
	cancelled bool
	seq       uint64
}

// Op is a snapshot of the operation in flight.
type Op struct {
	Name      string `json:"op"`
	ElapsedMS int64  `json:"elapsed_ms"`
	Cancelled bool   `json:"cancelled"`
}

// Ops returns the project's operation tracker. Deliberately lock-free: the
// long ops hold p.mu for their whole run, so a cancel that had to take it
// could never arrive in time.
func (p *Project) Ops() *OpTracker { return &p.ops }

// Begin marks name as running and returns the function that ends it. A
// cancel request from a previous run never leaks into the next one.
func (t *OpTracker) Begin(name string) func() {
	t.mu.Lock()
	t.op = name
	t.started = time.Now()
	t.cancelled = false
	t.seq++
	seq := t.seq
	t.mu.Unlock()
	return func() {
		t.mu.Lock()
		if t.seq == seq {
			t.op = ""
			t.cancelled = false
		}
		t.mu.Unlock()
	}
}

// Cancel asks the operation in flight to stop. Returns the op it hit, or ""
// when nothing was running. The engines are anytime: a cancelled run keeps
// the work it has already committed.
func (t *OpTracker) Cancel() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.op == "" {
		return ""
	}
	t.cancelled = true
	return t.op
}

// Cancelled reports whether the op in flight was asked to stop. Safe to pass
// straight to an engine as its cancel predicate.
func (t *OpTracker) Cancelled() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cancelled
}

// Current returns the running op, or ok=false when the project is idle.
func (t *OpTracker) Current() (Op, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.op == "" {
		return Op{}, false
	}
	return Op{
		Name:      t.op,
		ElapsedMS: time.Since(t.started).Milliseconds(),
		Cancelled: t.cancelled,
	}, true
}

// Elapsed is how long the op in flight has been running (0 when idle).
func (t *OpTracker) Elapsed() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.op == "" {
		return 0
	}
	return time.Since(t.started)
}
