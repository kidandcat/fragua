//go:build lean

package drc

import (
	"path/filepath"
	"testing"

	"github.com/mentasystems/fragua/internal/core"
	"github.com/mentasystems/fragua/internal/leanbridge"
)

// TestStressBoardUsesLean runs DRC on the routed stress board. Trace/trace
// clearance is the wired call site, so the helper must be asked about real
// copper.
func TestStressBoardUsesLean(t *testing.T) {
	before := leanbridge.RPCCalls.Load()
	path := filepath.Join("..", "..", "stress", "rp2040-minimal.fragua")
	p, err := core.LoadFromPath(path)
	if err != nil {
		t.Fatal(err)
	}
	p.RLock()
	rep := Check(p.Board(), p.Schematic(), DefaultOptions())
	p.RUnlock()
	calls := leanbridge.RPCCalls.Load() - before
	if calls == 0 {
		t.Fatal("DRC made no fragua-geom calls")
	}
	t.Logf("%s via %d geom calls", rep.Summary(), calls)
}
