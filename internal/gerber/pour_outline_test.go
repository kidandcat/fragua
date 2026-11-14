package gerber

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mentasystems/fragua/internal/core"
)

// TestPourFollowsPolygonOutline: with a polygon outline the pour region was
// the bounding rectangle inset 0.3 mm, so copper reached the milled edge on
// every concave or slanted side.
func TestPourFollowsPolygonOutline(t *testing.T) {
	mm := core.FromMM
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(mm(40), mm(30)))
	b.Outline = &o
	b.OutlinePoly = []core.Point{
		core.NewPoint(mm(0), mm(0)), core.NewPoint(mm(40), mm(0)), core.NewPoint(mm(20), mm(30)),
	}
	b.Pours = append(b.Pours, core.Pour{Net: "GND", Layer: core.LayerTop})

	g := writeCopperLayer(b, core.LayerTop, "F.Cu")
	at := func(x, y float64) string { return fmt.Sprintf("X%dY%d", coord(mm(x)), coord(mm(y))) }

	// The bounding-box fill corner (39.7, 29.7) lies outside the triangle.
	if strings.Contains(g, at(40-pourEdgeClearance.ToMM(), 30-pourEdgeClearance.ToMM())) {
		t.Fatalf("pour still fills the bounding rectangle:\n%s", g)
	}
	region := g[strings.Index(g, "G36*"):strings.Index(g, "G37*")]
	for _, v := range []string{at(0, 0) + "D02", at(40, 0) + "D01", at(20, 30) + "D01"} {
		if !strings.Contains(region, v) {
			t.Fatalf("pour region is missing outline vertex %s:\n%s", v, region)
		}
	}
	// The edge setback: every outline edge is stroked in clear polarity with a
	// round aperture twice the clearance wide.
	clear := g[strings.Index(g, "%LPC*%"):]
	if !strings.Contains(g, "C,0.600000") {
		t.Fatalf("no 0.6 mm round aperture for the edge clearance:\n%s", g)
	}
	for _, v := range []string{at(40, 0) + "D01", at(20, 30) + "D01", at(0, 0) + "D01"} {
		if !strings.Contains(clear, v) {
			t.Fatalf("edge clearance stroke missing %s:\n%s", v, clear)
		}
	}
}

// A rectangular board keeps the inset-rect fill.
func TestPourRectOutlineUnchanged(t *testing.T) {
	mm := core.FromMM
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(mm(40), mm(30)))
	b.Outline = &o
	b.Pours = append(b.Pours, core.Pour{Net: "GND", Layer: core.LayerTop})
	g := writeCopperLayer(b, core.LayerTop, "F.Cu")
	want := fmt.Sprintf("X%dY%dD01", coord(mm(40-pourEdgeClearance.ToMM())), coord(mm(30-pourEdgeClearance.ToMM())))
	if !strings.Contains(g, want) {
		t.Fatalf("rect pour lost its inset corner %s:\n%s", want, g)
	}
}
