package parts

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/mentasystems/fragua/internal/core"
)

func loadMod(t *testing.T, name string) *Part {
	t.Helper()
	p, err := LoadKicadMod(filepath.Join("testdata/kicad", name), "")
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return p
}

func TestSexpParser(t *testing.T) {
	nodes, err := parseSexp(`(footprint "My Fp" (layer "F.Cu")
		(pad "1" smd rect (at 1 -2 90) (size 0.5 0.25) (layers "F.Cu"))
		(descr "a \"quoted\" thing"))`)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].name() != "footprint" {
		t.Fatalf("nodes = %+v", nodes)
	}
	root := nodes[0]
	if got := root.arg(0); got != "My Fp" {
		t.Errorf("name = %q", got)
	}
	pad, ok := root.child("pad")
	if !ok {
		t.Fatal("no pad")
	}
	x, y, _ := pad.xy("at")
	if x != 1 || y != -2 {
		t.Errorf("at = %v,%v", x, y)
	}
	at, _ := pad.child("at")
	if at.argF(2) != 90 {
		t.Errorf("rot = %v", at.argF(2))
	}
	if !pad.hasAtom("smd") || !pad.hasAtom("rect") {
		t.Error("pad atoms lost")
	}
	d, _ := root.child("descr")
	if d.arg(0) != `a "quoted" thing` {
		t.Errorf("descr = %q", d.arg(0))
	}
	if _, err := parseSexp("(unclosed"); err == nil {
		t.Error("want error on unclosed list")
	}
	if _, err := parseSexp(")"); err == nil {
		t.Error("want error on stray )")
	}
}

// v5 `(module …)` dialect: bare atoms, (layer F.SilkS) (width 0.12).
func TestKicadModLegacySOT23(t *testing.T) {
	p := loadMod(t, "SOT-23.kicad_mod")
	if p.Entry.Key != "SOT-23" {
		t.Errorf("key = %q", p.Entry.Key)
	}
	if len(p.Entry.Pads) != 3 {
		t.Fatalf("pads = %d, want 3", len(p.Entry.Pads))
	}
	p1, ok := padByNumber(p.Entry, "1")
	if !ok {
		t.Fatal("no pad 1")
	}
	// (pad 1 smd rect (at -1 -0.95) (size 0.9 0.8)) — Y flips to +0.95.
	near(t, "pad1.x", p1.XMM, -1, 0.001)
	near(t, "pad1.y", p1.YMM, 0.95, 0.001)
	near(t, "pad1.w", p1.WMM, 0.9, 0.001)
	near(t, "pad1.h", p1.HMM, 0.8, 0.001)
	if p1.DrillMM != nil {
		t.Error("smd pad must have no drill")
	}
	if len(p.Entry.Silk) == 0 {
		t.Error("want F.SilkS lines")
	}
	// F.CrtYd is ±1.7 × ±1.75 and becomes the placement keep-out.
	if p.Entry.BodyRect == nil {
		t.Fatal("no body rect")
	}
	near(t, "crtyd.minX", p.Entry.BodyRect.MinXMM, -1.7, 0.001)
	near(t, "crtyd.maxY", p.Entry.BodyRect.MaxYMM, 1.75, 0.001)
	if p.Entry.Source != SourceKiCad {
		t.Errorf("source = %q", p.Entry.Source)
	}
	if len(p.Pins) != 3 {
		t.Errorf("pins = %d, want 3", len(p.Pins))
	}
}

// v6+ `(footprint …)` dialect: quoted atoms, (stroke (width …)), roundrect.
func TestKicadModModernSOT23(t *testing.T) {
	p := loadMod(t, "SOT-23_v9.kicad_mod")
	if len(p.Entry.Pads) != 3 {
		t.Fatalf("pads = %d, want 3", len(p.Entry.Pads))
	}
	p3, ok := padByNumber(p.Entry, "3")
	if !ok {
		t.Fatal("no pad 3")
	}
	near(t, "pad3.x", p3.XMM, 0.9375, 0.001)
	near(t, "pad3.w", p3.WMM, 1.475, 0.001)
	near(t, "pad3.h", p3.HMM, 0.6, 0.001)
	if len(p.Entry.Silk) == 0 {
		t.Error("want silk from the v9 stroke syntax")
	}
	if p.Entry.BodyRect == nil {
		t.Fatal("no courtyard")
	}
	if p.Entry.Description == "" {
		t.Error("want the (descr …) text")
	}
}

func TestKicadModThroughHole(t *testing.T) {
	p := loadMod(t, "PinHeader_1x04_P2.54mm_Vertical.kicad_mod")
	if len(p.Entry.Pads) != 4 {
		t.Fatalf("pads = %d, want 4", len(p.Entry.Pads))
	}
	for _, pad := range p.Entry.Pads {
		if pad.DrillMM == nil {
			t.Fatalf("pad %s: want a drill", pad.Number)
		}
		near(t, "drill", *pad.DrillMM, 1.0, 0.001)
		near(t, "pad size", pad.WMM, 1.7, 0.001)
	}
	// 2.54 mm pitch, Y-flipped so pin 1 ends up highest.
	near(t, "pitch", math.Abs(p.Entry.Pads[0].YMM-p.Entry.Pads[1].YMM), 2.54, 0.001)
}

func TestKicadModChipInfersDiscrete(t *testing.T) {
	p := loadMod(t, "R_0603_1608Metric.kicad_mod")
	if p.Kind != "resistor" {
		t.Errorf("kind = %q, want resistor (from (tags resistor))", p.Kind)
	}
	if p.RefPrefix != "R" {
		t.Errorf("ref prefix = %q, want R", p.RefPrefix)
	}
	if len(p.Entry.Pads) != 2 {
		t.Fatalf("pads = %d, want 2", len(p.Entry.Pads))
	}
}

func TestKicadModRejects(t *testing.T) {
	if _, err := ParseKicadMod([]byte("(nonsense 1 2)"), ""); err == nil {
		t.Error("want error without a footprint node")
	}
	if _, err := ParseKicadMod([]byte(`(footprint "X" (layer "F.Cu"))`), ""); err == nil {
		t.Error("want error without pads")
	}
	// np_thru_hole is mechanical: dropped, not turned into copper.
	p, err := ParseKicadMod([]byte(`(footprint "X" (layer "F.Cu")
		(pad "1" smd rect (at 0 0) (size 1 1) (layers "F.Cu"))
		(pad "" np_thru_hole circle (at 2 0) (size 3 3) (drill 3) (layers "*.Cu")))`), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Entry.Pads) != 1 {
		t.Errorf("pads = %d, want 1 (NPTH dropped)", len(p.Entry.Pads))
	}
}

func TestKicadModOvalDrillAndRotation(t *testing.T) {
	p, err := ParseKicadMod([]byte(`(footprint "X" (layer "F.Cu")
		(pad "1" thru_hole oval (at 0 0 90) (size 2 1) (drill oval 1.6 0.8) (layers "*.Cu"))
		(pad "2" smd custom (at 3 0) (size 0.5 0.5) (layers "F.Cu")
			(primitives (gr_poly (pts (xy -1 -1) (xy 1 -1) (xy 1 1) (xy -1 1)))))
		(fp_circle (center 0 0) (end 1 0) (stroke (width 0.1) (type solid)) (layer "F.SilkS"))
		(fp_rect (start -2 -2) (end 2 2) (stroke (width 0.05) (type solid)) (layer "F.CrtYd")))`), "K")
	if err != nil {
		t.Fatal(err)
	}
	p1, _ := padByNumber(p.Entry, "1")
	// rot 90 swaps the 2×1 size, and the oval drill keeps its smaller axis.
	near(t, "pad1.w", p1.WMM, 1, 0.001)
	near(t, "pad1.h", p1.HMM, 2, 0.001)
	if p1.DrillMM == nil {
		t.Fatal("want a drill")
	}
	near(t, "drill", *p1.DrillMM, 0.8, 0.001)
	// The custom pad grows to its primitive bbox.
	p2, _ := padByNumber(p.Entry, "2")
	near(t, "pad2.w", p2.WMM, 2, 0.001)
	if p.Entry.BodyRect == nil || p.Entry.BodyRect.MaxXMM < 2 {
		t.Errorf("courtyard = %+v", p.Entry.BodyRect)
	}
	if len(p.Entry.Silk) < 8 {
		t.Errorf("circle should flatten to segments, got %d", len(p.Entry.Silk))
	}
}

func TestKicadSymSmallResistor(t *testing.T) {
	syms, err := LoadKicadSym("testdata/kicad/R_Small.kicad_sym")
	if err != nil {
		t.Fatal(err)
	}
	if len(syms) != 1 || syms[0].Name != "R_Small" {
		t.Fatalf("symbols = %+v", syms)
	}
	s := syms[0]
	if s.RefPrefix != "R" {
		t.Errorf("ref prefix = %q", s.RefPrefix)
	}
	if len(s.Pins) != 2 {
		t.Fatalf("pins = %d, want 2", len(s.Pins))
	}
	// (at 0 2.54 270) points down into the body → the pin is at the top.
	if s.Pins[0].Number != "1" || s.Pins[0].Side != core.PinTop {
		t.Errorf("pin 1 = %+v", s.Pins[0])
	}
	if s.Pins[1].Side != core.PinBottom {
		t.Errorf("pin 2 = %+v", s.Pins[1])
	}
	if s.Description == "" {
		t.Error("want the Description property")
	}
}

func TestKicadSymRolesAndSides(t *testing.T) {
	syms, err := LoadKicadSym("testdata/kicad/AP1117-15.kicad_sym")
	if err != nil {
		t.Fatal(err)
	}
	byNum := map[string]core.SchPin{}
	for _, p := range syms[0].Pins {
		byNum[p.Number] = p
	}
	if len(byNum) != 3 {
		t.Fatalf("pins = %d, want 3", len(byNum))
	}
	if byNum["1"].Name != "GND" || byNum["1"].Role != core.PinPowerIn || byNum["1"].Side != core.PinBottom {
		t.Errorf("pin 1 = %+v", byNum["1"])
	}
	if byNum["2"].Name != "VO" || byNum["2"].Role != core.PinPowerOut || byNum["2"].Side != core.PinRight {
		t.Errorf("pin 2 = %+v", byNum["2"])
	}
	if byNum["3"].Name != "VI" || byNum["3"].Side != core.PinLeft {
		t.Errorf("pin 3 = %+v", byNum["3"])
	}
}

func TestMergeSymbolPins(t *testing.T) {
	p := loadMod(t, "SOT-23.kicad_mod")
	syms, err := LoadKicadSym("testdata/kicad/AP1117-15.kicad_sym")
	if err != nil {
		t.Fatal(err)
	}
	n := MergeSymbolPins(&p.Entry, syms[0])
	if n != 3 {
		t.Fatalf("merged = %d, want 3", n)
	}
	pad, _ := padByNumber(p.Entry, "2")
	if pad.Name != "VO" {
		t.Errorf("pad 2 name = %q, want VO", pad.Name)
	}
	if len(p.Entry.Pins) != 3 || p.Entry.Pins[0].Name != "GND" {
		t.Errorf("pins = %+v", p.Entry.Pins)
	}
	// A symbol whose numbers do not line up must not clobber anything.
	if got := MergeSymbolPins(&p.Entry, KicadSymbol{Pins: []core.SchPin{{Number: "99"}}}); got != 0 {
		t.Errorf("merged = %d, want 0", got)
	}
}

func TestKicadRootsHonourEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(KicadLibsEnv, dir)
	roots := KicadFootprintRoots()
	if len(roots) == 0 || roots[0] != dir {
		t.Fatalf("roots = %v, want %s first", roots, dir)
	}
	// A bare share root also resolves its footprints/ subdirectory.
	if roots[1] != filepath.Join(dir, "footprints") {
		t.Errorf("roots[1] = %q", roots[1])
	}
	if _, err := ResolveKicadFootprint("Nope", "Missing"); err == nil {
		t.Error("want error for a missing library")
	}
}

func TestResolveKicadFootprint(t *testing.T) {
	root := t.TempDir()
	pretty := filepath.Join(root, "Package_TO_SOT_SMD.pretty")
	if err := os.MkdirAll(pretty, 0o755); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile("testdata/kicad/SOT-23.kicad_mod")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pretty, "SOT-23.kicad_mod"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(KicadLibsEnv, root)

	got, err := ResolveKicadFootprint("Package_TO_SOT_SMD", "SOT-23")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(pretty, "SOT-23.kicad_mod") {
		t.Errorf("path = %q", got)
	}
	// The .pretty suffix is optional on the library name.
	if _, err := ResolveKicadFootprint("Package_TO_SOT_SMD.pretty", "SOT-23.kicad_mod"); err != nil {
		t.Errorf("suffixed form: %v", err)
	}
	// An empty library scans every .pretty in the root.
	if _, err := ResolveKicadFootprint("", "SOT-23"); err != nil {
		t.Errorf("library-less lookup: %v", err)
	}
	mods, err := CollectKicadMods(root)
	if err != nil || len(mods) != 1 {
		t.Errorf("CollectKicadMods = %v, %v", mods, err)
	}
	mods, err = CollectKicadMods(filepath.Join(pretty, "SOT-23.kicad_mod"))
	if err != nil || len(mods) != 1 {
		t.Errorf("CollectKicadMods(file) = %v, %v", mods, err)
	}
}
