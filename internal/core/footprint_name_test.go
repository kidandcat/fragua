package core

import "testing"

func TestPackageNameFromLibrary(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"library:c_0603", "C_0603_1608Metric"},
		{"c_0603", "C_0603_1608Metric"},
		{"r_0603", "R_0603_1608Metric"},
		{"R_0805", "R_0805_2012Metric"},
		{"Resistor_SMD:R_0805", "R_0805_2012Metric"},
		{"C_0603_1608Metric", "C_0603_1608Metric"},
		{"library:qfn-56", "QFN-56-1EP_7x7mm_P0.4mm"},
		{"sot23", "SOT-23"},
		{"soic8", "SOIC-8_3.9x4.9mm_P1.27mm"},
		{"", ""},
	}
	for _, c := range cases {
		if got := PackageNameFromLibrary(c.in); got != c.want {
			t.Errorf("PackageNameFromLibrary(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestFabRulesPresetJLCStandardVia(t *testing.T) {
	p := FabRulesPreset("jlcpcb-2l")
	if p == nil || p.MinViaDrillMM != 0.30 || p.MinViaDiameterMM != 0.60 {
		t.Fatalf("jlcpcb-2l via: %+v", p)
	}
	if p.MinAnnularRingMM < 0.15 || p.MinEdgeClearanceMM < 0.3 {
		t.Fatalf("jlcpcb-2l annular/edge: %+v", p)
	}
	alias := FabRulesPreset("jlcpcb")
	if alias == nil || alias.MinViaDrillMM != 0.30 {
		t.Fatalf("jlcpcb alias: %+v", alias)
	}
	via02 := FabRulesPreset("jlcpcb-2l-via02")
	if via02 == nil || via02.MinViaDrillMM != 0.20 || via02.MinViaDiameterMM != 0.45 {
		t.Fatalf("via02 opt-in: %+v", via02)
	}
	l4 := FabRulesPreset("jlcpcb-4l")
	if l4 == nil || l4.MinViaDrillMM != 0.30 || l4.MinViaDiameterMM != 0.60 {
		t.Fatalf("jlcpcb-4l via: %+v", l4)
	}
}

func TestActiveFabRulesDefaultVia03(t *testing.T) {
	fr := ActiveFabRules(NewBoard())
	if fr.MinViaDrillMM != 0.30 {
		t.Fatalf("default via drill %v want 0.30", fr.MinViaDrillMM)
	}
}

func TestToFootprintCopiesLCSC(t *testing.T) {
	lcsc := "C25804"
	mfr := "Yageo"
	mpn := "RC0603FR-0710KL"
	e := LibraryEntry{
		Key:          "r_0603",
		DefaultValue: "10k",
		LcscID:       &lcsc,
		MPN:          &mpn,
		Manufacturer: &mfr,
		Pads:         ResistorCapPads(),
		BodyRect:     &BodyRect{MinXMM: -0.8, MinYMM: -0.4, MaxXMM: 0.8, MaxYMM: 0.4},
	}
	fp := e.ToFootprint("R1", "", LayerTop, 0)
	if fp.LcscID != lcsc || fp.MPN != mpn || fp.Manufacturer != mfr {
		t.Fatalf("bom fields: lcsc=%q mpn=%q mfr=%q", fp.LcscID, fp.MPN, fp.Manufacturer)
	}
	if fp.Value != "10k" {
		t.Fatalf("default value %q", fp.Value)
	}
	if fp.BodyRect == nil {
		t.Fatal("body_rect not copied")
	}
}
