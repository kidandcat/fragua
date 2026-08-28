package router

import (
	"testing"

	"github.com/mentasystems/fragua/internal/core"
)

func TestStitchGeneratesViasForRequestedPour(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(20), core.FromMM(16)))
	b.Outline = &o
	b.Pours = []core.Pour{{
		Net: "GND", Layer: core.LayerTop,
		Stitching: &core.StitchPolicy{Enabled: true, PitchMM: 5},
	}}
	n := "GND"
	b.AddFootprint(&core.Footprint{
		ID: core.NewID(), Reference: "R1",
		Position: core.NewPoint(core.FromMM(4), core.FromMM(4)),
		Layer:    core.LayerBottom,
		Pads: []core.Pad{{
			Number: "1", Size: [2]core.Length{core.FromMM(1), core.FromMM(1)},
			Layer: core.LayerBottom, Net: &n,
		}},
	})
	added := StitchIsolatedPads(b, DefaultOptions())
	if added == 0 || len(b.Vias) == 0 {
		t.Fatalf("expected stitch vias, added=%d vias=%d", added, len(b.Vias))
	}
	for _, v := range b.Vias {
		if v.Net != "GND" {
			t.Fatalf("via net %s", v.Net)
		}
	}
}

// The pour stitcher validated a new via with a trace-only clearance check
// that could never fail, so a lattice barrel landed on top of a signal trace.
func TestStitchKeepsViasOffForeignCopper(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(30), core.FromMM(20)))
	b.Outline = &o
	b.AddFootprint(footprint("R1", 6, 10, []core.Pad{
		pad("1", -1, 0, "SIG"),
		pad("2", 1, 0, "GND"),
	}))
	b.AddFootprint(footprint("R2", 24, 10, []core.Pad{
		pad("1", -1, 0, "SIG"),
		pad("2", 1, 0, "GND"),
	}))
	// A SIG trace running straight along a stitch lattice row
	// (the grid starts at 0.80 mm and steps 2.54 mm, so y = 10.96 mm
	// puts a barrel dead on the copper).
	b.Traces = []core.Trace{{
		ID: core.NewID(), Net: "SIG", Layer: core.LayerTop, Width: core.FromMM(0.25),
		Start: core.NewPoint(core.FromMM(5), core.FromMM(10.96)),
		End:   core.NewPoint(core.FromMM(23), core.FromMM(10.96)),
	}}
	b.Pours = []core.Pour{
		{ID: core.NewID(), Net: "GND", Layer: core.LayerTop},
		{ID: core.NewID(), Net: "GND", Layer: core.LayerBottom},
	}
	StitchIsolatedPads(b, DefaultOptions())

	minClr := commitClearance(b)
	for _, v := range b.Vias {
		vx, vy, vr := v.Position.X.ToMM(), v.Position.Y.ToMM(), v.Diameter.ToMM()/2
		for _, tr := range b.Traces {
			if tr.Net == v.Net {
				continue
			}
			gap := distPointSeg(vx, vy, tr.Start.X.ToMM(), tr.Start.Y.ToMM(),
				tr.End.X.ToMM(), tr.End.Y.ToMM()) - vr - tr.Width.ToMM()/2
			if gap+1e-6 < minClr {
				t.Fatalf("stitch via %s at (%.2f,%.2f) is %.3f mm from trace %s, needs %.3f mm",
					v.Net, vx, vy, gap, tr.Net, minClr)
			}
		}
	}
}
