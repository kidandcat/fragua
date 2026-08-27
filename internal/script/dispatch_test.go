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

func TestNCAndPackFailsOnERCError(t *testing.T) {
	p := core.NewProject("t-erc")
	lib, err := core.OpenAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p.SetLibrary(lib)
	rs := RunScript(p, `
outline 20 20
sym U1 ic
  pin 1 L IN role=input
  pin 2 R NC role=nc
erc
`)
	if len(rs) == 0 {
		t.Fatal("no results")
	}
	ercLine := rs[len(rs)-1]
	if !ercLine.OK {
		t.Fatalf("erc verb failed: %s", ercLine.Result)
	}
	if !strings.Contains(ercLine.Result, "1 errors") && !strings.Contains(ercLine.Result, "error") {
		t.Fatalf("expected ERC error for open input: %s", ercLine.Result)
	}
	out := t.TempDir()
	rs2 := RunScript(p, "pack fab=jlcpcb out="+out+"\n")
	if len(rs2) != 1 || rs2[0].OK {
		t.Fatalf("pack must be NOT READY on ERC errors: %+v", rs2)
	}
	if !strings.Contains(rs2[0].Result, "NOT READY") {
		t.Fatalf("pack msg: %s", rs2[0].Result)
	}
}

func TestFiducialDiffStitch(t *testing.T) {
	p := core.NewProject("t-stretch")
	rs := RunScript(p, `
outline 30 20
fiducial 2 2 ref=FID1
diff USB_DP USB_DM
class USB90 impedance=90
pour GND layer=Top stitch=true pitch=5
`)
	allOK(t, rs)
	p.RLock()
	defer p.RUnlock()
	fp := p.Board().FootprintByRef("FID1")
	if fp == nil || !fp.Fiducial {
		t.Fatal("fiducial missing")
	}
	if p.Schematic().Nets["USB_DP"] == nil || p.Schematic().Nets["USB_DP"].DiffPair != "USB_DM" {
		t.Fatalf("diff pair: %+v", p.Schematic().Nets)
	}
	if p.Schematic().NetClasses["USB90"] == nil || p.Schematic().NetClasses["USB90"].ImpedanceOhms != 90 {
		t.Fatalf("impedance class: %+v", p.Schematic().NetClasses)
	}
	if len(p.Board().Pours) != 1 || !p.Board().Pours[0].StitchRequested() {
		t.Fatalf("pour stitch: %+v", p.Board().Pours)
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

// Layer tokens are resolved against the stackup: on a 4-layer board In1.Cu is
// copper 1, In2.Cu copper 2 and B.Cu copper 3 — the 2-layer shorthand used to
// answer "Bottom" for In1.Cu and "In1" for In2.Cu, and put B.Cu on copper 1.
func TestPourResolvesLayerNamesOn4Layer(t *testing.T) {
	p := core.NewProject("t-pour-4l")
	rs := RunScript(p, `
outline 20 20
stackup 4
pour GND layer=In1.Cu
pour +3V3 layer=In2.Cu
pour SHIELD layer=B.Cu
pour SIG layer=F.Cu
`)
	allOK(t, rs)
	wantMsg := []string{"pour GND on In1.Cu", "pour +3V3 on In2.Cu", "pour SHIELD on B.Cu", "pour SIG on F.Cu"}
	for i, want := range wantMsg {
		if got := rs[i+2].Result; got != want {
			t.Fatalf("result %d = %q, want %q", i+2, got, want)
		}
	}
	p.RLock()
	defer p.RUnlock()
	b := p.Board()
	got := map[string]uint8{}
	for _, pr := range b.Pours {
		got[pr.Net] = pr.Layer.Index
	}
	want := map[string]uint8{"SIG": 0, "GND": 1, "+3V3": 2, "SHIELD": 3}
	for net, idx := range want {
		if got[net] != idx {
			t.Fatalf("pour %s on copper %d, want %d (all: %+v)", net, got[net], idx, got)
		}
	}
	if len(b.Pours) != len(want) {
		t.Fatalf("expected %d pours, got %d: %+v", len(want), len(b.Pours), got)
	}
}

// auto-pour asks for Top and Bottom by name, so it must follow the stackup too.
func TestAutoPourHitsRealBottomOn4Layer(t *testing.T) {
	p := core.NewProject("t-autopour-4l")
	rs := RunScript(p, "outline 20 20\nstackup 4\nauto-pour GND\n")
	allOK(t, rs)
	p.RLock()
	defer p.RUnlock()
	var layers []uint8
	for _, pr := range p.Board().Pours {
		if pr.Net == "GND" {
			layers = append(layers, pr.Layer.Index)
		}
	}
	if len(layers) != 2 || layers[0] != 0 || layers[1] != 3 {
		t.Fatalf("auto-pour GND landed on %v, want [0 3]", layers)
	}
}

// A trace asks for the same layer names, from the same helper.
func TestTraceResolvesInnerLayerOn4Layer(t *testing.T) {
	p := core.NewProject("t-trace-4l")
	rs := RunScript(p, "outline 20 20\nstackup 4\ntrace SIG 1 1 5 1 layer=In2.Cu\ntrace SIG2 1 2 5 2 layer=B.Cu\n")
	allOK(t, rs)
	if !strings.Contains(rs[2].Result, "on In2.Cu") {
		t.Fatalf("trace result: %s", rs[2].Result)
	}
	p.RLock()
	defer p.RUnlock()
	got := map[string]uint8{}
	for _, tr := range p.Board().Traces {
		got[tr.Net] = tr.Layer.Index
	}
	if got["SIG"] != 2 || got["SIG2"] != 3 {
		t.Fatalf("trace layers %+v, want SIG=2 SIG2=3", got)
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

func TestTeardropAndImpedanceVerbs(t *testing.T) {
	p := core.NewProject("t-z")
	out := t.TempDir()
	rs := RunScript(p, `
outline 40 20
lib rpad
  pad 1 0 0 1.2 1.2
palette R1 rpad
place R1 10 10
net SIG R1.1
trace SIG 10 10 25 10 width=0.25
teardrop on
class Z50 impedance=50
impedance SIG
pack fab=jlcpcb out=`+out+`
`)
	allOK(t, rs)
	if !p.Board().Teardrops {
		t.Fatal("teardrop on did not persist")
	}
	cls := p.Schematic().NetClasses["Z50"]
	if cls == nil || cls.ImpedanceOhms != 50 || cls.TraceWidthMM < 2.5 || cls.TraceWidthMM > 3.5 {
		t.Fatalf("class Z50 should compute ~3 mm width: %+v", cls)
	}
	var zLine string
	for _, r := range rs {
		if r.Tool == "impedance" {
			zLine = r.Result
		}
	}
	if !strings.Contains(zLine, "microstrip") || !strings.Contains(zLine, "Z0=") {
		t.Fatalf("impedance SIG: %s", zLine)
	}
	gbr, err := os.ReadFile(filepath.Join(out, "t-z-fab", "t-z-F_Cu.gbr"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(gbr), "G36*") {
		t.Fatal("pack with teardrop on must put copper G36 in F_Cu")
	}
	ipc, err := os.ReadFile(filepath.Join(out, "t-z-fab", "t-z-ipc-d-356.ipc"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ipc), "P  UNITS CUST 0") || !strings.Contains(string(ipc), "327") {
		t.Fatalf("IPC-D-356A pack file:\n%s", ipc)
	}
}

func TestImpedanceMissingErErrors(t *testing.T) {
	p := core.NewProject("t-z-err")
	p.MutateBoard(func(b *core.Board) {
		b.Stackup = &core.LayerStackup{
			Layers:      []core.LayerSpec{{Name: "F.Cu"}, {Name: "B.Cu"}},
			Dielectrics: []core.Dielectric{{ThicknessMM: 1.6}},
		}
	})
	rs := RunScript(p, "impedance SIG\n")
	if len(rs) != 1 || rs[0].OK {
		t.Fatalf("unset Er must error: %+v", rs)
	}
	if !strings.Contains(rs[0].Result, "Er") {
		t.Fatalf("error should mention Er: %s", rs[0].Result)
	}
}

func TestSICheckVerb(t *testing.T) {
	p := core.NewProject("t-si")
	rs := RunScript(p, `
outline 40 30
stackup 4
class hs impedance=50
net-class CLK hs
pour GND layer=In1
trace CLK 5 5 20 5 layer=Top width=0.22
via CLK 6 5
via CLK 7 5
si-check
si-check CLK tol=0.50
si_check max_vias=1
`)
	allOK(t, rs)
	var runs []string
	var budget string
	for _, r := range rs {
		switch r.Tool {
		case "si-check":
			runs = append(runs, r.Result)
		case "si_check": // alias
			budget = r.Result
		}
	}
	if len(runs) != 2 {
		t.Fatalf("expected 2 si-check results, got %d", len(runs))
	}
	plain, widened := runs[0], runs[1]
	// 0.22 mm on the 4-layer top microstrip is ≈62 Ω against a 50 Ω target:
	// past 2× the default ±10% ⇒ an error, one line per finding.
	if !strings.Contains(plain, "si: 1 errors, 0 warnings (1 findings)") {
		t.Fatalf("si-check summary: %s", plain)
	}
	if !strings.Contains(plain, "impedance_deviation error net=CLK") {
		t.Fatalf("si-check detail line: %s", plain)
	}
	if !strings.Contains(widened, "si: 0 errors, 0 warnings (0 findings)") {
		t.Fatalf("tol=0.50 should absorb the deviation: %s", widened)
	}
	if !strings.Contains(budget, "via_budget warning net=CLK") {
		t.Fatalf("max_vias=1 with 2 vias: %s", budget)
	}
}

func TestSICheckRejectsBadTolerance(t *testing.T) {
	p := core.NewProject("t-si-bad")
	rs := RunScript(p, "outline 40 30\nsi-check tol=0\n")
	if len(rs) != 2 || rs[1].OK {
		t.Fatalf("tol=0 must error: %+v", rs)
	}
}
