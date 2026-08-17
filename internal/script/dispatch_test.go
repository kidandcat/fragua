package script

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mentasystems/fragua/internal/core"
)

func allOK(t *testing.T, rs []Result) {
	t.Helper()
	for _, r := range rs {
		if !r.OK {
			t.Fatalf("line %d %s failed: %s", r.Line, r.Tool, r.Result)
		}
	}
}

func TestOutlineAndStatus(t *testing.T) {
	p := core.NewProject("t1")
	rs := RunScript(p, "outline 40 30 radius=1.5\nstatus\n")
	allOK(t, rs)
	if len(rs) != 2 {
		t.Fatalf("results: %d", len(rs))
	}
	if !strings.Contains(rs[1].Result, "outline=40.00x30.00mm") {
		t.Fatalf("status: %s", rs[1].Result)
	}
	p.RLock()
	b := p.Board()
	if b.Outline == nil || b.Outline.Width().ToMM() != 40 {
		t.Fatalf("outline width")
	}
	p.RUnlock()
}

func TestTraceAndVia(t *testing.T) {
	p := core.NewProject("t2")
	// Manual copper + route (route may no-op without pad nets; verb must still OK).
	rs := RunScript(p, `
outline 20 20
trace GND 1 1 5 1 layer=Top width=0.2
via GND 5 1 drill=0.3 dia=0.6
route max_seconds=1
status
`)
	allOK(t, rs)
	p.RLock()
	defer p.RUnlock()
	b := p.Board()
	if len(b.Traces) < 1 {
		t.Fatalf("traces: %d", len(b.Traces))
	}
	foundGND := false
	for _, tr := range b.Traces {
		if tr.Net == "GND" {
			foundGND = true
			break
		}
	}
	if !foundGND {
		t.Fatalf("expected GND trace among %d traces", len(b.Traces))
	}
	if len(b.Vias) < 1 || b.Vias[0].Net != "GND" {
		t.Fatalf("vias: %+v", b.Vias)
	}
}

func TestAgentProductVerbs(t *testing.T) {
	p := core.NewProject("prod")
	rs := RunScript(p, `
outline 40 30
lib r0603
  pad 1 -0.8 0 0.9 0.9
  pad 2 0.8 0 0.9 0.9
sym R1 resistor key=r0603
sym R2 resistor key=r0603
palette R1 r0603
palette R2 r0603
place R1 8 8
place R2 32 22
net S R1.1 R2.1
net OUT R1.2 R2.2
auto-place R1 R2 seed=42 iters=400
pour GND layer=Top
auto-pour GND
stitch
drc
erc
status
`)
	allOK(t, rs)
	p.RLock()
	defer p.RUnlock()
	b := p.Board()
	if len(b.Footprints) != 2 {
		t.Fatalf("footprints %d", len(b.Footprints))
	}
	if len(b.Pours) < 1 {
		t.Fatalf("expected pour")
	}
}

func TestEdgePlaceAndMove(t *testing.T) {
	p := core.NewProject("edge")
	rs := RunScript(p, `
outline 40 20
lib term edge=true
  pad 1 -2.5 0 1.2 1.2
  pad 2 2.5 0 1.2 1.2
palette J1 term
edge-place J1 bottom along=20
move J1 18 2
rotate J1 0
status
`)
	allOK(t, rs)
	fp := p.Board().FootprintByRef("J1")
	if fp == nil || !fp.EdgeMounted {
		t.Fatalf("J1 missing or not edge-mounted")
	}
}

func TestRuleArea(t *testing.T) {
	p := core.NewProject("t3")
	rs := RunScript(p, `
outline 40 30
rule-area dense 10 10 20 20 clearance=0.1 width=0.12 priority=1
status
`)
	allOK(t, rs)
	p.RLock()
	defer p.RUnlock()
	b := p.Board()
	if len(b.RuleAreas) != 1 {
		t.Fatalf("rule areas: %d", len(b.RuleAreas))
	}
	a := b.RuleAreas[0]
	if a.Name != "dense" || a.ClearanceMM == nil || *a.ClearanceMM != 0.1 {
		t.Fatalf("area: %+v", a)
	}
}

func TestSymLibPalettePlace(t *testing.T) {
	p := core.NewProject("t4")
	// Isolate from ~/.pcb-library so lib Put does not mutate the user store.
	lib, err := core.OpenAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p.SetLibrary(lib)
	rs := RunScript(p, `
outline 40 30
lib r_0603
  pad 1 -0.8 0 0.8 0.9
  pad 2  0.8 0 0.8 0.9
sym R1 resistor key=r_0603 value=10k
palette R1 r_0603
place R1 10 15
net GND R1.1
status
`)
	allOK(t, rs)
	p.RLock()
	defer p.RUnlock()
	if _, ok := p.FindLibrary("r_0603"); !ok {
		t.Fatal("library missing")
	}
	if len(p.Schematic().Symbols) != 1 {
		t.Fatalf("symbols: %d", len(p.Schematic().Symbols))
	}
	fp := p.Board().FootprintByRef("R1")
	if fp == nil {
		t.Fatal("R1 not placed")
	}
	if len(fp.Pads) != 2 {
		t.Fatalf("pads: %d", len(fp.Pads))
	}
	if len(p.Palette()) != 0 {
		t.Fatalf("palette should be empty after place, got %d", len(p.Palette()))
	}
}

func TestSymLcscReachesPackBOM(t *testing.T) {
	p := core.NewProject("t-lcsc")
	lib, err := core.OpenAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p.SetLibrary(lib)
	out := t.TempDir()
	rs := RunScript(p, `
outline 20 20
lib r_0603 lcsc=C25804 mpn=RC0603FR-0710KL manufacturer=Yageo value=10k
  pad 1 -0.8 0 0.8 0.9
  pad 2  0.8 0 0.8 0.9
sym R1 resistor key=r_0603 value=10k lcsc=C25804
palette R1 r_0603
place R1 10 10
fab-rules jlcpcb
pack fab=jlcpcb out=`+out+`
`)
	allOK(t, rs)
	fp := p.Board().FootprintByRef("R1")
	if fp == nil || fp.LcscID != "C25804" {
		t.Fatalf("lcsc not on footprint: %+v", fp)
	}
	bom, err := os.ReadFile(filepath.Join(out, "t-lcsc-fab", "t-lcsc-bom.csv"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(bom)
	if !strings.Contains(text, "LCSC Part #") || !strings.Contains(text, "C25804") {
		t.Fatalf("BOM missing LCSC:\n%s", text)
	}
	if strings.Contains(text, "library:") {
		t.Fatalf("library: prefix in BOM:\n%s", text)
	}
	if _, err := os.Stat(filepath.Join(out, "t-lcsc-fab", "t-lcsc-F_Paste.gbr")); err != nil {
		t.Fatal("missing paste gerber")
	}
	readme, err := os.ReadFile(filepath.Join(out, "t-lcsc-fab", "README.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readme), "F_Paste") || !strings.Contains(string(readme), "Fragua") {
		t.Fatalf("README incomplete:\n%s", readme)
	}
}

func TestListLib(t *testing.T) {
	p := core.NewProject("t-lib-list")
	lib, err := core.OpenAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p.SetLibrary(lib)
	rs := RunScript(p, `
lib z_part
  pad 1 0 0 1 1
lib a_part
  pad 1 0 0 1 1
list-lib
`)
	allOK(t, rs)
	out := rs[len(rs)-1].Result
	if !strings.Contains(out, "2 entries") {
		t.Fatalf("list-lib: %s", out)
	}
	// Sorted keys: a_part before z_part
	ia := strings.Index(out, "a_part")
	iz := strings.Index(out, "z_part")
	if ia < 0 || iz < 0 || ia > iz {
		t.Fatalf("list-lib order: %s", out)
	}
}

func TestLayerAddRemove(t *testing.T) {
	p := core.NewProject("t5")
	rs := RunScript(p, `
outline 20 20
layer add In1.Cu signal
layer add In2.Cu signal
layer list
layer remove In2.Cu
layer remove In1.Cu
layer list
`)
	allOK(t, rs)
	p.RLock()
	defer p.RUnlock()
	s := p.Board().StackupOrDefault()
	if s.CopperCount() != 2 {
		t.Fatalf("copper count after round-trip: %d", s.CopperCount())
	}
}

func TestLayerAddInsertsBeforeBottom(t *testing.T) {
	p := core.NewProject("t5b")
	rs := RunScript(p, "outline 20 20\nlayer add In1.Cu signal\nlayer add In2.Cu plane\nlayer list\n")
	allOK(t, rs)
	p.RLock()
	defer p.RUnlock()
	s := p.Board().StackupOrDefault()
	if s.CopperCount() != 4 {
		t.Fatalf("copper %d", s.CopperCount())
	}
	want := []string{"F.Cu", "In1.Cu", "In2.Cu", "B.Cu"}
	for i, n := range want {
		if s.Layers[i].Name != n {
			t.Fatalf("layer[%d]=%s want %s", i, s.Layers[i].Name, n)
		}
	}
	if s.BottomLayer().Index != 3 {
		t.Fatalf("bottom %d", s.BottomLayer().Index)
	}
}

func TestFabRulesAndScreenshot(t *testing.T) {
	p := core.NewProject("t6")
	dir := t.TempDir()
	path := filepath.Join(dir, "board.svg")
	rs := RunScript(p, "outline 10 10\nfab-rules jlcpcb\nscreenshot "+path+"\n")
	allOK(t, rs)
	p.RLock()
	fr := p.Board().FabRules
	p.RUnlock()
	if fr == nil || fr.Preset != "jlcpcb-2l" {
		t.Fatalf("fab rules: %+v", fr)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "<svg") {
		t.Fatalf("screenshot not svg: %s", data[:min(40, len(data))])
	}
}

func TestCompactNeedsOutline(t *testing.T) {
	p := core.NewProject("t7")
	rs := RunScript(p, "compact\n")
	if len(rs) != 1 || rs[0].OK {
		t.Fatalf("compact without outline should fail: %+v", rs)
	}
	if !strings.Contains(rs[0].Result, "no outline") {
		t.Fatalf("msg: %s", rs[0].Result)
	}
}

func TestGenericICSymbol(t *testing.T) {
	p := core.NewProject("t8")
	rs := RunScript(p, `
sym U1 ic key=demo
  pin 1 L VCC role=power_in
  pin 2 R GND role=power_in
status
`)
	allOK(t, rs)
	p.RLock()
	defer p.RUnlock()
	var found *core.Symbol
	for _, s := range p.Schematic().Symbols {
		if s != nil && s.Reference == "U1" {
			found = s
		}
	}
	if found == nil || found.Kind.Kind != "generic_ic" || len(found.Kind.ICPins) != 2 {
		t.Fatalf("symbol: %+v", found)
	}
}
