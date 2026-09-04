package render

import (
	"strings"
	"testing"

	"github.com/mentasystems/fragua/internal/core"
)

// demoBoard is a two-part board with one routed net and one that is not,
// so the render carries copper, pads, a via and a ratsnest line.
func demoBoard() *core.Board {
	b := core.NewBoard()
	r := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(20), core.FromMM(12)))
	b.Outline = &r
	gnd, sig := "GND", "SIG"
	drill := core.FromMM(0.4)
	b.AddFootprint(&core.Footprint{
		Reference: "U1", Value: "esp32", Key: "esp32_s3_zero", LcscID: "C2913202",
		Position: core.NewPoint(core.FromMM(5), core.FromMM(6)),
		Pads: []core.Pad{
			{Number: "1", Name: "VCC", Size: [2]core.Length{core.FromMM(1), core.FromMM(1)}, Net: &sig},
			{Number: "2", Name: "GND", Offset: core.NewPoint(core.FromMM(2), 0), Size: [2]core.Length{core.FromMM(1), core.FromMM(1)}, Net: &gnd, Drill: &drill},
		},
	})
	b.AddFootprint(&core.Footprint{
		Reference: "R1", Value: "10k", Key: "r_0603",
		Position: core.NewPoint(core.FromMM(14), core.FromMM(6)),
		Pads: []core.Pad{
			{Number: "1", Size: [2]core.Length{core.FromMM(1), core.FromMM(1)}, Net: &sig},
			{Number: "2", Offset: core.NewPoint(core.FromMM(2), 0), Size: [2]core.Length{core.FromMM(1), core.FromMM(1)}, Net: &gnd},
		},
	})
	b.Traces = []core.Trace{{
		ID: core.NewID(), Net: "SIG", Layer: core.LayerTop, Width: core.FromMM(0.25),
		Start: core.NewPoint(core.FromMM(5), core.FromMM(6)), End: core.NewPoint(core.FromMM(14), core.FromMM(6)),
	}}
	b.Vias = []core.Via{{
		ID: core.NewID(), Net: "SIG", Position: core.NewPoint(core.FromMM(10), core.FromMM(6)),
		Drill: core.FromMM(0.3), Diameter: core.FromMM(0.6),
	}}
	return b
}

func TestBoardSVGLayerGroups(t *testing.T) {
	svg := BoardSVGWith(demoBoard(), Options{})
	for _, want := range []string{
		`data-layer="F.Cu"`, `data-layer="B.Cu"`, `data-layer="silk"`, `data-layer="edge"`,
		`data-layer="drills"`, `data-layer="pours"`, `data-layer="vias"`, `data-layer="mask"`,
		`data-layer="ratsnest"`, `data-layer="drc"`, `data-layer="pad-names"`, `data-layer="footprints"`,
	} {
		if !strings.Contains(svg, want) {
			t.Errorf("board SVG missing layer group %s", want)
		}
	}
	if !strings.Contains(svg, `data-copper-layers="2"`) {
		t.Error("board SVG should declare its copper layer count")
	}
}

func TestBoardSVGDataAttributes(t *testing.T) {
	svg := BoardSVGWith(demoBoard(), Options{})
	for _, want := range []string{
		`data-ref="U1"`, `data-ref="R1"`, `data-key="esp32_s3_zero"`,
		`data-pad="1"`, `data-pad-name="VCC"`, `data-through="1"`,
		`data-net="SIG"`, `data-net="GND"`, `data-id="`,
	} {
		if !strings.Contains(svg, want) {
			t.Errorf("board SVG missing attribute %s", want)
		}
	}
	// A trace carries both its id and its net so the UI can highlight either.
	if !strings.Contains(svg, `<line data-id=`) {
		t.Error("traces must carry data-id")
	}
	if !strings.Contains(svg, `<circle data-id=`) {
		t.Error("vias must carry data-id")
	}
}

func TestBoardSVGMarkers(t *testing.T) {
	svg := BoardSVGWith(demoBoard(), Options{Markers: []Marker{
		{ID: "d0", Severity: "error", Kind: "trace_pad_clearance", Net: "SIG", XMM: 8, YMM: 6},
		{ID: "d1", Severity: "warning", Kind: "unconnected_pad"}, // no location: skipped
	}})
	if !strings.Contains(svg, `data-marker="d0"`) {
		t.Error("located marker should be drawn")
	}
	if strings.Contains(svg, `data-marker="d1"`) {
		t.Error("marker without a location must not be drawn at the origin")
	}
}

func TestRatsnestOnlyUnroutedNets(t *testing.T) {
	b := demoBoard()
	rs := Ratsnest(b)
	if len(rs) != 1 {
		t.Fatalf("expected one ratsnest line (GND), got %d", len(rs))
	}
	if rs[0].Net != "GND" {
		t.Fatalf("ratsnest should skip the routed net, got %q", rs[0].Net)
	}
	// A pour counts as the connection.
	b.Pours = append(b.Pours, core.Pour{Net: "GND", Layer: core.LayerBottom})
	if got := Ratsnest(b); len(got) != 0 {
		t.Fatalf("poured net should have no ratsnest, got %d lines", len(got))
	}
}

func TestBoardSVGDeterministic(t *testing.T) {
	b := demoBoard()
	if BoardSVG(b) != BoardSVG(b) {
		t.Fatal("board SVG must be byte-stable for the same board")
	}
}

func schematicFixture() *core.Schematic {
	s := core.NewSchematic()
	u := &core.Symbol{ID: core.NewID(), Reference: "U1", Value: "ESP32", Kind: core.SymbolKind{
		Kind: "generic_ic",
		ICPins: []core.SchPin{
			{Number: "1", Name: "3V3", Side: core.PinLeft, Role: core.PinPowerIn},
			{Number: "2", Name: "GND", Side: core.PinLeft, Role: core.PinPowerIn},
			{Number: "3", Name: "IO1", Side: core.PinRight, Role: core.PinBidir},
			{Number: "4", Name: "IO2", Side: core.PinRight, Role: core.PinNC, NC: true},
		},
	}}
	r := &core.Symbol{ID: core.NewID(), Reference: "R1", Value: "10k", Kind: core.SymbolKind{Kind: "resistor"}}
	c := &core.Symbol{ID: core.NewID(), Reference: "C1", Value: "100nF", Kind: core.SymbolKind{Kind: "capacitor"}}
	for _, sym := range []*core.Symbol{u, r, c} {
		s.Symbols[sym.ID.String()] = sym
		s.SymbolOrder = append(s.SymbolOrder, sym.ID.String())
	}
	s.Nets["GND"] = &core.Net{Name: "GND", Connections: []core.NetConnection{
		{SymbolID: u.ID, PinNumber: "2"}, {SymbolID: c.ID, PinNumber: "2"},
	}}
	s.Nets["SIG"] = &core.Net{Name: "SIG", Connections: []core.NetConnection{
		{SymbolID: u.ID, PinNumber: "3"}, {SymbolID: r.ID, PinNumber: "1"},
	}}
	return s
}

func TestSchematicSVG(t *testing.T) {
	svg := SchematicSVG(schematicFixture())
	for _, want := range []string{
		`data-sym="U1"`, `data-sym="R1"`, `data-sym="C1"`,
		`data-pin="1"`, `data-pin-name="IO1"`, `data-net="GND"`, `data-net="SIG"`,
		`data-kind="generic_ic"`, `data-kind="capacitor"`,
	} {
		if !strings.Contains(svg, want) {
			t.Errorf("schematic SVG missing %s", want)
		}
	}
	// An unconnected pin is flagged, an NC pin is not an error.
	if !strings.Contains(svg, ">NC<") {
		t.Error("nc pin should carry an NC flag")
	}
	if !strings.Contains(svg, ">?<") {
		t.Error("unconnected pin should be flagged")
	}
	if SchematicSVG(schematicFixture()) != svg {
		t.Error("schematic SVG must be deterministic")
	}
}

func TestSchematicSVGEmpty(t *testing.T) {
	svg := SchematicSVG(core.NewSchematic())
	if !strings.Contains(svg, "empty schematic") || !strings.Contains(svg, "viewBox=") {
		t.Fatalf("empty schematic should render a labelled sheet: %s", clip(svg, 200))
	}
	if SchematicSVG(nil) == "" {
		t.Fatal("nil schematic should still render")
	}
}

func TestRefLessNatural(t *testing.T) {
	if !refLess("R2", "R10") {
		t.Error("R2 must sort before R10")
	}
	if refLess("U1", "C1") {
		t.Error("C1 sorts before U1 within a group")
	}
}
