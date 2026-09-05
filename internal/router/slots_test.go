package router

import (
	"math"
	"testing"

	"github.com/mentasystems/fragua/internal/core"
)

func TestMinCostMatchingPrefersCheap(t *testing.T) {
	// 2 pads, 3 slots. Pad0 only cheap on slot 0; pad1 cheap on 0 too
	// but can take slot 1. Matching should give slot 0 to pad0 and
	// slot 1 to pad1 (max cardinality, min cost).
	adj := [][][2]int64{
		{{0, 1}, {1, 50}},
		{{0, 1}, {1, 2}, {2, 80}},
	}
	got := minCostMatching(2, 3, adj)
	if got[0] != 0 || got[1] != 1 {
		t.Fatalf("got %v want [0 1]", got)
	}
}

func TestEscapeSlotsSpreadQFNVias(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(20), core.FromMM(20)))
	b.Outline = &o
	// 8 pads on the left side of a fake QFN at 0.4 mm pitch.
	var pads []core.Pad
	for i := 0; i < 8; i++ {
		n := "N" + string(rune('A'+i))
		pads = append(pads, pad(string(rune('1'+i)), -3.5, -1.4+float64(i)*0.4, n))
		// partner somewhere to the left so travel is outward
		b.AddFootprint(footprint("H"+string(rune('A'+i)), 2, 8+float64(i), []core.Pad{
			pad("1", 0, 0, n),
		}))
	}
	// pad() uses 1.0×1.2 — too fat for 0.4 mm. Rebuild with 0.2×0.2.
	for i := range pads {
		pads[i].Size = [2]core.Length{core.FromMM(0.2), core.FromMM(0.2)}
	}
	u1 := footprint("U1", 10, 10, pads)
	// 16+ pads to pass the QFN gate; fill other sides with dummy nets.
	for i := 8; i < 20; i++ {
		n := "D" + string(rune('A'+i-8))
		p := pad("x", 3.5, -1.4+float64(i-8)*0.4, n)
		p.Size = [2]core.Length{core.FromMM(0.2), core.FromMM(0.2)}
		u1.Pads = append(u1.Pads, p)
	}
	b.AddFootprint(u1)

	opts := DefaultOptions()
	opts.CellMM = 0.15
	opts.TraceWidthMM = 0.15
	opts.ViaDiameterMM = 0.45
	opts.ViaDrillMM = 0.20
	n := planFanout(b, opts)
	if n < 8 {
		t.Fatalf("expected ≥8 escape vias on the left row, got %d (vias=%d)", n, len(b.Vias))
	}
	minD := 99.0
	for i := 0; i < len(b.Vias); i++ {
		for j := i + 1; j < len(b.Vias); j++ {
			d := math.Hypot(
				b.Vias[i].Position.X.ToMM()-b.Vias[j].Position.X.ToMM(),
				b.Vias[i].Position.Y.ToMM()-b.Vias[j].Position.Y.ToMM(),
			)
			if d < minD {
				minD = d
			}
		}
	}
	if minD < 0.65 {
		t.Fatalf("vias packed tighter than one lane: min=%.3f mm", minD)
	}
}

func TestPlanFanoutIncludesWLP6(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(18), core.FromMM(14)))
	b.Outline = &o
	var pads []core.Pad
	// 2×3 WLP at 0.4 mm. SEL has a far partner so it is not a close hop.
	coords := [][3]float64{
		{-0.2, 0.4, 0}, {0.2, 0.4, 1},
		{-0.2, 0, 2}, {0.2, 0, 3},
		{-0.2, -0.4, 4}, {0.2, -0.4, 5},
	}
	names := []string{"OUT", "VSTOR", "GND", "LX", "EN", "SEL"}
	for i, c := range coords {
		p := pad(names[i], c[0], c[1], names[i])
		p.Size = [2]core.Length{core.FromMM(0.24), core.FromMM(0.24)}
		pads = append(pads, p)
		if names[i] == "GND" {
			continue
		}
		b.AddFootprint(footprint("P"+names[i], 2, 2+float64(i), []core.Pad{
			pad("1", 0, 0, names[i]),
		}))
	}
	u3 := footprint("U3", 9, 7, pads)
	b.AddFootprint(u3)

	opts := DefaultOptions()
	opts.CellMM = 0.127
	opts.TraceWidthMM = 0.127
	opts.ViaDiameterMM = 0.60
	opts.ViaDrillMM = 0.30
	n := planFanout(b, opts)
	if n < 1 {
		t.Fatalf("WLP-6 must get dogbone escapes (pad 0.24 < via 0.30); vias=%d added=%d", len(b.Vias), n)
	}
	for _, v := range b.Vias {
		// Via-in-pad is illegal: drill 0.30 > land 0.24.
		d := math.Hypot(v.Position.X.ToMM()-9, v.Position.Y.ToMM()-7)
		if d < 0.3 {
			t.Fatalf("via-in-pad on WLP (via at %.3f,%.3f, d=%.3f from package)",
				v.Position.X.ToMM(), v.Position.Y.ToMM(), d)
		}
	}
}

func TestPlanFanoutSkipsCloseTwoPinSignal(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(18), core.FromMM(14)))
	b.Outline = &o
	sel := pad("C2", 0.2, -0.4, "SEL")
	sel.Size = [2]core.Length{core.FromMM(0.24), core.FromMM(0.24)}
	gnd := pad("B1", -0.2, 0, "GND")
	gnd.Size = [2]core.Length{core.FromMM(0.24), core.FromMM(0.24)}
	out := pad("A1", -0.2, 0.4, "OUT")
	out.Size = [2]core.Length{core.FromMM(0.24), core.FromMM(0.24)}
	batt := pad("A2", 0.2, 0.4, "VSTOR")
	batt.Size = [2]core.Length{core.FromMM(0.24), core.FromMM(0.24)}
	lx := pad("B2", 0.2, 0, "LX")
	lx.Size = [2]core.Length{core.FromMM(0.24), core.FromMM(0.24)}
	en := pad("C1", -0.2, -0.4, "EN")
	en.Size = [2]core.Length{core.FromMM(0.24), core.FromMM(0.24)}
	b.AddFootprint(footprint("U3", 9, 7, []core.Pad{out, batt, gnd, lx, en, sel}))
	// R16 sits ~2.6 mm south-east — same-layer hop, no dogbone.
	b.AddFootprint(footprint("R16", 10.8, 4.5, []core.Pad{pad("1", 0, 0, "SEL")}))
	// Far partners so the other pins still qualify for escape.
	b.AddFootprint(footprint("C9", 2, 12, []core.Pad{pad("1", 0, 0, "OUT")}))
	b.AddFootprint(footprint("L2", 16, 7, []core.Pad{pad("1", 0, 0, "LX"), pad("2", 1.6, 0, "VSTOR")}))
	b.AddFootprint(footprint("R15", 2, 2, []core.Pad{pad("1", 0, 0, "EN")}))

	opts := DefaultOptions()
	opts.CellMM = 0.127
	opts.TraceWidthMM = 0.127
	opts.ViaDiameterMM = 0.60
	opts.ViaDrillMM = 0.30
	planFanout(b, opts)
	for _, v := range b.Vias {
		if v.Net == "SEL" {
			t.Fatalf("2-pin SEL %.2f mm from R16 must not dogbone (via at %.2f,%.2f)",
				2.6, v.Position.X.ToMM(), v.Position.Y.ToMM())
		}
	}
}

func TestViaFitsPadRejectsOversizeBarrel(t *testing.T) {
	t0 := slotTarget{hw: 0.12, hh: 0.12}
	if viaFitsPad(t0, 0.60, 0.30) {
		t.Fatal("0.30 drill must not fit a 0.24 pad")
	}
	t1 := slotTarget{hw: 0.40, hh: 0.40}
	if !viaFitsPad(t1, 0.60, 0.30) {
		t.Fatal("0.80 pad should accept a 0.60/0.30 via")
	}
}

func TestLeftoverEscapeUsesOuterRing(t *testing.T) {
	// Neighbours already occupy the 0.70 ring; the leftover pad must
	// take 1.35+ (not via-in-pad on the pin).
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(30), core.FromMM(30)))
	b.Outline = &o
	n := "GPIO8"
	p := pad("10", 0, 0, n)
	p.Size = [2]core.Length{core.FromMM(0.2), core.FromMM(0.5)}
	u1 := footprint("U1", 15, 15, []core.Pad{p})
	b.AddFootprint(u1)
	b.AddFootprint(footprint("J3", 2, 15, []core.Pad{pad("9", 0, 0, n)}))
	placed := []fanoutXY{
		{15, 15.40}, // neighbour via, 0.40 mm north of the pin
		{15, 14.60}, // neighbour via, 0.40 mm south
	}
	t0 := slotTarget{
		idx: 0, pad: &u1.Pads[0], net: n,
		cx: 15, cy: 15, hw: 0.1, hh: 0.25,
		tx: 2, ty: 15, ux: -1, uy: 0, side: 1, layer: 0,
	}
	nAdd := placeEscapes(b, u1, []slotTarget{t0}, false,
		[]float64{1.35, 1.70, 2.05, 2.40, 1.00, 0.70},
		[]float64{0, 0.40, -0.40, 0.80, -0.80},
		0.45, 0.20, 0.09, 0.225, 0.314, 0.539, &placed)
	if nAdd != 1 {
		t.Fatalf("expected 1 leftover via, got %d (vias=%d)", nAdd, len(b.Vias))
	}
	v := b.Vias[len(b.Vias)-1]
	d := math.Hypot(v.Position.X.ToMM()-15, v.Position.Y.ToMM()-15)
	if d < 1.2 {
		t.Fatalf("leftover via at %.3f mm; wanted outer ring ≥1.35", d)
	}
}
