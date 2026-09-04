package script

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mentasystems/fragua/internal/core"
	"github.com/mentasystems/fragua/internal/parts"
)

// fixtureFetcher serves the parts package's saved EasyEDA responses, so the
// verb tests never touch the network.
type fixtureFetcher struct {
	calls int
	fail  bool
}

func (f *fixtureFetcher) Fetch(_ context.Context, lcsc string) ([]byte, error) {
	f.calls++
	if f.fail {
		return nil, errors.New("network is down")
	}
	return os.ReadFile(filepath.Join("..", "parts", "testdata", "easyeda", lcsc+".json"))
}

// partsProject returns a project with a throwaway on-disk library and a
// fixture-backed fetcher.
func partsProject(t *testing.T, name string) (*core.Project, *fixtureFetcher) {
	t.Helper()
	lib, err := core.OpenAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := core.NewProject(name)
	p.SetLibrary(lib)
	f := &fixtureFetcher{}
	old := partsFetcher
	partsFetcher = f
	t.Cleanup(func() { partsFetcher = old })
	return p, f
}

func run(t *testing.T, p *core.Project, script string) []Result {
	t.Helper()
	rs := RunScript(p, script)
	allOK(t, rs)
	return rs
}

func TestPartLCSCSpawnsAndCaches(t *testing.T) {
	p, f := partsProject(t, "parts-lcsc")
	rs := run(t, p, "part C25804\n")
	if !strings.Contains(rs[0].Result, "→ R1") {
		t.Fatalf("want an auto R1 reference, got %q", rs[0].Result)
	}
	if !strings.Contains(rs[0].Result, "src=lcsc") || !strings.Contains(rs[0].Result, "(fetched)") {
		t.Fatalf("result = %q", rs[0].Result)
	}
	if f.calls != 1 {
		t.Fatalf("fetches = %d, want 1", f.calls)
	}
	// The entry is in the library, keyed by LCSC id.
	e, ok := p.FindLibrary("C25804")
	if !ok || len(e.Pads) != 2 || e.Source != parts.SourceLCSC {
		t.Fatalf("entry = %+v ok=%v", e, ok)
	}
	// The symbol and the palette footprint both exist, so place works now.
	rs = run(t, p, "outline 20 20\nplace R1 10 10\n")
	if !strings.Contains(rs[1].Result, "placed R1") {
		t.Fatalf("place = %q", rs[1].Result)
	}
	// A second `part` for the same id is served from the library, offline.
	t.Setenv(OfflineEnv, "1")
	rs = run(t, p, "part LCSC:C25804 as=R9\n")
	if !strings.Contains(rs[0].Result, "(cached)") {
		t.Fatalf("second part = %q", rs[0].Result)
	}
	if f.calls != 1 {
		t.Fatalf("cache miss: fetches = %d", f.calls)
	}
}

func TestPartOfflineWithoutCache(t *testing.T) {
	p, f := partsProject(t, "parts-offline")
	t.Setenv(OfflineEnv, "1")
	rs := RunScript(p, "part C2040\n")
	if rs[0].OK {
		t.Fatal("want an error when offline and uncached")
	}
	if !strings.Contains(rs[0].Result, OfflineEnv) {
		t.Errorf("error = %q", rs[0].Result)
	}
	if f.calls != 0 {
		t.Errorf("offline must not fetch, got %d calls", f.calls)
	}
}

func TestPartRefreshRefetches(t *testing.T) {
	p, f := partsProject(t, "parts-refresh")
	run(t, p, "part C1525\n")
	run(t, p, "part C1525 as=C9 refresh=true\n")
	if f.calls != 2 {
		t.Errorf("fetches = %d, want 2", f.calls)
	}
}

func TestPartRejectsGarbage(t *testing.T) {
	p, _ := partsProject(t, "parts-bad")
	for _, line := range []string{"part", "part RP2040", "part kicad:", "part kicad:NoSuchLib:NoSuchFp"} {
		if rs := RunScript(p, line+"\n"); rs[0].OK {
			t.Errorf("%q should have failed", line)
		}
	}
}

// The RP2040 carries pin names, so `net … U1.GND` must resolve to pad 57.
func TestPartNamedPinsFeedNet(t *testing.T) {
	p, _ := partsProject(t, "parts-names")
	run(t, p, `
outline 30 30
part C2040 as=U1
place U1 15 15
net GND U1.GND U1.57
net GPIO7 U1.GPIO7
`)
	p.RLock()
	defer p.RUnlock()
	sch := p.Schematic()
	gnd := sch.Nets["GND"]
	if gnd == nil || len(gnd.Connections) != 2 {
		t.Fatalf("GND net = %+v", gnd)
	}
	for _, c := range gnd.Connections {
		if c.PinNumber != "57" {
			t.Errorf("GND connection on pin %q, want 57", c.PinNumber)
		}
	}
	gpio := sch.Nets["GPIO7"]
	if gpio == nil || len(gpio.Connections) != 1 || gpio.Connections[0].PinNumber != "9" {
		t.Fatalf("GPIO7 net = %+v", gpio)
	}
	// The board pad is stamped too, so the router sees the net.
	fp := p.Board().FootprintByRef("U1")
	if fp == nil {
		t.Fatal("U1 not on the board")
	}
	stamped := 0
	for _, pad := range fp.Pads {
		if pad.Net != nil && *pad.Net == "GND" {
			stamped++
		}
	}
	if stamped != 1 {
		t.Errorf("GND pads stamped = %d, want 1", stamped)
	}
}

func TestLibGenChipAndSpawn(t *testing.T) {
	p, _ := partsProject(t, "parts-gen")
	rs := run(t, p, "lib-gen R0603 family=chip size=0603 as=R1 value=10k\n")
	if !strings.Contains(rs[0].Result, "src=ipc") || !strings.Contains(rs[0].Result, "→ R1") {
		t.Fatalf("result = %q", rs[0].Result)
	}
	e, ok := p.FindLibrary("R0603")
	if !ok || len(e.Pads) != 2 || e.SymbolKindName != "resistor" {
		t.Fatalf("entry = %+v ok=%v", e, ok)
	}
	if e.DefaultValue != "10k" {
		t.Errorf("value = %q", e.DefaultValue)
	}
	run(t, p, "outline 20 20\nplace R1 5 5\nnet VCC R1.1\n")

	// Without as= the entry lands but nothing is spawned.
	run(t, p, "lib-gen QFN32 family=qfn pins=32 pitch=0.5 body=5 ep=3.2 density=L\n")
	if e, ok := p.FindLibrary("QFN32"); !ok || len(e.Pads) != 33 {
		t.Fatalf("qfn entry = %+v ok=%v", e, ok)
	}
	if rs := RunScript(p, "place QFN32 5 5\n"); rs[0].OK {
		t.Error("lib-gen without as= must not put anything on the palette")
	}
}

func TestLibGenErrors(t *testing.T) {
	p, _ := partsProject(t, "parts-gen-err")
	for _, line := range []string{
		"lib-gen",
		"lib-gen X",
		"lib-gen X family=nope",
		"lib-gen X family=chip size=0666",
		"lib-gen X family=chip size=0603 density=huge",
		"lib-gen X family=chip size=0603 kind=wat",
		"lib-gen X family=soic pins=7",
	} {
		if rs := RunScript(p, line+"\n"); rs[0].OK {
			t.Errorf("%q should have failed", line)
		}
	}
}

func TestLibGenHeaderAndDIP(t *testing.T) {
	p, _ := partsProject(t, "parts-th")
	run(t, p, `
outline 40 40
lib-gen J1x04 family=header rows=1 pins=4 pitch=2.54 as=J1
lib-gen DIP8 family=dip pins=8 as=U1
place J1 10 10
place U1 25 20
net VCC J1.1 U1.8
`)
	e, _ := p.FindLibrary("J1x04")
	for _, pad := range e.Pads {
		if pad.DrillMM == nil {
			t.Fatalf("header pad %s must be through-hole", pad.Number)
		}
	}
	p.RLock()
	defer p.RUnlock()
	if n := p.Schematic().Nets["VCC"]; n == nil || len(n.Connections) != 2 {
		t.Fatalf("VCC net = %+v", n)
	}
}

func TestLibImportKicadFootprint(t *testing.T) {
	p, _ := partsProject(t, "parts-import")
	rs := run(t, p, "lib-import kicad ../parts/testdata/kicad/SOT-23.kicad_mod as=Q1\n")
	if !strings.Contains(rs[0].Result, "→ Q1") || !strings.Contains(rs[0].Result, "src=kicad") {
		t.Fatalf("result = %q", rs[0].Result)
	}
	e, ok := p.FindLibrary("SOT-23")
	if !ok || len(e.Pads) != 3 {
		t.Fatalf("entry = %+v ok=%v", e, ok)
	}
	run(t, p, "outline 20 20\nplace Q1 10 10\nnet GND Q1.3\n")

	// A directory import brings in everything and refuses a single as=/key=.
	p2, _ := partsProject(t, "parts-import-dir")
	rs = run(t, p2, "lib-import kicad ../parts/testdata/kicad\n")
	if !strings.Contains(rs[0].Result, "4 footprint(s)") {
		t.Fatalf("dir import = %q", rs[0].Result)
	}
	if r := RunScript(p2, "lib-import kicad ../parts/testdata/kicad key=X\n"); r[0].OK {
		t.Error("key= on a multi-footprint import should fail")
	}
	for _, line := range []string{"lib-import", "lib-import kicad", "lib-import kicad /nope/nope"} {
		if r := RunScript(p2, line+"\n"); r[0].OK {
			t.Errorf("%q should have failed", line)
		}
	}
}

func TestLibImportKicadSymbolMergesNames(t *testing.T) {
	p, _ := partsProject(t, "parts-import-sym")
	run(t, p, "lib-import kicad ../parts/testdata/kicad/SOT-23.kicad_mod\n")
	rs := run(t, p, "lib-import kicad ../parts/testdata/kicad/AP1117-15.kicad_sym key=SOT-23\n")
	if !strings.Contains(rs[0].Result, "3 pins named") {
		t.Fatalf("merge = %q", rs[0].Result)
	}
	e, _ := p.FindLibrary("SOT-23")
	if len(e.Pins) != 3 || e.Pins[1].Name != "VO" {
		t.Fatalf("pins = %+v", e.Pins)
	}
	// The pad picks up the name too, which is what `net Q1.VO` keys off.
	named := false
	for _, pad := range e.Pads {
		if pad.Number == "2" && pad.Name == "VO" {
			named = true
		}
	}
	if !named {
		t.Errorf("pad 2 kept no name: %+v", e.Pads)
	}
	if rs := RunScript(p, "lib-import kicad ../parts/testdata/kicad/R_Small.kicad_sym\n"); rs[0].OK {
		t.Error("a symbol matching no entry should fail")
	}
}

func TestPartKicadResolvesThroughEnv(t *testing.T) {
	root := t.TempDir()
	pretty := filepath.Join(root, "Package_TO_SOT_SMD.pretty")
	if err := os.MkdirAll(pretty, 0o755); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile("../parts/testdata/kicad/SOT-23_v9.kicad_mod")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pretty, "SOT-23.kicad_mod"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(parts.KicadLibsEnv, root)

	p, _ := partsProject(t, "parts-kicad-env")
	rs := run(t, p, `
outline 20 20
part kicad:Package_TO_SOT_SMD:SOT-23 as=Q1 sym=../parts/testdata/kicad/AP1117-15.kicad_sym:AP1117-15
place Q1 10 10
net VOUT Q1.VO
`)
	if !strings.Contains(rs[1].Result, "src=kicad") {
		t.Fatalf("part = %q", rs[1].Result)
	}
	p.RLock()
	defer p.RUnlock()
	n := p.Schematic().Nets["VOUT"]
	if n == nil || len(n.Connections) != 1 || n.Connections[0].PinNumber != "2" {
		t.Fatalf("VOUT net = %+v", n)
	}
}

func TestListParts(t *testing.T) {
	p, _ := partsProject(t, "parts-list")
	if rs := run(t, p, "list-parts\n"); !strings.Contains(rs[0].Result, "0 parts") {
		t.Fatalf("empty = %q", rs[0].Result)
	}
	run(t, p, `
part C25804
lib-gen R0603 family=chip size=0603
lib-import kicad ../parts/testdata/kicad/SOT-23.kicad_mod
`)
	rs := run(t, p, "list-parts\n")
	out := rs[0].Result
	for _, want := range []string{"3 part(s)", "src=lcsc", "src=ipc", "src=kicad",
		"lcsc=C25804", "mpn=0603WAF1002T5E", "jlc=basic", "pins=2"} {
		if !strings.Contains(out, want) {
			t.Errorf("list-parts missing %q in:\n%s", want, out)
		}
	}
	// Filter by source, then by key substring.
	if rs := run(t, p, "list-parts ipc\n"); !strings.Contains(rs[0].Result, "1 part(s)") {
		t.Errorf("source filter = %q", rs[0].Result)
	}
	if rs := run(t, p, "list-parts sot\n"); !strings.Contains(rs[0].Result, "SOT-23") {
		t.Errorf("substring filter = %q", rs[0].Result)
	}
	if rs := run(t, p, "list-parts zzz\n"); !strings.Contains(rs[0].Result, "0 parts matching") {
		t.Errorf("empty filter = %q", rs[0].Result)
	}
	// list-lib still works and now shows the source.
	if rs := run(t, p, "list-lib\n"); !strings.Contains(rs[0].Result, "3 entries") {
		t.Errorf("list-lib = %q", rs[0].Result)
	}
}

func TestNextReferenceSkipsUsed(t *testing.T) {
	p, _ := partsProject(t, "parts-refs")
	run(t, p, "part C25804\npart C1525\npart C25804 refresh=true\n")
	p.RLock()
	defer p.RUnlock()
	refs := map[string]bool{}
	for _, s := range p.Schematic().Symbols {
		refs[s.Reference] = true
	}
	for _, want := range []string{"R1", "C1", "R2"} {
		if !refs[want] {
			t.Errorf("missing reference %s (have %v)", want, refs)
		}
	}
}

func TestUsageMentionsPartsVerbs(t *testing.T) {
	u := Usage()
	for _, want := range []string{"part LCSC:C2040", "part kicad:", "lib-gen NAME", "lib-import kicad",
		"list-parts", "FRAGUA_OFFLINE", "FRAGUA_KICAD_LIBS"} {
		if !strings.Contains(u, want) {
			t.Errorf("usage missing %q", want)
		}
	}
}
