package script

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mentasystems/fragua/internal/core"
)

// TestRouteStaysOnPolygonOutline: the router, stitching and DRC only knew the
// outline's bounding box, so on a slotted board a net between the two arms
// was routed straight across the milled slot and DRC passed it.
func TestRouteStaysOnPolygonOutline(t *testing.T) {
	p := core.NewProject("slot")
	res := RunScript(p, strings.Join([]string{
		// 40x30 with a 4 mm slot from the top edge down to y=8.
		"outline-poly 0 0 40 0 40 30 22 30 22 8 18 8 18 30 0 30",
		"lib demo_r",
		"  pad 1 -0.775 0 0.95 0.95",
		"  pad 2 0.775 0 0.95 0.95",
		"sym R1 resistor key=demo_r value=10k",
		"sym R2 resistor key=demo_r value=10k",
		"palette R1 demo_r",
		"palette R2 demo_r",
		"place R1 9 25",
		"place R2 31 25",
		"net SIG R1.1 R2.1",
		"net GND R1.2 R2.2",
		"pour GND layer=Bottom",
		"route",
	}, "\n"))
	for _, r := range res {
		if !r.OK {
			t.Fatalf("line %d %s: %s", r.Line, r.Tool, r.Result)
		}
	}
	board := p.Board()
	if len(board.Traces) == 0 {
		t.Fatal("nothing routed")
	}
	slot := []core.Point{
		core.NewPoint(core.FromMM(18), core.FromMM(8)), core.NewPoint(core.FromMM(22), core.FromMM(8)),
		core.NewPoint(core.FromMM(22), core.FromMM(30)), core.NewPoint(core.FromMM(18), core.FromMM(30)),
	}
	for _, tr := range board.Traces {
		for i := 0; i <= 20; i++ {
			f := float64(i) / 20
			q := core.NewPoint(
				core.FromMM(tr.Start.X.ToMM()+f*(tr.End.X.ToMM()-tr.Start.X.ToMM())),
				core.FromMM(tr.Start.Y.ToMM()+f*(tr.End.Y.ToMM()-tr.Start.Y.ToMM())))
			if core.PointInPolygon(q, slot) {
				t.Fatalf("trace %s (%v -> %v) runs through the slot", tr.Net, tr.Start, tr.End)
			}
		}
	}
	for _, v := range board.Vias {
		if core.PointInPolygon(v.Position, slot) {
			t.Fatalf("via %s at %v sits in the slot", v.Net, v.Position)
		}
	}
	msg, _ := dispatch(p, "drc", "")
	if strings.Contains(msg, "edge") {
		t.Fatalf("drc after route reports edge problems:\n%s", msg)
	}
	t.Logf("drc: %s", firstLine(msg))
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// TestPlaceStaysOnPolygonOutline: placement legality only checked the
// bounding box, so place-legal and auto-place could drop parts in a milled
// slot.
func TestPlaceStaysOnPolygonOutline(t *testing.T) {
	for _, verb := range []string{"place-legal", "auto-place"} {
		t.Run(verb, func(t *testing.T) {
			p := core.NewProject("slot-place")
			lines := []string{
				// A U: 30x24 with a 14x16 slot from the top edge.
				"outline-poly 0 0 30 0 30 24 22 24 22 8 8 8 8 24 0 24",
				"lib demo_r",
				"  pad 1 -0.775 0 0.95 0.95",
				"  pad 2 0.775 0 0.95 0.95",
			}
			var refs []string
			for i := 1; i <= 8; i++ {
				ref := fmt.Sprintf("R%d", i)
				refs = append(refs, ref+".2")
				lines = append(lines, "sym "+ref+" resistor key=demo_r value=10k", "palette "+ref+" demo_r")
				if verb == "place-legal" {
					lines = append(lines, "place-legal "+ref)
				}
			}
			lines = append(lines, "net GND "+strings.Join(refs, " "))
			if verb == "auto-place" {
				lines = append(lines, "auto-place")
			}
			for _, r := range RunScript(p, strings.Join(lines, "\n")) {
				if !r.OK {
					t.Fatalf("line %d %s: %s", r.Line, r.Tool, r.Result)
				}
			}
			slot := []core.Point{
				core.NewPoint(core.FromMM(8), core.FromMM(8)), core.NewPoint(core.FromMM(22), core.FromMM(8)),
				core.NewPoint(core.FromMM(22), core.FromMM(24)), core.NewPoint(core.FromMM(8), core.FromMM(24)),
			}
			board := p.Board()
			if len(board.Footprints) != 8 {
				t.Fatalf("placed %d parts, want 8", len(board.Footprints))
			}
			for _, fp := range board.Footprints {
				for i := range fp.Pads {
					if c := core.PadWorldCenter(fp, &fp.Pads[i]); core.PointInPolygon(c, slot) {
						t.Fatalf("%s pad %s at %v lies in the slot", fp.Reference, fp.Pads[i].Number, c)
					}
				}
			}
		})
	}
}
