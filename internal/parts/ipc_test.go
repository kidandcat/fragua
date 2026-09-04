package parts

import (
	"fmt"
	"math"
	"testing"

	"github.com/mentasystems/fragua/internal/core"
)

func gen(t *testing.T, spec GenSpec) *Part {
	t.Helper()
	p, err := Generate(spec)
	if err != nil {
		t.Fatalf("Generate(%+v): %v", spec, err)
	}
	if p.Entry.Source != SourceIPC {
		t.Errorf("source = %q", p.Entry.Source)
	}
	if len(p.Entry.Pads) == 0 {
		t.Fatal("no pads")
	}
	if p.Entry.BodyRect == nil {
		t.Error("every generator must emit a courtyard")
	}
	if len(p.Entry.Silk) == 0 {
		t.Error("every generator must emit silk")
	}
	if len(p.Pins) != len(p.Entry.Pads) {
		t.Errorf("pins = %d, pads = %d", len(p.Pins), len(p.Entry.Pads))
	}
	assertUniquePads(t, p.Entry.Pads)
	assertPadsClear(t, p.Entry.Pads)
	return p
}

func assertUniquePads(t *testing.T, pads []core.LibraryPad) {
	t.Helper()
	seen := map[string]bool{}
	for _, p := range pads {
		if seen[p.Number] {
			t.Errorf("duplicate pad number %q", p.Number)
		}
		seen[p.Number] = true
		if p.WMM <= 0 || p.HMM <= 0 {
			t.Errorf("pad %s has non-positive size %.3f x %.3f", p.Number, p.WMM, p.HMM)
		}
		if p.DrillMM != nil && *p.DrillMM >= math.Min(p.WMM, p.HMM) {
			t.Errorf("pad %s drill %.3f leaves no annular ring in %.3f", p.Number, *p.DrillMM, math.Min(p.WMM, p.HMM))
		}
	}
}

// assertPadsClear checks no two lands overlap — a generator that produced
// touching copper would be un-routable and DRC-red on sight.
func assertPadsClear(t *testing.T, pads []core.LibraryPad) {
	t.Helper()
	for i := range pads {
		for j := i + 1; j < len(pads); j++ {
			a, b := pads[i], pads[j]
			dx := math.Abs(a.XMM-b.XMM) - (a.WMM+b.WMM)/2
			dy := math.Abs(a.YMM-b.YMM) - (a.HMM+b.HMM)/2
			if dx < -1e-9 && dy < -1e-9 {
				t.Errorf("pads %s and %s overlap", a.Number, b.Number)
			}
		}
	}
}

func TestParseDensity(t *testing.T) {
	for in, want := range map[string]Density{
		"": DensityNominal, "n": DensityNominal, "B": DensityNominal,
		"M": DensityMost, "most": DensityMost, "a": DensityMost,
		"L": DensityLeast, "least": DensityLeast, "c": DensityLeast,
	} {
		got, err := ParseDensity(in)
		if err != nil || got != want {
			t.Errorf("ParseDensity(%q) = %v,%v want %v", in, got, err, want)
		}
	}
	if _, err := ParseDensity("huge"); err == nil {
		t.Error("want error on a bad density")
	}
}

func TestGenChipSizes(t *testing.T) {
	for _, size := range ChipSizes() {
		p := gen(t, GenSpec{Key: "R" + size, Family: "chip", Size: size})
		if len(p.Entry.Pads) != 2 {
			t.Fatalf("%s: pads = %d", size, len(p.Entry.Pads))
		}
		if p.Kind != "resistor" {
			t.Errorf("%s: kind = %q, want resistor (from the R prefix)", size, p.Kind)
		}
		a, b := p.Entry.Pads[0], p.Entry.Pads[1]
		if a.Number != "1" || b.Number != "2" {
			t.Errorf("%s: numbers = %q,%q", size, a.Number, b.Number)
		}
		near(t, size+" symmetry", a.XMM+b.XMM, 0, 1e-9)
	}
	// The classic 0603 land: ~0.8 mm lands about 1.6 mm apart.
	p := gen(t, GenSpec{Key: "R0603", Family: "chip", Size: "0603"})
	near(t, "0603 pad len", p.Entry.Pads[0].WMM, 0.81, 0.15)
	near(t, "0603 pad width", p.Entry.Pads[0].HMM, 1.01, 0.15)
	near(t, "0603 pitch", math.Abs(p.Entry.Pads[0].XMM-p.Entry.Pads[1].XMM), 1.64, 0.2)

	if _, err := Generate(GenSpec{Key: "X", Family: "chip", Size: "0666"}); err == nil {
		t.Error("want error on an unknown chip size")
	}
	if _, err := Generate(GenSpec{Family: "chip", Size: "0603"}); err == nil {
		t.Error("want error without a key")
	}
	if _, err := Generate(GenSpec{Key: "X", Family: "nope"}); err == nil {
		t.Error("want error on an unknown family")
	}
}

// Denser levels must shrink the land; looser levels must grow it.
func TestGenChipDensityOrdering(t *testing.T) {
	var lens [3]float64
	for i, d := range []Density{DensityMost, DensityNominal, DensityLeast} {
		p := gen(t, GenSpec{Key: "C0402", Family: "chip", Size: "0402", Density: d})
		lens[i] = p.Entry.Pads[0].WMM
	}
	if !(lens[0] > lens[1] && lens[1] > lens[2]) {
		t.Errorf("pad length must fall M > N > L, got %v", lens)
	}
	p := gen(t, GenSpec{Key: "C0402", Family: "chip", Size: "0402"})
	if p.Kind != "capacitor" {
		t.Errorf("kind = %q, want capacitor", p.Kind)
	}
}

func TestGenSOT(t *testing.T) {
	for _, tc := range []struct {
		family string
		pads   int
	}{
		{"sot23", 3}, {"sot23-5", 5}, {"sot23-6", 6}, {"sot223", 4}, {"sot89", 4},
	} {
		p := gen(t, GenSpec{Key: tc.family, Family: tc.family})
		if len(p.Entry.Pads) != tc.pads {
			t.Errorf("%s: pads = %d, want %d", tc.family, len(p.Entry.Pads), tc.pads)
		}
	}
	// SOT-23: two lands on one side, one on the other, 0.95 mm apart.
	p := gen(t, GenSpec{Key: "SOT-23", Family: "sot23"})
	p1, _ := padByNumber(p.Entry, "1")
	p2, _ := padByNumber(p.Entry, "2")
	p3, _ := padByNumber(p.Entry, "3")
	near(t, "sot23 pitch", math.Abs(p1.YMM-p2.YMM), 0.95, 1e-6)
	if p1.XMM >= 0 || p3.XMM <= 0 {
		t.Errorf("rows on the wrong sides: %+v %+v", p1, p3)
	}
	near(t, "sot23 row symmetry", p1.XMM+p3.XMM, 0, 1e-9)
	near(t, "sot23 pad3 y", p3.YMM, 0, 1e-9)
}

func TestGenSOIC(t *testing.T) {
	p := gen(t, GenSpec{Key: "SOIC-8", Family: "soic", Pins: 8})
	if len(p.Entry.Pads) != 8 {
		t.Fatalf("pads = %d", len(p.Entry.Pads))
	}
	// 1-4 down the left, 5-8 back up the right (counter-clockwise).
	p1, _ := padByNumber(p.Entry, "1")
	p4, _ := padByNumber(p.Entry, "4")
	p5, _ := padByNumber(p.Entry, "5")
	p8, _ := padByNumber(p.Entry, "8")
	if p1.XMM >= 0 || p4.XMM >= 0 || p5.XMM <= 0 || p8.XMM <= 0 {
		t.Error("SOIC rows are on the wrong sides")
	}
	if p1.YMM <= p4.YMM {
		t.Error("pin 1 must be the topmost of the left row")
	}
	near(t, "soic pin 1/8 alignment", p1.YMM-p8.YMM, 0, 1e-9)
	near(t, "soic pitch", math.Abs(p1.YMM-p4.YMM), 3*1.27, 1e-6)
	// Wide-body SOIC has a bigger span than narrow.
	wide := gen(t, GenSpec{Key: "SOIC-16W", Family: "soic", Pins: 16, Body: 7.5})
	narrow := gen(t, GenSpec{Key: "SOIC-16", Family: "soic", Pins: 16})
	if math.Abs(wide.Entry.Pads[0].XMM) <= math.Abs(narrow.Entry.Pads[0].XMM) {
		t.Error("wide body must push the lands further out")
	}
	if _, err := Generate(GenSpec{Key: "X", Family: "soic", Pins: 7}); err == nil {
		t.Error("want error on an odd pin count")
	}
}

func TestGenTSSOP(t *testing.T) {
	p := gen(t, GenSpec{Key: "TSSOP-20", Family: "tssop", Pins: 20})
	if len(p.Entry.Pads) != 20 {
		t.Fatalf("pads = %d", len(p.Entry.Pads))
	}
	p1, _ := padByNumber(p.Entry, "1")
	p2, _ := padByNumber(p.Entry, "2")
	near(t, "tssop pitch", math.Abs(p1.YMM-p2.YMM), 0.65, 1e-6)
	// An explicit pitch wins.
	q := gen(t, GenSpec{Key: "MSOP-8", Family: "msop", Pins: 8, Pitch: 0.5})
	q1, _ := padByNumber(q.Entry, "1")
	q2, _ := padByNumber(q.Entry, "2")
	near(t, "msop pitch", math.Abs(q1.YMM-q2.YMM), 0.5, 1e-6)
}

func TestGenQFN(t *testing.T) {
	p := gen(t, GenSpec{Key: "QFN-32", Family: "qfn", Pins: 32, Pitch: 0.5, Body: 5, EPX: 3.2})
	if len(p.Entry.Pads) != 33 {
		t.Fatalf("pads = %d, want 32 + exposed pad", len(p.Entry.Pads))
	}
	ep, ok := padByNumber(p.Entry, "33")
	if !ok {
		t.Fatal("no exposed pad")
	}
	near(t, "ep size", ep.WMM, 3.2, 1e-9)
	near(t, "ep centre", ep.XMM, 0, 1e-9)
	// Pin 1 is top-left, numbering runs counter-clockwise.
	p1, _ := padByNumber(p.Entry, "1")
	p9, _ := padByNumber(p.Entry, "9")
	p17, _ := padByNumber(p.Entry, "17")
	p25, _ := padByNumber(p.Entry, "25")
	if p1.XMM >= 0 || p9.YMM >= 0 || p17.XMM <= 0 || p25.YMM <= 0 {
		t.Errorf("QFN rows out of order: %+v %+v %+v %+v", p1, p9, p17, p25)
	}
	// Lands must reach outside the body.
	if math.Abs(p1.XMM)+p1.WMM/2 <= 2.5 {
		t.Error("QFN land must protrude past the body edge")
	}
	// DFN has only two rows.
	d := gen(t, GenSpec{Key: "DFN-8", Family: "dfn", Pins: 8, Pitch: 0.5, Body: 2, BodyLen: 2})
	if len(d.Entry.Pads) != 8 {
		t.Fatalf("dfn pads = %d", len(d.Entry.Pads))
	}
	for _, pad := range d.Entry.Pads {
		if pad.XMM == 0 {
			t.Error("DFN lands must sit on the left/right rows only")
		}
	}
	if _, err := Generate(GenSpec{Key: "X", Family: "qfn", Pins: 30}); err == nil {
		t.Error("want error when pins is not a multiple of 4")
	}
}

func TestGenQFP(t *testing.T) {
	p := gen(t, GenSpec{Key: "LQFP-64", Family: "lqfp", Pins: 64, Pitch: 0.5, Body: 10})
	if len(p.Entry.Pads) != 64 {
		t.Fatalf("pads = %d", len(p.Entry.Pads))
	}
	p1, _ := padByNumber(p.Entry, "1")
	p16, _ := padByNumber(p.Entry, "16")
	p17, _ := padByNumber(p.Entry, "17")
	if p1.XMM >= 0 || p16.XMM >= 0 {
		t.Error("pins 1-16 belong to the left row")
	}
	if p17.YMM >= 0 {
		t.Error("pin 17 belongs to the bottom row")
	}
	near(t, "lqfp pitch", math.Abs(p1.YMM-p16.YMM), 15*0.5, 1e-6)
	// Side lands are rotated: tall on the left/right, wide on the bottom/top.
	if p1.WMM <= p1.HMM {
		t.Error("left-row land should be longer in X")
	}
	if p17.HMM <= p17.WMM {
		t.Error("bottom-row land should be longer in Y")
	}
}

func TestGenDIP(t *testing.T) {
	p := gen(t, GenSpec{Key: "DIP-8", Family: "dip", Pins: 8})
	if len(p.Entry.Pads) != 8 {
		t.Fatalf("pads = %d", len(p.Entry.Pads))
	}
	for _, pad := range p.Entry.Pads {
		if pad.DrillMM == nil {
			t.Fatalf("pad %s: DIP must be through-hole", pad.Number)
		}
		near(t, "dip drill", *pad.DrillMM, 0.75, 1e-9)
	}
	p1, _ := padByNumber(p.Entry, "1")
	p8, _ := padByNumber(p.Entry, "8")
	near(t, "dip row spacing", math.Abs(p1.XMM-p8.XMM), 7.62, 1e-9)
	near(t, "dip 1/8 alignment", p1.YMM-p8.YMM, 0, 1e-9)
	// A wide DIP row spacing is honoured.
	w := gen(t, GenSpec{Key: "DIP-28W", Family: "dip", Pins: 28, Spacing: 15.24})
	a, _ := padByNumber(w.Entry, "1")
	b, _ := padByNumber(w.Entry, "28")
	near(t, "dip-w spacing", math.Abs(a.XMM-b.XMM), 15.24, 1e-9)
}

func TestGenHeader(t *testing.T) {
	p := gen(t, GenSpec{Key: "J_1x04", Family: "header", Rows: 1, Pins: 4})
	if len(p.Entry.Pads) != 4 {
		t.Fatalf("pads = %d", len(p.Entry.Pads))
	}
	if p.RefPrefix != "J" {
		t.Errorf("ref prefix = %q, want J", p.RefPrefix)
	}
	p1, _ := padByNumber(p.Entry, "1")
	p2, _ := padByNumber(p.Entry, "2")
	near(t, "header pitch", math.Abs(p1.YMM-p2.YMM), 2.54, 1e-9)
	near(t, "header drill", *p1.DrillMM, 0.84, 1e-9)
	near(t, "header x", p1.XMM, 0, 1e-9)

	// 2 rows: odd/even alternate across the rows, still on-pitch.
	q := gen(t, GenSpec{Key: "J_2x05", Family: "header", Rows: 2, Pins: 5, Pitch: 2.54})
	if len(q.Entry.Pads) != 10 {
		t.Fatalf("2-row pads = %d", len(q.Entry.Pads))
	}
	q1, _ := padByNumber(q.Entry, "1")
	q2, _ := padByNumber(q.Entry, "2")
	q3, _ := padByNumber(q.Entry, "3")
	near(t, "2x row gap", math.Abs(q1.XMM-q2.XMM), 2.54, 1e-9)
	near(t, "2x same row", q1.XMM-q3.XMM, 0, 1e-9)
	near(t, "2x pitch", math.Abs(q1.YMM-q3.YMM), 2.54, 1e-9)

	// A 1.27 mm header uses a smaller post, so a smaller drill and land.
	f := gen(t, GenSpec{Key: "J_1x04_1.27", Family: "header", Rows: 1, Pins: 4, Pitch: 1.27})
	f1, _ := padByNumber(f.Entry, "1")
	near(t, "fine drill", *f1.DrillMM, 0.60, 1e-9)

	if _, err := Generate(GenSpec{Key: "X", Family: "header", Rows: 3, Pins: 4}); err == nil {
		t.Error("want error on rows=3")
	}
	if _, err := Generate(GenSpec{Key: "X", Family: "header", Rows: 1}); err == nil {
		t.Error("want error without pins")
	}
}

// The generated entries must survive the on-disk library round trip and spawn
// a usable symbol.
func TestGeneratedPartRoundTrips(t *testing.T) {
	lib, err := core.OpenAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := gen(t, GenSpec{Key: "R0603", Family: "chip", Size: "0603"})
	if _, err := lib.Put(p.Entry); err != nil {
		t.Fatal(err)
	}
	got, _ := lib.Get("R0603")
	kind, ok := got.SymbolKindFor()
	if !ok || kind.Kind != "resistor" {
		t.Fatalf("symbol kind = %+v ok=%v", kind, ok)
	}
	// resistor pins are 1/2, matching the pads, so `net R1.1` resolves.
	pins := kind.Pins()
	if len(pins) != 2 || pins[0].Number != "1" {
		t.Errorf("pins = %+v", pins)
	}
	// Placement margin must come out non-zero from the courtyard.
	if got.BodyKeepout().IsZero() {
		t.Error("want a non-zero placement margin from the courtyard")
	}
}

func ExampleGenerate() {
	p, _ := Generate(GenSpec{Key: "R0603", Family: "chip", Size: "0603"})
	pad := p.Entry.Pads[0]
	fmt.Printf("%s pads=%d pad1=%.2fx%.2f@%.2f kind=%s\n",
		p.Entry.Key, len(p.Entry.Pads), pad.WMM, pad.HMM, pad.XMM, p.Kind)
	// Output: R0603 pads=2 pad1=0.81x1.02@-0.82 kind=resistor
}
