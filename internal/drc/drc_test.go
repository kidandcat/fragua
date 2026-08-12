package drc

import (
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

func countKind(rep Report, k Kind) int {
	n := 0
	for _, v := range rep.Violations {
		if v.Kind == k {
			n++
		}
	}
	return n
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
			Size: [2]core.Length{core.FromMM(1), core.FromMM(1)},
			Layer: top, Net: netPtr("SIG"),
		}},
	})
	b.AddFootprint(&core.Footprint{
		ID: core.NewID(), Reference: "R2",
		Position: core.NewPoint(core.FromMM(25), core.FromMM(10)),
		Layer:    bot,
		Pads: []core.Pad{{
			Number: "1", Offset: core.Origin,
			Size: [2]core.Length{core.FromMM(1), core.FromMM(1)},
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
		ID: core.NewID(),
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
