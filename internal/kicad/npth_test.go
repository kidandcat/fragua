package kicad

import (
	"testing"

	"github.com/mentasystems/fragua/internal/core"
)

// TestZoneFillClearsNPTHHoles: the exported zone fill voided cutouts and
// keepouts but not non-plated holes.
func TestZoneFillClearsNPTHHoles(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(40), core.FromMM(40)))
	b.Outline = &o
	b.Holes = append(b.Holes, core.MountHole{
		ID: core.NewID(), Center: core.NewPoint(core.FromMM(20), core.FromMM(20)), Diameter: core.FromMM(3),
	})
	pour := core.Pour{Net: "GND", Layer: core.LayerTop}
	e := &exporter{board: b, stack: b.StackupOrDefault()}
	shapes, _ := e.pourShapes(&pour)
	found := false
	for _, s := range shapes {
		if s.kind == voidDisc && s.a == [2]float64{20, 20} && s.rad >= 2.0-1e-9 {
			found = true
		}
	}
	if !found {
		t.Fatalf("no void disc (r>=2.0) around the NPTH hole: %+v", shapes)
	}
	if !shapes[0].hit(21.9, 20, 0) {
		t.Fatal("point 0.4 mm off the drill wall must be inside the void")
	}
}
