package core

import "testing"

func TestOpTrackerLifecycle(t *testing.T) {
	p := NewProject("ops")
	if _, ok := p.Ops().Current(); ok {
		t.Fatal("a fresh project is idle")
	}
	if op := p.Ops().Cancel(); op != "" {
		t.Fatalf("nothing to cancel, got %q", op)
	}
	end := p.Ops().Begin("route")
	op, ok := p.Ops().Current()
	if !ok || op.Name != "route" {
		t.Fatalf("current op %+v ok=%v", op, ok)
	}
	if p.Ops().Cancelled() {
		t.Fatal("a fresh op is not cancelled")
	}
	if got := p.Ops().Cancel(); got != "route" {
		t.Fatalf("cancel returned %q", got)
	}
	if !p.Ops().Cancelled() {
		t.Fatal("cancel must be visible to the engine")
	}
	end()
	if _, ok := p.Ops().Current(); ok {
		t.Fatal("op should be idle after end")
	}
	// A cancel from the finished run must not kill the next one.
	end = p.Ops().Begin("auto-place")
	if p.Ops().Cancelled() {
		t.Fatal("a stale cancel leaked into the next op")
	}
	end()
}

// A stale end() from an op that was superseded must not clear the live one.
func TestOpTrackerStaleEnd(t *testing.T) {
	p := NewProject("ops")
	stale := p.Ops().Begin("route")
	end := p.Ops().Begin("compact")
	stale()
	op, ok := p.Ops().Current()
	if !ok || op.Name != "compact" {
		t.Fatalf("live op lost to a stale end: %+v ok=%v", op, ok)
	}
	end()
}
