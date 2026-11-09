package router

import (
	"testing"

	"github.com/mentasystems/fragua/internal/core"
)

// TestRouteOnlyNets: `route nets=…` routes just those nets, so a long net
// can take the channel before short ones fill it; a plain route then keeps
// it and finishes the rest. Unknown names fail loudly.
func TestRouteOnlyNets(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(40), core.FromMM(30)))
	b.Outline = &o
	b.AddFootprint(footprint("A", 6, 8, []core.Pad{pad("1", 0, 0, "N1"), pad("2", 0, 6, "N2")}))
	b.AddFootprint(footprint("B", 34, 8, []core.Pad{pad("1", 0, 0, "N1"), pad("2", 0, 6, "N2")}))

	opts := ParseOptions(DefaultOptions(), "nets=N2,NOPE")
	rep := Route(b, opts)
	got := map[string]string{}
	for _, n := range rep.PerNet {
		got[n.Net] = n.Outcome.Status + "/" + n.Outcome.Reason
	}
	if _, ok := got["N1"]; ok {
		t.Fatalf("N1 was not asked for: %v", got)
	}
	if got["NOPE"] != "failed/unknown_net" {
		t.Fatalf("NOPE: %q want failed/unknown_net", got["NOPE"])
	}
	if s, ok := got["N2"]; !ok || s[:2] != "ok" {
		t.Fatalf("N2: %q", s)
	}
	for _, tr := range b.Traces {
		if tr.Net != "N2" {
			t.Fatalf("copper for %s", tr.Net)
		}
	}

	rest := Route(b, DefaultOptions())
	if rest.Failed != 0 {
		t.Fatalf("rest: %s", rest.Summary())
	}
	for _, n := range rest.PerNet {
		if n.Net == "N2" && n.Outcome.Reason != "already_routed" {
			t.Fatalf("N2 re-laid: %+v", n.Outcome)
		}
	}
}
