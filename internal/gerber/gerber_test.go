package gerber

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mentasystems/fragua/internal/core"
)

func TestFabPackFiles(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(40), core.FromMM(30)))
	b.Outline = &o
	n := "GND"
	b.AddFootprint(&core.Footprint{
		ID: core.NewID(), Reference: "R1", Value: "10k", Library: "R_0805",
		Position: core.NewPoint(core.FromMM(10), core.FromMM(15)),
		Layer:    core.LayerTop,
		Pads: []core.Pad{
			{Number: "1", Offset: core.NewPoint(core.FromMM(-1), 0), Size: [2]core.Length{core.FromMM(1), core.FromMM(1.2)}, Layer: core.LayerTop, Net: &n},
			{Number: "2", Offset: core.NewPoint(core.FromMM(1), 0), Size: [2]core.Length{core.FromMM(1), core.FromMM(1.2)}, Layer: core.LayerTop},
		},
	})
	dir := t.TempDir()
	paths, err := WriteFabPack(b, "demo", dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 15 {
		t.Fatalf("files: %d want 15", len(paths))
	}
	want := []string{
		"demo-F_Cu.gbr", "demo-B_Cu.gbr", "demo-F_Mask.gbr", "demo-B_Mask.gbr",
		"demo-F_SilkS.gbr", "demo-B_SilkS.gbr", "demo-F_Paste.gbr", "demo-B_Paste.gbr",
		"demo-Edge_Cuts.gbr",
		"demo-PTH.drl", "demo-NPTH.drl", "demo-bom.csv", "demo-pos.csv",
		"demo-netlist.txt", "README.txt",
	}
	for i, p := range paths {
		base := filepath.Base(p)
		if base != want[i] {
			t.Fatalf("file %d: got %s want %s", i, base, want[i])
		}
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(base, ".gbr") {
			body := string(data)
			if !strings.Contains(body, "%FSLAX46Y46*%") {
				t.Fatalf("%s missing %%FSLAX46Y46*%%", base)
			}
			if !strings.Contains(body, "%MOMM*%") {
				t.Fatalf("%s missing %%MOMM*%%", base)
			}
			if !strings.HasPrefix(body, "G04 Fragua ") {
				t.Fatalf("%s missing G04 Fragua header comment", base)
			}
			if !strings.Contains(body, "%TF.GenerationSoftware,Fragua,") {
				t.Fatalf("%s missing Fragua GenerationSoftware", base)
			}
			if !strings.HasSuffix(strings.TrimSpace(body), "M02*") {
				t.Fatalf("%s missing M02* footer", base)
			}
		}
	}
}

func TestCopperHasX2FileFunction(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(20), core.FromMM(20)))
	b.Outline = &o
	b.AddFootprint(&core.Footprint{
		ID: core.NewID(), Reference: "R1", Value: "1k", Library: "R_0603",
		Position: core.NewPoint(core.FromMM(5), core.FromMM(5)),
		Layer:    core.LayerTop,
		Pads: []core.Pad{
			{Number: "1", Offset: core.NewPoint(core.FromMM(-0.8), 0), Size: [2]core.Length{core.FromMM(0.9), core.FromMM(0.9)}, Layer: core.LayerTop},
			{Number: "2", Offset: core.NewPoint(core.FromMM(0.8), 0), Size: [2]core.Length{core.FromMM(0.9), core.FromMM(0.9)}, Layer: core.LayerTop},
		},
	})
	dir := t.TempDir()
	if _, err := WriteFabPack(b, "x2", dir); err != nil {
		t.Fatal(err)
	}
	fCu, err := os.ReadFile(filepath.Join(dir, "x2-F_Cu.gbr"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(fCu)
	if !strings.Contains(body, "%FSLAX46Y46*%") {
		t.Fatal("F_Cu missing format spec")
	}
	if !strings.Contains(body, "%TF.FileFunction,Copper,L1,Top*%") {
		t.Fatalf("F_Cu missing X2 FileFunction: %s", body[:min(200, len(body))])
	}
	if !strings.Contains(body, "%TF.FilePolarity,Positive*%") {
		t.Fatal("F_Cu missing FilePolarity")
	}
	if !strings.Contains(body, "%TF.GenerationSoftware,Fragua,") {
		t.Fatal("F_Cu missing Fragua GenerationSoftware")
	}
	if !strings.Contains(body, "%TF.Part,Single*%") {
		t.Fatal("F_Cu missing Part")
	}
	// Two pad flashes.
	if c := strings.Count(body, "D03*"); c != 2 {
		t.Fatalf("F_Cu flashes: %d want 2", c)
	}
	// Interned aperture once (not redeclared per pad).
	if c := strings.Count(body, "%ADD"); c < 1 {
		t.Fatal("expected at least one ADD aperture")
	}
}

func TestMaskExpandedClearance(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(40), core.FromMM(30)))
	b.Outline = &o
	b.AddFootprint(&core.Footprint{
		ID: core.NewID(), Reference: "R1", Value: "10k", Library: "R_0805",
		Position: core.NewPoint(core.FromMM(10), core.FromMM(15)),
		Layer:    core.LayerTop,
		Pads: []core.Pad{
			{Number: "1", Offset: core.NewPoint(core.FromMM(-1), 0), Size: [2]core.Length{core.FromMM(1), core.FromMM(1.2)}, Layer: core.LayerTop},
			{Number: "2", Offset: core.NewPoint(core.FromMM(1), 0), Size: [2]core.Length{core.FromMM(1), core.FromMM(1.2)}, Layer: core.LayerTop},
		},
	})
	dir := t.TempDir()
	if _, err := WriteFabPack(b, "mask", dir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "mask-F_Mask.gbr"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	// 1.0 + 2*0.05 = 1.10; 1.2 + 2*0.05 = 1.30
	if !strings.Contains(body, "R,1.100000X1.300000") {
		t.Fatalf("expected expanded mask aperture, got:\n%s", body)
	}
	if strings.Count(body, "D03*") != 2 {
		t.Fatalf("mask flashes: %d", strings.Count(body, "D03*"))
	}
}

func TestBOMAndPosColumns(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(40), core.FromMM(30)))
	b.Outline = &o
	for _, fp := range []struct {
		ref, val string
		x        float64
	}{
		{"R1", "10k", 10},
		{"R2", "10k", 16},
		{"R3", "1k", 22},
	} {
		b.AddFootprint(&core.Footprint{
			ID: core.NewID(), Reference: fp.ref, Value: fp.val, Library: "Resistor_SMD:R_0805",
			Position: core.NewPoint(core.FromMM(fp.x), core.FromMM(15)),
			Layer:    core.LayerTop,
			Pads: []core.Pad{
				{Number: "1", Offset: core.NewPoint(core.FromMM(-1), 0), Size: [2]core.Length{core.FromMM(1), core.FromMM(1.2)}, Layer: core.LayerTop},
				{Number: "2", Offset: core.NewPoint(core.FromMM(1), 0), Size: [2]core.Length{core.FromMM(1), core.FromMM(1.2)}, Layer: core.LayerTop},
			},
		})
	}
	dir := t.TempDir()
	if _, err := WriteFabPack(b, "csv", dir); err != nil {
		t.Fatal(err)
	}
	bom, _ := os.ReadFile(filepath.Join(dir, "csv-bom.csv"))
	lines := strings.Split(strings.TrimSpace(string(bom)), "\n")
	if lines[0] != bomHeader {
		t.Fatalf("bom header: %s", lines[0])
	}
	if !strings.Contains(lines[0], "LCSC Part #") {
		t.Fatal("BOM must include LCSC Part # column")
	}
	if len(lines) != 3 {
		t.Fatalf("bom lines: %d want 3", len(lines))
	}
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "library:") {
		t.Fatalf("BOM must not emit library: prefix:\n%s", joined)
	}
	if !strings.Contains(joined, "R1,R2") || !strings.Contains(joined, ",2") {
		t.Fatalf("expected R1,R2 group qty 2:\n%s", joined)
	}
	if !strings.Contains(joined, "R_0805_2012Metric") {
		t.Fatalf("expected IPC-ish footprint:\n%s", joined)
	}

	pos, _ := os.ReadFile(filepath.Join(dir, "csv-pos.csv"))
	plines := strings.Split(strings.TrimSpace(string(pos)), "\n")
	if plines[0] != posHeader {
		t.Fatalf("pos header: %s", plines[0])
	}
	if len(plines) != 4 {
		t.Fatalf("pos lines: %d want 4", len(plines))
	}
	if !strings.HasPrefix(plines[1], "R1,10k,R_0805_2012Metric,10.0000,15.0000,0.00,top") {
		t.Fatalf("pos row: %s", plines[1])
	}
}

func TestExcellonPTHAndNPTH(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(40), core.FromMM(30)))
	b.Outline = &o
	drill := core.FromMM(0.8)
	b.AddFootprint(&core.Footprint{
		ID: core.NewID(), Reference: "J1", Value: "HDR", Library: "Connector",
		Position: core.NewPoint(core.FromMM(10), core.FromMM(10)),
		Layer:    core.LayerTop,
		Pads: []core.Pad{
			{Number: "1", Offset: core.Origin, Size: [2]core.Length{core.FromMM(1.6), core.FromMM(1.6)}, Layer: core.LayerTop, Drill: &drill},
		},
	})
	b.Vias = []core.Via{{
		ID: core.NewID(), Position: core.NewPoint(core.FromMM(20), core.FromMM(20)),
		Drill: core.FromMM(0.3), Diameter: core.FromMM(0.6), Net: "GND",
	}}
	b.MountHoles = []core.MountHole{{
		ID: core.NewID(), Center: core.NewPoint(core.FromMM(5), core.FromMM(5)),
		Diameter: core.FromMM(3.2),
	}}
	dir := t.TempDir()
	if _, err := WriteFabPack(b, "drill", dir); err != nil {
		t.Fatal(err)
	}
	pth, _ := os.ReadFile(filepath.Join(dir, "drill-PTH.drl"))
	pthBody := string(pth)
	for _, want := range []string{"M48", "FMAT,2", "METRIC,LZ,000.000", "G90", "M30"} {
		if !strings.Contains(pthBody, want) {
			t.Fatalf("PTH missing %q", want)
		}
	}
	if !strings.Contains(pthBody, "T1C") {
		t.Fatalf("PTH missing tool def:\n%s", pthBody)
	}
	// Both via and pad drill present.
	if !strings.Contains(pthBody, "X20.000Y20.000") {
		t.Fatalf("via hole missing:\n%s", pthBody)
	}
	if !strings.Contains(pthBody, "X10.000Y10.000") {
		t.Fatalf("pad drill missing:\n%s", pthBody)
	}

	npth, _ := os.ReadFile(filepath.Join(dir, "drill-NPTH.drl"))
	npthBody := string(npth)
	if !strings.Contains(npthBody, "T1C3.200") {
		t.Fatalf("NPTH tool:\n%s", npthBody)
	}
	if !strings.Contains(npthBody, "X5.000Y5.000") {
		t.Fatalf("mount hole missing:\n%s", npthBody)
	}
}

func TestMultiLayerCopperFilenames(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(40), core.FromMM(30)))
	b.Outline = &o
	b.Stackup = &core.LayerStackup{
		Layers: []core.LayerSpec{
			{Name: "F.Cu", Kind: core.LayerKindSignal},
			{Name: "In1.Cu", Kind: core.LayerKindPower},
			{Name: "In2.Cu", Kind: core.LayerKindPower},
			{Name: "B.Cu", Kind: core.LayerKindSignal},
		},
		Dielectrics: []core.Dielectric{{ThicknessMM: 0.5}, {ThicknessMM: 0.5}, {ThicknessMM: 0.5}},
	}
	dir := t.TempDir()
	paths, err := WriteFabPack(b, "demo4", dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(paths))
	for i, p := range paths {
		names[i] = filepath.Base(p)
	}
	for _, want := range []string{"demo4-F_Cu.gbr", "demo4-In1_Cu.gbr", "demo4-In2_Cu.gbr", "demo4-B_Cu.gbr"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing %s in %v", want, names)
		}
	}
	// Copper files first, in stackup order.
	if names[0] != "demo4-F_Cu.gbr" || names[3] != "demo4-B_Cu.gbr" {
		t.Fatalf("copper order: %v", names[:4])
	}
}

func TestPTHPadOnEveryCopperLayer(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(40), core.FromMM(30)))
	b.Outline = &o
	b.Stackup = &core.LayerStackup{
		Layers: []core.LayerSpec{
			{Name: "F.Cu", Kind: core.LayerKindSignal},
			{Name: "In1.Cu", Kind: core.LayerKindPower},
			{Name: "In2.Cu", Kind: core.LayerKindPower},
			{Name: "B.Cu", Kind: core.LayerKindSignal},
		},
	}
	drill := core.FromMM(0.8)
	b.AddFootprint(&core.Footprint{
		ID: core.NewID(), Reference: "U1", Value: "CONN", Library: "test",
		Position: core.NewPoint(core.FromMM(20), core.FromMM(15)),
		Layer:    core.LayerTop,
		Pads: []core.Pad{
			{Number: "1", Offset: core.Origin, Size: [2]core.Length{core.FromMM(1.6), core.FromMM(1.6)}, Layer: core.LayerTop, Drill: &drill},
		},
	})
	dir := t.TempDir()
	if _, err := WriteFabPack(b, "pth", dir); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pth-F_Cu.gbr", "pth-In1_Cu.gbr", "pth-In2_Cu.gbr", "pth-B_Cu.gbr"} {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), "D03*") {
			t.Fatalf("%s: PTH pad missing copper flash", name)
		}
	}
	for _, name := range []string{"pth-F_Mask.gbr", "pth-B_Mask.gbr"} {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), "D03*") {
			t.Fatalf("%s: PTH pad missing mask opening", name)
		}
	}
}

func TestEdgeCutsOutline(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(40), core.FromMM(30)))
	b.Outline = &o
	dir := t.TempDir()
	if _, err := WriteFabPack(b, "edge", dir); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "edge-Edge_Cuts.gbr"))
	body := string(data)
	if strings.Count(body, "D01*") != 4 {
		t.Fatalf("edge D01 count: %d want 4", strings.Count(body, "D01*"))
	}
	if !strings.Contains(body, "%TF.FileFunction,Profile,NP*%") {
		t.Fatal("edge missing Profile FileFunction")
	}
}

func TestTracesAndViasOnCopper(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(50), core.FromMM(50)))
	b.Outline = &o
	b.Traces = []core.Trace{{
		ID: core.NewID(), Layer: core.LayerTop,
		Start: core.NewPoint(core.FromMM(5), core.FromMM(5)),
		End:   core.NewPoint(core.FromMM(15), core.FromMM(5)),
		Width: core.FromMM(0.25), Net: "N1",
	}}
	b.Vias = []core.Via{{
		ID: core.NewID(), Position: core.NewPoint(core.FromMM(15), core.FromMM(5)),
		Drill: core.FromMM(0.3), Diameter: core.FromMM(0.6), Net: "N1",
	}}
	dir := t.TempDir()
	if _, err := WriteFabPack(b, "route", dir); err != nil {
		t.Fatal(err)
	}
	fCu, _ := os.ReadFile(filepath.Join(dir, "route-F_Cu.gbr"))
	body := string(fCu)
	if !strings.Contains(body, "C,0.250000") {
		t.Fatalf("trace aperture missing:\n%s", body)
	}
	if !strings.Contains(body, "C,0.600000") {
		t.Fatalf("via aperture missing:\n%s", body)
	}
	if !strings.Contains(body, "D01*") {
		t.Fatal("trace draw missing")
	}
	// Via flash on both layers.
	bCu, _ := os.ReadFile(filepath.Join(dir, "route-B_Cu.gbr"))
	if !strings.Contains(string(bCu), "D03*") {
		t.Fatal("via should flash on B_Cu")
	}
}

func TestPasteHasFraguaAndFileFunction(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(20), core.FromMM(20)))
	b.Outline = &o
	drill := core.FromMM(0.8)
	b.AddFootprint(&core.Footprint{
		ID: core.NewID(), Reference: "R1", Value: "10k", Library: "library:r_0603",
		Position: core.NewPoint(core.FromMM(5), core.FromMM(5)),
		Layer:    core.LayerTop,
		Pads: []core.Pad{
			{Number: "1", Offset: core.NewPoint(core.FromMM(-0.8), 0), Size: [2]core.Length{core.FromMM(0.8), core.FromMM(0.9)}, Layer: core.LayerTop},
			{Number: "2", Offset: core.NewPoint(core.FromMM(0.8), 0), Size: [2]core.Length{core.FromMM(0.8), core.FromMM(0.9)}, Layer: core.LayerTop},
		},
	})
	b.AddFootprint(&core.Footprint{
		ID: core.NewID(), Reference: "J1", Value: "HDR", Library: "Connector",
		Position: core.NewPoint(core.FromMM(12), core.FromMM(12)),
		Layer:    core.LayerTop,
		Pads: []core.Pad{
			{Number: "1", Offset: core.Origin, Size: [2]core.Length{core.FromMM(1.6), core.FromMM(1.6)}, Layer: core.LayerTop, Drill: &drill},
		},
	})
	dir := t.TempDir()
	if _, err := WriteFabPack(b, "paste", dir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "paste-F_Paste.gbr"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	if !strings.Contains(body, "%TF.GenerationSoftware,Fragua,") {
		t.Fatal("paste missing Fragua GenerationSoftware")
	}
	if !strings.Contains(body, "%TF.FileFunction,Paste,Top*%") {
		t.Fatalf("paste missing FileFunction: %s", body[:min(240, len(body))])
	}
	if c := strings.Count(body, "D03*"); c != 2 {
		t.Fatalf("paste flashes: %d want 2 (SMD only, no PTH)", c)
	}
	readme, err := os.ReadFile(filepath.Join(dir, "README.txt"))
	if err != nil {
		t.Fatal(err)
	}
	rs := string(readme)
	for _, want := range []string{"Fragua", "F_Paste", "Top solder paste", "layer map", "millimetres", "counterclockwise"} {
		if !strings.Contains(strings.ToLower(rs), strings.ToLower(want)) && !strings.Contains(rs, want) {
			t.Fatalf("README missing %q:\n%s", want, rs)
		}
	}
}

func TestBOMHasLCSCColumnNoLibraryPrefix(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(20), core.FromMM(20)))
	b.Outline = &o
	b.AddFootprint(&core.Footprint{
		ID: core.NewID(), Reference: "C1", Value: "100n", Library: "library:c_0603",
		LcscID: "C14663", Position: core.NewPoint(core.FromMM(4), core.FromMM(4)),
		Layer: core.LayerTop,
		Pads: []core.Pad{
			{Number: "1", Offset: core.NewPoint(core.FromMM(-0.8), 0), Size: [2]core.Length{core.FromMM(0.8), core.FromMM(0.9)}, Layer: core.LayerTop},
		},
	})
	b.AddFootprint(&core.Footprint{
		ID: core.NewID(), Reference: "C2", Value: "100n", Library: "library:c_0603",
		Position: core.NewPoint(core.FromMM(8), core.FromMM(4)),
		Layer: core.LayerTop,
		Pads: []core.Pad{
			{Number: "1", Offset: core.NewPoint(core.FromMM(-0.8), 0), Size: [2]core.Length{core.FromMM(0.8), core.FromMM(0.9)}, Layer: core.LayerTop},
		},
	})
	dir := t.TempDir()
	if _, err := WriteFabPack(b, "lcsc", dir); err != nil {
		t.Fatal(err)
	}
	bom, _ := os.ReadFile(filepath.Join(dir, "lcsc-bom.csv"))
	text := string(bom)
	if !strings.Contains(text, "LCSC Part #") {
		t.Fatalf("missing LCSC column:\n%s", text)
	}
	if strings.Contains(text, "library:") {
		t.Fatalf("library: prefix leaked:\n%s", text)
	}
	if !strings.Contains(text, "C_0603_1608Metric") {
		t.Fatalf("expected IPC footprint:\n%s", text)
	}
	if !strings.Contains(text, "C14663") {
		t.Fatalf("LCSC number dropped:\n%s", text)
	}
	// C2 has no LCSC — still a row, empty cell, not invented.
	if strings.Count(text, "C25804") != 0 {
		t.Fatal("invented an LCSC number")
	}
}

func TestInnerCopperFileFunction(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(20), core.FromMM(20)))
	b.Outline = &o
	s := core.Default4Layer()
	b.Stackup = &s
	dir := t.TempDir()
	if _, err := WriteFabPack(b, "in", dir); err != nil {
		t.Fatal(err)
	}
	in1, _ := os.ReadFile(filepath.Join(dir, "in-In1_Cu.gbr"))
	if !strings.Contains(string(in1), "%TF.FileFunction,Copper,L2,Inr*%") {
		t.Fatalf("In1 FileFunction:\n%s", in1[:min(300, len(in1))])
	}
	in2, _ := os.ReadFile(filepath.Join(dir, "in-In2_Cu.gbr"))
	if !strings.Contains(string(in2), "%TF.FileFunction,Copper,L3,Inr*%") {
		t.Fatalf("In2 FileFunction:\n%s", in2[:min(300, len(in2))])
	}
	bCu, _ := os.ReadFile(filepath.Join(dir, "in-B_Cu.gbr"))
	if !strings.Contains(string(bCu), "%TF.FileFunction,Copper,L4,Bot*%") {
		t.Fatalf("B.Cu 4L FileFunction:\n%s", bCu[:min(300, len(bCu))])
	}
}

func TestNetlistAndFiducial(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(20), core.FromMM(20)))
	b.Outline = &o
	n := "GND"
	b.AddFootprint(&core.Footprint{
		ID: core.NewID(), Reference: "R1", Value: "10k", Library: "r_0603",
		Position: core.NewPoint(core.FromMM(5), core.FromMM(5)),
		Layer:    core.LayerTop,
		Pads: []core.Pad{
			{Number: "1", Offset: core.NewPoint(core.FromMM(-0.8), 0), Size: [2]core.Length{core.FromMM(0.8), core.FromMM(0.9)}, Layer: core.LayerTop, Net: &n},
		},
	})
	b.AddFootprint(&core.Footprint{
		ID: core.NewID(), Reference: "FID1", Value: "FIDUCIAL", Library: "fiducial",
		Fiducial: true, Position: core.NewPoint(core.FromMM(2), core.FromMM(2)),
		Layer: core.LayerTop,
		Pads: []core.Pad{
			{Number: "1", Size: [2]core.Length{core.FromMM(1), core.FromMM(1)}, Layer: core.LayerTop},
		},
	})
	dir := t.TempDir()
	if _, err := WriteFabPack(b, "nl", dir); err != nil {
		t.Fatal(err)
	}
	nl, err := os.ReadFile(filepath.Join(dir, "nl-netlist.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(nl), "GND: R1.1") {
		t.Fatalf("netlist:\n%s", nl)
	}
	bom, _ := os.ReadFile(filepath.Join(dir, "nl-bom.csv"))
	if strings.Contains(string(bom), "FID1") {
		t.Fatalf("fiducial must not appear in BOM:\n%s", bom)
	}
	pos, _ := os.ReadFile(filepath.Join(dir, "nl-pos.csv"))
	if !strings.Contains(string(pos), "FID1") {
		t.Fatalf("fiducial should be in CPL:\n%s", pos)
	}
}
