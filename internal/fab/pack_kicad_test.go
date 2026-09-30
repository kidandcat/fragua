package fab

import (
	"archive/zip"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mentasystems/fragua/internal/core"
)

// Every fab profile's minimum via must satisfy that same profile's annular
// ring rule. A preset that cannot meet its own floor fails DRC on every via
// the router is allowed to place.
func TestProfileMinimumViaMeetsItsOwnAnnularRing(t *testing.T) {
	for _, name := range []string{"jlcpcb", "jlcpcb-2l-via02", "jlcpcb-4l", "jlcpcb-4l-via02", "pcbway", "generic"} {
		p, err := ProfileByName(name)
		if err != nil {
			t.Fatal(err)
		}
		ring := (p.MinViaDiameterMM - p.MinDrillMM) / 2
		if ring < p.MinAnnularRingMM-1e-9 {
			t.Fatalf("%s: min via %.3f/%.3f leaves %.4f mm of ring but the profile demands %.4f",
				name, p.MinDrillMM, p.MinViaDiameterMM, ring, p.MinAnnularRingMM)
		}
	}
}

// The pack ships the board as a KiCad file next to the gerbers.
func TestPackShipsKiCadBoard(t *testing.T) {
	p := core.NewProject("packdemo")
	b := p.Board()
	b.Outline = &core.Rect{Max: core.Point{X: core.FromMM(20), Y: core.FromMM(20)}}
	net := "GND"
	b.AddFootprint(&core.Footprint{
		Reference: "R1", Value: "10k", Key: "r_0603", Layer: core.LayerTop,
		Position: core.Point{X: core.FromMM(10), Y: core.FromMM(10)},
		Pads: []core.Pad{
			{Number: "1", Offset: core.Point{X: core.FromMM(-0.8)}, Size: [2]core.Length{core.FromMM(0.9), core.FromMM(0.9)}, Layer: core.LayerTop, Net: &net},
			{Number: "2", Offset: core.Point{X: core.FromMM(0.8)}, Size: [2]core.Length{core.FromMM(0.9), core.FromMM(0.9)}, Layer: core.LayerTop, Net: &net},
		},
	})
	dir := t.TempDir()
	res, err := Pack(p, "jlcpcb", dir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range res.Files {
		if filepath.Ext(f) == ".kicad_pcb" {
			found = true
		}
	}
	if !found {
		t.Fatalf("pack did not report a .kicad_pcb: %v", res.Files)
	}
	zr, err := zip.OpenReader(res.ZipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, ".kicad_pcb") {
			return
		}
	}
	t.Fatal("the fab zip has no .kicad_pcb")
}

// Component-clearance failures are ordinary DRC errors: pack still writes the
// zip (it only refuses on ERC) and the count includes them.
func TestPackReportsBodyClearance(t *testing.T) {
	p := core.NewProject("touching-modules")
	b := p.Board()
	o := core.Rect{Max: core.Point{X: core.FromMM(60), Y: core.FromMM(30)}}
	b.Outline = &o
	add := func(ref string, x float64, key string) {
		b.AddFootprint(&core.Footprint{
			ID: core.NewID(), Reference: ref, Key: key, Layer: core.LayerTop,
			Position: core.Point{X: core.FromMM(x), Y: core.FromMM(12)},
			BodyRect: &core.BodyRect{MinXMM: -8, MinYMM: -5, MaxXMM: 8, MaxYMM: 5},
			Pads: []core.Pad{
				{
					Number: "1",
					Offset: core.NewPoint(core.FromMM(-7.5), 0),
					Size:   [2]core.Length{core.FromMM(1), core.FromMM(1)},
					Layer:  core.LayerTop,
				},
				{
					Number: "2",
					Offset: core.NewPoint(core.FromMM(7.5), 0),
					Size:   [2]core.Length{core.FromMM(1), core.FromMM(1)},
					Layer:  core.LayerTop,
				},
			},
		})
	}
	// Outlines [4, 20] and [20.1, 36.1]: 0.10 mm apart, courtyards do not overlap.
	add("U1", 12, "esp32_s3_zero_top")
	add("U2", 28.1, "xl1262_lora")
	res, err := Pack(p, "jlcpcb", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if res.ERCErrors != 0 {
		t.Fatalf("erc errors %d; pack should only be refusing on those", res.ERCErrors)
	}
	if res.DRCErrors < 1 {
		t.Fatalf("drc_err=%d, want the body clearance counted", res.DRCErrors)
	}
	if res.ZipPath == "" {
		t.Fatal("pack dropped the zip")
	}
}
