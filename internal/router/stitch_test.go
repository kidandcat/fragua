package router

import (
	"math"
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

// pourNeedsViaTie decided "is this pour already tied through?" from whichever
// pad it met first while ranging over the footprint MAP. A board with both a
// through-hole GND pad standing in the pour and an SMD GND pad on the other
// layer therefore answered true or false depending on Go's map order, and the
// same script stitched 0 or 1 vias run to run. The barrel wins: it ties the
// pour whatever else is on the board.
func TestPourTieIsDecidedByTheBarrelNotMapOrder(t *testing.T) {
	build := func() (*core.Board, *core.Pour) {
		b := core.NewBoard()
		o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(30), core.FromMM(20)))
		b.Outline = &o
		gnd := "GND"
		// A through-hole GND pad in the middle of the pour: already tied.
		b.AddFootprint(&core.Footprint{
			ID: core.NewID(), Reference: "J1",
			Position: core.NewPoint(core.FromMM(15), core.FromMM(10)),
			Layer:    core.LayerTop,
			Pads: []core.Pad{{
				Number: "1", Size: [2]core.Length{core.FromMM(1.7), core.FromMM(1.7)},
				Layer: core.LayerTop, Net: &gnd, Drill: ptrLen(core.FromMM(1.0)),
			}},
		})
		// An SMD GND pad on the far layer, which on its own would ask for a tie.
		b.AddFootprint(&core.Footprint{
			ID: core.NewID(), Reference: "C1",
			Position: core.NewPoint(core.FromMM(6), core.FromMM(6)),
			Layer:    core.LayerBottom,
			Pads: []core.Pad{{
				Number: "1", Size: [2]core.Length{core.FromMM(1), core.FromMM(1)},
				Layer: core.LayerBottom, Net: &gnd,
			}},
		})
		b.Pours = []core.Pour{{Net: "GND", Layer: core.LayerTop}}
		return b, &b.Pours[0]
	}
	for i := 0; i < 50; i++ {
		b, pr := build()
		if pourNeedsViaTie(b, pr) {
			t.Fatalf("run %d: pour reported untied although a GND barrel stands in it", i)
		}
	}
}

func ptrLen(l core.Length) *core.Length { return &l }

// A 2-layer GND pour that already touches the Top pads still needs a via
// next to each return pad so the Bottom island is tied at that node. The
// old lattice started at the outline corner and left U.GND / Cin.GND
// floating on the far face.
func TestStitchPlacesViasNextToSMDReturnPads(t *testing.T) {
	gnd := "GND"
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(30), core.FromMM(20)))
	b.Outline = &o
	u3 := &core.Footprint{
		ID: core.NewID(), Reference: "U3", Layer: core.LayerTop,
		Position: core.NewPoint(core.FromMM(10), core.FromMM(10)),
		Pads: []core.Pad{{
			Number: "GND", Offset: core.Origin,
			Size:  [2]core.Length{core.FromMM(0.8), core.FromMM(0.9)},
			Layer: core.LayerTop, Net: &gnd,
		}},
	}
	cin := &core.Footprint{
		ID: core.NewID(), Reference: "Cin", Layer: core.LayerTop,
		Position: core.NewPoint(core.FromMM(18), core.FromMM(10)),
		Pads: []core.Pad{{
			Number: "2", Offset: core.Origin,
			Size:  [2]core.Length{core.FromMM(0.8), core.FromMM(0.9)},
			Layer: core.LayerTop, Net: &gnd,
		}},
	}
	b.AddFootprint(u3)
	b.AddFootprint(cin)
	b.Pours = []core.Pour{
		{ID: core.NewID(), Net: "GND", Layer: core.LayerTop},
		{ID: core.NewID(), Net: "GND", Layer: core.LayerBottom},
	}

	added := StitchIsolatedPads(b, DefaultOptions())
	if added == 0 || len(b.Vias) == 0 {
		t.Fatalf("expected pad-local stitch vias, added=%d vias=%d", added, len(b.Vias))
	}

	near := func(fp *core.Footprint) bool {
		c := core.PadWorldCenter(fp, &fp.Pads[0])
		for _, v := range b.Vias {
			if v.Net != "GND" {
				continue
			}
			if math.Hypot(v.Position.X.ToMM()-c.X.ToMM(), v.Position.Y.ToMM()-c.Y.ToMM()) <= 1.5 {
				return true
			}
		}
		return false
	}
	if !near(u3) {
		t.Fatalf("no GND via within 1.5 mm of U3.GND; vias=%v", viaXY(b))
	}
	if !near(cin) {
		t.Fatalf("no GND via within 1.5 mm of Cin.GND; vias=%v", viaXY(b))
	}
	// A via only in the outline corner is the failure mode this covers.
	cornerOnly := true
	for _, v := range b.Vias {
		x, y := v.Position.X.ToMM(), v.Position.Y.ToMM()
		if x > 4 && y > 4 && x < 26 && y < 16 {
			cornerOnly = false
			break
		}
	}
	if cornerOnly {
		t.Fatalf("stitch vias landed only on the outline lattice: %v", viaXY(b))
	}
}

func viaXY(b *core.Board) [][2]float64 {
	out := make([][2]float64, len(b.Vias))
	for i, v := range b.Vias {
		out[i] = [2]float64{v.Position.X.ToMM(), v.Position.Y.ToMM()}
	}
	return out
}
