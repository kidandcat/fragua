package router

import (
	"testing"

	"github.com/mentasystems/fragua/internal/core"
)

// clearance= used to be a dead knob: applyFabCeiling overwrote it with the
// fab minimum, so `route clearance=0.28` produced byte-identical copper to
// the default. The fab minimum is a floor, not a fixed value.
func TestClearanceOptionIsHonouredAboveTheFabFloor(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(40), core.FromMM(20)))
	b.Outline = &o
	b.AddFootprint(footprint("R1", 10, 10, []core.Pad{pad("1", -1, 0, "VCC"), pad("2", 1, 0, "GND")}))
	fab := core.ActiveFabRules(b)
	if fab.MinClearanceMM <= 0 {
		t.Skip("board has no fab profile")
	}
	wide := fab.MinClearanceMM + 0.15

	asked := ParseOptions(DefaultOptions(), "clearance=0.28")
	if !asked.ClearanceSet || asked.ClearanceMM != 0.28 {
		t.Fatalf("ParseOptions dropped clearance=: %+v", asked)
	}

	o1 := DefaultOptions()
	o1.ClearanceMM, o1.ClearanceSet = wide, true
	if got := applyFabCeiling(b, o1).ClearanceMM; got != wide {
		t.Fatalf("an explicit clearance above the fab floor must survive: got %v, want %v", got, wide)
	}

	// Tighter than the process allows still collapses onto the floor.
	o2 := DefaultOptions()
	o2.ClearanceMM, o2.ClearanceSet = fab.MinClearanceMM/2, true
	if got := applyFabCeiling(b, o2).ClearanceMM; got != fab.MinClearanceMM {
		t.Fatalf("a request under the fab floor must clamp to it: got %v", got)
	}

	// No request at all: the fab floor, exactly as before.
	if got := applyFabCeiling(b, DefaultOptions()).ClearanceMM; got != fab.MinClearanceMM {
		t.Fatalf("the default must stay at the fab floor: got %v", got)
	}
}

// The knob has to reach the copper, not just the options struct.
func TestWiderClearanceChangesTheRoutedCopper(t *testing.T) {
	build := func() *core.Board {
		b := core.NewBoard()
		o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(30), core.FromMM(16)))
		b.Outline = &o
		b.AddFootprint(footprint("R1", 6, 8, []core.Pad{pad("1", -1, 0, "A"), pad("2", 1, 0, "B")}))
		b.AddFootprint(footprint("R2", 14, 8, []core.Pad{pad("1", -1, 0, "B"), pad("2", 1, 0, "C")}))
		b.AddFootprint(footprint("R3", 22, 8, []core.Pad{pad("1", -1, 0, "C"), pad("2", 1, 0, "A")}))
		return b
	}
	base := build()
	Route(base, DefaultOptions())
	wide := build()
	o := ParseOptions(DefaultOptions(), "clearance=1.2")
	Route(wide, o)
	same := len(base.Traces) == len(wide.Traces)
	if same {
		for i := range base.Traces {
			if base.Traces[i].Start != wide.Traces[i].Start || base.Traces[i].End != wide.Traces[i].End {
				same = false
				break
			}
		}
	}
	if same {
		t.Fatal("clearance=1.2 produced identical copper to the default: the knob is dead")
	}
}
