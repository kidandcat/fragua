package router

import (
	"math"
	"testing"

	"github.com/mentasystems/fragua/internal/core"
)

func thtPad(num string, ox, oy float64, net string, drillMM float64) core.Pad {
	p := pad(num, ox, oy, net)
	p.Size = [2]core.Length{core.FromMM(1.7), core.FromMM(1.7)}
	d := core.FromMM(drillMM)
	p.Drill = &d
	return p
}

func TestHoleMapRespectsFabGap(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(20), core.FromMM(20)))
	b.Outline = &o
	b.AddFootprint(footprint("J1", 10, 10, []core.Pad{thtPad("1", 0, 0, "SIG", 1.0)}))
	h := newHoleMap(b)
	// gap = dist - 0.5 (pad) - 0.15 (via) must reach 0.50 mm ⇒ dist ≥ 1.15 mm.
	if h.ok(10.0+1.10, 10, 0.30) {
		t.Fatal("a via 1.10 mm from a 1.0 mm drill must be refused")
	}
	if !h.ok(10.0+1.30, 10, 0.30) {
		t.Fatal("a via 1.30 mm from a 1.0 mm drill is legal")
	}
	h.add(10.0+1.30, 10, 0.30)
	if h.ok(10.0+1.35, 10, 0.30) {
		t.Fatal("a freshly added drill must constrain the next one")
	}
}

// The A* refuses to change layers where the barrel would break the fab's
// hole-to-hole gap — the check that used to be missing entirely, so the
// search happily flipped layers on top of a through-hole pad.
func TestViaSiteRefusesCellsOnDrills(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(20), core.FromMM(20)))
	b.Outline = &o
	b.AddFootprint(footprint("J1", 10, 10, []core.Pad{thtPad("1", 0, 0, "SIG", 1.0)}))
	g := newGrid(b, DefaultOptions())
	cx, cy, ok := g.worldToCell(core.FromMM(10), core.FromMM(10))
	if !ok {
		t.Fatal("pad cell off grid")
	}
	if g.viaSiteOK(cx, cy, "SIG") {
		t.Fatal("a via on top of a 1.0 mm drill must be refused, own net or not")
	}
	fx, fy, _ := g.worldToCell(core.FromMM(16), core.FromMM(16))
	if !g.viaSiteOK(fx, fy, "SIG") {
		t.Fatal("open copper 8 mm from any drill must accept a via")
	}
}

// The router used to drop a layer-change via straight onto a through-hole
// pad it was landing on, which DRC then reported as hole_to_hole. A drilled
// pad is copper on every layer, so no via is owed there in the first place.
func TestRouterKeepsViasOffDrills(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(40), core.FromMM(30)))
	b.Outline = &o
	// Two THT headers with a wall of foreign pads between them, so the
	// router has to change layers to get across.
	b.AddFootprint(footprint("J1", 6, 15, []core.Pad{thtPad("1", 0, 0, "SIG", 1.0)}))
	b.AddFootprint(footprint("J2", 34, 15, []core.Pad{thtPad("1", 0, 0, "SIG", 1.0)}))
	var wall []core.Pad
	for i, y := range []float64{-9, -6, -3, 0, 3, 6, 9} {
		wall = append(wall, padNoNet(string(rune('a'+i)), 0, y))
	}
	b.AddFootprint(footprint("U1", 20, 15, wall))

	opts := DefaultOptions()
	opts.MaxSeconds = 20
	Route(b, opts)

	minGap := core.ActiveFabRules(b).MinHoleToHoleMM
	if minGap <= 0 {
		minGap = 0.50
	}
	for _, v := range b.Vias {
		vx, vy, vr := v.Position.X.ToMM(), v.Position.Y.ToMM(), v.Drill.ToMM()/2
		for _, fp := range footprintsStable(b) {
			for i := range fp.Pads {
				p := &fp.Pads[i]
				if p.Drill == nil || *p.Drill <= 0 {
					continue
				}
				c := core.PadWorldCenter(fp, p)
				gap := math.Hypot(c.X.ToMM()-vx, c.Y.ToMM()-vy) - vr - p.Drill.ToMM()/2
				if gap+1e-6 < minGap {
					t.Fatalf("via %s at (%.2f,%.2f) is %.3f mm from %s.%s, fab needs %.3f mm",
						v.Net, vx, vy, gap, fp.Reference, p.Number, minGap)
				}
			}
		}
		for _, w := range b.Vias {
			if w.ID == v.ID {
				continue
			}
			gap := math.Hypot(w.Position.X.ToMM()-vx, w.Position.Y.ToMM()-vy) - vr - w.Drill.ToMM()/2
			if gap+1e-6 < minGap {
				t.Fatalf("vias %.2f,%.2f and %.2f,%.2f are %.3f mm apart, fab needs %.3f mm",
					vx, vy, w.Position.X.ToMM(), w.Position.Y.ToMM(), gap, minGap)
			}
		}
	}
}
