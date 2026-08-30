package drc

import (
	"strings"
	"testing"

	"github.com/mentasystems/fragua/internal/core"
)

func netPtr(s string) *string { return &s }

func pad(num string, offX, offY float64, net string) core.Pad {
	var n *string
	if net != "" {
		n = netPtr(net)
	}
	return core.Pad{
		Number: num,
		Offset: core.NewPoint(core.FromMM(offX), core.FromMM(offY)),
		Size:   [2]core.Length{core.FromMM(1.0), core.FromMM(1.2)},
		Layer:  core.LayerTop,
		Net:    n,
	}
}

func fp(ref string, x, y float64, pads []core.Pad) *core.Footprint {
	return &core.Footprint{
		ID:        core.NewID(),
		Reference: ref,
		Position:  core.NewPoint(core.FromMM(x), core.FromMM(y)),
		Layer:     core.LayerTop,
		Pads:      pads,
	}
}

func outline(w, h float64) core.Rect {
	return core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(w), core.FromMM(h)))
}

func addTrace(b *core.Board, net string, x0, y0, x1, y1, widthMM float64) {
	b.Traces = append(b.Traces, core.Trace{
		ID:    core.NewID(),
		Layer: core.LayerTop,
		Start: core.NewPoint(core.FromMM(x0), core.FromMM(y0)),
		End:   core.NewPoint(core.FromMM(x1), core.FromMM(y1)),
		Width: core.FromMM(widthMM),
		Net:   net,
	})
}

func TestPadPadClearanceFires(t *testing.T) {
	b := core.NewBoard()
	o := outline(50, 20)
	b.Outline = &o
	// Two pads, 0.05 mm apart at the edges → under 0.2 mm clearance.
	b.AddFootprint(fp("R1", 10.0, 10.0, []core.Pad{pad("1", 0, 0, "A")}))
	b.AddFootprint(fp("R2", 11.05, 10.0, []core.Pad{pad("1", 0, 0, "B")}))
	rep := Check(b, nil, DefaultOptions())
	if countKind(rep, KindPadPadClearance) == 0 {
		t.Fatalf("expected pad-pad clearance error, got %+v", rep)
	}
}

func TestEmptyBoardClean(t *testing.T) {
	b := core.NewBoard()
	o := outline(10, 10)
	b.Outline = &o
	rep := Check(b, nil, DefaultOptions())
	if rep.Errors != 0 {
		t.Fatalf("empty board errors: %+v", rep)
	}
}

func TestUnconnectedPadIsFlagged(t *testing.T) {
	b := core.NewBoard()
	o := outline(50, 20)
	b.Outline = &o
	b.AddFootprint(fp("R1", 10.0, 10.0, []core.Pad{pad("1", 0, 0, "VCC")}))
	rep := Check(b, nil, DefaultOptions())
	if countKind(rep, KindUnconnectedPad) == 0 {
		t.Fatalf("expected UnconnectedPad, got %+v", rep)
	}
}

func TestTraceTouchingPadMarksConnected(t *testing.T) {
	b := core.NewBoard()
	o := outline(50, 20)
	b.Outline = &o
	b.AddFootprint(fp("R1", 10.0, 10.0, []core.Pad{pad("1", 0, 0, "VCC")}))
	addTrace(b, "VCC", 10.0, 10.0, 20.0, 10.0, 0.25)
	rep := Check(b, nil, DefaultOptions())
	if countKind(rep, KindUnconnectedPad) != 0 {
		t.Fatalf("trace should connect pad, got %+v", rep)
	}
}

func TestFullyRoutedTwoResistorBoardNoNetSplit(t *testing.T) {
	// Two 0805-style resistors sharing nets A and B, each pair fully routed
	// on parallel vertical tracks so the nets stay clear of each other.
	b := core.NewBoard()
	o := outline(40, 30)
	b.Outline = &o
	b.AddFootprint(fp("R1", 10.0, 8.0, []core.Pad{
		pad("1", -1.0, 0, "A"),
		pad("2", 1.0, 0, "B"),
	}))
	b.AddFootprint(fp("R2", 10.0, 20.0, []core.Pad{
		pad("1", -1.0, 0, "A"),
		pad("2", 1.0, 0, "B"),
	}))
	// Pad centers: R1.1 @(9,8) R1.2 @(11,8) · R2.1 @(9,20) R2.2 @(11,20)
	addTrace(b, "A", 9.0, 8.0, 9.0, 20.0, 0.25)
	addTrace(b, "B", 11.0, 8.0, 11.0, 20.0, 0.25)

	rep := Check(b, nil, DefaultOptions())
	if n := countKind(rep, KindNetSplit); n != 0 {
		t.Fatalf("expected 0 NetSplit on fully routed board, got %d: %+v", n, rep.Violations)
	}
	if n := countKind(rep, KindUnconnectedPad); n != 0 {
		t.Fatalf("expected 0 UnconnectedPad, got %d: %+v", n, rep.Violations)
	}
}

func TestNetSplitTwoIslands(t *testing.T) {
	// Two GND pads each with a local stub that never meet.
	b := core.NewBoard()
	o := outline(40, 20)
	b.Outline = &o
	b.AddFootprint(fp("R1", 5.0, 10.0, []core.Pad{pad("1", 0, 0, "GND")}))
	b.AddFootprint(fp("R2", 25.0, 10.0, []core.Pad{pad("1", 0, 0, "GND")}))
	addTrace(b, "GND", 5.0, 10.0, 12.0, 10.0, 0.25)  // touches R1 only
	addTrace(b, "GND", 18.0, 10.0, 25.0, 10.0, 0.25) // touches R2 only

	rep := Check(b, nil, DefaultOptions())
	if countKind(rep, KindNetSplit) == 0 {
		t.Fatalf("expected NetSplit, got %+v", rep.Violations)
	}
	// Both pads have local copper → not UnconnectedPad.
	if countKind(rep, KindUnconnectedPad) != 0 {
		t.Fatalf("split with local copper must not double-report UnconnectedPad: %+v", rep.Violations)
	}
}

func TestViaBridgesLayersInConnectivity(t *testing.T) {
	// R1 pad on top routed to via; R2 pad on bottom — same net, joined by via.
	b := core.NewBoard()
	o := outline(40, 20)
	b.Outline = &o
	top := core.LayerTop
	bot := core.LayerBottom
	b.AddFootprint(&core.Footprint{
		ID: core.NewID(), Reference: "R1",
		Position: core.NewPoint(core.FromMM(5), core.FromMM(10)),
		Layer:    top,
		Pads: []core.Pad{{
			Number: "1", Offset: core.Origin,
			Size:  [2]core.Length{core.FromMM(1), core.FromMM(1)},
			Layer: top, Net: netPtr("SIG"),
		}},
	})
	b.AddFootprint(&core.Footprint{
		ID: core.NewID(), Reference: "R2",
		Position: core.NewPoint(core.FromMM(25), core.FromMM(10)),
		Layer:    bot,
		Pads: []core.Pad{{
			Number: "1", Offset: core.Origin,
			Size:  [2]core.Length{core.FromMM(1), core.FromMM(1)},
			Layer: bot, Net: netPtr("SIG"),
		}},
	})
	// Top: pad R1 → via at (15,10)
	b.Traces = append(b.Traces, core.Trace{
		ID: core.NewID(), Layer: top,
		Start: core.NewPoint(core.FromMM(5), core.FromMM(10)),
		End:   core.NewPoint(core.FromMM(15), core.FromMM(10)),
		Width: core.FromMM(0.25), Net: "SIG",
	})
	// Bottom: via → pad R2
	b.Traces = append(b.Traces, core.Trace{
		ID: core.NewID(), Layer: bot,
		Start: core.NewPoint(core.FromMM(15), core.FromMM(10)),
		End:   core.NewPoint(core.FromMM(25), core.FromMM(10)),
		Width: core.FromMM(0.25), Net: "SIG",
	})
	b.Vias = append(b.Vias, core.Via{
		ID:       core.NewID(),
		Position: core.NewPoint(core.FromMM(15), core.FromMM(10)),
		Drill:    core.FromMM(0.3),
		Diameter: core.FromMM(0.6),
		Net:      "SIG",
	})

	rep := Check(b, nil, DefaultOptions())
	if n := countKind(rep, KindNetSplit); n != 0 {
		t.Fatalf("via should join layers, NetSplit=%d: %+v", n, rep.Violations)
	}
	if n := countKind(rep, KindUnconnectedPad); n != 0 {
		t.Fatalf("via path should touch pads, UnconnectedPad=%d: %+v", n, rep.Violations)
	}
}

func TestTraceTraceClearanceFires(t *testing.T) {
	b := core.NewBoard()
	o := outline(40, 20)
	b.Outline = &o
	// Parallel traces 0.1 mm apart centerline, each 0.25 mm wide → gap negative.
	addTrace(b, "A", 5.0, 10.0, 20.0, 10.0, 0.25)
	addTrace(b, "B", 5.0, 10.3, 20.0, 10.3, 0.25)
	rep := Check(b, nil, DefaultOptions())
	if countKind(rep, KindTraceTraceClearance) == 0 {
		t.Fatalf("expected TraceTraceClearance, got %+v", rep.Violations)
	}
}

func TestBodyOffBoardWhenPadsPastOutline(t *testing.T) {
	b := core.NewBoard()
	o := outline(20, 20)
	b.Outline = &o
	// Pad centered at (19.5, 10) with 1×1.2 size → right edge at 20.0 OK...
	// Place further out so pad AABB exceeds outline.
	b.AddFootprint(fp("R1", 22.0, 10.0, []core.Pad{pad("1", 0, 0, "")}))
	rep := Check(b, nil, DefaultOptions())
	if countKind(rep, KindBodyOffBoard) == 0 {
		t.Fatalf("expected BodyOffBoard, got %+v", rep.Violations)
	}
}

func TestMultiSegmentRouteConnectsPads(t *testing.T) {
	// Trace chain with colinear meeting endpoints (common router output).
	b := core.NewBoard()
	o := outline(40, 20)
	b.Outline = &o
	b.AddFootprint(fp("R1", 5.0, 10.0, []core.Pad{pad("1", 0, 0, "N")}))
	b.AddFootprint(fp("R2", 30.0, 10.0, []core.Pad{pad("1", 0, 0, "N")}))
	addTrace(b, "N", 5.0, 10.0, 15.0, 10.0, 0.25)
	addTrace(b, "N", 15.0, 10.0, 15.0, 12.0, 0.25)
	addTrace(b, "N", 15.0, 12.0, 30.0, 12.0, 0.25)
	addTrace(b, "N", 30.0, 12.0, 30.0, 10.0, 0.25)

	rep := Check(b, nil, DefaultOptions())
	if n := countKind(rep, KindNetSplit); n != 0 {
		t.Fatalf("multi-segment route must form one island, NetSplit=%d: %+v", n, rep.Violations)
	}
	if n := countKind(rep, KindUnconnectedPad); n != 0 {
		t.Fatalf("pads must be touched, UnconnectedPad=%d: %+v", n, rep.Violations)
	}
}

func TestViaTooCloseToForeignPad(t *testing.T) {
	b := core.NewBoard()
	o := outline(40, 20)
	b.Outline = &o
	// 1×1.2 pad at (10,10); via 0.6 dia at x=10.9 → gap 0.10 < 0.20 clearance.
	b.AddFootprint(fp("R1", 10.0, 10.0, []core.Pad{pad("1", 0, 0, "A")}))
	b.Vias = append(b.Vias, core.Via{
		ID:       core.NewID(),
		Position: core.NewPoint(core.FromMM(10.9), core.FromMM(10.0)),
		Drill:    core.FromMM(0.3),
		Diameter: core.FromMM(0.6),
		Net:      "B",
	})
	rep := Check(b, nil, DefaultOptions())
	if countKind(rep, KindViaPadClearance) == 0 {
		t.Fatalf("expected via-pad clearance, got %+v", rep.Violations)
	}
}

func TestViaSameNetMayTouchPad(t *testing.T) {
	b := core.NewBoard()
	o := outline(40, 20)
	b.Outline = &o
	b.AddFootprint(fp("R1", 10.0, 10.0, []core.Pad{pad("1", 0, 0, "A")}))
	b.Vias = append(b.Vias, core.Via{
		ID:       core.NewID(),
		Position: core.NewPoint(core.FromMM(11.0), core.FromMM(10.0)),
		Drill:    core.FromMM(0.3),
		Diameter: core.FromMM(0.6),
		Net:      "A",
	})
	rep := Check(b, nil, DefaultOptions())
	if countKind(rep, KindViaPadClearance) != 0 {
		t.Fatalf("same-net via must not flag via-pad: %+v", rep.Violations)
	}
}

func TestAnnularRingTooSmall(t *testing.T) {
	b := core.NewBoard()
	o := outline(40, 20)
	b.Outline = &o
	// (0.45-0.30)/2 = 0.075 < 0.15
	b.Vias = append(b.Vias, core.Via{
		ID:       core.NewID(),
		Position: core.NewPoint(core.FromMM(10), core.FromMM(10)),
		Drill:    core.FromMM(0.3),
		Diameter: core.FromMM(0.45),
		Net:      "GND",
	})
	rep := Check(b, nil, DefaultOptions())
	if countKind(rep, KindAnnularRing) == 0 {
		t.Fatalf("expected annular_ring, got %+v", rep.Violations)
	}
}

func TestCourtyardOverlap(t *testing.T) {
	b := core.NewBoard()
	o := outline(40, 20)
	b.Outline = &o
	// Two 1×1.2 pads whose courtyards (pad + 0.25 mm) overlap.
	b.AddFootprint(fp("R1", 10.0, 10.0, []core.Pad{pad("1", 0, 0, "A")}))
	b.AddFootprint(fp("R2", 11.2, 10.0, []core.Pad{pad("1", 0, 0, "B")}))
	rep := Check(b, nil, DefaultOptions())
	if countKind(rep, KindCourtyardOverlap) == 0 {
		t.Fatalf("expected courtyard overlap, got %+v", rep.Violations)
	}
}

func TestPourCoversPadOnlyWhenInsideIsland(t *testing.T) {
	b := core.NewBoard()
	o := outline(40, 20)
	b.Outline = &o
	// Pad near the edge, inside outline but outside the 0.3 mm pour inset
	// (pad center at 0.2, 10) — pour must NOT claim coverage.
	b.AddFootprint(fp("R1", 0.2, 10.0, []core.Pad{pad("1", 0, 0, "GND")}))
	b.Pours = []core.Pour{{Net: "GND", Layer: core.LayerTop}}
	rep := Check(b, nil, DefaultOptions())
	if countKind(rep, KindUnconnectedPad) == 0 {
		t.Fatalf("pad outside pour island must stay unconnected, got %+v", rep.Violations)
	}
	// Pad well inside the board is covered by the outline pour.
	b2 := core.NewBoard()
	b2.Outline = &o
	b2.AddFootprint(fp("R1", 10.0, 10.0, []core.Pad{pad("1", 0, 0, "GND")}))
	b2.Pours = []core.Pour{{Net: "GND", Layer: core.LayerTop}}
	rep2 := Check(b2, nil, DefaultOptions())
	if countKind(rep2, KindUnconnectedPad) != 0 {
		t.Fatalf("pad inside pour should be connected: %+v", rep2.Violations)
	}
}

func TestDefaultMinDrillIs03(t *testing.T) {
	if DefaultOptions().MinDrill.ToMM() < 0.299 {
		t.Fatalf("default min drill %v want 0.3", DefaultOptions().MinDrill.ToMM())
	}
}

func TestIsolatedPourNoVia(t *testing.T) {
	b := core.NewBoard()
	o := outline(40, 20)
	b.Outline = &o
	// Top pour GND, bottom pad GND, no via.
	b.Pours = []core.Pour{{Net: "GND", Layer: core.LayerTop}}
	b.AddFootprint(&core.Footprint{
		ID: core.NewID(), Reference: "R1",
		Position: core.NewPoint(core.FromMM(10), core.FromMM(10)),
		Layer:    core.LayerBottom,
		Pads: []core.Pad{{
			Number: "1", Size: [2]core.Length{core.FromMM(1), core.FromMM(1)},
			Layer: core.LayerBottom, Net: netPtr("GND"),
		}},
	})
	rep := Check(b, nil, DefaultOptions())
	if countKind(rep, KindIsolatedPour) == 0 {
		t.Fatalf("expected isolated_pour, got %+v", rep.Violations)
	}
}

func TestEmptyStitchingDoesNotPass(t *testing.T) {
	b := core.NewBoard()
	o := outline(40, 20)
	b.Outline = &o
	b.Pours = []core.Pour{{
		Net: "GND", Layer: core.LayerTop,
		Stitching: &core.StitchPolicy{}, // empty {}
	}}
	rep := Check(b, nil, DefaultOptions())
	if countKind(rep, KindUnstitchedPour) == 0 {
		t.Fatalf("empty stitching:{} must not silently pass, got %+v", rep.Violations)
	}
}

func TestHoleToHoleTooClose(t *testing.T) {
	b := core.NewBoard()
	o := outline(40, 20)
	b.Outline = &o
	b.Vias = []core.Via{
		{ID: core.NewID(), Position: core.NewPoint(core.FromMM(10), core.FromMM(10)), Drill: core.FromMM(0.3), Diameter: core.FromMM(0.6), Net: "A"},
		{ID: core.NewID(), Position: core.NewPoint(core.FromMM(10.4), core.FromMM(10)), Drill: core.FromMM(0.3), Diameter: core.FromMM(0.6), Net: "B"},
	}
	rep := Check(b, nil, DefaultOptions())
	if countKind(rep, KindHoleToHole) == 0 {
		t.Fatalf("expected hole_to_hole, got %+v", rep.Violations)
	}
}

func TestTeardropSameNetNoClearanceHit(t *testing.T) {
	b := core.NewBoard()
	o := outline(40, 20)
	b.Outline = &o
	b.Teardrops = true
	b.AddFootprint(fp("R1", 10.0, 10.0, []core.Pad{pad("1", 0, 0, "VCC")}))
	addTrace(b, "VCC", 10.0, 10.0, 25.0, 10.0, 0.25)
	rep := Check(b, nil, DefaultOptions())
	if countKind(rep, KindTeardropClearance) != 0 {
		t.Fatalf("same-net teardrop must not self-clearance: %+v", rep.Violations)
	}
	if countKind(rep, KindTracePadClearance) != 0 {
		t.Fatalf("same-net pad/trace: %+v", rep.Violations)
	}
	if countKind(rep, KindUnconnectedPad) != 0 {
		t.Fatalf("teardrop board should stay connected: %+v", rep.Violations)
	}
}

func TestImpedanceMismatchWarning(t *testing.T) {
	b := core.NewBoard()
	o := outline(40, 20)
	b.Outline = &o
	addTrace(b, "USB", 5.0, 10.0, 20.0, 10.0, 0.25) // far from 50 Ω ~3 mm
	sch := core.NewSchematic()
	sch.Nets["USB"] = &core.Net{Name: "USB", Class: "USB50"}
	sch.NetClasses["USB50"] = &core.NetClass{Name: "USB50", ImpedanceOhms: 50}
	rep := Check(b, sch, DefaultOptions())
	if countKind(rep, KindImpedanceMismatch) == 0 {
		t.Fatalf("expected impedance warning for 0.25 mm vs ~3 mm: %+v", rep.Violations)
	}
}

// `nc REF.PIN` is the designer saying "this pin is meant to be unused". The
// board checks used to ignore the mark, so a breakout with one unused pin
// warned on every DRC run and there was no way to silence it.
func TestNoConnectPinsAreNotDangling(t *testing.T) {
	b := core.NewBoard()
	o := outline(20, 20)
	b.Outline = &o
	b.AddFootprint(fp("U9", 10, 10, []core.Pad{
		pad("1", -1, 0, "VCC"),
		pad("2", 1, 0, ""), // unused, no net
	}))
	b.AddFootprint(fp("R1", 15, 10, []core.Pad{
		pad("1", -1, 0, "VCC"),
		pad("2", 1, 0, "OUT"),
	}))
	addTrace(b, "VCC", 9, 10, 14, 10, 0.25)
	sch := core.NewSchematic()

	danglingOn := func(rep Report, key string) bool {
		for _, v := range rep.Violations {
			if v.Kind == KindSmallComponentDangling && strings.Contains(v.Message, key) {
				return true
			}
		}
		return false
	}

	if !danglingOn(Check(b, sch, DefaultOptions()), "U9.2") {
		t.Fatal("an unnetted pad with no nc mark must still be reported")
	}

	id := core.NewID()
	sch.Symbols = map[string]*core.Symbol{id.String(): {
		ID: id, Reference: "U9",
		Kind: core.SymbolKind{Kind: "generic_ic", ICPins: []core.SchPin{
			{Number: "1", Role: core.PinPowerIn},
			{Number: "2", Role: core.PinNC, NC: true},
		}},
	}}
	if danglingOn(Check(b, sch, DefaultOptions()), "U9.2") {
		t.Fatal("an nc pin must not be reported as dangling")
	}
}

func countKind(rep Report, k Kind) int {
	n := 0
	for _, v := range rep.Violations {
		if v.Kind == k {
			n++
		}
	}
	return n
}

// TestCopperInKeepout: nothing outside the router read Keepout.NoCopper, so a
// pad or trace sitting in a no-copper zone — under a module antenna, say —
// passed DRC silently.
func TestCopperInKeepout(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(40), core.FromMM(40)))
	b.Outline = &o
	ko := core.RectFromCorners(core.NewPoint(core.FromMM(20), core.FromMM(20)), core.NewPoint(core.FromMM(30), core.FromMM(30)))
	b.Keepouts = append(b.Keepouts, core.Keepout{ID: core.NewID(), Rect: &ko, NoCopper: true})
	b.AddFootprint(fp("R1", 25, 25, []core.Pad{pad("1", 0, 0, "GND")}))
	b.AddFootprint(fp("R2", 5, 5, []core.Pad{pad("1", 0, 0, "GND")}))
	b.Traces = append(b.Traces, core.Trace{
		ID: core.NewID(), Net: "SIG", Width: core.FromMM(0.2), Layer: core.LayerTop,
		Start: core.NewPoint(core.FromMM(22), core.FromMM(28)),
		End:   core.NewPoint(core.FromMM(28), core.FromMM(28)),
	})
	b.Vias = append(b.Vias, core.Via{
		ID: core.NewID(), Net: "GND", Diameter: core.FromMM(0.6), Drill: core.FromMM(0.3),
		Position: core.NewPoint(core.FromMM(24), core.FromMM(22)),
	})

	rep := Check(b, nil, DefaultOptions())
	var got []string
	for _, v := range rep.Violations {
		if v.Kind == KindCopperInKeepout {
			got = append(got, v.Message)
		}
	}
	if len(got) != 3 {
		t.Fatalf("copper_in_keepout: %d want 3 (pad, trace, via): %v", len(got), got)
	}
	for _, want := range []string{"pad R1.1", "trace SIG", "via GND"} {
		found := false
		for _, m := range got {
			if strings.Contains(m, want) {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing violation for %s: %v", want, got)
		}
	}
	for _, m := range got {
		if strings.Contains(m, "R2") {
			t.Fatalf("R2 is outside the keepout: %v", got)
		}
	}
}
