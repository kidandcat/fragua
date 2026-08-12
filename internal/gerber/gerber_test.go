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
	if len(paths) != 11 {
		t.Fatalf("files: %d want 11", len(paths))
	}
	want := []string{
		"demo-F_Cu.gbr", "demo-B_Cu.gbr", "demo-F_Mask.gbr", "demo-B_Mask.gbr",
		"demo-F_SilkS.gbr", "demo-B_SilkS.gbr", "demo-Edge_Cuts.gbr",
		"demo-PTH.drl", "demo-NPTH.drl", "demo-bom.csv", "demo-pos.csv",
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
			if !strings.HasPrefix(body, "G04 pcb ") {
				t.Fatalf("%s missing G04 header comment", base)
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
	if lines[0] != "Reference,Value,Footprint,Quantity" {
		t.Fatalf("bom header: %s", lines[0])
	}
	if len(lines) != 3 {
		t.Fatalf("bom lines: %d want 3", len(lines))
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "R1 R2") || !strings.Contains(joined, ",2") {
		t.Fatalf("expected R1 R2 group qty 2:\n%s", joined)
	}

	pos, _ := os.ReadFile(filepath.Join(dir, "csv-pos.csv"))
	plines := strings.Split(strings.TrimSpace(string(pos)), "\n")
	if plines[0] != "Reference,Value,Footprint,X,Y,Rotation,Side" {
		t.Fatalf("pos header: %s", plines[0])
	}
	if len(plines) != 4 {
		t.Fatalf("pos lines: %d want 4", len(plines))
	}
	if !strings.HasPrefix(plines[1], "R1,10k,Resistor_SMD:R_0805,10.0000,15.0000,0.00,top") {
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
