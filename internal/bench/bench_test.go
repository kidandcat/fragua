package bench

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const tinyBoard = `# bench: place=auto
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
`

func TestRunDirScriptBoard(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tiny.txt"), []byte(tinyBoard), 0o644); err != nil {
		t.Fatal(err)
	}
	run, err := RunDir(dir, Options{Seed: 42, BudgetSeconds: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Results) != 1 {
		t.Fatalf("results = %d, want 1", len(run.Results))
	}
	r := run.Results[0]
	if r.Error != "" {
		t.Fatalf("board failed: %s", r.Error)
	}
	if !r.AutoPlaced {
		t.Fatal("a script board with no place verb must be auto-placed")
	}
	if r.Parts != 2 || r.Nets != 2 {
		t.Fatalf("parts=%d nets=%d, want 2 and 2", r.Parts, r.Nets)
	}
	if r.RoutedNets != 2 || r.DRCErrors != 0 {
		t.Fatalf("two resistors on a 40x30 board must route clean: routed=%d/%d drc=%d",
			r.RoutedNets, r.Nets, r.DRCErrors)
	}
	if run.Failures() != 0 || run.Unclean() != 0 {
		t.Fatalf("failures=%d unclean=%d", run.Failures(), run.Unclean())
	}
	if md := run.Markdown(); !strings.Contains(md, "| tiny |") || !strings.Contains(md, "auto") {
		t.Fatalf("markdown row missing:\n%s", md)
	}
	raw, err := run.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var back Run
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("bench json does not round-trip: %v", err)
	}
	if back.Meta.Seed != 42 {
		t.Fatalf("meta seed = %d, want 42", back.Meta.Seed)
	}
}

// A script that places its own parts is taken as pre-placed; the directive
// overrides either way.
func TestPlaceDetection(t *testing.T) {
	if scriptPlaces("outline 10 10\nnet A B.1 C.1\n") {
		t.Fatal("a script with no place verb must not count as pre-placed")
	}
	if scriptPlaces("outline 10 10\nplace R1 2 2\n") {
		t.Fatal("a plain place is only the drop, not a placement run")
	}
	if !scriptPlaces("outline 10 10\nauto-place seed=1\n") {
		t.Fatal("auto-place must count as already placed")
	}
	if scriptPlaces("lib x\n  auto-place\n") {
		t.Fatal("an indented line inside a block is not a verb")
	}
	if !placeDirective("# bench: place=auto\n", false) {
		t.Fatal("place=auto must force auto-place")
	}
	if placeDirective("# bench: place=manual\n", true) {
		t.Fatal("place=manual must skip auto-place")
	}
	if !placeDirective("# nothing here\n", true) {
		t.Fatal("no directive must keep the default")
	}
}

func TestDiscoverAcceptsSingleFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "one.txt")
	if err := os.WriteFile(p, []byte(tinyBoard), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := Discover(p)
	if err != nil || len(files) != 1 || files[0] != p {
		t.Fatalf("Discover(file) = %v, %v", files, err)
	}
	files, err = Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("Discover(dir) picked up non-board files: %v", files)
	}
}
