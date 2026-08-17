package core

import "testing"

func TestBuildTeardropsOffByDefault(t *testing.T) {
	b := NewBoard()
	n := "SIG"
	b.AddFootprint(&Footprint{
		ID: NewID(), Reference: "R1",
		Position: NewPoint(FromMM(5), FromMM(5)),
		Layer:    LayerTop,
		Pads: []Pad{{
			Number: "1", Size: [2]Length{FromMM(1.2), FromMM(1.2)},
			Layer: LayerTop, Net: &n,
		}},
	})
	b.Traces = []Trace{{
		ID: NewID(), Layer: LayerTop, Net: n,
		Start: NewPoint(FromMM(5), FromMM(5)),
		End:   NewPoint(FromMM(15), FromMM(5)),
		Width: FromMM(0.25),
	}}
	if got := BuildTeardrops(b); len(got) != 0 {
		t.Fatalf("teardrops off: got %d", len(got))
	}
	b.Teardrops = true
	got := BuildTeardrops(b)
	if len(got) != 1 {
		t.Fatalf("teardrops on: got %d want 1", len(got))
	}
	if got[0].Net != n || got[0].Layer.Index != 0 {
		t.Fatalf("teardrop meta: %+v", got[0])
	}
	if len(got[0].Poly) != 4 {
		t.Fatalf("flare vertices: %d", len(got[0].Poly))
	}
	// Wide end is at the pad (x≈5), larger than the trace (0.25 mm).
	aabb := got[0].AABB()
	if aabb.Min.X.ToMM() > 5.1 {
		t.Fatalf("flare should start at the pad, aabb=%+v", aabb)
	}
	spanY := aabb.Height().ToMM()
	if spanY < 1.0 {
		t.Fatalf("wide end should match pad (~1.2 mm), spanY=%.3f", spanY)
	}
	// Length ~1.25 × pad radius (0.6 mm) = 0.75 mm along +X.
	if aabb.Width().ToMM() < 0.4 || aabb.Width().ToMM() > 1.2 {
		t.Fatalf("teardrop length (aabb width)=%.3f mm", aabb.Width().ToMM())
	}
}

func TestBuildTeardropsViaJunction(t *testing.T) {
	b := NewBoard()
	b.Teardrops = true
	b.Vias = []Via{{
		ID: NewID(), Position: NewPoint(FromMM(8), FromMM(8)),
		Drill: FromMM(0.3), Diameter: FromMM(0.6), Net: "GND",
	}}
	b.Traces = []Trace{{
		ID: NewID(), Layer: LayerTop, Net: "GND",
		Start: NewPoint(FromMM(8), FromMM(8)),
		End:   NewPoint(FromMM(8), FromMM(18)),
		Width: FromMM(0.2),
	}}
	got := BuildTeardrops(b)
	if len(got) != 1 {
		t.Fatalf("via teardrop: %d", len(got))
	}
	if got[0].AABB().Height().ToMM() < 0.3 {
		t.Fatalf("via flare too short: %+v", got[0].AABB())
	}
}
