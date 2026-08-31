package router

import (
	"testing"

	"github.com/mentasystems/fragua/internal/core"
)

// Two passes can each decide the same net changes layer at the same cell.
// The second drill is not a second connection, it is the same hole ordered
// twice, and DRC rightly called it hole_to_hole -0.300 mm against itself —
// which every drone-x pack had to have deleted by hand before fab.
func TestDuplicateViasAreDropped(t *testing.T) {
	via := func(x, y float64, net string) core.Via {
		return core.Via{
			ID:       core.NewID(),
			Position: core.NewPoint(core.FromMM(x), core.FromMM(y)),
			Drill:    core.FromMM(0.3),
			Diameter: core.FromMM(0.6),
			Net:      net,
		}
	}
	b := core.NewBoard()
	b.Vias = []core.Via{
		via(10, 10, "N1"),
		via(10, 10, "N1"),    // the same hole ordered twice
		via(10, 10, "N2"),    // a different net at the same point is not a duplicate
		via(10.05, 10, "N1"), // a hair apart is viaSiteOK's business, not ours
		via(20, 20, "N1"),
	}
	dropDuplicateVias(b)
	if len(b.Vias) != 4 {
		t.Fatalf("expected the one exact duplicate gone, got %d vias", len(b.Vias))
	}
	// The survivor is the first one, so the result does not depend on order
	// of iteration anywhere.
	if b.Vias[0].Position.X != core.FromMM(10) || b.Vias[1].Net != "N2" {
		t.Fatalf("dedupe must keep the first of each hole in order, got %+v", b.Vias)
	}
	dropDuplicateVias(b)
	if len(b.Vias) != 4 {
		t.Fatal("dedupe must be idempotent")
	}
}

// A routed board must never ship two barrels on the same point and net.
func TestRouteShipsNoDuplicateVias(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(60), core.FromMM(40)))
	b.Outline = &o
	for i, p := range [][2]float64{{8, 8}, {30, 6}, {52, 10}, {50, 32}, {10, 30}} {
		b.AddFootprint(footprint("T"+string(rune('1'+i)), p[0], p[1], []core.Pad{
			pad("1", 0, 0, "BUS"),
			pad("2", 0, 2.54, "GND"),
		}))
	}
	b.Pours = []core.Pour{{Net: "GND", Layer: core.LayerTop}}
	opts := DefaultOptions()
	opts.MaxSeconds = 30
	Route(b, opts)
	seen := map[[3]any]bool{}
	for _, v := range b.Vias {
		k := [3]any{v.Position.X, v.Position.Y, v.Net}
		if seen[k] {
			t.Fatalf("duplicate via shipped at %v on %s", v.Position, v.Net)
		}
		seen[k] = true
	}
}
