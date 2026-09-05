package placer

import (
	"math"
	"testing"

	"github.com/mentasystems/fragua/internal/core"
)

func setupBoostIsland(t *testing.T) *core.Board {
	t.Helper()
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(22), core.FromMM(16)))
	b.Outline = &o
	u3 := footprint("U3", 11, 8, []core.Pad{
		pad("1", -1.002703, 0.95, "OUT"),
		pad("2", -1.002703, 0, "GND"),
		pad("3", -1.002703, -0.95, "EN"),
		pad("4", 1.002703, -0.95, "IN"),
		pad("5", 1.002703, 0, "SEL"),
		pad("6", 1.002703, 0.95, "LX"),
	})
	u3.Pinned = true
	u3.BodyRect = &core.BodyRect{MinXMM: -1.9019, MinYMM: -1.7, MaxXMM: 1.9019, MaxYMM: 1.7}
	b.AddFootprint(u3)
	mk := func(ref string, x, y float64, n1, n2 string, ind bool) {
		fp := footprint(ref, x, y, []core.Pad{pad("1", -1.002204, 0, n1), pad("2", 1.002204, 0, n2)})
		if ind {
			fp.Key = "l_2016"
			fp.Description = "chip inductor"
			fp.BodyRect = &core.BodyRect{MinXMM: -1.7019, MinYMM: -1.0069, MaxXMM: 1.7019, MaxYMM: 1.0069}
		} else {
			fp.BodyRect = &core.BodyRect{MinXMM: -1.48, MinYMM: -0.76, MaxXMM: 1.48, MaxYMM: 0.76}
		}
		b.AddFootprint(fp)
	}
	mk("L2", 5, 5, "LX", "IN", true)
	mk("Cin", 6, 6, "IN", "GND", false)
	mk("Cout", 7, 7, "OUT", "GND", false)
	mk("Rsel", 8, 8, "SEL", "EN", false)
	mk("Cen", 9, 9, "EN", "GND", false)
	return b
}

// DefaultOptions uses a 2 mm SA assembly gap; island seating must still
// land the inductor pad-to-pad on LX/IN.
func TestPlaceBoostIslandDespiteWideSolderGap(t *testing.T) {
	b := setupBoostIsland(t)
	opts := DefaultOptions()
	opts.Seed = 42
	if _, err := Place(b, nil, opts); err != nil {
		t.Fatal(err)
	}
	u3 := b.FootprintByRef("U3")
	l2 := b.FootprintByRef("L2")
	lx := core.PadWorldCenter(u3, &u3.Pads[5])
	lpad := core.PadWorldCenter(l2, &l2.Pads[0])
	d := math.Hypot(lx.X.ToMM()-lpad.X.ToMM(), lx.Y.ToMM()-lpad.Y.ToMM())
	if d > 2.5 {
		t.Fatalf("L2.LX pad is %.2f mm from U3.LX after Place (L2 at %.2f,%.2f); want ≤ 2.5",
			d, l2.Position.X.ToMM(), l2.Position.Y.ToMM())
	}
}
