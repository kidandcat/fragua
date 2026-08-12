package placer

import (
	"testing"

	"github.com/mentasystems/fragua/internal/core"
)

func pad(num string, ox, oy float64, net string) core.Pad {
	n := net
	return core.Pad{
		Number: num,
		Offset: core.NewPoint(core.FromMM(ox), core.FromMM(oy)),
		Size:   [2]core.Length{core.FromMM(1.0), core.FromMM(1.2)},
		Layer:  core.LayerTop,
		Net:    &n,
	}
}

func footprint(ref string, x, y float64, pads []core.Pad) *core.Footprint {
	return &core.Footprint{
		ID:        core.NewID(),
		Reference: ref,
		Library:   "demo",
		Position:  core.NewPoint(core.FromMM(x), core.FromMM(y)),
		Layer:     core.LayerTop,
		Pads:      pads,
	}
}

func TestPlacerReducesHPWL(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(50), core.FromMM(30)))
	b.Outline = &o
	b.AddFootprint(footprint("R1", 5, 5, []core.Pad{
		pad("1", -1, 0, "S"),
		pad("2", 1, 0, "OUT"),
	}))
	b.AddFootprint(footprint("R2", 45, 25, []core.Pad{
		pad("1", -1, 0, "S"),
		pad("2", 1, 0, "OUT"),
	}))

	opts := DefaultOptions()
	opts.Seed = 42
	// relax solder gap for this synthetic case so SA can pack same-net parts
	opts.SolderGapMM = 0.5
	rep, err := Place(b, []string{"R1", "R2"}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if rep.FinalHPWLMM >= rep.InitialHPWLMM {
		t.Fatalf("expected HPWL drop, got %.2f → %.2f", rep.InitialHPWLMM, rep.FinalHPWLMM)
	}
	if rep.FinalHPWLMM >= 40 {
		t.Fatalf("HPWL still high: %.2f", rep.FinalHPWLMM)
	}
}

func TestGerberFabPack(t *testing.T) {
	// see gerber package
}
