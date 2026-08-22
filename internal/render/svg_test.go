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

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
