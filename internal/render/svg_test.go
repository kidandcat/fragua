package render

import (
	"strings"
	"testing"

	"github.com/mentasystems/fragua/internal/core"
)

func TestBoardSVGEmptyHasIntrinsicSize(t *testing.T) {
	svg := BoardSVG(core.NewBoard())
	if !strings.Contains(svg, `xmlns="http://www.w3.org/2000/svg"`) || !strings.Contains(svg, "viewBox=") {
		t.Fatalf("empty SVG missing xmlns/viewBox: %s", clip(svg, 160))
	}
	if strings.Contains(svg, `width="100%"`) {
		t.Fatal("empty SVG must not use percentage width")
	}
	if !strings.Contains(svg, "empty board") {
		t.Fatal("empty SVG should be labeled so the observer is not a blank canvas")
	}
	if BoardSVG(nil) == "" || !strings.Contains(BoardSVG(nil), "viewBox=") {
		t.Fatal("nil board should still produce a viewBox SVG")
	}
}

func TestBoardSVGOutlineVisible(t *testing.T) {
	b := core.NewBoard()
	r := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(40), core.FromMM(30)))
	b.Outline = &r
	svg := BoardSVG(b)
	if !strings.Contains(svg, "40.0 mm") || !strings.Contains(svg, "30.0 mm") {
		t.Fatalf("outlined SVG missing dimension labels: %s", clip(svg, 200))
	}
	if !strings.Contains(svg, `fill="#5a3a1f"`) {
		t.Fatal("outlined SVG missing substrate fill")
	}
}

// outline-poly is the real edge (Gerber, DXF and 3D use it). The 2D view used
// to draw only its bounding box, so an X-frame showed as a square board.
func TestBoardSVGDrawsOutlinePoly(t *testing.T) {
	b := core.NewBoard()
	mm := core.FromMM
	r := core.RectFromCorners(core.Origin, core.NewPoint(mm(40), mm(30)))
	b.Outline = &r
	b.OutlinePoly = []core.Point{
		core.NewPoint(0, 0), core.NewPoint(mm(40), 0), core.NewPoint(mm(20), mm(30)),
	}
	svg := BoardSVG(b)
	tri := "M0.000,0.000 L40.000,0.000 L20.000,30.000 Z"
	if strings.Count(svg, tri) != 2 {
		t.Fatalf("substrate and edge must both follow outline-poly (%q): %s", tri, clip(svg, 400))
	}
	if strings.Contains(svg, `<rect x="0.000" y="0.000" width="40.000" height="30.000" rx=`) {
		t.Fatal("the bounding box must not be drawn as the board edge")
	}
	if !strings.Contains(svg, "40.0 mm") {
		t.Fatal("dimension labels still come from the bounding box")
	}
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func TestPourOpacityVisible(t *testing.T) {
	b := core.NewBoard()
	r := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(20), core.FromMM(12)))
	b.Outline = &r
	b.Pours = append(b.Pours, core.Pour{Net: "GND", Layer: core.LayerTop})
	svg := BoardSVG(b)
	if !strings.Contains(svg, `fill-opacity="0.45"`) {
		t.Fatalf("pour should use Hand-like opacity 0.45: %s", clip(svg, 240))
	}
}

func TestSmallPadNetLabelEmitted(t *testing.T) {
	b := core.NewBoard()
	r := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(10), core.FromMM(10)))
	b.Outline = &r
	net := "VSTOR"
	b.AddFootprint(&core.Footprint{
		Reference: "U3", Value: "max17220", Key: "max17220_wlp6",
		Position: core.NewPoint(core.FromMM(5), core.FromMM(5)),
		Pads: []core.Pad{
			{Number: "5", Name: "IN", Size: [2]core.Length{core.FromMM(0.25), core.FromMM(0.25)}, Net: &net},
		},
	})
	svg := BoardSVG(b)
	idx := strings.Index(svg, `data-layer="pad-names"`)
	if idx < 0 {
		t.Fatal("missing pad-names group")
	}
	chunk := svg[idx:]
	if end := strings.Index(chunk, "</g>"); end > 0 {
		chunk = chunk[:end]
	}
	if !strings.Contains(chunk, ">VSTOR<") {
		t.Fatalf("small pad with net should emit net label in pad-names: %s", clip(chunk, 300))
	}
	if !strings.Contains(chunk, `fill="#f0e68c"`) {
		t.Fatalf("small-pad offset label should use light fill: %s", clip(chunk, 300))
	}
}

// The pour preview was the outline rect even on a polygon board, so copper
// appeared to fill the milled-away area.
func TestPourPreviewFollowsPolygonOutline(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(40), core.FromMM(30)))
	b.Outline = &o
	b.OutlinePoly = []core.Point{core.NewPoint(0, 0), core.NewPoint(core.FromMM(40), 0), core.NewPoint(core.FromMM(20), core.FromMM(30))}
	b.Pours = append(b.Pours, core.Pour{Net: "GND", Layer: core.LayerTop})
	var sb strings.Builder
	writePours(&sb, b, b.StackupOrDefault())
	got := sb.String()
	if !strings.Contains(got, `d="M0.000,0.000 L40.000,0.000 L20.000,30.000 Z`) {
		t.Fatalf("pour preview does not follow the polygon:\n%s", got)
	}
	if strings.Contains(got, "M0.300,0.300 h39.400") {
		t.Fatalf("pour preview still fills the bounding rect:\n%s", got)
	}
}
