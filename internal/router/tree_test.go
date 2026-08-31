package router

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/mentasystems/fragua/internal/core"
)

func terminals(xy ...[2]float64) []padLoc {
	out := make([]padLoc, 0, len(xy))
	for _, p := range xy {
		out = append(out, padLoc{
			p: core.NewPoint(core.FromMM(p[0]), core.FromMM(p[1])),
		})
	}
	return out
}

// A seven-terminal net used to get exactly one tree shape: the seed retry was
// gated on len(pads) <= 6, so the two I2C stars on drone-x — same topology,
// one pad pitch apart — were each built one way and one way only, and every
// repair pass afterwards rebuilt that same shape. SCL's shape worked, SDA's
// did not, and nothing in the engine could ever try SDA's another way.
func TestTreeAttemptsRetryPastSixTerminals(t *testing.T) {
	pads := terminals(
		[2]float64{5, 5}, [2]float64{40, 8}, [2]float64{75, 6}, [2]float64{72, 40},
		[2]float64{70, 70}, [2]float64{38, 68}, [2]float64{6, 66},
	)
	plan := treeAttempts(pads, 0, true)
	if len(plan) < 2 {
		t.Fatalf("a seven-terminal net must get more than one tree shape, got %d", len(plan))
	}
	seeds := map[int]bool{}
	orders := map[growthOrder]bool{}
	for _, a := range plan {
		if a.seed < 0 || a.seed >= len(pads) {
			t.Fatalf("seed %d out of range", a.seed)
		}
		seeds[a.seed] = true
		orders[a.order] = true
	}
	if len(seeds) < 2 {
		t.Fatalf("expected alternative seeds, got %v", seeds)
	}
	if !orders[growNearest] || !orders[growFarthest] {
		t.Fatalf("expected both growth orders, got %v", orders)
	}
	if len(plan) > maxTreeAttempts {
		t.Fatalf("plan must stay bounded, got %d > %d", len(plan), maxTreeAttempts)
	}
}

// The alternatives are free for a net that already closes: the first shape is
// still the one the router has always built, and only a failure ever costs a
// second search.
func TestTreeAttemptsKeepTheOldShapeFirst(t *testing.T) {
	pads := terminals(
		[2]float64{1, 1}, [2]float64{10, 1}, [2]float64{20, 1},
		[2]float64{30, 1}, [2]float64{40, 1},
	)
	for _, seed := range []int{0, 2, 4} {
		plan := treeAttempts(pads, seed, true)
		if len(plan) == 0 {
			t.Fatalf("seed %d: empty plan", seed)
		}
		if plan[0] != (treeAttempt{seed: seed, order: growNearest}) {
			t.Fatalf("seed %d: first shape must be the old one, got %+v", seed, plan[0])
		}
	}
}

// The first pass keeps the plan it has always had — every seed for a small
// net, one shape for a big one — and pays nothing for the alternatives. A
// search that finds nothing walks the whole grid before it says so, and
// spending that while every net is still queued costs more nets than it
// saves: dropping the small-net seed retries from the first pass took
// drone-x from 52/54 to 50/54 on its own.
func TestTreeAttemptsDoNotExploreInTheFirstPass(t *testing.T) {
	spread := terminals(
		[2]float64{5, 5}, [2]float64{40, 8}, [2]float64{75, 6}, [2]float64{72, 40},
		[2]float64{70, 70}, [2]float64{38, 68}, [2]float64{6, 66},
	)
	plan := treeAttempts(spread, 2, false)
	if len(plan) != 1 {
		t.Fatalf("a seven-terminal net got one shape in the first pass, got %d", len(plan))
	}
	if plan[0] != (treeAttempt{seed: 2, order: growNearest}) {
		t.Fatalf("the one shape must be the old one, got %+v", plan[0])
	}

	// Six terminals or fewer: every seed, nearest-first, ascending — exactly
	// the loop routeNetAtOnce used to run inline.
	small := terminals(
		[2]float64{1, 1}, [2]float64{30, 1}, [2]float64{60, 1}, [2]float64{60, 40},
	)
	want := []treeAttempt{
		{seed: 2, order: growNearest}, {seed: 0, order: growNearest},
		{seed: 1, order: growNearest}, {seed: 3, order: growNearest},
	}
	got := treeAttempts(small, 2, false)
	if len(got) != len(want) {
		t.Fatalf("small-net first-pass plan changed: got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("small-net first-pass plan changed at %d: %+v vs %+v", i, got[i], want[i])
		}
	}

	if DefaultOptions().exploreTrees {
		t.Fatal("exploring must be off by default; Route turns it on for the repair passes")
	}
}

// Two terminals are one hop, and one hop has only one order. Such a net must
// not pay for a growth order that cannot differ.
func TestTreeAttemptsTwoTerminalNetHasOneOrder(t *testing.T) {
	pads := terminals([2]float64{1, 1}, [2]float64{20, 1})
	for _, a := range treeAttempts(pads, 0, true) {
		if a.order != growNearest {
			t.Fatalf("two-terminal net should only grow nearest-first, got %+v", a)
		}
	}
}

func TestTreeAttemptsAreDeterministic(t *testing.T) {
	pads := terminals(
		[2]float64{5, 5}, [2]float64{40, 8}, [2]float64{75, 6}, [2]float64{72, 40},
		[2]float64{70, 70}, [2]float64{38, 68}, [2]float64{6, 66},
	)
	first := treeAttempts(pads, 1, true)
	for i := 0; i < 20; i++ {
		got := treeAttempts(pads, 1, true)
		if len(got) != len(first) {
			t.Fatalf("plan length drifted: %d vs %d", len(got), len(first))
		}
		for k := range got {
			if got[k] != first[k] {
				t.Fatalf("plan drifted at %d: %+v vs %+v", k, got[k], first[k])
			}
		}
	}
}

func TestNextTerminalHonoursGrowthOrder(t *testing.T) {
	// 0 seeds the tree; 1 is next door, 3 is across the board.
	pads := terminals(
		[2]float64{0, 0}, [2]float64{2, 0}, [2]float64{20, 0}, [2]float64{80, 0},
	)
	connected := map[int]bool{0: true}
	if got := nextTerminal(pads, connected, nil, growNearest); got != 1 {
		t.Fatalf("nearest-first should take the neighbour, got %d", got)
	}
	if got := nextTerminal(pads, connected, nil, growFarthest); got != 3 {
		t.Fatalf("trunk-first should take the remote terminal, got %d", got)
	}
}

// A branch that finds no path is parked, not fatal: nextTerminal must skip it
// so the rest of the tree can be laid and searched from.
func TestNextTerminalSkipsDeferredBranches(t *testing.T) {
	pads := terminals(
		[2]float64{0, 0}, [2]float64{2, 0}, [2]float64{20, 0}, [2]float64{80, 0},
	)
	connected := map[int]bool{0: true}
	skip := map[int]bool{1: true}
	if got := nextTerminal(pads, connected, skip, growNearest); got != 2 {
		t.Fatalf("deferred terminal should be skipped, got %d", got)
	}
	if got := nextTerminal(pads, connected, map[int]bool{1: true, 2: true, 3: true}, growNearest); got != -1 {
		t.Fatalf("a fully deferred sweep should report nothing left, got %d", got)
	}
}

// Distance is measured to the tree, not to the seed: once 3 is in, the far
// terminal 2 is nearer the tree than the neighbour 1 is remote from it.
func TestNextTerminalMeasuresAgainstTheWholeTree(t *testing.T) {
	pads := terminals(
		[2]float64{0, 0}, [2]float64{2, 0}, [2]float64{60, 0}, [2]float64{80, 0},
	)
	connected := map[int]bool{0: true, 3: true}
	if got := nextTerminal(pads, connected, nil, growNearest); got != 1 {
		t.Fatalf("expected the 2 mm neighbour, got %d", got)
	}
	if got := nextTerminal(pads, connected, nil, growFarthest); got != 2 {
		t.Fatalf("expected the terminal 20 mm off the tree, got %d", got)
	}
}

// Three pads in a triangle: the third is the same Manhattan distance from
// both of the first two, so which one anchors its hop is a tie. routeDirect
// used to break it by ranging the connected set — a Go map — and laid the
// segment off a different pad from one run to the next.
func TestDirectHopsPickTheSameAnchorEveryRun(t *testing.T) {
	build := func() *core.Board {
		b := core.NewBoard()
		o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(30), core.FromMM(30)))
		b.Outline = &o
		b.AddFootprint(footprint("R1", 10, 10, []core.Pad{pad("1", 0, 0, "N")}))
		b.AddFootprint(footprint("R2", 14, 10, []core.Pad{pad("1", 0, 0, "N")}))
		b.AddFootprint(footprint("R3", 12, 14, []core.Pad{pad("1", 0, 0, "N")}))
		return b
	}
	key := func(b *core.Board) string {
		var s []string
		for _, tr := range b.Traces {
			s = append(s, fmt.Sprintf("%d:%d,%d->%d,%d", tr.Layer.Index,
				tr.Start.X, tr.Start.Y, tr.End.X, tr.End.Y))
		}
		sort.Strings(s)
		return strings.Join(s, "|")
	}
	first := ""
	for run := 0; run < 25; run++ {
		b := build()
		if rep := Route(b, DefaultOptions()); rep.Failed != 0 {
			t.Fatalf("run %d: %s", run, rep.Summary())
		}
		got := key(b)
		if run == 0 {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("run %d laid different copper:\n  %s\n  %s", run, first, got)
		}
	}
}

// A seven-terminal bus spread over a crowded board is the shape that stalled
// drone-x. It has to close, and it has to close the same way twice: the tree
// used to be grown by ranging Go maps, so two identical calls disagreed.
func TestSevenTerminalBusRoutesRepeatably(t *testing.T) {
	build := func() *core.Board {
		b := core.NewBoard()
		o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(80), core.FromMM(60)))
		b.Outline = &o
		bus := [][2]float64{
			{8, 8}, {40, 6}, {72, 10}, {74, 30}, {70, 52}, {38, 54}, {10, 50},
		}
		for i, p := range bus {
			b.AddFootprint(footprint("T"+string(rune('1'+i)), p[0], p[1], []core.Pad{
				pad("1", 0, 0, "BUS"),
				pad("2", 0, 2.54, "LOCAL"+string(rune('1'+i))),
			}))
		}
		// Clutter the middle so the bus cannot take a straight line home.
		for i := 0; i < 6; i++ {
			b.AddFootprint(footprint("X"+string(rune('1'+i)), 24+float64(i)*6, 30, []core.Pad{
				padNoNet("1", 0, 0),
			}))
		}
		return b
	}
	opts := DefaultOptions()
	opts.MaxSeconds = 60

	var wantTraces, wantVias int
	for run := 0; run < 2; run++ {
		b := build()
		rep := Route(b, opts)
		status := ""
		for _, n := range rep.PerNet {
			if n.Net == "BUS" {
				status = n.Outcome.Status + "/" + n.Outcome.Reason
			}
		}
		if status == "" || status[:2] != "ok" {
			t.Fatalf("run %d: seven-terminal bus should close, got %q (%s)", run, status, rep.Summary())
		}
		if run == 0 {
			wantTraces, wantVias = len(b.Traces), len(b.Vias)
			continue
		}
		if len(b.Traces) != wantTraces || len(b.Vias) != wantVias {
			t.Fatalf("route is not repeatable: run0 %d traces/%d vias, run1 %d/%d",
				wantTraces, wantVias, len(b.Traces), len(b.Vias))
		}
	}
}
