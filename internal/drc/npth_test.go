package drc

import (
	"strings"
	"testing"

	"github.com/mentasystems/fragua/internal/core"
)

// TestCopperToNPTH: only hole-to-hole spacing was checked against non-plated
// holes, so a trace routed straight across a mounting hole (and later cut in
// two by the drill) passed DRC with zero errors. Cutouts were not checked
// against copper at all.
func TestCopperToNPTH(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(40), core.FromMM(40)))
	b.Outline = &o
	b.MountHoles = append(b.MountHoles, core.MountHole{
		ID: core.NewID(), Center: core.NewPoint(core.FromMM(10), core.FromMM(10)), Diameter: core.FromMM(2.7),
	})
	// Legacy alias must count too.
	b.Holes = append(b.Holes, core.MountHole{
		ID: core.NewID(), Center: core.NewPoint(core.FromMM(30), core.FromMM(10)), Diameter: core.FromMM(2.4),
	})
	b.Cutouts = append(b.Cutouts, core.Cutout{ID: core.NewID(), Polygon: []core.Point{
		core.NewPoint(core.FromMM(18), core.FromMM(25)),
		core.NewPoint(core.FromMM(22), core.FromMM(25)),
		core.NewPoint(core.FromMM(22), core.FromMM(28)),
		core.NewPoint(core.FromMM(18), core.FromMM(28)),
	}})
	// Trace straight through the first hole.
	b.Traces = append(b.Traces, core.Trace{
		ID: core.NewID(), Net: "SIG", Width: core.FromMM(0.2), Layer: core.LayerTop,
		Start: core.NewPoint(core.FromMM(5), core.FromMM(10)),
		End:   core.NewPoint(core.FromMM(15), core.FromMM(10)),
	})
	// Via 0.3 mm from the second hole's wall (needs 0.5).
	b.Vias = append(b.Vias, core.Via{
		ID: core.NewID(), Net: "GND", Diameter: core.FromMM(0.6), Drill: core.FromMM(0.3),
		Position: core.NewPoint(core.FromMM(30+1.2+0.3+0.3), core.FromMM(10)),
	})
	// Trace crossing the cutout.
	b.Traces = append(b.Traces, core.Trace{
		ID: core.NewID(), Net: "EDGE", Width: core.FromMM(0.2), Layer: core.LayerBottom,
		Start: core.NewPoint(core.FromMM(15), core.FromMM(26.5)),
		End:   core.NewPoint(core.FromMM(25), core.FromMM(26.5)),
	})
	// Clear copper: a trace 1 mm from the first hole's wall, a via far away.
	b.Traces = append(b.Traces, core.Trace{
		ID: core.NewID(), Net: "OK", Width: core.FromMM(0.2), Layer: core.LayerTop,
		Start: core.NewPoint(core.FromMM(5), core.FromMM(12.45)),
		End:   core.NewPoint(core.FromMM(15), core.FromMM(12.45)),
	})

	rep := Check(b, nil, DefaultOptions())
	var holeMsgs, cutMsgs []string
	for _, v := range rep.Violations {
		switch {
		case v.Kind == KindCopperToHole:
			holeMsgs = append(holeMsgs, v.Message)
		case v.Kind == KindEdgeClearance && strings.Contains(v.Message, "cutout"):
			cutMsgs = append(cutMsgs, v.Message)
		}
	}
	if len(holeMsgs) != 2 {
		t.Fatalf("copper_to_hole: %d want 2 (trace SIG, via GND): %v", len(holeMsgs), holeMsgs)
	}
	for _, want := range []string{"trace SIG", "via GND"} {
		if !strings.Contains(strings.Join(holeMsgs, "\n"), want) {
			t.Fatalf("missing %q in %v", want, holeMsgs)
		}
	}
	if strings.Contains(strings.Join(holeMsgs, "\n"), "trace OK") {
		t.Fatalf("trace OK clears the hole by 1 mm: %v", holeMsgs)
	}
	if len(cutMsgs) != 1 || !strings.Contains(cutMsgs[0], "trace EDGE") {
		t.Fatalf("cutout clearance: %v want one for trace EDGE", cutMsgs)
	}
}

// An explicit keepout diameter wider than drill + margin is the rule.
func TestCopperToNPTHKeepoutDiameter(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(40), core.FromMM(40)))
	b.Outline = &o
	ko := core.FromMM(8)
	b.MountHoles = append(b.MountHoles, core.MountHole{
		ID: core.NewID(), Center: core.NewPoint(core.FromMM(20), core.FromMM(20)),
		Diameter: core.FromMM(3.2), KeepoutDiameter: &ko,
	})
	// 3 mm from the centre: clear of drill+0.5 (2.1) but inside the 4 mm keepout.
	b.Vias = append(b.Vias, core.Via{
		ID: core.NewID(), Net: "GND", Diameter: core.FromMM(0.6), Drill: core.FromMM(0.3),
		Position: core.NewPoint(core.FromMM(23), core.FromMM(20)),
	})
	rep := Check(b, nil, DefaultOptions())
	if n := countKind(rep, KindCopperToHole); n != 1 {
		t.Fatalf("copper_to_hole: %d want 1: %+v", n, rep.Violations)
	}
}
