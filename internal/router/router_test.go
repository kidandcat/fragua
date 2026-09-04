package router

import (
	"math"
	"testing"
	"time"

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

func padNoNet(num string, ox, oy float64) core.Pad {
	return core.Pad{
		Number: num,
		Offset: core.NewPoint(core.FromMM(ox), core.FromMM(oy)),
		Size:   [2]core.Length{core.FromMM(3.0), core.FromMM(3.0)},
		Layer:  core.LayerTop,
		Net:    nil,
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

func TestSinglePadAndPourNetsAreOk(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(40), core.FromMM(20)))
	b.Outline = &o
	b.AddFootprint(footprint("R1", 10, 10, []core.Pad{
		pad("1", -1, 0, "VCC"),
		pad("2", 1, 0, "GND"),
	}))
	b.Pours = []core.Pour{{Net: "GND", Layer: core.LayerTop}}
	rep := Route(b, DefaultOptions())
	by := map[string]string{}
	for _, n := range rep.PerNet {
		by[n.Net] = n.Outcome.Status
	}
	if by["VCC"] != "ok" || by["GND"] != "ok" {
		t.Fatalf("single-pad/pour should be ok, got %+v", rep.PerNet)
	}
	if len(b.Traces) != 0 {
		t.Fatalf("pour/single-pad should lay no copper, got %d traces", len(b.Traces))
	}
}

func TestRoutesTwoResistors(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(40), core.FromMM(20)))
	b.Outline = &o
	b.AddFootprint(footprint("R1", 10, 10, []core.Pad{
		pad("1", -1, 0, "VCC"),
		pad("2", 1, 0, "OUT"),
	}))
	b.AddFootprint(footprint("R2", 20, 10, []core.Pad{
		pad("1", -1, 0, "OUT"),
		pad("2", 1, 0, "GND"),
	}))

	rep := Route(b, DefaultOptions())
	if rep.Failed != 0 {
		t.Fatalf("expected no failed nets, report=%s per=%+v", rep.Summary(), rep.PerNet)
	}
	if rep.TraceCount < 1 || len(b.Traces) < 1 {
		t.Fatalf("expected segments>0, report=%+v board traces=%d", rep, len(b.Traces))
	}
	var outOK bool
	for _, n := range rep.PerNet {
		if n.Net == "OUT" && n.Outcome.Status == "ok" {
			outOK = true
			if n.Outcome.LengthMM <= 0 {
				t.Fatalf("OUT length_mm should be >0: %+v", n.Outcome)
			}
			if n.Outcome.LowerBoundMM <= 0 {
				t.Fatalf("OUT lower_bound_mm should be >0: %+v", n.Outcome)
			}
		}
		if n.Outcome.Status == "failed" && n.Net == "OUT" {
			t.Fatalf("OUT failed: %+v", n.Outcome)
		}
	}
	if !outOK {
		t.Fatalf("OUT not ok: %+v", rep.PerNet)
	}
	t.Logf("two-resistor measure: %s traces=%d", rep.Summary(), len(b.Traces))
}

func TestThetaStarEmitsDiagonal(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(40), core.FromMM(30)))
	b.Outline = &o
	b.AddFootprint(footprint("R1", 10, 10, []core.Pad{
		pad("1", 0, 0, "DIAG"),
	}))
	b.AddFootprint(footprint("R2", 30, 20, []core.Pad{
		pad("1", 0, 0, "DIAG"),
	}))
	rep := Route(b, DefaultOptions())
	ok := false
	for _, n := range rep.PerNet {
		if n.Net == "DIAG" && n.Outcome.Status == "ok" {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("DIAG should route, got %+v", rep.PerNet)
	}
	hasDiag := false
	for _, tr := range b.Traces {
		dx := math.Abs(tr.Start.X.ToMM() - tr.End.X.ToMM())
		dy := math.Abs(tr.Start.Y.ToMM() - tr.End.Y.ToMM())
		if dx > 1e-6 && dy > 1e-6 {
			hasDiag = true
			break
		}
	}
	if !hasDiag {
		t.Fatalf("expected at least one any-angle segment, traces=%d", len(b.Traces))
	}
}

// Three pads on one net must grow a tree (Steiner/Prim multi-source), not
// three independent star segments that miss the middle connection.
func TestRoutesThreePadsTree(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(50), core.FromMM(40)))
	b.Outline = &o
	// L-shaped: A at (10,10), B at (30,10), C at (30,30) — all net NET.
	b.AddFootprint(footprint("P1", 10, 10, []core.Pad{pad("1", 0, 0, "NET")}))
	b.AddFootprint(footprint("P2", 30, 10, []core.Pad{pad("1", 0, 0, "NET")}))
	b.AddFootprint(footprint("P3", 30, 30, []core.Pad{pad("1", 0, 0, "NET")}))

	rep := Route(b, DefaultOptions())
	var netRes *Outcome
	for i := range rep.PerNet {
		if rep.PerNet[i].Net == "NET" {
			netRes = &rep.PerNet[i].Outcome
		}
	}
	if netRes == nil {
		t.Fatalf("NET missing from report: %+v", rep.PerNet)
	}
	if netRes.Status != "ok" {
		t.Fatalf("NET failed: %+v report=%s", netRes, rep.Summary())
	}
	if netRes.TraceSegments < 2 {
		t.Fatalf("expected tree (≥2 segments) for 3 pads, got %d", netRes.TraceSegments)
	}
	if len(b.Traces) < 2 {
		t.Fatalf("board should have ≥2 traces, got %d", len(b.Traces))
	}
	// Steiner/Prim: total length should not greatly exceed MST Manhattan lower bound.
	if netRes.LowerBoundMM > 0 && netRes.LengthMM > netRes.LowerBoundMM*3 {
		t.Fatalf("path too long: length=%.2f lb=%.2f (possible star not tree)", netRes.LengthMM, netRes.LowerBoundMM)
	}
}

// Obstacle footprint + keepout in the middle forces a detour; route must still succeed.
func TestRoutesAroundObstacle(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(60), core.FromMM(40)))
	b.Outline = &o
	// Source left, target right, both on net SIG.
	b.AddFootprint(footprint("A", 8, 20, []core.Pad{pad("1", 0, 0, "SIG")}))
	b.AddFootprint(footprint("B", 52, 20, []core.Pad{pad("1", 0, 0, "SIG")}))
	// Fat no-net copper block sitting on the straight line between A and B.
	b.AddFootprint(footprint("BLOCK", 30, 20, []core.Pad{
		padNoNet("1", 0, -4),
		padNoNet("2", 0, 0),
		padNoNet("3", 0, 4),
	}))
	// Keepout wall across both copper layers so the router cannot sneak under.
	kr := core.RectFromCorners(
		core.NewPoint(core.FromMM(28), core.FromMM(12)),
		core.NewPoint(core.FromMM(32), core.FromMM(28)),
	)
	b.Keepouts = append(b.Keepouts, core.Keepout{
		ID:       core.NewID(),
		Rect:     &kr,
		NoCopper: true,
	})

	rep := Route(b, DefaultOptions())
	var ok bool
	for _, n := range rep.PerNet {
		if n.Net == "SIG" {
			if n.Outcome.Status != "ok" {
				t.Fatalf("SIG failed around obstacle: %+v summary=%s", n.Outcome, rep.Summary())
			}
			ok = true
			// Straight-line Manhattan is ~44 mm; must not cut through the wall.
			if n.Outcome.LengthMM < 30 {
				t.Fatalf("suspiciously short path %.2f mm — may have cut through obstacle", n.Outcome.LengthMM)
			}
		}
	}
	if !ok {
		t.Fatalf("SIG not in report: %+v", rep.PerNet)
	}
	if len(b.Traces) < 1 {
		t.Fatalf("expected copper on board")
	}
}

// max_seconds must bound wall time; router must not hang on a dense board.
func TestMaxSecondsRespected(t *testing.T) {
	b := core.NewBoard()
	// Larger outline so the grid has room; many nets to fill the budget.
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(80), core.FromMM(60)))
	b.Outline = &o
	// Scatter many 2-pad nets that partially contend for space.
	n := 0
	for y := 5.0; y <= 55; y += 8 {
		for x := 5.0; x <= 70; x += 12 {
			net := "N" + string(rune('A'+n%26)) + string(rune('0'+n/26))
			b.AddFootprint(footprint("L"+string(rune('A'+n%26))+string(rune('0'+n/26)), x, y, []core.Pad{
				pad("1", 0, 0, net),
			}))
			b.AddFootprint(footprint("R"+string(rune('A'+n%26))+string(rune('0'+n/26)), x+6, y+3, []core.Pad{
				pad("1", 0, 0, net),
			}))
			n++
			if n >= 20 {
				break
			}
		}
		if n >= 20 {
			break
		}
	}

	opts := DefaultOptions()
	opts.MaxSeconds = 0.25 // 250 ms hard budget
	start := time.Now()
	rep := Route(b, opts)
	elapsed := time.Since(start)
	// Allow generous slack for CI load, but must not approach multi-second hang.
	if elapsed > 3*time.Second {
		t.Fatalf("router hung: elapsed=%v max_seconds=0.25 report=%s", elapsed, rep.Summary())
	}
	// ElapsedMS in report should also be bounded (seconds not minutes).
	if rep.ElapsedMS > 3000 {
		t.Fatalf("report ElapsedMS=%d too large", rep.ElapsedMS)
	}
}

func TestParseOptions(t *testing.T) {
	o := DefaultOptions()
	o = ParseOptions(o, "max_seconds=1.5 cell=0.3 via_cost=10 clearance=0.15 organic=false fine_escape=true teardrop=true")
	if o.MaxSeconds != 1.5 {
		t.Fatalf("max_seconds: got %v", o.MaxSeconds)
	}
	if o.CellMM != 0.3 {
		t.Fatalf("cell: got %v", o.CellMM)
	}
	if o.ViaCost != 10 {
		t.Fatalf("via_cost: got %v", o.ViaCost)
	}
	if o.ClearanceMM != 0.15 {
		t.Fatalf("clearance: got %v", o.ClearanceMM)
	}
	if o.Organic {
		t.Fatal("organic should be false")
	}
	if !o.FineEscape {
		t.Fatal("fine_escape should be true")
	}
	if !o.Teardrops || !o.TeardropsSet {
		t.Fatal("teardrop=true should set Teardrops")
	}
}

func TestDefaultOptionsMatchRust(t *testing.T) {
	o := DefaultOptions()
	if o.CellMM != 0.25 || o.TraceWidthMM != 0.25 || o.ClearanceMM != 0.40 {
		t.Fatalf("geometry defaults: %+v", o)
	}
	if o.ViaCost != 8 || o.ViaDrillMM != 0.30 || o.ViaDiameterMM != 0.60 {
		t.Fatalf("via defaults: %+v", o)
	}
	if o.MaxSeconds != DefaultBudgetSeconds {
		t.Fatalf("max_seconds: got %v want %v", o.MaxSeconds, DefaultBudgetSeconds)
	}
	if !o.Organic || o.FineEscape || o.Negotiate {
		t.Fatalf("flags: organic=%v fine_escape=%v negotiate=%v", o.Organic, o.FineEscape, o.Negotiate)
	}
}

// Organic string-pull should not increase length and must keep OUT connected.
func TestOrganicShortensOrPreserves(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(40), core.FromMM(20)))
	b.Outline = &o
	b.AddFootprint(footprint("R1", 10, 10, []core.Pad{
		pad("1", -1, 0, "VCC"),
		pad("2", 1, 0, "OUT"),
	}))
	b.AddFootprint(footprint("R2", 30, 10, []core.Pad{
		pad("1", -1, 0, "OUT"),
		pad("2", 1, 0, "GND"),
	}))

	opts := DefaultOptions()
	opts.Organic = false
	repOff := Route(b, opts)
	lenOff := repOff.TotalLengthMM
	segsOff := len(b.Traces)

	b2 := core.NewBoard()
	b2.Outline = &o
	b2.AddFootprint(footprint("R1", 10, 10, []core.Pad{
		pad("1", -1, 0, "VCC"),
		pad("2", 1, 0, "OUT"),
	}))
	b2.AddFootprint(footprint("R2", 30, 10, []core.Pad{
		pad("1", -1, 0, "OUT"),
		pad("2", 1, 0, "GND"),
	}))
	optsOn := DefaultOptions()
	repOn := Route(b2, optsOn)
	if repOn.Failed > 0 {
		t.Fatalf("organic run failed: %s", repOn.Summary())
	}
	if repOn.TotalLengthMM > lenOff*1.01+0.01 {
		t.Fatalf("organic lengthened path: off=%.3f on=%.3f", lenOff, repOn.TotalLengthMM)
	}
	// Usually fewer grid segments after string-pull (or equal on trivial boards).
	if len(b2.Traces) > segsOff*2 {
		t.Fatalf("organic exploded segments: off=%d on=%d", segsOff, len(b2.Traces))
	}
	_ = repOff
}

func TestOwnNetPadsNotHardBlocked(t *testing.T) {
	// Two pads same net; ensure grid stamps them as net name and A* can enter.
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(30), core.FromMM(20)))
	b.Outline = &o
	b.AddFootprint(footprint("A", 5, 10, []core.Pad{pad("1", 0, 0, "VCC")}))
	b.AddFootprint(footprint("B", 25, 10, []core.Pad{pad("1", 0, 0, "VCC")}))
	g := newGrid(b, DefaultOptions())
	ax, ay, ok := g.worldToCell(core.FromMM(5), core.FromMM(10))
	if !ok {
		t.Fatal("pad A off grid")
	}
	if !g.passable(ax, ay, 0, "VCC") {
		t.Fatalf("own-net pad A not passable: cell=%q", g.blocked[0][ay*g.w+ax])
	}
	if g.passable(ax, ay, 0, "OTHER") {
		t.Fatalf("foreign net should not enter VCC pad")
	}
	rep := Route(b, DefaultOptions())
	for _, n := range rep.PerNet {
		if n.Net == "VCC" && n.Outcome.Status != "ok" {
			t.Fatalf("VCC failed: %+v", n.Outcome)
		}
	}
}

func TestSummary(t *testing.T) {
	r := Report{PerNet: []NetResult{{Net: "A", Outcome: Outcome{Status: "ok"}}}, TraceCount: 2, ViaCount: 1, TotalLengthMM: 3.5, ElapsedMS: 12}
	s := r.Summary()
	if s == "" {
		t.Fatal("empty summary")
	}
}

func TestSignalOtherLayerSkipsPlanes(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(20), core.FromMM(20)))
	b.Outline = &o
	b.Apply4Layer()
	g := newGrid(b, DefaultOptions())
	if g.layers != 4 {
		t.Fatalf("layers=%d", g.layers)
	}
	if got := signalOtherLayer(g, 0); got != 3 {
		t.Fatalf("top should hop to B.Cu, got %d", got)
	}
	if got := signalOtherLayer(g, 3); got != 0 {
		t.Fatalf("B.Cu should hop to F.Cu, got %d", got)
	}
}

func TestApplyFabCeilingUsesJLCPCB(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(20), core.FromMM(20)))
	b.Outline = &o
	opts := applyFabCeiling(b, DefaultOptions())
	if opts.ClearanceMM != 0.127 {
		t.Fatalf("2L clearance=%v want 0.127", opts.ClearanceMM)
	}
	if opts.ViaDrillMM != 0.30 || opts.ViaDiameterMM != 0.60 {
		t.Fatalf("2L via drill/dia=%v/%v want 0.30/0.60", opts.ViaDrillMM, opts.ViaDiameterMM)
	}
	if b.FabRules == nil || b.FabRules.Preset != "jlcpcb-2l" {
		t.Fatalf("fab rules not persisted: %+v", b.FabRules)
	}
	b.Apply4Layer()
	opts = applyFabCeiling(b, DefaultOptions())
	if opts.ClearanceMM < 0.08 || opts.ClearanceMM > 0.09 {
		t.Fatalf("4L clearance=%v want ~0.0889", opts.ClearanceMM)
	}
}

// A branch leaving the middle of a trunk is a T-junction, not a split net:
// padIslands must see one island, or the router throws away good rails.
func TestPadIslandsAcceptsTeeJunction(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(40), core.FromMM(30)))
	b.Outline = &o
	mk := func(x, y float64) core.Point { return core.NewPoint(core.FromMM(x), core.FromMM(y)) }
	w := core.FromMM(0.25)
	b.Traces = []core.Trace{
		{ID: core.NewID(), Net: "RAIL", Layer: core.LayerTop, Width: w, Start: mk(5, 15), End: mk(35, 15)},
		{ID: core.NewID(), Net: "RAIL", Layer: core.LayerTop, Width: w, Start: mk(20, 15), End: mk(20, 25)},
	}
	pads := []padLoc{
		{ref: "A", p: mk(5, 15), layer: 0},
		{ref: "B", p: mk(35, 15), layer: 0},
		{ref: "C", p: mk(20, 25), layer: 0},
	}
	if n := padIslands(b, "RAIL", pads); n != 1 {
		t.Fatalf("T-junction rail should be one island, got %d", n)
	}
	pads = append(pads, padLoc{ref: "D", p: mk(35, 25), layer: 0})
	if n := padIslands(b, "RAIL", pads); n != 2 {
		t.Fatalf("an unconnected pad is its own island, got %d", n)
	}
}

// routeDirect lays every hop as a single trace on one layer, so it must refuse
// a net whose pads are not all on the same layer: there is no via in that code
// path. It used to take the layer of whichever pad it was reaching for and draw
// the whole segment on it, which produced a BOTTOM trace starting at the centre
// of a TOP pad — copper that touches nothing, reported as "ok" by the router
// and only ever caught by DRC as an `unconnected_pad` *warning*. The minus
// pilot board shipped three open nets that way (MSP_TX, CRSF_TX, XSHUT_D).
func TestRouteDirectRefusesCrossLayerNets(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(30), core.FromMM(20)))
	b.Outline = &o
	top := footprint("U1", 8, 10, []core.Pad{pad("1", 0, 0, "SIG")})
	bot := footprint("J1", 14, 10, []core.Pad{pad("1", 0, 0, "SIG")})
	bot.Layer = core.LayerBottom
	bot.Pads[0].Layer = core.LayerBottom
	b.AddFootprint(top)
	b.AddFootprint(bot)

	opts := DefaultOptions()
	g := newGrid(b, opts)
	pads := []padLoc{
		{ref: "U1", p: core.PadWorldCenter(top, &top.Pads[0]), layer: core.LayerTop.Index},
		{ref: "J1", p: core.PadWorldCenter(bot, &bot.Pads[0]), layer: core.LayerBottom.Index},
	}
	if out := routeDirect(b, g, "SIG", pads, opts); out.Status == "ok" {
		t.Fatalf("routeDirect closed a cross-layer net with no via: %+v", out)
	}
	if len(b.Traces) != 0 {
		t.Fatalf("routeDirect must commit no copper when it refuses, got %d traces", len(b.Traces))
	}
	// Same-layer pads are still its job.
	bot.Layer = core.LayerTop
	bot.Pads[0].Layer = core.LayerTop
	pads[1].layer = core.LayerTop.Index
	if out := routeDirect(b, newGrid(b, opts), "SIG", pads, opts); out.Status != "ok" {
		t.Fatalf("routeDirect must still close a same-layer net: %+v", out)
	}
}

// End to end: a two-pad net across the two faces comes back with a via and
// with copper on both layers, or it is not routed at all. Never a silent open.
func TestCrossLayerNetGetsAVia(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(30), core.FromMM(20)))
	b.Outline = &o
	top := footprint("U1", 8, 10, []core.Pad{pad("1", 0, 0, "SIG"), pad("2", 0, 3, "GND")})
	bot := footprint("J1", 20, 10, []core.Pad{pad("1", 0, 0, "SIG"), pad("2", 0, 3, "GND")})
	bot.Layer = core.LayerBottom
	for i := range bot.Pads {
		bot.Pads[i].Layer = core.LayerBottom
	}
	b.AddFootprint(top)
	b.AddFootprint(bot)
	opts := DefaultOptions()
	opts.MaxSeconds = 30
	rep := Route(b, opts)
	ok := false
	for _, n := range rep.PerNet {
		if n.Net == "SIG" && n.Outcome.Status == "ok" {
			ok = true
		}
	}
	if !ok {
		t.Skip("SIG not routed; the invariant under test is only about ok nets")
	}
	vias := 0
	for _, v := range b.Vias {
		if v.Net == "SIG" {
			vias++
		}
	}
	if vias == 0 {
		t.Fatal("a routed cross-layer net must own at least one via")
	}
}
