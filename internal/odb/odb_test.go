package odb

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/mentasystems/fragua/internal/core"
)

func profileOf(t *testing.T, b *core.Board) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "board.tgz")
	if err := Export(b, path); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			t.Fatal("no steps/pcb/profile in the archive")
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Name == "steps/pcb/profile" {
			raw, _ := io.ReadAll(tr)
			return string(raw)
		}
	}
}

// The profile was always the outline's bounding box, so a shaped board went
// to the fab as a rectangle.
func TestProfileFollowsPolygonOutline(t *testing.T) {
	mm := core.FromMM
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(mm(40), mm(30)))
	b.Outline = &o
	b.OutlinePoly = []core.Point{core.NewPoint(0, 0), core.NewPoint(mm(40), 0), core.NewPoint(mm(20), mm(30))}
	want := "OB 0.000000 0.000000\nOS 40.000000 0.000000\nOS 20.000000 30.000000\nOS 0.000000 0.000000\nOE\n"
	if got := profileOf(t, b); got != want {
		t.Fatalf("profile =\n%s\nwant\n%s", got, want)
	}
	b.OutlinePoly = nil
	if got := profileOf(t, b); got == want {
		t.Fatalf("rect board must keep the rectangular profile")
	}
}
