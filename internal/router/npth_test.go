package router

import (
	"math"
	"testing"

	"github.com/mentasystems/fragua/internal/core"
)

// TestRouteAvoidsNPTHHolesAndCutouts: the router only knew NPTH holes for via
// drill spacing and never saw cutouts, so a net whose straight path crossed a
// mounting hole was routed right through it.
func TestRouteAvoidsNPTHHolesAndCutouts(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(60), core.FromMM(40)))
	b.Outline = &o
	b.AddFootprint(footprint("A", 8, 20, []core.Pad{pad("1", 0, 0, "SIG")}))
	b.AddFootprint(footprint("B", 52, 20, []core.Pad{pad("1", 0, 0, "SIG")}))
	hole := core.MountHole{ID: core.NewID(), Center: core.NewPoint(core.FromMM(22), core.FromMM(20)), Diameter: core.FromMM(2.7)}
	b.MountHoles = append(b.MountHoles, hole)
	slot := []core.Point{
		core.NewPoint(core.FromMM(36), core.FromMM(18)),
		core.NewPoint(core.FromMM(39), core.FromMM(18)),
		core.NewPoint(core.FromMM(39), core.FromMM(22)),
		core.NewPoint(core.FromMM(36), core.FromMM(22)),
	}
	b.Cutouts = append(b.Cutouts, core.Cutout{ID: core.NewID(), Polygon: slot})

	rep := Route(b, DefaultOptions())
	for _, n := range rep.PerNet {
		if n.Net == "SIG" && n.Outcome.Status != "ok" {
			t.Fatalf("SIG failed: %+v summary=%s", n.Outcome, rep.Summary())
		}
	}
	if len(b.Traces) == 0 {
		t.Fatal("expected copper on board")
	}
	hc := ptMM(hole.Center)
	for _, tr := range b.Traces {
		a, c := ptMM(tr.Start), ptMM(tr.End)
		gap := pointSegDist(hc, a, c) - tr.Width.ToMM()/2 - hole.Diameter.ToMM()/2
		if gap < core.MountHoleClearanceMM-1e-3 {
			t.Fatalf("trace %v→%v is %.3f mm from the hole wall (< %.2f)", a, c, gap, core.MountHoleClearanceMM)
		}
		for i := range slot {
			p, q := ptMM(slot[i]), ptMM(slot[(i+1)%len(slot)])
			if g := segSegDist(a, c, p, q) - tr.Width.ToMM()/2; g < routerEdgeClearanceMM-1e-3 {
				t.Fatalf("trace %v→%v is %.3f mm from the cutout edge", a, c, g)
			}
		}
		mid := p2{(a[0] + c[0]) / 2, (a[1] + c[1]) / 2}
		if core.PointInPolygon(core.NewPoint(core.FromMM(mid[0]), core.FromMM(mid[1])), slot) {
			t.Fatalf("trace %v→%v runs inside the cutout", a, c)
		}
	}
	for _, v := range b.Vias {
		p := ptMM(v.Position)
		if d := math.Hypot(p[0]-hc[0], p[1]-hc[1]) - v.Diameter.ToMM()/2 - hole.Diameter.ToMM()/2; d < core.MountHoleClearanceMM-1e-3 {
			t.Fatalf("via at %v is %.3f mm from the hole wall", p, d)
		}
	}
}

// TestStitchKeepsViasOffNPTHHoles: stitching skipped cutouts and keepouts but
// would drop a via onto the copper ring around a mounting hole.
func TestStitchKeepsViasOffNPTHHoles(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(40), core.FromMM(40)))
	b.Outline = &o
	b.Pours = []core.Pour{{
		Net: "GND", Layer: core.LayerTop,
		Stitching: &core.StitchPolicy{Enabled: true, PitchMM: 1},
	}}
	b.MountHoles = append(b.MountHoles, core.MountHole{
		ID: core.NewID(), Center: core.NewPoint(core.FromMM(20), core.FromMM(20)), Diameter: core.FromMM(3.2),
	})
	n := "GND"
	b.AddFootprint(&core.Footprint{
		ID: core.NewID(), Reference: "R1",
		Position: core.NewPoint(core.FromMM(20), core.FromMM(2)),
		Layer:    core.LayerBottom,
		Pads: []core.Pad{{
			Number: "1", Size: [2]core.Length{core.FromMM(1), core.FromMM(1)},
			Layer: core.LayerBottom, Net: &n,
		}},
	})
	StitchIsolatedPads(b, DefaultOptions())
	if len(b.Vias) == 0 {
		t.Fatal("expected stitch vias somewhere on the board")
	}
	for _, v := range b.Vias {
		d := math.Hypot(v.Position.X.ToMM()-20, v.Position.Y.ToMM()-20) - v.Diameter.ToMM()/2 - 1.6
		if d < core.MountHoleClearanceMM-1e-3 {
			t.Fatalf("via at %.2f,%.2f is %.3f mm from the hole wall", v.Position.X.ToMM(), v.Position.Y.ToMM(), d)
		}
	}
}
