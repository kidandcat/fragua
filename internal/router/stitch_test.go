package router

import (
	"testing"

	"github.com/mentasystems/fragua/internal/core"
)

func TestStitchGeneratesViasForRequestedPour(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(20), core.FromMM(16)))
	b.Outline = &o
	b.Pours = []core.Pour{{
		Net: "GND", Layer: core.LayerTop,
		Stitching: &core.StitchPolicy{Enabled: true, PitchMM: 5},
	}}
	n := "GND"
	b.AddFootprint(&core.Footprint{
		ID: core.NewID(), Reference: "R1",
		Position: core.NewPoint(core.FromMM(4), core.FromMM(4)),
		Layer:    core.LayerBottom,
		Pads: []core.Pad{{
			Number: "1", Size: [2]core.Length{core.FromMM(1), core.FromMM(1)},
			Layer: core.LayerBottom, Net: &n,
		}},
	})
	added := StitchIsolatedPads(b, DefaultOptions())
	if added == 0 || len(b.Vias) == 0 {
		t.Fatalf("expected stitch vias, added=%d vias=%d", added, len(b.Vias))
	}
	for _, v := range b.Vias {
		if v.Net != "GND" {
			t.Fatalf("via net %s", v.Net)
		}
	}
}
