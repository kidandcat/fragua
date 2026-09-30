package core

import "testing"

func TestRectGapMM(t *testing.T) {
	r := func(x0, y0, x1, y1 float64) Rect {
		return RectFromCorners(NewPoint(FromMM(x0), FromMM(y0)), NewPoint(FromMM(x1), FromMM(y1)))
	}
	if g := RectGapMM(r(0, 0, 1, 1), r(1, 0, 2, 1)); g != 0 {
		t.Fatalf("touching edges: gap %v", g)
	}
	if g := RectGapMM(r(0, 0, 1, 1), r(1.5, 0, 2, 1)); abs(g-0.5) > 1e-9 {
		t.Fatalf("separated: gap %v", g)
	}
	if g := RectGapMM(r(0, 0, 1, 1), r(0.8, 0, 2, 1)); g >= 0 || abs(g-(-0.2)) > 1e-9 {
		t.Fatalf("overlap: gap %v", g)
	}
	// Corner gap is the Euclidean distance, not the smaller axis.
	if g := RectGapMM(r(0, 0, 1, 1), r(1.3, 1.4, 2, 2)); abs(g-0.5) > 1e-6 {
		t.Fatalf("corner gap %v, want 0.5", g)
	}
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func TestComponentRulesDefaultsAndOverride(t *testing.T) {
	if g := NewBoard().ComponentRules(); g.CourtyardMarginMM != 0.25 || g.MinBodyGapMM != 0.5 || g.MinModuleGapMM != 1 {
		t.Fatalf("defaults %+v", g)
	}
	b := NewBoard()
	z := 0.0
	body := 0.8
	b.FabRules = &FabRules{MinBodyGapMM: &body, CourtyardMarginMM: &z}
	got := b.ComponentRules()
	if got.MinBodyGapMM != 0.8 || got.CourtyardMarginMM != 0 || got.MinModuleGapMM != 1 {
		t.Fatalf("override %+v", got)
	}
}

func TestIsModuleFromKeyOrFlag(t *testing.T) {
	plain := &Footprint{Reference: "R1", Key: "r_0603"}
	if plain.IsModule() {
		t.Fatal("0603 is not a module")
	}
	esp := &Footprint{Reference: "U1", Key: "esp32_s3_zero_top"}
	lora := &Footprint{Reference: "U2", Key: "xl1262_lora"}
	flag := &Footprint{Reference: "U3", Key: "custom", Module: true}
	qfn := &Footprint{Reference: "U4", Key: "esp32_s3_qfn56"}
	if !esp.IsModule() || !lora.IsModule() || !flag.IsModule() {
		t.Fatalf("esp %v lora %v flag %v", esp.IsModule(), lora.IsModule(), flag.IsModule())
	}
	if qfn.IsModule() {
		t.Fatal("a bare ESP32 QFN is a chip, not a module")
	}
}

func edgePads() []Pad {
	// Lands that reach the outline, so the rect is a package body rather
	// than an IPC courtyard grown past the pads.
	sz := [2]Length{FromMM(1), FromMM(1)}
	return []Pad{
		{Number: "1", Offset: NewPoint(FromMM(-8.5), FromMM(0)), Size: sz, Layer: LayerTop},
		{Number: "2", Offset: NewPoint(FromMM(8.5), FromMM(0)), Size: sz, Layer: LayerTop},
	}
}

func bodyHit(hits []ComponentHit) (ComponentHit, bool) {
	for _, h := range hits {
		if h.Kind == HitBodyClearance {
			return h, true
		}
	}
	return ComponentHit{}, false
}

func TestBodyClearanceOnTouchingModules(t *testing.T) {
	// Package outlines 0.10 mm apart. With no courtyard of their own, the
	// 0.25 mm margin is added and the courtyards overlap; the body gap is
	// still the 0.10 mm the finding must report.
	a := &Footprint{
		ID: NewID(), Reference: "U1", Key: "esp32_s3_zero_top", Layer: LayerTop,
		Position: NewPoint(FromMM(10), FromMM(10)),
		BodyRect: &BodyRect{MinXMM: -9, MinYMM: -5, MaxXMM: 9, MaxYMM: 5},
		Pads:     edgePads(),
	}
	b := &Footprint{
		ID: NewID(), Reference: "U2", Key: "xl1262_lora", Layer: LayerTop,
		Position: NewPoint(FromMM(28.1), FromMM(10)),
		BodyRect: &BodyRect{MinXMM: -9, MinYMM: -5, MaxXMM: 9, MaxYMM: 5},
		Pads:     edgePads(),
	}
	hits := ComponentHits(a, b, DefaultComponentRules())
	h, ok := bodyHit(hits)
	if !ok {
		t.Fatalf("hits %+v", hits)
	}
	if h.A != "U1" || h.B != "U2" {
		t.Fatalf("refs %s %s", h.A, h.B)
	}
	if abs(h.Gap-0.1) > 1e-6 || h.Need != 1 {
		t.Fatalf("gap %v need %v", h.Gap, h.Need)
	}
	// A chip pair at the same 0.10 mm fails the 0.5 mm floor too, but a
	// 0.60 mm chip gap is legal while a module pair at 0.60 mm is not.
	a.Key, b.Key = "soic8", "soic8"
	a.Module, b.Module = false, false
	hits = ComponentHits(a, b, DefaultComponentRules())
	h, ok = bodyHit(hits)
	if !ok || h.Need != 0.5 {
		t.Fatalf("chip hits %+v", hits)
	}
	b.Position = NewPoint(FromMM(28.6), FromMM(10))
	if hits := ComponentHits(a, b, DefaultComponentRules()); len(hits) != 0 {
		t.Fatalf("0.6 mm chip gap must pass, got %+v", hits)
	}
	// A module next to a chip uses the 0.5 mm body floor. Two modules at
	// the same 0.60 mm still fail the 1 mm floor.
	a.Key = "esp32_s3_zero_top"
	if hits := ComponentHits(a, b, DefaultComponentRules()); len(hits) != 0 {
		t.Fatalf("0.6 mm module-to-chip gap must pass, got %+v", hits)
	}
	b.Key = "xl1262_lora"
	hits = ComponentHits(a, b, DefaultComponentRules())
	h, ok = bodyHit(hits)
	if !ok || h.Need != 1 || len(hits) != 1 {
		t.Fatalf("0.6 mm module-to-module gap must fail on the body only, got %+v", hits)
	}
}

func TestModuleToPassiveUsesBodyGap(t *testing.T) {
	// Fecha door strip: a 0603 decap 0.922 mm from the LoRa module (and
	// 0.972 mm from the OLED) must not take the 1 mm module floor.
	module := func(ref, key string, x float64) *Footprint {
		return &Footprint{
			ID: NewID(), Reference: ref, Key: key, Layer: LayerTop,
			Position: NewPoint(FromMM(x), FromMM(10)),
			BodyRect: &BodyRect{MinXMM: -9, MinYMM: -5, MaxXMM: 9, MaxYMM: 5},
			Pads:     edgePads(),
		}
	}
	u2 := module("U2", "xl1262_lora", 10)
	c3 := module("C3", "c_0603", 28.922)
	if hits := ComponentHits(u2, c3, DefaultComponentRules()); len(hits) != 0 {
		t.Fatalf("0.922 mm module-to-0603 must pass, got %+v", hits)
	}
	ds1 := module("DS1", "oled_module", 10)
	c3.Position = NewPoint(FromMM(28.972), FromMM(10))
	if hits := ComponentHits(ds1, c3, DefaultComponentRules()); len(hits) != 0 {
		t.Fatalf("0.972 mm module-to-0603 must pass, got %+v", hits)
	}
	// Same spacing between two modules is still a body clearance.
	other := module("U1", "esp32_s3_zero_top", 28.922)
	hits := ComponentHits(u2, other, DefaultComponentRules())
	h, ok := bodyHit(hits)
	if !ok || h.Need != 1 || abs(h.Gap-0.922) > 1e-6 {
		t.Fatalf("module pair at 0.922 mm: %+v", hits)
	}
}

func TestIPCCourtyardTouchDoesNotDoubleCount(t *testing.T) {
	// Pad 1×1, courtyard 0.25 mm past it. Centres 1.5 mm apart: courtyards
	// touch and the inset bodies are exactly 0.5 mm apart, which is legal.
	body := &BodyRect{MinXMM: -0.75, MinYMM: -0.75, MaxXMM: 0.75, MaxYMM: 0.75}
	pad := []Pad{{Number: "1", Size: [2]Length{FromMM(1), FromMM(1)}, Layer: LayerTop}}
	a := &Footprint{
		ID: NewID(), Reference: "R1", Key: "r_0603", Layer: LayerTop,
		BodyRect: body, Pads: pad,
	}
	b := &Footprint{
		ID: NewID(), Reference: "R2", Key: "r_0603", Layer: LayerTop,
		Position: NewPoint(FromMM(1.5), FromMM(0)),
		BodyRect: body, Pads: pad,
	}
	if hits := ComponentHits(a, b, DefaultComponentRules()); len(hits) != 0 {
		t.Fatalf("touching IPC courtyards must pass, got %+v", hits)
	}
}
