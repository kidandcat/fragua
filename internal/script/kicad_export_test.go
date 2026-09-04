package script

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mentasystems/fragua/internal/core"
)

func kicadProject(t *testing.T) *core.Project {
	t.Helper()
	p := core.NewProject("kicad-demo")
	res := RunScript(p, strings.Join([]string{
		"outline 20 12",
		"lib demo_r",
		"  pad 1 -0.775 0 0.95 0.95",
		"  pad 2 0.775 0 0.95 0.95",
		"sym R1 resistor key=demo_r value=10k",
		"sym R2 resistor key=demo_r value=10k",
		"palette R1 demo_r",
		"palette R2 demo_r",
		"place R1 5 6",
		"place R2 14 6",
		"net SIG R1.1 R2.1",
		"net GND R1.2 R2.2",
		"pour GND layer=Bottom",
	}, "\n"))
	for _, r := range res {
		if !r.OK {
			t.Fatalf("setup line %d %s: %s", r.Line, r.Tool, r.Result)
		}
	}
	return p
}

// `pack fab=kicad` must route around the fab-profile path entirely: KiCad is
// not a fab house, and the export must not fail on an unknown profile.
func TestPackKiCadWritesFile(t *testing.T) {
	p := kicadProject(t)
	out := filepath.Join(t.TempDir(), "board.kicad_pcb")
	msg, err := dispatch(p, "pack", "fab=kicad out="+out)
	if err != nil {
		t.Fatalf("pack fab=kicad: %v", err)
	}
	if !strings.Contains(msg, "KiCad") {
		t.Fatalf("unexpected message %q", msg)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, want := range []string{"(kicad_pcb", "(version 20241229)", `(generator "fragua")`, "(footprint ", "Edge.Cuts"} {
		if !strings.Contains(body, want) {
			t.Fatalf("exported board is missing %q", want)
		}
	}
}

// out= a directory names the file after the project.
func TestPackKiCadIntoDirectory(t *testing.T) {
	p := kicadProject(t)
	dir := t.TempDir()
	if _, err := dispatch(p, "pack", "fab=kicad out="+dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "kicad-demo.kicad_pcb")); err != nil {
		t.Fatalf("expected kicad-demo.kicad_pcb in %s: %v", dir, err)
	}
}

func TestIsKiCadProvider(t *testing.T) {
	for _, ok := range []string{"kicad", "KiCad", "kicad_pcb"} {
		if !IsKiCadProvider(ok) {
			t.Fatalf("%q should be the KiCad provider", ok)
		}
	}
	for _, no := range []string{"jlcpcb", "pcbway", "generic", ""} {
		if IsKiCadProvider(no) {
			t.Fatalf("%q must not be the KiCad provider", no)
		}
	}
}
