package placer

import (
	"fmt"
	"math"
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

// A part the agent positioned with `place` is pinned: a bare auto-place
// (no refs) must leave it exactly where it was put, the same way it leaves
// edge-mounted connectors alone. Naming it explicitly still moves it.
func TestPinnedFootprintSurvivesBareAutoPlace(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(50), core.FromMM(30)))
	b.Outline = &o
	anchor := footprint("U1", 40, 25, []core.Pad{
		pad("1", -1, 0, "S"),
		pad("2", 1, 0, "OUT"),
	})
	anchor.Pinned = true
	b.AddFootprint(anchor)
	b.AddFootprint(footprint("R1", 5, 5, []core.Pad{
		pad("1", -1, 0, "S"),
		pad("2", 1, 0, "OUT"),
	}))

	want := anchor.Position
	opts := DefaultOptions()
	opts.Seed = 42
	if _, err := Place(b, nil, opts); err != nil {
		t.Fatal(err)
	}
	got := b.FootprintByRef("U1").Position
	if got != want {
		t.Fatalf("pinned U1 moved: %v → %v", want, got)
	}
	if b.FootprintByRef("R1").Position == core.NewPoint(core.FromMM(5), core.FromMM(5)) {
		t.Fatal("expected the unpinned R1 to be arranged")
	}

	// Explicit ref is the escape hatch: a board of nothing but pinned parts
	// still has something to anneal when they are named.
	b2 := core.NewBoard()
	b2.Outline = &o
	only := footprint("U1", 40, 25, []core.Pad{pad("1", -1, 0, "S")})
	only.Pinned = true
	b2.AddFootprint(only)
	if _, err := Place(b2, []string{"U1"}, opts); err != nil {
		t.Fatalf("naming a pinned ref should still place it: %v", err)
	}
}

// edge-place must turn the footprint's mating face toward the outside of the
// board. Before, only `left` consulted EdgeSide, so an ESP32-S3-Zero
// (EdgeSide=bottom, USB-C on its -Y face) edge-placed `right` came out 180°
// off with the connector buried in the board.
func TestEdgePlaceTurnsTheMatingFaceOutward(t *testing.T) {
	bottom, top := core.EdgeBottom, core.EdgeTop
	cases := []struct {
		side string
		face *core.EdgeSide
		want float64
	}{
		{"right", &bottom, 90},
		{"left", &bottom, 270},
		{"top", &bottom, 180},
		{"bottom", &bottom, 0},
		{"right", nil, 90}, // no declared face = -Y, same as bottom
		{"top", &top, 0},   // screw terminal: wire entry already at +Y
		{"bottom", &top, 180},
		{"left", &top, 90}, // the one case the old code special-cased
	}
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(40), core.FromMM(30)))
	for _, c := range cases {
		fp := footprint("J1", 20, 15, []core.Pad{pad("1", -1.27, 0, "A"), pad("2", 1.27, 0, "B")})
		fp.EdgeMounted = true
		fp.EdgeSide = c.face
		EdgePlace(fp, o, c.side, nil, EdgeClearancesFor(nil))
		if fp.Rotation != c.want {
			face := "nil"
			if c.face != nil {
				face = string(*c.face)
			}
			t.Errorf("edge-place %s (face %s) rot = %.0f, want %.0f", c.side, face, fp.Rotation, c.want)
		}
	}
}

// A part with a declared body is bigger than its pads: a screw terminal's wire
// mouth, a module wider than its castellated rows. auto-place used to seat a
// neighbour inside that body because the hard overlap test looked at pads
// only, and DRC then reported a courtyard_overlap the agent had to fix by hand.
func TestPlacerRespectsADeclaredCourtyard(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(40), core.FromMM(20)))
	b.Outline = &o
	// J1's pads are 5 mm wide but its body reaches 6 mm past them in +Y.
	j1 := footprint("J1", 8, 8, []core.Pad{pad("1", -2.5, 0, "A"), pad("2", 2.5, 0, "B")})
	j1.PlacementMargin = core.PlacementMargin{TopMM: 6, RightMM: 1.5, BottomMM: 1.5, LeftMM: 1.5}
	j1.Pinned = true
	b.AddFootprint(j1)
	// U1 starts inside that mouth. Pads alone read this as clear.
	u1 := footprint("U1", 8, 12, []core.Pad{pad("1", -1, 0, "A"), pad("2", 1, 0, "N")})
	b.AddFootprint(u1)

	if !firstOverlapperGap(b, u1, 0.2) {
		t.Fatal("U1 sits inside J1's declared courtyard; the placer must see it")
	}
	opts := DefaultOptions()
	opts.Seed = 5
	opts.SolderGapMM = 0.2
	if _, err := Place(b, []string{"U1"}, opts); err != nil {
		t.Fatal(err)
	}
	if firstOverlapperGap(b, b.FootprintByRef("U1"), 0.2) {
		t.Fatalf("U1 stayed in the courtyard at (%.2f, %.2f)",
			u1.Position.X.ToMM(), u1.Position.Y.ToMM())
	}
}

// thtPad is a plated barrel: a square pad with a drill through it.
func thtPad(num string, ox, oy, sizeMM, drillMM float64, net string) core.Pad {
	p := pad(num, ox, oy, net)
	p.Size = [2]core.Length{core.FromMM(sizeMM), core.FromMM(sizeMM)}
	d := core.FromMM(drillMM)
	p.Drill = &d
	return p
}

// edge-place used to slide the PAD BBOX until it kissed the outline, whatever
// the part was. That is right for a castellated module and wrong for anything
// with a hole in it: a 2.54 mm header came out with its 1.0 mm barrels 0.35 mm
// from the routed edge, and a screw terminal hung its wire mouth off the board.
// The rule is now: no drills → pad bbox, flush; drills → declared courtyard
// (else pad bbox) plus the fab copper/drill-to-edge floor.
func TestEdgePlaceKeepsThroughHoleBarrelsOffTheEdge(t *testing.T) {
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(40), core.FromMM(30)))
	cl := EdgeClearancesFor(nil)

	j1 := footprint("J1", 20, 15, []core.Pad{
		thtPad("1", -3.81, 0, 1.7, 1.0, "A"),
		thtPad("2", -1.27, 0, 1.7, 1.0, "B"),
		thtPad("3", 1.27, 0, 1.7, 1.0, "C"),
		thtPad("4", 3.81, 0, 1.7, 1.0, "D"),
	})
	j1.EdgeMounted = true
	EdgePlace(j1, o, "bottom", nil, cl)

	for i := range j1.Pads {
		pad := &j1.Pads[i]
		copperGap := (core.PadWorldAABB(j1, pad).Min.Y - o.Min.Y).ToMM()
		if copperGap+1e-9 < cl.CopperMM {
			t.Errorf("pad %s copper %.3f mm from the edge, want >= %.3f",
				pad.Number, copperGap, cl.CopperMM)
		}
		hole := core.RectFromCenter(core.PadWorldCenter(j1, pad), *pad.Drill, *pad.Drill)
		drillGap := (hole.Min.Y - o.Min.Y).ToMM()
		if drillGap+1e-9 < cl.DrillMM {
			t.Errorf("pad %s barrel %.3f mm from the edge, want >= %.3f",
				pad.Number, drillGap, cl.DrillMM)
		}
	}
	// Near the edge, not parked in the middle of the board: the tightest
	// constraint is what decides, nothing more.
	if got := j1.Position.Y.ToMM(); got > 1.5 {
		t.Errorf("J1 pulled %.3f mm inboard; the clearance alone should decide", got)
	}
	// auto-place re-snaps every edge-mounted part; that must not undo this.
	before := j1.Position
	SnapToNearestEdge(j1, o, cl)
	if j1.Position != before {
		t.Errorf("re-snap moved J1 from %v to %v; snapping must be a fixed point",
			before, j1.Position)
	}
}

// A screw terminal's body is much bigger than its two barrels: the wire mouth
// sticks 4 mm past the pads. Snapping by pads hung that mouth off the board.
// The declared courtyard is the extent, so the block sits fully on the board
// with its mouth ending exactly at the edge the wires leave by.
func TestEdgePlaceSeatsAScrewTerminalBodyOnTheBoard(t *testing.T) {
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(40), core.FromMM(30)))
	cl := EdgeClearancesFor(nil)
	top := core.EdgeTop

	j2 := footprint("J2", 20, 15, []core.Pad{
		thtPad("1", -2.5, 0, 2.0, 1.2, "A"),
		thtPad("2", 2.5, 0, 2.0, 1.2, "B"),
	})
	j2.EdgeMounted = true
	j2.EdgeSide = &top
	j2.PlacementMargin = core.PlacementMargin{TopMM: 4, RightMM: 1.5, BottomMM: 2.5, LeftMM: 1.5}
	EdgePlace(j2, o, "top", nil, cl)

	body, ok := core.CourtyardWorld(j2)
	if !ok {
		t.Fatal("screw terminal has no courtyard")
	}
	if d := (o.Max.Y - body.Max.Y).ToMM(); math.Abs(d) > 1e-6 {
		t.Errorf("wire mouth %.3f mm from the top edge, want flush", d)
	}
	if body.Min.X < o.Min.X || body.Max.X > o.Max.X || body.Min.Y < o.Min.Y {
		t.Errorf("body %v hangs off the outline %v", body, o)
	}
	for i := range j2.Pads {
		pad := &j2.Pads[i]
		if gap := (o.Max.Y - core.PadWorldAABB(j2, pad).Max.Y).ToMM(); gap+1e-9 < cl.CopperMM {
			t.Errorf("pad %s copper %.3f mm from the edge", pad.Number, gap)
		}
	}
}

// The castellated case must not regress: an ESP32-S3-Zero has SMD half-hole
// rows and a body that reaches past them, and the module's USB-C end is only
// reachable by a cable if that body is allowed to overhang the short edge.
func TestEdgePlaceKeepsACastellatedModuleFlush(t *testing.T) {
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(88), core.FromMM(20)))
	cl := EdgeClearancesFor(nil)
	bottom := core.EdgeBottom

	u1 := footprint("U1", 40, 10, []core.Pad{
		pad("V5", 7.62, -10.16, "V5"),
		pad("GND", 7.62, 0, "GND"),
		pad("GPIO6", 7.62, 10.16, "S1"),
		pad("GPIO7", -7.62, 10.16, "S2"),
		pad("TX", -7.62, -10.16, "S3"),
	})
	u1.EdgeMounted = true
	u1.EdgeSide = &bottom
	u1.BodyRect = &core.BodyRect{MinXMM: -9, MinYMM: -12.75, MaxXMM: 9, MaxYMM: 12.75}
	EdgePlace(u1, o, "right", nil, cl)

	if u1.Rotation != 90 {
		t.Fatalf("rot = %.0f, want 90 (USB-C face pointing +X)", u1.Rotation)
	}
	pads, ok := footprintBounds(u1)
	if !ok {
		t.Fatal("no pad bounds")
	}
	if d := (o.Max.X - pads.Max.X).ToMM(); math.Abs(d) > 1e-6 {
		t.Errorf("castellated row %.3f mm from the edge, want flush", d)
	}
	body, ok := core.CourtyardWorld(u1)
	if !ok {
		t.Fatal("no courtyard")
	}
	if body.Max.X <= o.Max.X {
		t.Errorf("module body ends at %.2f, inside the edge %.2f: the USB-C shell has to overhang",
			body.Max.X.ToMM(), o.Max.X.ToMM())
	}
}

func TestPullPassivesSeatsBoostPowerIsland(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(40), core.FromMM(30)))
	b.Outline = &o
	u3 := footprint("U3", 20, 15, []core.Pad{
		pad("OUT", 2.5, 0, "OUT"),
		pad("GND", 0, -2.5, "GND"),
		pad("LX", -2.5, 0, "LX"),
		pad("IN", 0, 2.5, "IN"),
	})
	u3.Pinned = true
	b.AddFootprint(u3)
	l2 := footprint("L2", 3, 3, []core.Pad{
		pad("1", -0.8, 0, "LX"),
		pad("2", 0.8, 0, "IN"),
	})
	l2.Key = "l_2016"
	l2.Description = "chip inductor"
	b.AddFootprint(l2)
	cin := footprint("Cin", 36, 4, []core.Pad{
		pad("1", -0.8, 0, "IN"),
		pad("2", 0.8, 0, "GND"),
	})
	b.AddFootprint(cin)
	cout := footprint("Cout", 36, 26, []core.Pad{
		pad("1", -0.8, 0, "OUT"),
		pad("2", 0.8, 0, "GND"),
	})
	b.AddFootprint(cout)

	opts := DefaultOptions()
	opts.SolderGapMM = 0.3
	PullPassivesToAnchors(b, []*core.Footprint{l2, cin, cout}, opts)

	lx := core.PadWorldCenter(u3, &u3.Pads[2]) // LX
	out := core.PadWorldCenter(u3, &u3.Pads[0])
	lPos := l2.Position
	cPos := cout.Position

	lPad := core.PadWorldCenter(l2, &l2.Pads[0]) // LX pad on L2
	dPad := math.Hypot(lPad.X.ToMM()-lx.X.ToMM(), lPad.Y.ToMM()-lx.Y.ToMM())
	dLToLX := math.Hypot(lPos.X.ToMM()-lx.X.ToMM(), lPos.Y.ToMM()-lx.Y.ToMM())
	dLToCorner := math.Hypot(lPos.X.ToMM()-3, lPos.Y.ToMM()-3)
	if dPad > 2.5 {
		t.Fatalf("L2.LX pad is %.2f mm from U3.LX (L2 at %.2f,%.2f); want pad-to-pad ≤ 2.5 mm on the island",
			dPad, lPos.X.ToMM(), lPos.Y.ToMM())
	}
	if dLToLX >= 8 {
		t.Fatalf("L2 is %.2f mm from U3.LX (at %.2f,%.2f); want it seated on the LX island, not the corner",
			dLToLX, lPos.X.ToMM(), lPos.Y.ToMM())
	}
	if dLToLX > dLToCorner+0.5 && dLToCorner < 2 {
		t.Fatalf("L2 stayed in the corner (%.2f,%.2f); LX is at (%.2f,%.2f)",
			lPos.X.ToMM(), lPos.Y.ToMM(), lx.X.ToMM(), lx.Y.ToMM())
	}

	dCToOut := math.Hypot(cPos.X.ToMM()-out.X.ToMM(), cPos.Y.ToMM()-out.Y.ToMM())
	dCToFar := math.Hypot(cPos.X.ToMM()-36, cPos.Y.ToMM()-26)
	if dCToOut >= 8 {
		t.Fatalf("Cout is %.2f mm from U3.OUT (at %.2f,%.2f); want it on the OUT island",
			dCToOut, cPos.X.ToMM(), cPos.Y.ToMM())
	}
	if dCToOut > dCToFar+0.5 && dCToFar < 2 {
		t.Fatalf("Cout stayed on the far edge (%.2f,%.2f); OUT is at (%.2f,%.2f)",
			cPos.X.ToMM(), cPos.Y.ToMM(), out.X.ToMM(), out.Y.ToMM())
	}
}

func TestPullPassivesLeavesPinnedFootprintsAlone(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(40), core.FromMM(30)))
	b.Outline = &o
	u3 := footprint("U3", 20, 15, []core.Pad{
		pad("LX", -2.5, 0, "LX"),
		pad("IN", 2.5, 0, "IN"),
	})
	u3.Pinned = true
	b.AddFootprint(u3)
	l2 := footprint("L2", 4, 4, []core.Pad{
		pad("1", -0.8, 0, "LX"),
		pad("2", 0.8, 0, "IN"),
	})
	l2.Pinned = true
	b.AddFootprint(l2)
	want := l2.Position
	PullPassivesToAnchors(b, []*core.Footprint{l2}, DefaultOptions())
	if l2.Position != want {
		t.Fatalf("pinned L2 moved: %v → %v", want, l2.Position)
	}
}
