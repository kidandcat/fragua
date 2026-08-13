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

func TestSASeedIsDeterministic(t *testing.T) {
	mk := func() *core.Board {
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
		return b
	}
	opts := DefaultOptions()
	opts.Seed = 42
	opts.GlobalStage = false
	a, err := Place(mk(), []string{"R1", "R2"}, opts)
	if err != nil {
		t.Fatal(err)
	}
	b1 := mk()
	if _, err := Place(b1, []string{"R1", "R2"}, opts); err != nil {
		t.Fatal(err)
	}
	b2 := mk()
	if _, err := Place(b2, []string{"R1", "R2"}, opts); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"R1", "R2"} {
		p1 := b1.FootprintByRef(ref)
		p2 := b2.FootprintByRef(ref)
		if p1.Position != p2.Position || p1.Rotation != p2.Rotation {
			t.Fatalf("%s positions differ across same-seed runs: %+v vs %+v", ref, p1.Position, p2.Position)
		}
	}
	if a.FinalHPWLMM >= a.InitialHPWLMM {
		t.Fatalf("expected HPWL drop, got %.2f → %.2f", a.InitialHPWLMM, a.FinalHPWLMM)
	}
}

func TestGerberFabPack(t *testing.T) {
	// see gerber package
}
