package parts

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/mentasystems/fragua/internal/core"
)

// fixtureFetcher serves the saved EasyEDA responses so every test is offline.
type fixtureFetcher struct{ dir string }

func (f fixtureFetcher) Fetch(_ context.Context, lcsc string) ([]byte, error) {
	return os.ReadFile(filepath.Join(f.dir, lcsc+".json"))
}

func fixtures() fixtureFetcher { return fixtureFetcher{dir: "testdata/easyeda"} }

func loadPart(t *testing.T, lcsc string) *Part {
	t.Helper()
	raw, err := fixtures().Fetch(context.Background(), lcsc)
	if err != nil {
		t.Fatalf("fixture %s: %v", lcsc, err)
	}
	p, err := ParseEasyEDA(raw, lcsc)
	if err != nil {
		t.Fatalf("parse %s: %v", lcsc, err)
	}
	return p
}

func padByNumber(e core.LibraryEntry, num string) (core.LibraryPad, bool) {
	for _, p := range e.Pads {
		if p.Number == num {
			return p, true
		}
	}
	return core.LibraryPad{}, false
}

func near(t *testing.T, what string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %.4f, want %.4f ±%.3f", what, got, want, tol)
	}
}

func TestNormaliseLCSC(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"C2040", "C2040", true},
		{"c2040", "C2040", true},
		{"LCSC:C2040", "C2040", true},
		{"lcsc:c1525", "C1525", true},
		{"kicad:Lib:FP", "", false},
		{"RP2040", "", false},
	} {
		got, ok := NormaliseLCSC(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("NormaliseLCSC(%q) = %q,%v want %q,%v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// C1525 is a 0402 MLCC: two 0.50 × 0.54 mm pads at ±0.42 mm, capacitor symbol.
func TestEasyEDAChipCapacitor(t *testing.T) {
	p := loadPart(t, "C1525")
	if len(p.Entry.Pads) != 2 {
		t.Fatalf("pads = %d, want 2", len(p.Entry.Pads))
	}
	p1, ok := padByNumber(p.Entry, "1")
	if !ok {
		t.Fatal("no pad 1")
	}
	near(t, "pad1.x", p1.XMM, -0.42, 0.02)
	near(t, "pad1.y", p1.YMM, 0, 0.01)
	near(t, "pad1.w", p1.WMM, 0.50, 0.01)
	near(t, "pad1.h", p1.HMM, 0.54, 0.01)
	if p1.DrillMM != nil {
		t.Error("chip pad should be SMD")
	}
	if p.Kind != "capacitor" {
		t.Errorf("kind = %q, want capacitor", p.Kind)
	}
	if p.RefPrefix != "C" {
		t.Errorf("ref prefix = %q, want C", p.RefPrefix)
	}
	if p.Entry.MPN == nil || *p.Entry.MPN != "CL05B104KO5NNNC" {
		t.Errorf("mpn = %v", p.Entry.MPN)
	}
	if p.Entry.JLCClass == nil || *p.Entry.JLCClass != "Basic Part" {
		t.Errorf("jlc class = %v", p.Entry.JLCClass)
	}
	if p.Entry.Datasheet == nil || *p.Entry.Datasheet == "" {
		t.Error("want a datasheet link")
	}
	if len(p.Entry.Silk) == 0 {
		t.Error("want silk from the TRACK/ARC shapes")
	}
	if p.Entry.BodyRect == nil {
		t.Error("want a body rect")
	}
}

// C25804 is a 0603 resistor — the discrete-kind inference path off LCSC tags.
func TestEasyEDAChipResistor(t *testing.T) {
	p := loadPart(t, "C25804")
	if p.Kind != "resistor" {
		t.Errorf("kind = %q, want resistor", p.Kind)
	}
	if p.RefPrefix != "R" {
		t.Errorf("ref prefix = %q, want R", p.RefPrefix)
	}
	if p.Entry.DefaultValue != "10kΩ" {
		t.Errorf("value = %q", p.Entry.DefaultValue)
	}
	if len(p.Entry.Pads) != 2 {
		t.Fatalf("pads = %d, want 2", len(p.Entry.Pads))
	}
	// IPC-ish 0603 land: ~0.9 × 0.95 mm pads about 1.5 mm apart.
	dx := math.Abs(p.Entry.Pads[0].XMM - p.Entry.Pads[1].XMM)
	near(t, "pad pitch", dx, 1.5, 0.2)
}

// C2040 is the RP2040: LQFN-56 + exposed pad, 57 pads and 57 named pins.
func TestEasyEDARP2040(t *testing.T) {
	p := loadPart(t, "C2040")
	if len(p.Entry.Pads) != 57 {
		t.Fatalf("pads = %d, want 57", len(p.Entry.Pads))
	}
	if len(p.Pins) != 57 {
		t.Fatalf("pins = %d, want 57", len(p.Pins))
	}
	if p.Kind != "generic_ic" {
		t.Errorf("kind = %q, want generic_ic", p.Kind)
	}
	if p.RefPrefix != "U" {
		t.Errorf("ref prefix = %q, want U", p.RefPrefix)
	}
	// Pad 56 is drawn rotated 270°, so w/h must come out swapped: the QFN
	// finger is 0.20 mm across and 0.85 mm long.
	p56, ok := padByNumber(p.Entry, "56")
	if !ok {
		t.Fatal("no pad 56")
	}
	near(t, "pad56.w", p56.WMM, 0.20, 0.01)
	near(t, "pad56.h", p56.HMM, 0.85, 0.01)
	// Pad 57 is the 3.1 mm exposed thermal pad at the centre.
	p57, ok := padByNumber(p.Entry, "57")
	if !ok {
		t.Fatal("no pad 57")
	}
	near(t, "ep.w", p57.WMM, 3.10, 0.02)
	near(t, "ep.x", p57.XMM, 0, 0.05)
	// 0.4 mm pitch between neighbouring fingers on the same edge.
	p55, _ := padByNumber(p.Entry, "55")
	near(t, "pitch", math.Abs(p56.XMM-p55.XMM), 0.40, 0.01)

	// Pin names must land on the pads so `net U1.GND` works as well as U1.57.
	byNum := map[string]core.SchPin{}
	for _, pin := range p.Pins {
		byNum[pin.Number] = pin
	}
	if byNum["9"].Name != "GPIO7" {
		t.Errorf("pin 9 name = %q, want GPIO7", byNum["9"].Name)
	}
	if byNum["9"].Role != core.PinBidir {
		t.Errorf("pin 9 role = %q, want bidir", byNum["9"].Role)
	}
	if byNum["57"].Name != "GND" {
		t.Errorf("pin 57 name = %q, want GND", byNum["57"].Name)
	}
	if p9, _ := padByNumber(p.Entry, "9"); p9.Name != "GPIO7" {
		t.Errorf("pad 9 name = %q, want GPIO7", p9.Name)
	}
	if p.Entry.Manufacturer == nil {
		t.Error("want a manufacturer")
	}
	if p.Entry.JLCClass == nil || *p.Entry.JLCClass != "Extended Part" {
		t.Errorf("jlc class = %v", p.Entry.JLCClass)
	}
}

// C165948 is a 16-pin USB-C receptacle: through-hole shell pads with oval
// (slotted) drills, POLYGON pads, and duplicated pad labels.
func TestEasyEDAUSBC(t *testing.T) {
	p := loadPart(t, "C165948")
	if len(p.Entry.Pads) != 16 {
		t.Fatalf("pads = %d, want 16", len(p.Entry.Pads))
	}
	// Shell pad "1" is OVAL, holeR 1.5748 u → a 0.80 mm bit; the 5.9055 u
	// slot extension must not widen the drill past the 1.20 mm pad.
	shell, ok := padByNumber(p.Entry, "1")
	if !ok {
		t.Fatal("no shell pad 1")
	}
	if shell.DrillMM == nil {
		t.Fatal("shell pad should be through-hole")
	}
	near(t, "shell drill", *shell.DrillMM, 0.80, 0.01)
	near(t, "shell w", shell.WMM, 1.20, 0.01)
	// The POLYGON signal pads keep their point-list bbox.
	poly, ok := padByNumber(p.Entry, "A1B12")
	if !ok {
		t.Fatal("no pad A1B12")
	}
	near(t, "poly w", poly.WMM, 0.60, 0.02)
	near(t, "poly h", poly.HMM, 1.30, 0.02)
	// Every pad number is unique so `net` can address each one.
	seen := map[string]bool{}
	for _, pad := range p.Entry.Pads {
		if seen[pad.Number] {
			t.Errorf("duplicate pad number %q", pad.Number)
		}
		seen[pad.Number] = true
	}
}

func TestEasyEDAErrors(t *testing.T) {
	if _, err := ParseEasyEDA([]byte("not json"), "C1"); err == nil {
		t.Error("want error on bad JSON")
	}
	if _, err := ParseEasyEDA([]byte(`{"success":false}`), "C1"); err == nil {
		t.Error("want error on missing result")
	}
	if _, err := ParseEasyEDA([]byte(`{"result":{"title":"x"}}`), "C1"); err == nil {
		t.Error("want error on missing footprint")
	}
}

func TestRotatedExtent(t *testing.T) {
	for _, tc := range []struct{ w, h, rot, wantW, wantH float64 }{
		{2, 1, 0, 2, 1},
		{2, 1, 180, 2, 1},
		{2, 1, 90, 1, 2},
		{2, 1, 270, 1, 2},
		{2, 1, -90, 1, 2},
	} {
		w, h := rotatedExtent(tc.w, tc.h, tc.rot)
		if w != tc.wantW || h != tc.wantH {
			t.Errorf("rotatedExtent(%v,%v,%v) = %v,%v want %v,%v", tc.w, tc.h, tc.rot, w, h, tc.wantW, tc.wantH)
		}
	}
}

func TestPartsRoundTripThroughLibrary(t *testing.T) {
	lib, err := core.OpenAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := loadPart(t, "C2040")
	if _, err := lib.Put(p.Entry); err != nil {
		t.Fatal(err)
	}
	got, ok := lib.Get("C2040")
	if !ok {
		t.Fatal("entry not stored")
	}
	if got.Source != SourceLCSC {
		t.Errorf("source = %q", got.Source)
	}
	if len(got.Pins) != 57 {
		t.Errorf("pins = %d, want 57", len(got.Pins))
	}
	kind, ok := got.SymbolKindFor()
	if !ok || kind.Kind != "generic_ic" || len(kind.Pins()) != 57 {
		t.Errorf("symbol kind = %+v ok=%v", kind, ok)
	}
	// Re-open from disk: the cache must survive a restart.
	lib2, err := core.OpenAt(lib.Root())
	if err != nil {
		t.Fatal(err)
	}
	if e2, ok := lib2.Get("C2040"); !ok || len(e2.Pins) != 57 || e2.Source != SourceLCSC {
		t.Errorf("reload lost part data: ok=%v pins=%d src=%q", ok, len(e2.Pins), e2.Source)
	}
}

func ExampleParseEasyEDA() {
	raw, _ := os.ReadFile("testdata/easyeda/C25804.json")
	p, _ := ParseEasyEDA(raw, "C25804")
	fmt.Printf("%s %s pads=%d kind=%s\n", p.Entry.Key, *p.Entry.MPN, len(p.Entry.Pads), p.Kind)
	// Output: C25804 0603WAF1002T5E pads=2 kind=resistor
}
