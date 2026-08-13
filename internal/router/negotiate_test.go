package router

import (
	"testing"

	"github.com/mentasystems/fragua/internal/core"
)

func TestNegotiateDoesNotBreakTwoResistors(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(40), core.FromMM(20)))
	b.Outline = &o
	b.AddFootprint(footprint("R1", 10, 10, []core.Pad{
		pad("1", -1, 0, "VCC"),
		pad("2", 1, 0, "OUT"),
	}))
	b.AddFootprint(footprint("R2", 20, 10, []core.Pad{
		pad("1", -1, 0, "OUT"),
		pad("2", 1, 0, "GND"),
	}))
	opts := DefaultOptions()
	opts.Negotiate = true
	rep := Route(b, opts)
	if rep.Failed != 0 {
		t.Fatalf("negotiate must not break the two-resistor board: %s per=%+v", rep.Summary(), rep.PerNet)
	}
	if !copperClearanceLegal(b, 0.20) {
		t.Fatal("negotiate left illegal copper")
	}
}

func TestRipLongHaulKeepsFanoutStub(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(40), core.FromMM(20)))
	b.Outline = &o
	n := "GPIO0"
	b.AddFootprint(footprint("U1", 10, 10, []core.Pad{pad("1", 0, 0, n)}))
	stubEnd := core.NewPoint(core.FromMM(10.7), core.FromMM(10))
	farEnd := core.NewPoint(core.FromMM(30), core.FromMM(10))
	padP := core.NewPoint(core.FromMM(10), core.FromMM(10))
	b.Traces = []core.Trace{
		{ID: core.NewID(), Net: n, Layer: core.LayerTop, Width: core.FromMM(0.15), Start: padP, End: stubEnd},
		{ID: core.NewID(), Net: n, Layer: core.LayerTop, Width: core.FromMM(0.15), Start: stubEnd, End: farEnd},
	}
	b.Vias = []core.Via{{
		ID: core.NewID(), Net: n, Position: stubEnd,
		Drill: core.FromMM(0.2), Diameter: core.FromMM(0.45),
	}}
	ripLongHaul(b, n)
	if len(b.Traces) != 1 {
		t.Fatalf("expected fanout stub kept, got %d traces", len(b.Traces))
	}
	if len(b.Vias) != 1 {
		t.Fatalf("via should stay, got %d", len(b.Vias))
	}
	if hypotMM(b.Traces[0].Start, b.Traces[0].End) > fanoutStubMaxMM {
		t.Fatal("kept the long-haul instead of the stub")
	}
}
