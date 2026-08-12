package core

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadStressBoard(t *testing.T) {
	// Repo root: two levels up from internal/core when tests run from package dir.
	candidates := []string{
		filepath.Join("..", "..", "stress", "rp2040-minimal.fragua"),
		filepath.Join("stress", "rp2040-minimal.fragua"),
	}
	var path string
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			path = c
			break
		}
	}
	if path == "" {
		t.Skip("stress board not found")
	}
	p, err := LoadFromPath(path)
	if err != nil {
		t.Fatal(err)
	}
	b := p.Board()
	if b == nil || b.Outline == nil {
		t.Fatal("missing outline")
	}
	if len(b.Footprints) != 36 {
		t.Fatalf("footprints: got %d want 36", len(b.Footprints))
	}
	if len(b.Traces) != 290 {
		t.Fatalf("traces: got %d want 290", len(b.Traces))
	}
	if len(b.Vias) != 80 {
		t.Fatalf("vias: got %d want 80", len(b.Vias))
	}
	sch := p.Schematic()
	if len(sch.Nets) != 39 {
		t.Fatalf("nets: got %d want 39", len(sch.Nets))
	}
	// Round-trip to temp
	dir := t.TempDir()
	out := filepath.Join(dir, "round.fragua")
	if err := p.SaveToPath(out); err != nil {
		t.Fatal(err)
	}
	p2, err := LoadFromPath(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(p2.Board().Footprints) != 36 {
		t.Fatal("round-trip footprint count mismatch")
	}
}
