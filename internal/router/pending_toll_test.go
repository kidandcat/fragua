package router

import (
	"testing"
	"time"

	"github.com/mentasystems/fragua/internal/core"
)

func tollGrid(t *testing.T, pending ...string) (*grid, []padObs) {
	t.Helper()
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(40), core.FromMM(20)))
	b.Outline = &o
	// A short pin row of an unrouted connector, plus one lone pad.
	b.AddFootprint(footprint("J1", 20, 10, []core.Pad{
		pad("1", 0, -2.54, "S1"), pad("2", 0, 0, "S2"), pad("3", 0, 2.54, "S3"),
	}))
	b.AddFootprint(footprint("R9", 5, 5, []core.Pad{pad("1", 0, 0, "LONE")}))
	opts := DefaultOptions()
	opts.pending = map[string]bool{}
	for _, n := range pending {
		opts.pending[n] = true
	}
	g := newGrid(b, opts)
	return g, g.pads
}

// Copper laid across the pads of a net that still has to be routed is what
// walls a connector in. The search pays a toll to cross one; nothing else.
func TestPendingPenaltyPricesUnroutedPadsOnly(t *testing.T) {
	g, _ := tollGrid(t, "S1", "S2", "S3", "LONE")
	cx, cy, ok := g.worldToCell(core.FromMM(20), core.FromMM(10))
	if !ok {
		t.Fatal("pin cell off grid")
	}
	if got := g.pendingPenalty(cx, cy, 0, "OTHER"); got <= 0 {
		t.Fatalf("crossing an unrouted pin face must cost something, got %v", got)
	}
	// The same cell is free for the net that owns those pads.
	if got := g.pendingPenalty(cx, cy, 0, "S2"); got != 0 {
		t.Fatalf("a net must not be charged for its own pads, got %v", got)
	}
	// Far from every pad there is nothing to protect.
	fx, fy, _ := g.worldToCell(core.FromMM(35), core.FromMM(18))
	if got := g.pendingPenalty(fx, fy, 0, "OTHER"); got != 0 {
		t.Fatalf("open field must be free, got %v", got)
	}
	// The toll rises with the crowding and is capped, so no cell can ever
	// cost more than the way round the board.
	ceiling := pendingEscapeToll * pendingTollMaxUnits
	bx, by, _ := g.worldToCell(core.FromMM(20), core.FromMM(11.27))
	if face := g.pendingPenalty(bx, by, 0, "OTHER"); face > ceiling+1e-9 {
		t.Fatalf("toll %v exceeds the cap %v", face, ceiling)
	}
	lx, ly, _ := g.worldToCell(core.FromMM(5), core.FromMM(5))
	if lone := g.pendingPenalty(lx, ly, 0, "OTHER"); lone <= 0 || lone > ceiling+1e-9 {
		t.Fatalf("a lone unrouted pad must cost something inside the cap, got %v", lone)
	}
}

// Once a net is routed its pads stop being protected, or the last nets on the
// board would be routing through a field of tolls nobody is waiting on.
func TestRoutedPadsCarryNoToll(t *testing.T) {
	g, _ := tollGrid(t) // nothing pending
	cx, cy, _ := g.worldToCell(core.FromMM(20), core.FromMM(10))
	if got := g.pendingPenalty(cx, cy, 0, "OTHER"); got != 0 {
		t.Fatalf("with nothing left to route there is nothing to protect, got %v", got)
	}
}

// The toll must reach the search: a rail whose straight line runs down an
// unrouted connector's pin face has to prefer the way round.
func TestSearchPrefersTheWayRoundAnUnroutedPinFace(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(40), core.FromMM(24)))
	b.Outline = &o
	b.AddFootprint(footprint("J1", 20, 12, []core.Pad{
		pad("1", 0, -3.81, "S1"), pad("2", 0, -1.27, "S2"),
		pad("3", 0, 1.27, "S3"), pad("4", 0, 3.81, "S4"),
	}))
	opts := applyFabCeiling(b, DefaultOptions())
	opts.widths = newNetWidths(b, opts)

	run := func(pending map[string]bool) float64 {
		o := opts
		o.pending = pending
		g := newGrid(b, o)
		from := core.NewPoint(core.FromMM(4), core.FromMM(12))
		to := core.NewPoint(core.FromMM(36), core.FromMM(12))
		src := []cellKey{}
		if cx, cy, ok := g.worldToCell(from.X, from.Y); ok {
			src = append(src, cellKey{cx, cy, 0})
		}
		path, ok := g.aStarMultiAt(src, to, 0, false, "RAIL", time.Time{}, false)
		if !ok {
			t.Fatal("the rail must still route")
		}
		// How close the path comes to the pin column.
		best := 1e9
		for _, p := range path {
			if d := absF(p.x.ToMM() - 20); d < best {
				best = d
			}
		}
		return best
	}
	free := run(nil)
	tolled := run(map[string]bool{"S1": true, "S2": true, "S3": true, "S4": true})
	if tolled <= free {
		t.Fatalf("the toll did not push the rail off the pin face: %.2f mm with toll, %.2f mm without", tolled, free)
	}
}

func absF(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
