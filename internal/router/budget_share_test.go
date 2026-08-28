package router

import (
	"strings"
	"testing"
	"time"

	"github.com/mentasystems/fragua/internal/core"
)

// A net used to get a flat 3 s of A* no matter how much clock the call had,
// so a 600 s route starved a corner-to-corner power hop and then reported it
// as "unreachable". The slice must scale with what is left.
func TestNetBudgetSharesTheRemainingClock(t *testing.T) {
	big := netBudget(600*time.Second, 22, 4)
	if big <= minNetBudget {
		t.Fatalf("a 600 s budget must give a net more than the %v floor, got %v", minNetBudget, big)
	}
	if big > maxNetBudget {
		t.Fatalf("per-net slice must stay under %v, got %v", maxNetBudget, big)
	}
	// Half the remainder is reserved for the repair passes.
	if want := 300 * time.Second / 22; big > want+time.Millisecond {
		t.Fatalf("slice %v exceeds the half-of-remaining share %v", big, want)
	}
	// A fat bus crosses the board, so it gets a double slice.
	if fat := netBudget(600*time.Second, 22, 12); fat <= big {
		t.Fatalf("a >=8-pad bus must not get less than a small net: %v vs %v", fat, big)
	}
	// The floor still holds when the clock is nearly gone, and no slice may
	// ever exceed the time actually left.
	if got := netBudget(2*time.Second, 40, 2); got != 2*time.Second {
		t.Fatalf("slice must be capped by the clock left, got %v", got)
	}
	if got := netBudget(20*time.Second, 200, 2); got != minNetBudget {
		t.Fatalf("slice must never drop under the %v floor, got %v", minNetBudget, got)
	}
}

// Running out of clock inside A* is not the same as a pad being walled in.
// Reporting "unreachable" for a timeout sent readers hunting for a geometric
// blockage that was not there.
func TestBudgetExhaustionIsNotReportedAsUnreachable(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(60), core.FromMM(40)))
	b.Outline = &o
	b.AddFootprint(footprint("R1", 5, 5, []core.Pad{pad("1", 0, 0, "N1")}))
	b.AddFootprint(footprint("R2", 55, 35, []core.Pad{pad("1", 0, 0, "N1")}))
	opts := applyFabCeiling(b, DefaultOptions())
	opts.widths = newNetWidths(b, opts)
	g := newGrid(b, opts)
	pads := []padLoc{
		{ref: "R1", p: core.NewPoint(core.FromMM(5), core.FromMM(5))},
		{ref: "R2", p: core.NewPoint(core.FromMM(55), core.FromMM(35))},
	}
	// A* that stops on the clock must say so, so the net is reported as
	// out of budget and not as walled in.
	if _, ok := g.aStarMultiAt(g.padSources(pads[0]), pads[1].p, 0, false, "N1",
		time.Now().Add(-time.Second), true); ok {
		t.Fatal("expected the expired deadline to stop the search")
	}
	if !g.timedOut {
		t.Fatal("A* stopped on the clock but did not record it")
	}
	out := routeNetFrom(b, g, "N1", pads, 0, nil, opts, time.Now().Add(-time.Second), true)
	if out.Status == "ok" {
		t.Fatalf("expected the expired deadline to stop the net, got %+v", out)
	}
	if strings.HasPrefix(out.Reason, "unreachable") {
		t.Fatalf("a spent budget must not be reported as unreachable: %q", out.Reason)
	}
	// The same board with a real budget routes, so the pad was never
	// unreachable in the first place.
	if out := routeNetFrom(b, g, "N1", pads, 0, nil, opts, time.Now().Add(30*time.Second), true); out.Status != "ok" {
		t.Fatalf("the net is routable given the clock: %+v", out)
	}
	if g.timedOut {
		t.Fatal("a successful search must clear the timeout flag")
	}
}
