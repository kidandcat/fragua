package gerber

import (
	"strings"
	"testing"

	"github.com/mentasystems/fragua/internal/core"
)

// TestPourClearsNPTHHoles: the pour voided cutouts and keepouts but not
// non-plated holes, so copper filled right up to (and under) the drill.
func TestPourClearsNPTHHoles(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(40), core.FromMM(40)))
	b.Outline = &o
	b.MountHoles = append(b.MountHoles, core.MountHole{
		ID: core.NewID(), Center: core.NewPoint(core.FromMM(20), core.FromMM(20)), Diameter: core.FromMM(3),
	})
	b.Pours = append(b.Pours, core.Pour{Net: "GND", Layer: core.LayerTop})

	g := writeCopperLayer(b, core.LayerTop, "F.Cu")
	if n := strings.Count(g, "G36*"); n != 2 {
		t.Fatalf("regions: %d want 2 (board fill + hole void)", n)
	}
	if !strings.Contains(g, "%LPC*%") {
		t.Fatal("hole void must be emitted in clear polarity")
	}
	// Drill radius 1.5 + 0.5 margin, circumscribed 32-gon: the vertex on +X
	// sits at 20 + 2/cos(pi/32) = 22.0097 mm.
	if !strings.Contains(g, "X22009") {
		t.Fatalf("missing hole void vertex at x≈22.0097:\n%s", g)
	}
}
