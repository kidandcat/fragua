package router

import (
	"testing"
	"time"

	"github.com/mentasystems/fragua/internal/core"
)

// A net that does not finish must leave the board exactly as it found it.
// The partial passes commit their short hops as they go; when the net later
// died those stubs stayed behind as dangling pads, and every further attempt
// at the same net laid them a second time on top of themselves.
func TestFailedNetLeavesNoPartialCopper(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(40), core.FromMM(16)))
	b.Outline = &o
	// P1/P2 sit close enough for a clear hop; P3 is walled in by a
	// no-copper keepout, so the net can never finish.
	b.AddFootprint(footprint("P1", 5, 8, []core.Pad{pad("1", 0, 0, "N")}))
	b.AddFootprint(footprint("P2", 9, 8, []core.Pad{pad("1", 0, 0, "N")}))
	b.AddFootprint(footprint("P3", 34, 8, []core.Pad{pad("1", 0, 0, "N")}))
	wall := core.RectFromCorners(core.NewPoint(core.FromMM(20), core.FromMM(-1)),
		core.NewPoint(core.FromMM(23), core.FromMM(17)))
	b.Keepouts = []core.Keepout{{ID: core.NewID(), Rect: &wall, NoCopper: true}}

	opts := applyFabCeiling(b, DefaultOptions())
	opts.widths = newNetWidths(b, opts)
	g := newGrid(b, opts)
	pads := []padLoc{
		{ref: "P1", p: core.NewPoint(core.FromMM(5), core.FromMM(8))},
		{ref: "P2", p: core.NewPoint(core.FromMM(9), core.FromMM(8))},
		{ref: "P3", p: core.NewPoint(core.FromMM(34), core.FromMM(8))},
	}
	out := routeNetAt(b, g, "N", pads, opts, time.Now().Add(5*time.Second), true)
	if out.Status == "ok" {
		t.Skip("the wall did not block the net; nothing to assert")
	}
	if n := len(b.Traces); n != 0 {
		t.Fatalf("a failed net left %d trace(s) behind: %+v", n, b.Traces)
	}
	if n := len(b.Vias); n != 0 {
		t.Fatalf("a failed net left %d via(s) behind", n)
	}

	// And a second attempt must not stack a duplicate of the same stub.
	_ = routeNetAt(b, g, "N", pads, opts, time.Now().Add(5*time.Second), true)
	if n := len(b.Traces); n != 0 {
		t.Fatalf("a retry of a failed net left %d trace(s) behind", n)
	}
}

// Route as a whole must not ship a board carrying stubs of nets it reports
// as failed: those are dangling pads, not connections.
func TestRouteShipsNoCopperForFailedNets(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(40), core.FromMM(16)))
	b.Outline = &o
	b.AddFootprint(footprint("P1", 5, 8, []core.Pad{pad("1", 0, 0, "N")}))
	b.AddFootprint(footprint("P2", 9, 8, []core.Pad{pad("1", 0, 0, "N")}))
	b.AddFootprint(footprint("P3", 34, 8, []core.Pad{pad("1", 0, 0, "N")}))
	wall := core.RectFromCorners(core.NewPoint(core.FromMM(20), core.FromMM(-1)),
		core.NewPoint(core.FromMM(23), core.FromMM(17)))
	b.Keepouts = []core.Keepout{{ID: core.NewID(), Rect: &wall, NoCopper: true}}

	opts := DefaultOptions()
	opts.MaxSeconds = 5
	rep := Route(b, opts)
	failed := map[string]bool{}
	for _, n := range rep.PerNet {
		if n.Outcome.Status != "ok" {
			failed[n.Net] = true
		}
	}
	if !failed["N"] {
		t.Skip("the wall did not block the net; nothing to assert")
	}
	for _, tr := range b.Traces {
		if failed[tr.Net] {
			t.Fatalf("net %q is reported failed but has copper at %.2f,%.2f",
				tr.Net, tr.Start.X.ToMM(), tr.Start.Y.ToMM())
		}
	}
}
