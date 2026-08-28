package router

import (
	"math/rand"
	"testing"

	"github.com/mentasystems/fragua/internal/core"
)

// The bucketed pad lookup is an optimisation, never a change of answer: for
// every query it must agree with the linear scan it replaced.
func TestPadIndexAgreesWithLinearScan(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	nets := []string{"", "GND", "+5V", "SDA"}
	var pads []padObs
	for i := 0; i < 300; i++ {
		x := rng.Float64() * 64
		y := rng.Float64() * 50
		w := 0.2 + rng.Float64()*2.5
		h := 0.2 + rng.Float64()*2.5
		pads = append(pads, padObs{
			net: nets[rng.Intn(len(nets))], layer: uint8(rng.Intn(2)),
			through: rng.Intn(4) == 0,
			minX:    x, minY: y, maxX: x + w, maxY: y + h,
		})
	}
	// Pads hanging off the outline must still be found from inside it.
	pads = append(pads, padObs{net: "+5V", layer: 0, minX: -1.5, minY: 10, maxX: 0.2, maxY: 11})
	pads = append(pads, padObs{net: "GND", layer: 0, minX: 63.8, minY: 20, maxX: 66, maxY: 21})

	withIdx := &grid{pads: pads, padIdx: newPadIndex(pads, 0, 0, 64, 50)}
	plain := &grid{pads: pads}
	if withIdx.padIdx == nil {
		t.Fatal("index not built")
	}
	for i := 0; i < 20000; i++ {
		x := -3 + rng.Float64()*70
		y := -3 + rng.Float64()*56
		layer := uint8(rng.Intn(2))
		net := nets[rng.Intn(len(nets))]
		need := 0.05 + rng.Float64()*1.8
		want := plain.nearForeignPad(x, y, layer, net, need)
		got := withIdx.nearForeignPad(x, y, layer, net, need)
		if got != want {
			t.Fatalf("nearForeignPad(%.3f,%.3f,L%d,%q,%.3f) = %v, linear scan says %v",
				x, y, layer, net, need, got, want)
		}
	}
}

// newGrid must always file its pads, or every clearance query silently falls
// back to the slow path that made a cross-board hop cost seconds.
func TestNewGridBuildsThePadIndex(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(40), core.FromMM(20)))
	b.Outline = &o
	b.AddFootprint(footprint("R1", 10, 10, []core.Pad{pad("1", -1, 0, "VCC"), pad("2", 1, 0, "GND")}))
	g := newGrid(b, DefaultOptions())
	if g.padIdx == nil {
		t.Fatal("newGrid left the pad index empty")
	}
	if !g.nearForeignPad(9, 10, 0, "GND", 0.3) {
		t.Fatal("the VCC pad at 9,10 must be found through the index")
	}
}
