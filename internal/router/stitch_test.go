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

// The stitcher tested "is this point in the pour" against the board bounding
// box alone, so it sprayed its lattice straight through no-copper keepouts —
// under a module antenna, in drone-x's case — and through milled cutouts.
func TestStitchKeepsViasOutOfVoids(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(40), core.FromMM(40)))
	b.Outline = &o
	b.Pours = []core.Pour{{
		Net: "GND", Layer: core.LayerTop,
		Stitching: &core.StitchPolicy{Enabled: true, PitchMM: 2},
	}}
	ko := core.RectFromCorners(core.NewPoint(core.FromMM(25), core.FromMM(25)), core.NewPoint(core.FromMM(35), core.FromMM(35)))
	b.Keepouts = append(b.Keepouts, core.Keepout{ID: core.NewID(), Rect: &ko, NoCopper: true})
	b.Cutouts = append(b.Cutouts, core.Cutout{ID: core.NewID(), Polygon: []core.Point{
		core.NewPoint(core.FromMM(5), core.FromMM(5)),
		core.NewPoint(core.FromMM(15), core.FromMM(5)),
		core.NewPoint(core.FromMM(15), core.FromMM(15)),
		core.NewPoint(core.FromMM(5), core.FromMM(15)),
	}})
	n := "GND"
	b.AddFootprint(&core.Footprint{
		ID: core.NewID(), Reference: "R1",
		Position: core.NewPoint(core.FromMM(20), core.FromMM(2)),
		Layer:    core.LayerBottom,
		Pads: []core.Pad{{
			Number: "1", Size: [2]core.Length{core.FromMM(1), core.FromMM(1)},
			Layer: core.LayerBottom, Net: &n,
		}},
	})
	StitchIsolatedPads(b, DefaultOptions())
	if len(b.Vias) == 0 {
		t.Fatal("expected stitch vias somewhere on the board")
	}
	for _, v := range b.Vias {
		x, y := v.Position.X.ToMM(), v.Position.Y.ToMM()
		if x >= 25 && x <= 35 && y >= 25 && y <= 35 {
			t.Fatalf("via at %.2f,%.2f is inside the no-copper keepout", x, y)
		}
		if x >= 4.7 && x <= 15.3 && y >= 4.7 && y <= 15.3 {
			t.Fatalf("via at %.2f,%.2f is inside the cutout", x, y)
		}
	}
}

// The second poured net on a two-layer board is a set of slivers. The default
// 2.54 mm stitch grid lands no legal site inside one, so the pour used to come
// out isolated - and when it did stitch, it carpeted every grid site instead of
// stopping at the via that tied it.
func TestStitchTiesSliverPourWithoutCarpeting(t *testing.T) {
	n3v3 := "+3V3"
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(30), core.FromMM(30)))
	b.Outline = &o
	// One +3V3 pad on each face, so the pour on either face needs a via tie.
	top := &core.Footprint{
		ID: core.NewID(), Reference: "U1", Layer: core.LayerTop,
		Position: core.NewPoint(core.FromMM(8), core.FromMM(15)),
		Pads: []core.Pad{{Number: "1", Offset: core.Origin,
			Size:  [2]core.Length{core.FromMM(1), core.FromMM(1)},
			Layer: core.LayerTop, Net: &n3v3}},
	}
	bot := &core.Footprint{
		ID: core.NewID(), Reference: "C1", Layer: core.LayerBottom,
		Position: core.NewPoint(core.FromMM(22), core.FromMM(15)),
		Pads: []core.Pad{{Number: "1", Offset: core.Origin,
			Size:  [2]core.Length{core.FromMM(1), core.FromMM(1)},
			Layer: core.LayerBottom, Net: &n3v3}},
	}
	b.AddFootprint(top)
	b.AddFootprint(bot)
	b.Pours = append(b.Pours,
		core.Pour{ID: core.NewID(), Net: "+3V3", Layer: core.LayerTop},
		core.Pour{ID: core.NewID(), Net: "+3V3", Layer: core.LayerBottom})

	added := StitchIsolatedPads(b, DefaultOptions())
	if added == 0 {
		t.Fatal("expected the pours to be tied")
	}
	if added > 8 {
		t.Fatalf("tying two pours should not need %d vias", added)
	}
	for i := range b.Pours {
		if pourNeedsViaTie(b, &b.Pours[i]) {
			t.Fatalf("pour on layer %d still needs a tie", b.Pours[i].Layer.Index)
		}
	}
}
