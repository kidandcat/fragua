package router

import (
	"math"
	"testing"

	"github.com/mentasystems/fragua/internal/core"
	"github.com/mentasystems/fragua/internal/impedance"
)

// classBoard is a 4-layer board with three 2-pad nets: one impedance class,
// one plain-width class, one classless.
func classBoard(t *testing.T) (*core.Board, *core.Schematic) {
	t.Helper()
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(40), core.FromMM(20)))
	b.Outline = &o
	b.AddFootprint(footprint("R1", 10, 10, []core.Pad{
		pad("1", -1, 0, "USBDP"),
		pad("2", 1, 0, "PWRA"),
	}))
	b.AddFootprint(footprint("R2", 20, 10, []core.Pad{
		pad("1", -1, 0, "USBDP"),
		pad("2", 1, 0, "SIG1"),
	}))
	b.AddFootprint(footprint("R3", 30, 10, []core.Pad{
		pad("1", -1, 0, "PWRA"),
		pad("2", 1, 0, "SIG1"),
	}))
	b.Apply4Layer()

	sch := core.NewSchematic()
	sch.NetClasses["usb"] = &core.NetClass{Name: "usb", ImpedanceOhms: 45}
	sch.NetClasses["pwr"] = &core.NetClass{Name: "pwr", TraceWidthMM: 0.60}
	sch.NetToClass["USBDP"] = "usb"
	sch.NetToClass["PWRA"] = "pwr"
	return b, sch
}

// impedanceWidth is the closed-form width for target on copper layer.
func impedanceWidth(t *testing.T, b *core.Board, layer uint8, target float64) float64 {
	t.Helper()
	p, err := impedance.LineParams(b.StackupOrDefault(), int(layer))
	if err != nil {
		t.Fatalf("line params layer %d: %v", layer, err)
	}
	w, err := impedance.WidthForZ(p, target)
	if err != nil {
		t.Fatalf("width for 45 ohm on layer %d: %v", layer, err)
	}
	return w
}

func TestRouteHonorsClassWidthPrecedence(t *testing.T) {
	b, sch := classBoard(t)
	opts := DefaultOptions()
	opts.Schematic = sch
	rep := Route(b, opts)
	if rep.Failed != 0 {
		t.Fatalf("expected no failed nets, report=%s per=%+v", rep.Summary(), rep.PerNet)
	}
	seen := map[string]int{}
	for _, tr := range b.Traces {
		seen[tr.Net]++
		got := tr.Width.ToMM()
		var want float64
		switch tr.Net {
		case "USBDP":
			want = impedanceWidth(t, b, tr.Layer.Index, 45)
		case "PWRA":
			want = 0.60
		case "SIG1":
			want = DefaultOptions().TraceWidthMM
		default:
			t.Fatalf("unexpected net %q on the board", tr.Net)
		}
		if math.Abs(got-want) > 1e-3 {
			t.Fatalf("net %s on layer %d: width %.4f mm, want %.4f mm", tr.Net, tr.Layer.Index, got, want)
		}
	}
	for _, n := range []string{"USBDP", "PWRA", "SIG1"} {
		if seen[n] == 0 {
			t.Fatalf("net %s laid no copper: %+v", n, rep.PerNet)
		}
	}
}

// The impedance width is layer-dependent: microstrip outside, stripline
// inside. Same class, same target, different width.
func TestNetWidthsPerLayer(t *testing.T) {
	b, sch := classBoard(t)
	// Symmetric core: the closed form only solves stripline when both
	// adjacent dielectrics match (the default 4L stack is asymmetric).
	b.Stackup = &core.LayerStackup{
		Layers: []core.LayerSpec{
			{Name: "F.Cu", Kind: core.LayerKindSignal, CopperWeightOz: 1},
			{Name: "In1.Cu", Kind: core.LayerKindSignal, CopperWeightOz: 1},
			{Name: "In2.Cu", Kind: core.LayerKindSignal, CopperWeightOz: 1},
			{Name: "B.Cu", Kind: core.LayerKindSignal, CopperWeightOz: 1},
		},
		Dielectrics: []core.Dielectric{
			{ThicknessMM: 0.21, Er: 4.6},
			{ThicknessMM: 0.21, Er: 4.6},
			{ThicknessMM: 0.21, Er: 4.6},
		},
	}
	opts := DefaultOptions()
	opts.Schematic = sch
	nw := newNetWidths(b, opts)
	if nw == nil {
		t.Fatal("net widths not built from the schematic")
	}
	top := nw.widthMM("USBDP", 0)
	inner := nw.widthMM("USBDP", 1)
	if math.Abs(top-impedanceWidth(t, b, 0, 45)) > 1e-6 {
		t.Fatalf("top width %.4f mm is not the microstrip solution", top)
	}
	if math.Abs(inner-impedanceWidth(t, b, 1, 45)) > 1e-6 {
		t.Fatalf("inner width %.4f mm is not the stripline solution", inner)
	}
	if math.Abs(top-inner) < 1e-3 {
		t.Fatalf("microstrip and stripline solved to the same width %.4f mm", top)
	}
	if max := nw.maxMM("USBDP"); max < top-1e-9 {
		t.Fatalf("max width %.4f mm is narrower than the top width %.4f mm", max, top)
	}
	if w := nw.widthMM("SIG1", 0); w != 0 {
		t.Fatalf("classless net should defer to the option default, got %.4f mm", w)
	}
	if w := opts.widthFor("SIG1", 0); w != opts.TraceWidthMM {
		t.Fatalf("classless net width %.4f mm, want the default %.4f mm", w, opts.TraceWidthMM)
	}
}

// An impedance target the stackup cannot solve falls back to the class
// width, and a class narrower than the fab floor is raised to it.
func TestNetWidthsFallbacks(t *testing.T) {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(20), core.FromMM(20)))
	b.Outline = &o
	b.Stackup = &core.LayerStackup{
		Layers: []core.LayerSpec{
			{Name: "F.Cu", Kind: core.LayerKindSignal, CopperWeightOz: 1},
			{Name: "B.Cu", Kind: core.LayerKindSignal, CopperWeightOz: 1},
		},
	} // no dielectric: LineParams cannot solve
	sch := core.NewSchematic()
	sch.NetClasses["usb"] = &core.NetClass{Name: "usb", ImpedanceOhms: 45, TraceWidthMM: 0.25}
	sch.NetClasses["hair"] = &core.NetClass{Name: "hair", TraceWidthMM: 0.02}
	sch.NetToClass["USBDP"] = "usb"
	sch.NetToClass["THIN"] = "hair"

	opts := DefaultOptions()
	opts.Schematic = sch
	nw := newNetWidths(b, opts)
	if got := nw.widthMM("USBDP", 0); math.Abs(got-0.25) > 1e-9 {
		t.Fatalf("unsolvable stackup should fall back to the class width, got %.4f mm", got)
	}
	floor := core.ActiveFabRules(b).MinTraceWidthMM
	if got := nw.widthMM("THIN", 0); math.Abs(got-floor) > 1e-9 {
		t.Fatalf("class below the fab floor: width %.4f mm, want %.4f mm", got, floor)
	}
}
