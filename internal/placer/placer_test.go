package placer

import (
	"fmt"
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

// A no_place keepout is a placement exclusion: an antenna zone, a milled slot,
// a screw head. The placer used to ignore core.Keepout entirely, so auto-place
// would seat parts inside one whenever that shortened a net.
func TestPlacerStaysOutOfNoPlaceKeepout(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(50), core.FromMM(30)))
	b.Outline = &o
	// Two anchors either side of a forbidden band; the cheapest wirelength
	// for the movable part is dead centre, which is inside the band.
	b.AddFootprint(footprint("U1", 5, 15, []core.Pad{pad("1", 0, 0, "N")}))
	b.AddFootprint(footprint("U2", 45, 15, []core.Pad{pad("1", 0, 0, "N")}))
	b.AddFootprint(footprint("C1", 25, 15, []core.Pad{
		pad("1", -0.8, 0, "N"),
		pad("2", 0.8, 0, "N"),
	}))
	ko := core.RectFromCorners(
		core.NewPoint(core.FromMM(18), core.FromMM(6)),
		core.NewPoint(core.FromMM(32), core.FromMM(24)))
	b.Keepouts = append(b.Keepouts, core.Keepout{
		ID: core.NewID(), Rect: &ko, NoPlace: true, NoCopper: true,
	})

	opts := DefaultOptions()
	opts.Seed = 7
	opts.SolderGapMM = 0.5
	if _, err := Place(b, []string{"C1"}, opts); err != nil {
		t.Fatal(err)
	}
	c1 := b.FootprintByRef("C1")
	if c1 == nil {
		t.Fatal("C1 vanished")
	}
	if hitsNoPlace(b, c1) {
		t.Fatalf("C1 was placed inside the no_place keepout at (%.2f, %.2f)",
			c1.Position.X.ToMM(), c1.Position.Y.ToMM())
	}
	// It must still be a real placement, not a refusal to move at all: inside
	// the outline and clear of the other parts at the gap this run asked for.
	if !padsInside(c1, b.Outline, opts.EdgeClearanceMM) ||
		firstOverlapperGap(b, c1, opts.SolderGapMM) {
		t.Fatalf("C1 ended in an illegal pose at (%.2f, %.2f)",
			c1.Position.X.ToMM(), c1.Position.Y.ToMM())
	}
}

// A part the script dropped inside an exclusion must be walked out, not left
// there: hitsNoPlace only refuses moves *into* a keepout, the score penalty is
// what makes staying put expensive.
func TestPlacerEvictsPartAlreadyInKeepout(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(50), core.FromMM(30)))
	b.Outline = &o
	b.AddFootprint(footprint("U1", 40, 15, []core.Pad{pad("1", 0, 0, "N")}))
	b.AddFootprint(footprint("C1", 10, 15, []core.Pad{
		pad("1", -0.8, 0, "N"),
		pad("2", 0.8, 0, "N"),
	}))
	ko := core.RectFromCorners(
		core.NewPoint(core.FromMM(4), core.FromMM(8)),
		core.NewPoint(core.FromMM(16), core.FromMM(22)))
	b.Keepouts = append(b.Keepouts, core.Keepout{
		ID: core.NewID(), Rect: &ko, NoPlace: true,
	})

	opts := DefaultOptions()
	opts.Seed = 11
	opts.SolderGapMM = 0.5
	if _, err := Place(b, []string{"C1"}, opts); err != nil {
		t.Fatal(err)
	}
	if c1 := b.FootprintByRef("C1"); hitsNoPlace(b, c1) {
		t.Fatalf("C1 stayed in the keepout at (%.2f, %.2f)",
			c1.Position.X.ToMM(), c1.Position.Y.ToMM())
	}
}

// `auto-place solder_gap=N` has to be able to buy room on a small board: the
// hard overlap rejection used to ignore it and always apply the 2.0 mm package
// constant, so a dense 30 mm carrier had no legal sites at all.
func TestSolderGapOptionRelaxesTheHardOverlapTest(t *testing.T) {
	mk := func() (*core.Board, *core.Footprint) {
		b := core.NewBoard()
		o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(12), core.FromMM(12)))
		b.Outline = &o
		b.AddFootprint(footprint("U1", 6, 6, []core.Pad{pad("1", 0, 0, "N")}))
		probe := footprint("C1", 8.2, 6, []core.Pad{
			pad("1", -0.8, 0, "N"),
			pad("2", 0.8, 0, "N"),
		})
		b.AddFootprint(probe)
		return b, b.FootprintByRef("C1")
	}
	b, probe := mk()
	if !firstOverlapperGap(b, probe, 2.0) {
		t.Fatal("at the 2.0 mm package gap these two must read as overlapping")
	}
	if firstOverlapperGap(b, probe, 0.3) {
		t.Fatal("at a 0.3 mm assembly gap they must not: the copper is 0.4 mm apart")
	}
	// And the option has to reach the annealer, not just the helper.
	opts := DefaultOptions()
	opts.Seed = 3
	opts.SolderGapMM = 0.3
	if _, err := Place(b, []string{"C1"}, opts); err != nil {
		t.Fatal(err)
	}
}

// A part that is ALREADY overlapping when Place is called has to be walked out.
// The hard veto only refuses moves into an overlap; without a score gradient
// the annealer left a part that started on top of an IC exactly where it was.
func TestPlacerRepairsAnOverlappingStart(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(40), core.FromMM(40)))
	b.Outline = &o
	// A wide IC, and a decap dropped straight on top of it.
	var pads []core.Pad
	for i := 0; i < 8; i++ {
		pads = append(pads, pad(fmt.Sprint(i+1), -3.5+float64(i), 0, "N"))
	}
	b.AddFootprint(footprint("U1", 20, 20, pads))
	b.AddFootprint(footprint("C1", 20, 20, []core.Pad{
		pad("1", -0.8, 0, "N"),
		pad("2", 0.8, 0, "N"),
	}))

	opts := DefaultOptions()
	opts.Seed = 5
	opts.SolderGapMM = 0.5
	if _, err := Place(b, []string{"C1"}, opts); err != nil {
		t.Fatal(err)
	}
	c1 := b.FootprintByRef("C1")
	if firstOverlapperGap(b, c1, opts.SolderGapMM) {
		t.Fatalf("C1 stayed on top of U1 at (%.2f, %.2f)",
			c1.Position.X.ToMM(), c1.Position.Y.ToMM())
	}
}

// Two SMD parts on opposite faces are a board thickness apart and cannot
// collide. Treating them as if they could threw away the whole bottom side.
func TestOppositeFacesDoNotCollide(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(20), core.FromMM(20)))
	b.Outline = &o
	top := footprint("U1", 10, 10, []core.Pad{pad("1", 0, 0, "N")})
	bot := footprint("J1", 10, 10, []core.Pad{pad("1", 0, 0, "N")})
	bot.Layer = core.LayerBottom
	b.AddFootprint(top)
	b.AddFootprint(bot)

	probe := b.FootprintByRef("J1")
	if firstOverlapperGap(b, probe, 0.5) {
		t.Fatal("bottom-side SMD must not collide with a top-side SMD part")
	}
	// A through-hole pad IS on every layer, so it must still collide.
	d := core.FromMM(0.8)
	probe.Pads[0].Drill = &d
	if !firstOverlapperGap(b, probe, 0.5) {
		t.Fatal("a through-hole pad collides with the other face")
	}
}
