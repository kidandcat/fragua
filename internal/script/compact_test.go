package script

import (
	"math"
	"testing"

	"github.com/mentasystems/fragua/internal/core"
)

// The feasibility probe must route at the class widths the final route
// uses, or compact sizes the board against copper it will never lay.
func TestCompactFeasibleUsesNetClassWidth(t *testing.T) {
	p := core.NewProject("t-compact-width")
	rs := RunScript(p, `
outline 40 24
lib rpad
  pad 1 -1 0 1.0 1.2
  pad 2 1 0 1.0 1.2
palette R1 rpad
palette R2 rpad
place R1 12 12
place R2 26 12
net VBUS R1.1 R2.1
class power width=0.60
net-class VBUS power
`)
	allOK(t, rs)

	b := p.Board().Clone()
	if !compactFeasible(b, p.Schematic(), 40, 24, 42, 300, 10, 0) {
		t.Fatal("probe should find the current size feasible")
	}
	n := 0
	for _, tr := range b.Traces {
		if tr.Net != "VBUS" {
			continue
		}
		n++
		if got := tr.Width.ToMM(); math.Abs(got-0.60) > 1e-3 {
			t.Fatalf("probe routed VBUS at %.3f mm, want the class width 0.600 mm", got)
		}
	}
	if n == 0 {
		t.Fatal("probe laid no VBUS copper")
	}
}
