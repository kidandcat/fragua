package core

import "testing"

func TestDefault4Layer(t *testing.T) {
	s := Default4Layer()
	if s.CopperCount() != 4 {
		t.Fatalf("copper %d", s.CopperCount())
	}
	if !s.IsPlane(1) || !s.IsPlane(2) {
		t.Fatal("inner layers should be planes")
	}
	if s.IsPlane(0) || s.IsPlane(3) {
		t.Fatal("outers should be signal")
	}
	if s.BottomLayer().Index != 3 {
		t.Fatalf("bottom index %d", s.BottomLayer().Index)
	}
}

func TestInsertInnerBeforeBottom(t *testing.T) {
	s := Default2Layer()
	s.PushLayer(LayerSpec{Name: "In1.Cu", Kind: LayerKindSignal}, Dielectric{ThicknessMM: 0.2, Er: 4.5})
	s.PushLayer(LayerSpec{Name: "In2.Cu", Kind: LayerKindSignal}, Dielectric{ThicknessMM: 0.2, Er: 4.5})
	if len(s.Layers) != 4 {
		t.Fatalf("layers %d", len(s.Layers))
	}
	want := []string{"F.Cu", "In1.Cu", "In2.Cu", "B.Cu"}
	for i, n := range want {
		if s.Layers[i].Name != n {
			t.Fatalf("layer[%d]=%s want %s", i, s.Layers[i].Name, n)
		}
	}
}

func TestApply4LayerRemapsBottomPour(t *testing.T) {
	b := NewBoard()
	b.Pours = []Pour{{Net: "GND", Layer: LayerBottom}}
	b.Traces = []Trace{{Net: "SCK", Layer: LayerBottom}}
	b.Apply4Layer()
	if b.Stackup == nil || b.Stackup.CopperCount() != 4 {
		t.Fatal("expected 4-layer stackup")
	}
	if b.Pours[0].Layer.Index != 3 {
		t.Fatalf("old bottom pour now at %d want 3", b.Pours[0].Layer.Index)
	}
	if b.Traces[0].Layer.Index != 3 {
		t.Fatalf("old bottom trace now at %d want 3", b.Traces[0].Layer.Index)
	}
	var gndIn, pwrIn bool
	for _, p := range b.Pours {
		if p.Net == "GND" && p.Layer.Index == 1 {
			gndIn = true
		}
		if p.Net == "+3V3" && p.Layer.Index == 2 {
			pwrIn = true
		}
	}
	if !gndIn || !pwrIn {
		t.Fatalf("inner pours missing: gnd=%v 3v3=%v pours=%+v", gndIn, pwrIn, b.Pours)
	}
}

func TestLayerJSONIn1IsIndex1(t *testing.T) {
	var l Layer
	if err := l.UnmarshalJSON([]byte(`"In1"`)); err != nil {
		t.Fatal(err)
	}
	if l.Index != 1 {
		t.Fatalf("In1 -> %d want 1", l.Index)
	}
	if err := l.UnmarshalJSON([]byte(`"In2.Cu"`)); err != nil {
		t.Fatal(err)
	}
	if l.Index != 2 {
		t.Fatalf("In2.Cu -> %d want 2", l.Index)
	}
}
