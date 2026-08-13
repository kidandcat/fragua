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
