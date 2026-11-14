package drc

import (
	"strings"
	"testing"

	"github.com/mentasystems/fragua/internal/core"
)

func p(x, y float64) core.Point { return core.NewPoint(core.FromMM(x), core.FromMM(y)) }

// TestEdgeFollowsPolygonOutline: edge clearance and body-off-board were
// checked against the outline's bounding box, so copper and parts sitting in
// a milled notch (inside the box, outside the board) passed DRC.
func TestEdgeFollowsPolygonOutline(t *testing.T) {
	b := core.NewBoard()
	// 40x30 with a V notch from the top edge down to (20,10).
	b.OutlinePoly = []core.Point{p(0, 0), p(40, 0), p(40, 30), p(20, 10), p(0, 30)}
	o := core.RectFromCorners(p(0, 0), p(40, 30))
	b.Outline = &o
	via := func(net string, x, y float64) {
		b.Vias = append(b.Vias, core.Via{ID: core.NewID(), Net: net, Diameter: core.FromMM(0.6), Drill: core.FromMM(0.3), Position: p(x, y)})
	}
	via("NOTCH", 20, 20)    // in the notch: off the board
	via("APEX", 20, 9.5)    // 0.2 mm from the notch apex after its radius
	via("CLEAR", 10, 4)     // well inside
	via("CORNERX", 39.5, 1) // 0.2 mm from the right edge: rect check already caught it
	// Both ends on the board, but the middle crosses the notch.
	b.Traces = append(b.Traces, core.Trace{
		ID: core.NewID(), Net: "BRIDGE", Width: core.FromMM(0.2), Layer: core.LayerTop,
		Start: p(8, 20), End: p(32, 20),
	})
	b.Traces = append(b.Traces, core.Trace{
		ID: core.NewID(), Net: "OK", Width: core.FromMM(0.2), Layer: core.LayerTop,
		Start: p(5, 5), End: p(35, 5),
	})
	// A part whose pads sit in the notch.
	b.AddFootprint(&core.Footprint{
		ID: core.NewID(), Reference: "U1", Position: p(20, 25), Layer: core.LayerTop,
		Pads: []core.Pad{
			{Number: "1", Offset: p(-1, 0), Size: [2]core.Length{core.FromMM(0.6), core.FromMM(0.6)}, Layer: core.LayerTop},
			{Number: "2", Offset: p(1, 0), Size: [2]core.Length{core.FromMM(0.6), core.FromMM(0.6)}, Layer: core.LayerTop},
		},
	})

	rep := Check(b, nil, DefaultOptions())
	var edge []string
	offBoard := false
	for _, v := range rep.Violations {
		switch v.Kind {
		case KindEdgeClearance:
			edge = append(edge, v.Message)
		case KindBodyOffBoard:
			offBoard = offBoard || strings.Contains(v.Message, "U1")
		}
	}
	all := strings.Join(edge, "\n")
	for _, want := range []string{"via NOTCH", "via APEX", "via CORNERX", "trace BRIDGE", "pad U1.1", "pad U1.2"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing edge violation %q; got:\n%s", want, all)
		}
	}
	for _, bad := range []string{"CLEAR", "trace OK"} {
		if strings.Contains(all, bad) {
			t.Errorf("false edge violation for %q:\n%s", bad, all)
		}
	}
	if !offBoard {
		t.Errorf("U1 sits in the notch but no body_off_board violation")
	}
}
