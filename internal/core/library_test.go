package core

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestLibraryPutGetListRoundtrip(t *testing.T) {
	dir := t.TempDir()
	lib, err := OpenAt(dir)
	if err != nil {
		t.Fatal(err)
	}

	lcsc := "C25804"
	mpn := "RC0603FR-0710KL"
	side := EdgeTop
	body := &BodyRect{MinXMM: -1.5, MinYMM: -0.8, MaxXMM: 1.5, MaxYMM: 0.8}
	in := LibraryEntry{
		Key:          "r_0603",
		Description:  "0603 chip resistor",
		DefaultValue: "10k",
		EdgeMounted:  false,
		EdgeSide:     &side,
		Elevated:     false,
		Pads: []LibraryPad{
			{Number: "1", Name: "", XMM: -0.75, YMM: 0, WMM: 0.4, HMM: 0.5},
			{Number: "2", Name: "", XMM: 0.75, YMM: 0, WMM: 0.4, HMM: 0.5},
		},
		Silk: []LibrarySilk{
			{Kind: "line", Layer: SilkTop, X1MM: -0.9, Y1MM: -0.4, X2MM: 0.9, Y2MM: -0.4, WidthMM: 0.1},
		},
		LcscID:   &lcsc,
		MPN:      &mpn,
		BodyRect: body,
	}

	stored, err := lib.Put(in)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Key != "r_0603" {
		t.Fatalf("key: %q", stored.Key)
	}
	if stored.CreatedAt == 0 {
		t.Fatal("created_at not set")
	}
	// Body + silk should inflate placement margin.
	if stored.PlacementMargin.IsZero() {
		t.Fatalf("expected non-zero placement margin from body/silk: %+v", stored.PlacementMargin)
	}

	got, ok := lib.Get("r_0603")
	if !ok {
		t.Fatal("Get missing after Put")
	}
	if got.Description != in.Description {
		t.Fatalf("description: got %q want %q", got.Description, in.Description)
	}
	if len(got.Pads) != 2 {
		t.Fatalf("pads: %d", len(got.Pads))
	}
	if got.LcscID == nil || *got.LcscID != lcsc {
		t.Fatalf("lcsc: %+v", got.LcscID)
	}
	if got.MPN == nil || *got.MPN != mpn {
		t.Fatalf("mpn: %+v", got.MPN)
	}
	if got.BodyRect == nil {
		t.Fatal("body_rect missing")
	}

	// Second entry so List is multi-key and sorted.
	if _, err := lib.Put(LibraryEntry{
		Key:  "c_0603",
		Pads: []LibraryPad{{Number: "1", XMM: 0, YMM: 0, WMM: 0.8, HMM: 0.8}},
	}); err != nil {
		t.Fatal(err)
	}

	keys := lib.List()
	if len(keys) != 2 {
		t.Fatalf("List: got %v", keys)
	}
	if keys[0] != "c_0603" || keys[1] != "r_0603" {
		t.Fatalf("List not sorted: %v", keys)
	}

	// Re-open from disk.
	lib2, err := OpenAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	keys2 := lib2.List()
	if len(keys2) != 2 || keys2[0] != "c_0603" {
		t.Fatalf("reopen List: %v", keys2)
	}
	got2, ok := lib2.Get("r_0603")
	if !ok || len(got2.Pads) != 2 {
		t.Fatalf("reopen Get: ok=%v pads=%d", ok, len(got2.Pads))
	}
	// index.json exists
	if _, err := os.Stat(filepath.Join(dir, "index.json")); err != nil {
		t.Fatal(err)
	}
}

func TestLibraryPutPreservesAttachments(t *testing.T) {
	dir := t.TempDir()
	lib, err := OpenAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = lib.Put(LibraryEntry{
		Key:  "mod",
		Pads: []LibraryPad{{Number: "1", XMM: 0, YMM: 0, WMM: 1, HMM: 1}},
		Attachments: []Attachment{
			{ID: "a1", Kind: "photo", Filename: "x.jpg", Mime: "image/jpeg", AddedAt: 1},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Re-put without attachments → preserve.
	out, err := lib.Put(LibraryEntry{
		Key:         "mod",
		Description: "updated",
		Pads:        []LibraryPad{{Number: "1", XMM: 0, YMM: 0, WMM: 1, HMM: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Description != "updated" {
		t.Fatal(out.Description)
	}
	if len(out.Attachments) != 1 || out.Attachments[0].ID != "a1" {
		t.Fatalf("attachments not preserved: %+v", out.Attachments)
	}
}

func TestLibraryElevatedBodyKeepout(t *testing.T) {
	e := LibraryEntry{
		Key:      "oled",
		Elevated: true,
		Pads: []LibraryPad{
			{Number: "1", XMM: -2, YMM: 0, WMM: 1, HMM: 1},
			{Number: "2", XMM: 2, YMM: 0, WMM: 1, HMM: 1},
		},
	}
	k := e.BodyKeepout()
	if !k.Elevated {
		t.Fatal("elevated flag missing from body keepout")
	}
	low := PlacementMargin{}
	if !k.ClearsOver(low) {
		t.Fatal("elevated should clear over non-elevated")
	}
	if k.ClearsOver(PlacementMargin{Elevated: true}) {
		t.Fatal("two elevated bodies still collide")
	}
}

func TestLibraryEmptyKeyRejected(t *testing.T) {
	lib, err := OpenAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lib.Put(LibraryEntry{}); err == nil {
		t.Fatal("expected empty key error")
	}
}

func TestProjectOpensLibrary(t *testing.T) {
	p := NewProject("demo")
	if p.Library() == nil {
		t.Fatal("nil library")
	}
	// Isolate from real ~/.pcb-library for this assertion.
	lib, err := OpenAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p.SetLibrary(lib)
	if _, err := p.Library().Put(LibraryEntry{
		Key:  "x",
		Pads: []LibraryPad{{Number: "1", XMM: 0, YMM: 0, WMM: 1, HMM: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	keys := p.Library().List()
	if len(keys) != 1 || keys[0] != "x" {
		t.Fatalf("keys: %v", keys)
	}
}

// A KF301 screw terminal's 4 mm wire mouth exists in the authored
// placement_margin and nowhere else — no pad, no silk line, no body_rect.
// BodyKeepout used to re-derive the margin from geometry alone and throw it
// away, which let edge-place seat the block with its mouth off the board.
func TestBodyKeepoutKeepsAnAuthoredMargin(t *testing.T) {
	drill := 1.2
	e := LibraryEntry{
		Key: "screw_term_2p",
		Pads: []LibraryPad{
			{Number: "1", XMM: -2.5, YMM: 0, WMM: 2, HMM: 2, DrillMM: &drill},
			{Number: "2", XMM: 2.5, YMM: 0, WMM: 2, HMM: 2, DrillMM: &drill},
		},
		Silk: []LibrarySilk{
			{Kind: "text", XMM: 0, YMM: -4, SizeMM: 0.7, Text: "{REF}"},
		},
		PlacementMargin: PlacementMargin{TopMM: 4, RightMM: 1.5, BottomMM: 2.5, LeftMM: 1.5},
	}
	got := e.BodyKeepout()
	if got.TopMM != 4 {
		t.Errorf("top margin = %.2f, want the authored 4.00 (the wire mouth)", got.TopMM)
	}
	if got.LeftMM != 1.5 || got.RightMM != 1.5 {
		t.Errorf("side margins = %.2f/%.2f, want the authored 1.50", got.LeftMM, got.RightMM)
	}
	// The silk reference sits 3.35 mm below the pads, further than the
	// authored 2.5: the derivation still wins where it is the wider one.
	if math.Abs(got.BottomMM-3.35) > 1e-9 {
		t.Errorf("bottom margin = %.2f, want the derived silk hull 3.35", got.BottomMM)
	}
}
