package router

import (
	"math"
	"strings"
	"testing"

	"github.com/mentasystems/fragua/internal/core"
)

// wallPad is a big plated obstacle: through-hole, so it walls off every copper
// layer the way a real connector shell or mounting barrel does. Each bar owns
// a single-pad net — foreign copper the router must clear, with nothing of its
// own to route.
func wallPad(num, net string, ox, oy, w, h float64) core.Pad {
	drill := core.FromMM(0.3)
	n := net
	return core.Pad{
		Number: num,
		Offset: core.NewPoint(core.FromMM(ox), core.FromMM(oy)),
		Size:   [2]core.Length{core.FromMM(w), core.FromMM(h)},
		Layer:  core.LayerTop,
		Drill:  &drill,
		Net:    &n,
	}
}

// finePitchDummy is an 8-pad 0.4 mm-pitch package. It carries no nets: it is
// there only so the router recognises a fine-pitch board and drops to the fab
// floor (0.0889 mm on jlcpcb-4l) — the width an escape neck uses.
func finePitchDummy(x, y float64) *core.Footprint {
	var pads []core.Pad
	for i := 0; i < 8; i++ {
		pads = append(pads, core.Pad{
			Number: string(rune('1' + i)),
			Offset: core.NewPoint(core.FromMM(-1.4+0.4*float64(i)), 0),
			Size:   [2]core.Length{core.FromMM(0.25), core.FromMM(0.55)},
			Layer:  core.LayerTop,
		})
	}
	return footprint("U9", x, y, pads)
}

// gateBoard is a 4-layer fine-pitch board where net SIG must cross a plated
// wall at x=wallX through a single gap of gapMM centred on gateY. Sizing the
// gap is how the test picks which widths fit.
func gateBoard(t *testing.T, wallX, gateY, gapMM float64) (*core.Board, *core.Schematic) {
	t.Helper()
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(30), core.FromMM(20)))
	b.Outline = &o
	b.AddFootprint(footprint("R1", 10.2, 10, []core.Pad{pad("1", 0, 0, "SIG")}))
	b.AddFootprint(footprint("R2", 20, 10, []core.Pad{pad("1", 0, 0, "SIG")}))
	b.AddFootprint(finePitchDummy(25, 4))
	// Two bars leaving one gap. Both run past the outline so nothing walks
	// around the ends.
	lo := gateY - gapMM/2 // top edge of the lower bar
	hi := gateY + gapMM/2 // bottom edge of the upper bar
	b.AddFootprint(footprint("W1", wallX, 0, []core.Pad{
		wallPad("1", "WALLA", 0, (lo-1)/2, 0.2, lo+1),
		wallPad("2", "WALLB", 0, (hi+21)/2, 0.2, 21-hi),
	}))
	b.Apply4Layer()

	sch := core.NewSchematic()
	sch.NetClasses["usb"] = &core.NetClass{Name: "usb", ImpedanceOhms: 45, TraceWidthMM: 0.20}
	sch.NetToClass["SIG"] = "usb"
	return b, sch
}

func sigOutcome(t *testing.T, rep Report) Outcome {
	t.Helper()
	for _, n := range rep.PerNet {
		if n.Net == "SIG" {
			return n.Outcome
		}
	}
	t.Fatalf("net SIG missing from the report: %+v", rep.PerNet)
	return Outcome{}
}

// A 0.6 mm gate is too tight for the 45 Ω width (0.414 mm) but roomy for the
// fab floor. The net must route by necking down where it leaves the pad and
// widening back to the controlled width in the open field.
func TestImpedanceNetNecksAtEscape(t *testing.T) {
	b, sch := gateBoard(t, 11, 10, 0.6)
	opts := DefaultOptions()
	opts.Schematic = sch
	rep := Route(b, opts)

	out := sigOutcome(t, rep)
	if out.Status != "ok" {
		t.Fatalf("SIG must route through the gate by necking: %+v (%s)", out, rep.Summary())
	}
	if !out.Necked {
		t.Fatalf("SIG routed but was not reported as necked: %+v", out)
	}
	if out.FallbackWidthMM != 0 {
		t.Fatalf("SIG must keep its nominal width, not fall back: %+v", out)
	}
	if !strings.Contains(rep.Summary(), "1 necked at escapes") {
		t.Fatalf("summary hides the neck: %s", rep.Summary())
	}

	nominal := impedanceWidth(t, b, 0, 45)
	floor := core.ActiveFabRules(b).MinTraceWidthMM
	pads := []core.Point{
		core.NewPoint(core.FromMM(10.2), core.FromMM(10)),
		core.NewPoint(core.FromMM(20), core.FromMM(10)),
	}
	nearPad := func(tr core.Trace) bool {
		mx := (tr.Start.X.ToMM() + tr.End.X.ToMM()) / 2
		my := (tr.Start.Y.ToMM() + tr.End.Y.ToMM()) / 2
		for _, p := range pads {
			if math.Hypot(mx-p.X.ToMM(), my-p.Y.ToMM()) <= escapeZoneRadiusMM {
				return true
			}
		}
		return false
	}
	neckedAtPad, nominalInField := false, false
	for _, tr := range b.Traces {
		if tr.Net != "SIG" {
			continue
		}
		w := tr.Width.ToMM()
		if w < floor-1e-9 || w > nominal+1e-9 {
			t.Fatalf("SIG segment width %.4f mm is outside [%.4f, %.4f]", w, floor, nominal)
		}
		switch {
		case nearPad(tr) && math.Abs(w-floor) < 1e-6:
			neckedAtPad = true
		case !nearPad(tr) && math.Abs(w-nominal) < 1e-6:
			nominalInField = true
		case !nearPad(tr) && w < nominal-1e-6:
			t.Fatalf("SIG necks %.4f mm in the open field at (%.2f,%.2f): only escapes may neck",
				w, tr.Start.X.ToMM(), tr.Start.Y.ToMM())
		}
	}
	if !neckedAtPad {
		t.Fatalf("no segment necked to the fab floor near a pad; widths=%v", sigWidths(b))
	}
	if !nominalInField {
		t.Fatalf("no open-field segment at the nominal %.4f mm; widths=%v", nominal, sigWidths(b))
	}
}

// Same board, but the gate sits mid-span where no escape zone reaches: necking
// cannot help, so the net drops to the class width — and the report says so.
func TestImpedanceNetFallsBackToClassWidth(t *testing.T) {
	b, sch := gateBoard(t, 15, 15, 0.8)
	opts := DefaultOptions()
	opts.Schematic = sch
	rep := Route(b, opts)

	out := sigOutcome(t, rep)
	if out.Status != "ok" {
		t.Fatalf("SIG must route at the fallback width: %+v (%s)", out, rep.Summary())
	}
	if math.Abs(out.FallbackWidthMM-0.20) > 1e-9 {
		t.Fatalf("fallback width %.4f mm, want the class width 0.20 mm: %+v", out.FallbackWidthMM, out)
	}
	if !strings.Contains(rep.Summary(), "1 at reduced width") {
		t.Fatalf("summary hides the width fallback: %s", rep.Summary())
	}
	nominal := impedanceWidth(t, b, 0, 45)
	for _, tr := range b.Traces {
		if tr.Net == "SIG" && tr.Width.ToMM() > 0.20+1e-9 {
			t.Fatalf("SIG kept a %.4f mm segment after falling back below %.4f mm", tr.Width.ToMM(), nominal)
		}
	}
}

func sigWidths(b *core.Board) []float64 {
	var out []float64
	for _, tr := range b.Traces {
		if tr.Net == "SIG" {
			out = append(out, tr.Width.ToMM())
		}
	}
	return out
}

// escapeTraces cuts one run into its necked and its nominal stretch, and
// leaves a run that never enters a zone as a single segment.
func TestEscapeTracesSplitAtZoneEdge(t *testing.T) {
	opts := DefaultOptions()
	opts.TraceWidthMM = 0.40
	opts.neck = newNeckPlan([]padLoc{{p: core.NewPoint(core.FromMM(10), core.FromMM(10))}}, 0.10, 0.40)
	if opts.neck == nil {
		t.Fatal("neck plan not built")
	}
	a := core.NewPoint(core.FromMM(10), core.FromMM(10))
	bp := core.NewPoint(core.FromMM(16), core.FromMM(10))
	segs := opts.escapeTraces("SIG", 0, a, bp)
	if len(segs) != 2 {
		t.Fatalf("want a necked + a nominal segment, got %d: %+v", len(segs), sigWidths(&core.Board{Traces: segs}))
	}
	if math.Abs(segs[0].Width.ToMM()-0.10) > 1e-9 || math.Abs(segs[1].Width.ToMM()-0.40) > 1e-9 {
		t.Fatalf("widths %.3f then %.3f mm, want 0.10 then 0.40", segs[0].Width.ToMM(), segs[1].Width.ToMM())
	}
	if segs[0].Start != a || segs[1].End != bp {
		t.Fatalf("split lost the run endpoints: %+v", segs)
	}
	if segs[0].End != segs[1].Start {
		t.Fatalf("split left a gap: %v vs %v", segs[0].End, segs[1].Start)
	}
	cut := segs[0].End.X.ToMM() - 10
	if math.Abs(cut-escapeZoneRadiusMM) > 2*escapeSampleMM {
		t.Fatalf("cut at %.3f mm from the pad, want the %.2f mm zone edge", cut, escapeZoneRadiusMM)
	}
	// Nothing to neck once the run is out in the open.
	far := opts.escapeTraces("SIG", 0,
		core.NewPoint(core.FromMM(20), core.FromMM(20)), core.NewPoint(core.FromMM(25), core.FromMM(20)))
	if len(far) != 1 || math.Abs(far[0].Width.ToMM()-0.40) > 1e-9 {
		t.Fatalf("open-field run must stay one nominal segment, got %+v", far)
	}
}
