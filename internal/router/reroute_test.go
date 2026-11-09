package router

import (
	"testing"

	"github.com/mentasystems/fragua/internal/core"
)

// TestRouteTwiceKeepsCopper: a second `route` rebuilt every net from scratch
// on top of the copper the first pass had laid, so each finished net came
// out doubled (and the copy ignored what the first one already occupied).
func TestRouteTwiceKeepsCopper(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(40), core.FromMM(30)))
	b.Outline = &o
	b.AddFootprint(footprint("A", 6, 8, []core.Pad{pad("1", 0, 0, "N1"), pad("2", 0, 6, "N2")}))
	b.AddFootprint(footprint("B", 34, 8, []core.Pad{pad("1", 0, 0, "N1"), pad("2", 0, 6, "N2")}))
	b.AddFootprint(footprint("C", 20, 24, []core.Pad{pad("1", 0, 0, "N2")}))

	first := Route(b, DefaultOptions())
	if first.Failed != 0 {
		t.Fatalf("first pass: %s", first.Summary())
	}
	traces, vias := len(b.Traces), len(b.Vias)
	if traces == 0 {
		t.Fatal("expected copper after the first pass")
	}

	second := Route(b, DefaultOptions())
	if second.Failed != 0 {
		t.Fatalf("second pass: %s", second.Summary())
	}
	if len(b.Traces) != traces || len(b.Vias) != vias {
		t.Fatalf("second pass changed copper: traces %d→%d, vias %d→%d", traces, len(b.Traces), vias, len(b.Vias))
	}
	for _, n := range second.PerNet {
		if (n.Net == "N1" || n.Net == "N2") && n.Outcome.Reason != "already_routed" {
			t.Fatalf("net %s: %+v, want already_routed", n.Net, n.Outcome)
		}
	}

	// A net cut back to a stub is still finished.
	var kept []core.Trace
	cut := false
	for _, tr := range b.Traces {
		if tr.Net == "N1" && !cut {
			cut = true
			continue
		}
		kept = append(kept, tr)
	}
	b.Traces = kept
	third := Route(b, DefaultOptions())
	if third.Failed != 0 {
		t.Fatalf("third pass: %s", third.Summary())
	}
	if islands := padIslands(b, "N1", netPadsFor(b, "N1")); islands != 1 {
		t.Fatalf("N1 still in %d islands", islands)
	}
	for _, n := range third.PerNet {
		if n.Net == "N2" && n.Outcome.Reason != "already_routed" {
			t.Fatalf("N2 was untouched, got %+v", n.Outcome)
		}
	}
}

func netPadsFor(b *core.Board, net string) []padLoc {
	var out []padLoc
	for _, fp := range b.Footprints {
		for i := range fp.Pads {
			if p := &fp.Pads[i]; p.Net != nil && *p.Net == net {
				out = append(out, padLoc{ref: fp.Reference, p: core.PadWorldCenter(fp, p), layer: p.Layer.Index})
			}
		}
	}
	return out
}
