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
	// check names order
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
			if !strings.Contains(string(data), "%FSLAX") && !strings.Contains(string(data), "M02") {
				t.Fatalf("%s missing gerber markers", base)
			}
		}
	}
}
