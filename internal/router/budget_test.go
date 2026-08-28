package router

import (
	"math"
	"testing"
	"time"

	"github.com/mentasystems/fragua/internal/core"
)

// A route call is never unbounded: 0 means "the default budget", not "forever".
func TestClampBudget(t *testing.T) {
	cases := []struct {
		in, want float64
	}{
		{0, DefaultBudgetSeconds},
		{-1, DefaultBudgetSeconds},
		{math.NaN(), DefaultBudgetSeconds},
		{math.Inf(1), DefaultBudgetSeconds},
		{math.Inf(-1), DefaultBudgetSeconds},
		{1e9, MaxBudgetSeconds},
		{MaxBudgetSeconds + 1, MaxBudgetSeconds},
		{30, 30},
		{MaxBudgetSeconds, MaxBudgetSeconds},
	}
	for _, c := range cases {
		if got := ClampBudget(c.in); got != c.want {
			t.Fatalf("ClampBudget(%v) = %v, want %v", c.in, got, c.want)
		}
	}
	if MaxBudgetSeconds > 600 {
		t.Fatalf("route budget ceiling must stay at or under 10 minutes, got %v", MaxBudgetSeconds)
	}
}

// max_seconds=0 used to mean "no deadline" and let a hard board spin for
// hours. It now falls back to the default budget.
func TestParseOptionsClampsBudget(t *testing.T) {
	for _, arg := range []string{"max_seconds=0", "max_seconds=-5"} {
		if got := ParseOptions(DefaultOptions(), arg).MaxSeconds; got != DefaultBudgetSeconds {
			t.Fatalf("%s: got %v, want %v", arg, got, DefaultBudgetSeconds)
		}
	}
	if got := ParseOptions(DefaultOptions(), "max_seconds=99999").MaxSeconds; got != MaxBudgetSeconds {
		t.Fatalf("max_seconds=99999: got %v, want %v", got, MaxBudgetSeconds)
	}
}

// Route honours a zero budget as the default one — it must still terminate,
// and still hand back the copper it managed to lay (anytime behaviour).
func TestRouteWithZeroBudgetTerminates(t *testing.T) {
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
	opts := DefaultOptions()
	opts.MaxSeconds = 0
	start := time.Now()
	rep := Route(b, opts)
	if el := time.Since(start); el > 30*time.Second {
		t.Fatalf("max_seconds=0 must not run unbounded: elapsed=%v", el)
	}
	if rep.Failed != 0 {
		t.Fatalf("expected all nets routed: %s", rep.Summary())
	}
}
