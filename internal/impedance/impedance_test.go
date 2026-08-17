package impedance

import (
	"math"
	"strings"
	"testing"

	"github.com/mentasystems/fragua/internal/core"
)

func TestMicrostrip50OhmOn16mmFR4(t *testing.T) {
	// Classic rule of thumb: 50 Ω microstrip on ~1.6 mm FR-4, 1 oz, is ~3 mm.
	p := Params{Kind: KindMicrostrip, HMM: 1.6, Er: 4.6, TMM: 0.035, LayerName: "F.Cu"}
	w, err := WidthForZ(p, 50)
	if err != nil {
		t.Fatal(err)
	}
	if w < 2.7 || w > 3.4 {
		t.Fatalf("50 Ω width on 1.6 mm FR-4: %.3f mm (want ~3 mm ±10%%)", w)
	}
	z, err := Z0(p, w)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(z-50) > 1 {
		t.Fatalf("round-trip Z=%.2f want 50", z)
	}
}

func TestMicrostrip50OhmOnThinDielectricIsNarrower(t *testing.T) {
	thick := Params{Kind: KindMicrostrip, HMM: 1.6, Er: 4.6, TMM: 0.035}
	thin := Params{Kind: KindMicrostrip, HMM: 0.2, Er: 4.6, TMM: 0.035}
	w16, err := WidthForZ(thick, 50)
	if err != nil {
		t.Fatal(err)
	}
	w02, err := WidthForZ(thin, 50)
	if err != nil {
		t.Fatal(err)
	}
	if w02 >= w16*0.4 {
		t.Fatalf("0.2 mm dielectric should be much narrower than 1.6 mm: %.3f vs %.3f", w02, w16)
	}
	if w02 < 0.25 || w02 > 0.55 {
		t.Fatalf("50 Ω on 0.2 mm FR-4: %.3f mm (want ~0.35–0.45 mm)", w02)
	}
}

func TestHammerstadKnownVector(t *testing.T) {
	// W=3.0 mm, H=1.6 mm, εr=4.6, T=0.035 → Z0 near 50 Ω (within 10%).
	z, err := Z0(Params{Kind: KindMicrostrip, HMM: 1.6, Er: 4.6, TMM: 0.035}, 3.0)
	if err != nil {
		t.Fatal(err)
	}
	if z < 45 || z > 55 {
		t.Fatalf("Z0(W=3 mm, H=1.6)=%.2f want ~50", z)
	}
}

func TestStriplineIPC2141(t *testing.T) {
	// Symmetric 50 Ω ballpark: H=0.2, εr=4.2, T=0.035, W≈0.15.
	p := Params{Kind: KindStripline, HMM: 0.2, Er: 4.2, TMM: 0.035}
	z, err := Z0(p, 0.15)
	if err != nil {
		t.Fatal(err)
	}
	if z < 40 || z > 60 {
		t.Fatalf("stripline Z0=%.2f (want ~50)", z)
	}
	w, err := WidthForZ(p, 50)
	if err != nil {
		t.Fatal(err)
	}
	z2, _ := Z0(p, w)
	if math.Abs(z2-50) > 1.5 {
		t.Fatalf("stripline solve: W=%.3f Z=%.2f", w, z2)
	}
}

func TestMissingErIsError(t *testing.T) {
	s := core.LayerStackup{
		Layers:      []core.LayerSpec{{Name: "F.Cu", CopperWeightOz: 1}, {Name: "B.Cu", CopperWeightOz: 1}},
		Dielectrics: []core.Dielectric{{ThicknessMM: 1.6}}, // Er unset
	}
	_, err := LineParams(s, 0)
	if err == nil {
		t.Fatal("expected error for unset Er")
	}
}

func TestMissingThicknessIsError(t *testing.T) {
	s := core.LayerStackup{
		Layers:      []core.LayerSpec{{Name: "F.Cu", CopperWeightOz: 1}, {Name: "B.Cu", CopperWeightOz: 1}},
		Dielectrics: []core.Dielectric{{Er: 4.6}},
	}
	_, err := LineParams(s, 0)
	if err == nil {
		t.Fatal("expected error for unset thickness")
	}
}

func TestNoDielectricIsError(t *testing.T) {
	s := core.LayerStackup{
		Layers: []core.LayerSpec{{Name: "F.Cu"}, {Name: "B.Cu"}},
	}
	_, err := LineParams(s, 0)
	if err == nil {
		t.Fatal("expected error for empty dielectrics")
	}
}

func TestDefault2LayerIsMicrostrip(t *testing.T) {
	p, err := LineParams(core.Default2Layer(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != KindMicrostrip || p.Er != 4.6 || p.HMM != 1.5 {
		t.Fatalf("default 2L: %+v", p)
	}
}

func TestAsymmetricInnerIsError(t *testing.T) {
	s := core.Default4Layer()
	// In1.Cu is index 1; H=0.21 vs 1.065 — asymmetric.
	_, err := LineParams(s, 1)
	if err == nil {
		t.Fatal("4L In1 is asymmetric; calculator must refuse")
	}
}

func TestDefault4LayerOuterIsMicrostrip(t *testing.T) {
	p, err := LineParams(core.Default4Layer(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != KindMicrostrip || p.HMM != 0.21 {
		t.Fatalf("4L F.Cu: %+v", p)
	}
}

func TestAnalyzeNetDiffPairNote(t *testing.T) {
	b := core.NewBoard()
	b.Traces = []core.Trace{{
		Net: "USB_DP", Layer: core.LayerTop,
		Start: core.Origin, End: core.NewPoint(core.FromMM(10), 0),
		Width: core.FromMM(0.3),
	}}
	sch := core.NewSchematic()
	sch.Nets["USB_DP"] = &core.Net{Name: "USB_DP", DiffPair: "USB_DM"}
	r, err := AnalyzeNet(b, sch, "USB_DP")
	if err != nil {
		t.Fatal(err)
	}
	if r.Z0 <= 0 {
		t.Fatalf("expected single-ended Z, got %+v", r)
	}
	if r.Note == "" || !strings.Contains(r.Note, "single-ended") {
		t.Fatalf("diff pair must document single-ended limit: %q", r.Note)
	}
	if strings.Contains(r.Note, "Zdiff=") || strings.Contains(r.Format("USB_DP"), "Zdiff=") {
		t.Fatal("must not print a fake differential Z")
	}
}
