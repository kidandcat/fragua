package kicad

import (
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mentasystems/fragua/internal/core"
)

func mustID(s string) core.ID {
	id, err := core.ParseID(s)
	if err != nil {
		panic(err)
	}
	return id
}

// smallBoard is a two-part 2-layer board with one routed net, a via, a GND
// pour, an NPTH hole and a keepout: one of every construct the writer emits.
func smallBoard() *core.Board {
	b := core.NewBoard()
	b.Outline = &core.Rect{Max: core.Point{X: core.FromMM(20), Y: core.FromMM(12)}}
	gnd, sig := "GND", "SIG"
	drill := core.FromMM(0.9)
	r1 := &core.Footprint{
		ID: mustID("11111111-1111-4111-8111-111111111111"), Reference: "R1", Value: "10k",
		Library: "demo", Key: "r_0603", Position: core.Point{X: core.FromMM(5), Y: core.FromMM(6)},
		Layer: core.LayerTop,
		Pads: []core.Pad{
			{Number: "1", Offset: core.Point{X: core.FromMM(-0.775)}, Size: [2]core.Length{core.FromMM(0.95), core.FromMM(0.95)}, Layer: core.LayerTop, Net: &sig},
			{Number: "2", Offset: core.Point{X: core.FromMM(0.775)}, Size: [2]core.Length{core.FromMM(0.95), core.FromMM(0.95)}, Layer: core.LayerTop, Net: &gnd},
		},
		Silk: []core.FootprintSilkItem{
			{Kind: "text", Layer: core.SilkTop, Position: core.Point{Y: core.FromMM(1.2)}, Text: "{REF}", Size: core.FromMM(0.5), Width: core.FromMM(0.0625)},
			{Kind: "line", Layer: core.SilkTop, Start: core.Point{X: core.FromMM(-1.5)}, End: core.Point{X: core.FromMM(1.5)}, Width: core.FromMM(0.12)},
		},
	}
	j1 := &core.Footprint{
		ID: mustID("22222222-2222-4222-8222-222222222222"), Reference: "J1", Value: "hdr",
		Library: "demo", Key: "header_1x02", Position: core.Point{X: core.FromMM(15), Y: core.FromMM(6)},
		Rotation: 90, Layer: core.LayerTop,
		Pads: []core.Pad{
			{Number: "1", Offset: core.Point{X: core.FromMM(-1.27)}, Size: [2]core.Length{core.FromMM(1.7), core.FromMM(1.7)}, Layer: core.LayerTop, Net: &sig, Drill: &drill},
			{Number: "2", Offset: core.Point{X: core.FromMM(1.27)}, Size: [2]core.Length{core.FromMM(1.7), core.FromMM(1.7)}, Layer: core.LayerTop, Net: &gnd, Drill: &drill},
		},
	}
	b.AddFootprint(r1)
	b.AddFootprint(j1)
	b.Traces = []core.Trace{{
		ID: core.NewID(), Layer: core.LayerTop, Net: sig, Width: core.FromMM(0.25),
		Start: core.Point{X: core.FromMM(5.775), Y: core.FromMM(6)},
		End:   core.Point{X: core.FromMM(13.73), Y: core.FromMM(6)},
	}}
	b.Vias = []core.Via{{
		ID: core.NewID(), Net: sig, Position: core.Point{X: core.FromMM(10), Y: core.FromMM(6)},
		Drill: core.FromMM(0.3), Diameter: core.FromMM(0.6),
	}}
	b.Pours = []core.Pour{
		{Net: gnd, Layer: core.LayerTop},
		{Net: gnd, Layer: core.LayerBottom},
	}
	b.MountHoles = []core.MountHole{{ID: core.NewID(), Center: core.Point{X: core.FromMM(2), Y: core.FromMM(2)}, Diameter: core.FromMM(2.2), Label: "H1"}}
	b.Keepouts = []core.Keepout{{ID: core.NewID(), NoCopper: true, Rect: &core.Rect{
		Min: core.Point{X: core.FromMM(17), Y: core.FromMM(9)},
		Max: core.Point{X: core.FromMM(19), Y: core.FromMM(11)},
	}}}
	b.SilkTexts = []core.SilkText{{Layer: core.SilkTop, Position: core.Point{X: core.FromMM(10), Y: core.FromMM(11)}, Text: "FRAGUA", Size: core.FromMM(1), Width: core.FromMM(0.15)}}
	return b
}

func export(t *testing.T, b *core.Board, opts Options) (string, *node) {
	t.Helper()
	s, err := Export(b, opts)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	n, err := parseSexpr(s)
	if err != nil {
		t.Fatalf("emitted file does not parse as s-expression: %v", err)
	}
	if n.tag() != "kicad_pcb" {
		t.Fatalf("root tag = %q, want kicad_pcb", n.tag())
	}
	return s, n
}

func TestExportHeaderAndLayers(t *testing.T) {
	_, root := export(t, smallBoard(), DefaultOptions())
	if got := root.first("version").arg(0); got != FormatVersion {
		t.Fatalf("version = %q, want %q", got, FormatVersion)
	}
	if got := root.first("generator").arg(0); got != Generator {
		t.Fatalf("generator = %q, want %q", got, Generator)
	}
	layers := root.first("layers")
	if layers == nil {
		t.Fatal("no layers block")
	}
	names := map[string]string{}
	for _, l := range layers.list[1:] {
		names[l.tag()] = l.arg(0)
	}
	// KiCad 9 copper ordinals: F.Cu 0, B.Cu 2 (not the pre-9 31).
	if names["0"] != "F.Cu" || names["2"] != "B.Cu" {
		t.Fatalf("2-layer copper ordinals wrong: %v", names)
	}
	for _, want := range []string{"F.SilkS", "B.SilkS", "F.Mask", "Edge.Cuts", "F.CrtYd", "F.Fab"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("layers block is missing %s", want)
		}
	}
}

func TestExportFourLayerStackup(t *testing.T) {
	b := smallBoard()
	b.Apply4Layer()
	_, root := export(t, b, DefaultOptions())
	names := map[string]string{}
	for _, l := range root.first("layers").list[1:] {
		names[l.tag()] = l.arg(0)
	}
	want := map[string]string{"0": "F.Cu", "4": "In1.Cu", "6": "In2.Cu", "2": "B.Cu"}
	for ord, name := range want {
		if names[ord] != name {
			t.Fatalf("copper ordinal %s = %q, want %q (all: %v)", ord, names[ord], name, names)
		}
	}
	setup := root.first("setup")
	if setup == nil || setup.first("stackup") == nil {
		t.Fatal("4-layer board exported without a stackup")
	}
	cu := 0
	for _, l := range setup.first("stackup").find("layer") {
		if l.first("type").arg(0) == "copper" {
			cu++
		}
	}
	if cu != 4 {
		t.Fatalf("stackup copper layers = %d, want 4", cu)
	}
}

func TestExportNetsPadsAndCopper(t *testing.T) {
	b := smallBoard()
	_, root := export(t, b, DefaultOptions())

	nets := root.find("net")
	if len(nets) != 3 { // "", GND, SIG
		t.Fatalf("net count = %d, want 3", len(nets))
	}
	if nets[0].arg(0) != "0" || nets[0].arg(1) != "" {
		t.Fatalf("net 0 must be the unconnected net, got %q %q", nets[0].arg(0), nets[0].arg(1))
	}
	byName := map[string]string{}
	for _, n := range nets {
		byName[n.arg(1)] = n.arg(0)
	}

	fps := root.find("footprint")
	// Two parts plus one NPTH mounting-hole footprint.
	if len(fps) != 3 {
		t.Fatalf("footprint count = %d, want 3", len(fps))
	}
	pads := root.findAll("pad")
	if len(pads) != 5 {
		t.Fatalf("pad count = %d, want 5 (4 component + 1 NPTH)", len(pads))
	}
	kinds := map[string]int{}
	for _, p := range pads {
		kinds[p.arg(1)]++
		if n := p.first("net"); n != nil {
			if byName[n.arg(1)] != n.arg(0) {
				t.Fatalf("pad net %q has index %s, net table says %s", n.arg(1), n.arg(0), byName[n.arg(1)])
			}
		}
	}
	if kinds["smd"] != 2 || kinds["thru_hole"] != 2 || kinds["np_thru_hole"] != 1 {
		t.Fatalf("pad types = %v, want 2 smd / 2 thru_hole / 1 np_thru_hole", kinds)
	}

	if got := len(root.find("segment")); got != 1 {
		t.Fatalf("segment count = %d, want 1", got)
	}
	if got := len(root.find("via")); got != 1 {
		t.Fatalf("via count = %d, want 1", got)
	}
	// Two pours plus one keepout zone.
	zones := root.find("zone")
	if len(zones) != 3 {
		t.Fatalf("zone count = %d, want 3", len(zones))
	}
	keepouts := 0
	filled := 0
	for _, z := range zones {
		if z.first("keepout") != nil {
			keepouts++
			continue
		}
		if z.first("polygon") == nil {
			t.Fatal("pour zone has no outline polygon")
		}
		filled += len(z.find("filled_polygon"))
	}
	if keepouts != 1 {
		t.Fatalf("keepout zones = %d, want 1", keepouts)
	}
	if filled == 0 {
		t.Fatal("filled zones requested but no filled_polygon was emitted")
	}
}

func TestZoneOutlineOnly(t *testing.T) {
	opts := DefaultOptions()
	opts.FillZones = false
	_, root := export(t, smallBoard(), opts)
	for _, z := range root.find("zone") {
		if len(z.find("filled_polygon")) != 0 {
			t.Fatal("zones=outline still emitted a filled_polygon")
		}
	}
}

// The Y flip is the one transform that, if wrong, silently mirrors a board.
func TestCoordinateFlipAndRotation(t *testing.T) {
	b := smallBoard()
	_, root := export(t, b, DefaultOptions())
	var r1 *node
	for _, fp := range root.find("footprint") {
		for _, p := range fp.find("property") {
			if p.arg(0) == "Reference" && p.arg(1) == "R1" {
				r1 = fp
			}
		}
	}
	if r1 == nil {
		t.Fatal("R1 not found in the export")
	}
	at := r1.first("at")
	// Outline is 20x12 with its top at y=12; R1 sits at (5, 6) Y-up.
	if at.arg(0) != "5" || at.arg(1) != "6" {
		t.Fatalf("R1 at = (%s, %s), want (5, 6)", at.arg(0), at.arg(1))
	}
	// The silk text sits 1.2 mm above the part in Fragua, so below it in KiCad.
	for _, txt := range r1.findAll("fp_text") {
		if txt.arg(1) == "R1" {
			if y := txt.first("at").arg(1); y != "-1.2" {
				t.Fatalf("footprint silk y = %s, want -1.2 (Y flipped)", y)
			}
		}
	}
	// J1 is rotated 90°: the footprint and its pads carry the same angle.
	for _, fp := range root.find("footprint") {
		for _, p := range fp.find("property") {
			if p.arg(0) != "Reference" || p.arg(1) != "J1" {
				continue
			}
			if a := fp.first("at").arg(2); a != "90" {
				t.Fatalf("J1 footprint angle = %q, want 90", a)
			}
			for _, pad := range fp.find("pad") {
				if a := pad.first("at").arg(2); a != "90" {
					t.Fatalf("J1 pad angle = %q, want 90", a)
				}
			}
		}
	}
	// The board silk text at y=11 lands 1 mm below the flip origin.
	txt := root.first("gr_text")
	if txt == nil || txt.arg(0) != "FRAGUA" {
		t.Fatal("board silk text missing")
	}
	if y := txt.first("at").arg(1); y != "1" {
		t.Fatalf("board silk y = %s, want 1", y)
	}
}

func TestEdgeCutsClosed(t *testing.T) {
	_, root := export(t, smallBoard(), DefaultOptions())
	lines := root.find("gr_line")
	edges := 0
	for _, l := range lines {
		if l.first("layer").arg(0) == "Edge.Cuts" {
			edges++
		}
	}
	if edges != 4 {
		t.Fatalf("Edge.Cuts segments = %d, want 4 for a rectangular outline", edges)
	}
}

func TestRoundedOutlineArcs(t *testing.T) {
	b := smallBoard()
	b.OutlineCornerRadius = core.FromMM(1.5)
	_, root := export(t, b, DefaultOptions())
	if got := len(root.find("gr_arc")); got != 4 {
		t.Fatalf("gr_arc count = %d, want 4 for a rounded outline", got)
	}
}

// A board must export byte-identically twice: UUIDs are derived, not random.
func TestExportDeterministic(t *testing.T) {
	b := smallBoard()
	a, err := Export(b, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	c, err := Export(b, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if a != c {
		t.Fatal("two exports of the same board differ")
	}
}

// A reference or value carrying a quote, a backslash or a control byte must
// still produce a file that parses: KiCad escapes only " and \.
func TestHostileStringsStayParseable(t *testing.T) {
	b := smallBoard()
	fp := b.FootprintByRef("R1")
	fp.Value = "10k \"±1%\" \\ line\nbreak\x1b"
	fp.Description = "a \"quoted\" \\ description\t"
	s, err := Export(b, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	root, err := parseSexpr(s)
	if err != nil {
		t.Fatalf("hostile strings broke the s-expression: %v", err)
	}
	for _, f := range root.findAll("property") {
		if f.arg(0) != "Value" || !strings.Contains(f.arg(1), "±1%") {
			continue
		}
		if strings.ContainsAny(f.arg(1), "\n\x1b\t") {
			t.Fatalf("control characters survived into the file: %q", f.arg(1))
		}
		return
	}
	t.Fatal("the hostile Value field never made it into the export")
}

func TestNumFormatting(t *testing.T) {
	cases := map[float64]string{0: "0", -0.0000001: "0", 1: "1", 1.5: "1.5", -2.25: "-2.25", 0.0001: "0.0001"}
	for in, want := range cases {
		if got := num(in); got != want {
			t.Fatalf("num(%v) = %q, want %q", in, got, want)
		}
	}
}

// Every reference board in the repo must export and re-parse. This is the
// regression net: a model field the writer forgets shows up as a parse error
// or a missing item, not as a board KiCad silently refuses to open.
func TestExportRepoBoards(t *testing.T) {
	for _, dir := range []string{"../../bench/boards", "../../stress"} {
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, ent := range ents {
			if ent.IsDir() || filepath.Ext(ent.Name()) != ".fragua" {
				continue
			}
			path := filepath.Join(dir, ent.Name())
			t.Run(filepath.Base(path), func(t *testing.T) {
				p, err := core.LoadFromPath(path)
				if err != nil {
					t.Fatalf("load: %v", err)
				}
				board := p.Board()
				s, err := Export(board, DefaultOptions())
				if err != nil {
					t.Fatalf("export: %v", err)
				}
				root, err := parseSexpr(s)
				if err != nil {
					t.Fatalf("re-parse: %v", err)
				}
				if got, want := len(root.findAll("footprint")), len(board.Footprints)+len(board.MountHoles)+len(board.Holes); got != want {
					t.Fatalf("footprints = %d, want %d", got, want)
				}
				if got, want := len(root.find("segment")), len(board.Traces); got != want {
					t.Fatalf("segments = %d, want %d", got, want)
				}
				if got, want := len(root.find("via")), len(board.Vias); got != want {
					t.Fatalf("vias = %d, want %d", got, want)
				}
			})
		}
	}
}

// kicadCLI is the macOS install path. Fragua never shells out at runtime
// (VISION: pure Go, no external binaries); this is a test-only cross-check
// that a real KiCad accepts what we write, skipped when it is not installed.
const kicadCLI = "/Applications/KiCad/KiCad.app/Contents/MacOS/kicad-cli"

func kicadCLIPath() string {
	if p := os.Getenv("FRAGUA_KICAD_CLI"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if _, err := os.Stat(kicadCLI); err == nil {
		return kicadCLI
	}
	if p, err := exec.LookPath("kicad-cli"); err == nil {
		return p
	}
	return ""
}

func TestKiCadCLILoadsExport(t *testing.T) {
	cli := kicadCLIPath()
	if cli == "" {
		t.Skip("kicad-cli not installed; set FRAGUA_KICAD_CLI to run this check")
	}
	src := "../../bench/boards/rp2040-minimal.fragua"
	if _, err := os.Stat(src); err != nil {
		t.Skipf("%s missing", src)
	}
	p, err := core.LoadFromPath(src)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "board.kicad_pcb")
	if err := WriteFile(p.Board(), out, DefaultOptions()); err != nil {
		t.Fatal(err)
	}
	// Exporting gerbers forces a full load, layer resolve and plot: if KiCad
	// disagrees with anything we wrote, it fails here.
	gerbers := filepath.Join(dir, "gerbers")
	if err := os.MkdirAll(gerbers, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(cli, "pcb", "export", "gerbers", "-o", gerbers, out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("kicad-cli pcb export gerbers failed: %v\n%s", err, b)
	}
	// DRC is allowed to report violations (Fragua's rules are its own); it
	// must not fail to parse the board.
	rpt := filepath.Join(dir, "drc.rpt")
	cmd = exec.Command(cli, "pcb", "drc", "--exit-code-violations", "-o", rpt, out)
	b, err := cmd.CombinedOutput()
	if err != nil && !strings.Contains(string(b), "Found") {
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 5 {
			t.Fatalf("kicad-cli pcb drc failed to load the board: %v\n%s", err, b)
		}
	}
	t.Logf("kicad-cli drc output:\n%s", b)
}

// A quick sanity check on the fill rasteriser: the copper it claims must stay
// off every pad that is not on the pour's net.
func TestZoneFillClearsForeignPads(t *testing.T) {
	b := smallBoard()
	s, err := Export(b, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	root, err := parseSexpr(s)
	if err != nil {
		t.Fatal(err)
	}
	// R1 pad 1 (net SIG) is at world (4.225, 6) → KiCad (4.225, 6).
	const px, py = 4.225, 6.0
	const halfPad = 0.475
	for _, z := range root.find("zone") {
		if z.first("net_name").arg(0) != "GND" || z.first("layer").arg(0) != "F.Cu" {
			continue
		}
		for _, fp := range z.find("filled_polygon") {
			var xs, ys []float64
			for _, xy := range fp.first("pts").find("xy") {
				x, _ := strconv.ParseFloat(xy.arg(0), 64)
				y, _ := strconv.ParseFloat(xy.arg(1), 64)
				xs, ys = append(xs, x), append(ys, y)
			}
			minX, maxX := minMax(xs)
			minY, maxY := minMax(ys)
			if maxX > px-halfPad-pourClearanceMM && minX < px+halfPad+pourClearanceMM &&
				maxY > py-halfPad-pourClearanceMM && minY < py+halfPad+pourClearanceMM {
				t.Fatalf("GND fill rect [%g..%g]x[%g..%g] intrudes on the SIG pad clearance", minX, maxX, minY, maxY)
			}
		}
	}
}

// The fill must also clear foreign traces and vias, not just pads: a plane
// that swallowed a signal trace would be a short, and the export would be
// worse than useless.
func TestZoneFillClearsForeignTraceAndVia(t *testing.T) {
	b := smallBoard()
	_, root := export(t, b, DefaultOptions())
	var tr *core.Trace
	for i := range b.Traces {
		if b.Traces[i].Layer.IsTop() {
			tr = &b.Traces[i]
		}
	}
	if tr == nil {
		t.Fatal("no top-layer trace in the fixture")
	}
	flip := (&exporter{board: b, stack: b.StackupOrDefault()}).originY().ToMM()
	ax := [2]float64{tr.Start.X.ToMM(), flip - tr.Start.Y.ToMM()}
	bx := [2]float64{tr.End.X.ToMM(), flip - tr.End.Y.ToMM()}
	need := tr.Width.ToMM()/2 + pourClearanceMM
	via := b.Vias[0]
	vc := [2]float64{via.Position.X.ToMM(), flip - via.Position.Y.ToMM()}
	viaNeed := via.Diameter.ToMM()/2 + pourClearanceMM

	for _, z := range root.find("zone") {
		if z.first("net_name").arg(0) != "GND" || z.first("layer").arg(0) != "F.Cu" {
			continue
		}
		for _, fp := range z.find("filled_polygon") {
			for _, xy := range fp.first("pts").find("xy") {
				x, _ := strconv.ParseFloat(xy.arg(0), 64)
				y, _ := strconv.ParseFloat(xy.arg(1), 64)
				if d := segDist(x, y, ax, bx); d < need-1e-9 {
					t.Fatalf("fill corner (%g, %g) is %.4f mm from the SIG trace, need %.4f", x, y, d, need)
				}
				if d := math.Hypot(x-vc[0], y-vc[1]); d < viaNeed-1e-9 {
					t.Fatalf("fill corner (%g, %g) is %.4f mm from the SIG via, need %.4f", x, y, d, viaNeed)
				}
			}
		}
	}
}

func minMax(v []float64) (float64, float64) {
	lo, hi := v[0], v[0]
	for _, x := range v {
		if x < lo {
			lo = x
		}
		if x > hi {
			hi = x
		}
	}
	return lo, hi
}
