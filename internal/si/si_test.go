package si

import (
	"strings"
	"testing"

	"github.com/mentasystems/fragua/internal/core"
)

// board4L is a 4-layer board (F.Cu / In1.Cu plane / In2.Cu plane / B.Cu)
// with a 40x30 mm outline — the stack the impedance numbers below assume.
func board4L() *core.Board {
	b := core.NewBoard()
	s := core.Default4Layer()
	b.Stackup = &s
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(40), core.FromMM(30)))
	b.Outline = &o
	return b
}

func board2L() *core.Board {
	b := core.NewBoard()
	o := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(40), core.FromMM(30)))
	b.Outline = &o
	return b
}

func addTrace(b *core.Board, net string, layer uint8, x0, y0, x1, y1, widthMM float64) {
	b.Traces = append(b.Traces, core.Trace{
		ID:    core.NewID(),
		Layer: core.Layer{Index: layer},
		Start: core.NewPoint(core.FromMM(x0), core.FromMM(y0)),
		End:   core.NewPoint(core.FromMM(x1), core.FromMM(y1)),
		Width: core.FromMM(widthMM),
		Net:   net,
	})
}

func addVia(b *core.Board, net string, x, y float64) {
	b.Vias = append(b.Vias, core.Via{
		ID:       core.NewID(),
		Position: core.NewPoint(core.FromMM(x), core.FromMM(y)),
		Drill:    core.FromMM(0.3),
		Diameter: core.FromMM(0.6),
		Net:      net,
	})
}

// addPour adds a pour on layer; an empty rect (all zeros) means outline pour.
func addPour(b *core.Board, net string, layer uint8, poly ...[2]float64) {
	p := core.Pour{ID: core.NewID(), Net: net, Layer: core.Layer{Index: layer}}
	for _, pt := range poly {
		p.Polygon = append(p.Polygon, core.NewPoint(core.FromMM(pt[0]), core.FromMM(pt[1])))
	}
	b.Pours = append(b.Pours, p)
}

// schWithClass wires net → class with an impedance target.
func schWithClass(net, class string, cls core.NetClass) *core.Schematic {
	sch := core.NewSchematic()
	cls.Name = class
	sch.NetClasses[class] = &cls
	sch.Nets[net] = &core.Net{Name: net, Class: class}
	return sch
}

// connect gives a net the pin connections a real netlist would have, so the
// unrouted check can tell "nothing to route" from "route failed".
func connect(sch *core.Schematic, net string, pins ...string) {
	n := sch.Nets[net]
	if n == nil {
		n = &core.Net{Name: net}
		sch.Nets[net] = n
	}
	for _, p := range pins {
		n.Connections = append(n.Connections, core.NetConnection{SymbolID: core.NewID(), PinNumber: p})
	}
}

func countKind(rep Report, k Kind) int {
	n := 0
	for _, v := range rep.Violations {
		if v.Kind == k {
			n++
		}
	}
	return n
}

func firstOfKind(t *testing.T, rep Report, k Kind) Violation {
	t.Helper()
	for _, v := range rep.Violations {
		if v.Kind == k {
			return v
		}
	}
	t.Fatalf("no %s violation in %+v", k, rep.Violations)
	return Violation{}
}

// ---------------------------------------------------------------------------
// unrouted nets
// ---------------------------------------------------------------------------

// The blind spot: every other check reads copper, so a net the router failed
// to route passed them all and the board looked clean.
func TestUnroutedNetInScopeWarns(t *testing.T) {
	b := board4L()
	addPour(b, "GND", 1)
	sch := schWithClass("USB_DP", "hs", core.NetClass{ImpedanceOhms: 90})
	connect(sch, "USB_DP", "1", "2")
	rep := Check(b, sch, DefaultOptions())
	if n := countKind(rep, KindNotRouted); n != 1 {
		t.Fatalf("expected 1 not_routed warning, got %d: %+v", n, rep.Violations)
	}
	if len(rep.Violations) != 1 {
		t.Fatalf("not_routed should be the only finding: %+v", rep.Violations)
	}
	v := firstOfKind(t, rep, KindNotRouted)
	if v.Severity != SeverityWarning || v.Net != "USB_DP" {
		t.Fatalf("severity/net: %+v", v)
	}
	if !strings.Contains(v.Message, "no routed copper") {
		t.Fatalf("message: %s", v.Message)
	}
	if rep.Errors != 0 || rep.Warnings != 1 {
		t.Fatalf("summary: %s", rep.Summary())
	}
}

func TestRoutedNetGetsNoNotRouted(t *testing.T) {
	b := board4L()
	addPour(b, "GND", 1)
	addTrace(b, "CLK", 0, 5, 5, 20, 5, 0.34)
	sch := schWithClass("CLK", "hs", core.NetClass{ImpedanceOhms: 50})
	connect(sch, "CLK", "1", "2")
	rep := Check(b, sch, DefaultOptions())
	if countKind(rep, KindNotRouted) != 0 {
		t.Fatalf("a routed net is not unrouted: %+v", rep.Violations)
	}
	if len(rep.Violations) != 0 {
		t.Fatalf("expected a clean report: %+v", rep.Violations)
	}
}

// Vias alone are copper: a net escaped to another layer is not "unrouted".
func TestNetWithOnlyViasIsRouted(t *testing.T) {
	b := board4L()
	addPour(b, "GND", 1)
	addVia(b, "CLK", 10, 10)
	sch := schWithClass("CLK", "hs", core.NetClass{ImpedanceOhms: 50})
	connect(sch, "CLK", "1", "2")
	if rep := Check(b, sch, DefaultOptions()); countKind(rep, KindNotRouted) != 0 {
		t.Fatalf("via copper counts as routed: %+v", rep.Violations)
	}
}

// A net with fewer than two connections has nothing to route.
func TestSinglePinNetIsNotFlagged(t *testing.T) {
	b := board4L()
	addPour(b, "GND", 1)
	sch := schWithClass("CLK", "hs", core.NetClass{ImpedanceOhms: 50})
	connect(sch, "CLK", "1")
	if rep := Check(b, sch, DefaultOptions()); countKind(rep, KindNotRouted) != 0 {
		t.Fatalf("single-pin net has nothing to route: %+v", rep.Violations)
	}
}

// unknown_net and not_routed are mutually exclusive.
func TestUnknownNetDoesNotAlsoGetNotRouted(t *testing.T) {
	b := board4L()
	addPour(b, "GND", 1)
	opts := DefaultOptions()
	opts.Nets = []string{"TYPO"}
	rep := Check(b, core.NewSchematic(), opts)
	if countKind(rep, KindUnknownNet) != 1 || countKind(rep, KindNotRouted) != 0 {
		t.Fatalf("expected unknown_net only: %+v", rep.Violations)
	}
}

// An unrouted diff-pair half must not also produce a skew number against
// copper that does not exist.
func TestUnroutedDiffPairHalfSkipsSkew(t *testing.T) {
	b := board4L()
	addPour(b, "GND", 1)
	addTrace(b, "D_P", 0, 5, 5, 25, 5, 0.2) // routed; D_N never routed
	sch := diffPairSchematic(0.5)
	connect(sch, "D_P", "1", "2")
	connect(sch, "D_N", "1", "2")
	rep := Check(b, sch, DefaultOptions())
	if n := countKind(rep, KindNotRouted); n != 1 {
		t.Fatalf("expected 1 not_routed (D_N), got %d: %+v", n, rep.Violations)
	}
	if countKind(rep, KindDiffPairSkew) != 0 {
		t.Fatalf("skew against no copper says nothing: %+v", rep.Violations)
	}
	if v := firstOfKind(t, rep, KindNotRouted); v.Net != "D_N" {
		t.Fatalf("wrong half flagged: %+v", v)
	}
}

// not_routed lands before the findings of the other checks.
func TestNotRoutedIsReportedFirst(t *testing.T) {
	b := board4L()
	addPour(b, "GND", 1)
	addTrace(b, "SCK", 0, 5, 5, 20, 5, 0.22) // SCK routed and off-target
	sch := schWithClass("SCK", "hs", core.NetClass{ImpedanceOhms: 50})
	sch.Nets["USB_DP"] = &core.Net{Name: "USB_DP", Class: "hs"}
	connect(sch, "SCK", "1", "2")
	connect(sch, "USB_DP", "1", "2")
	rep := Check(b, sch, DefaultOptions())
	if len(rep.Violations) != 2 {
		t.Fatalf("expected not_routed + impedance deviation: %+v", rep.Violations)
	}
	if rep.Violations[0].Kind != KindNotRouted {
		t.Fatalf("not_routed must come first: %+v", rep.Violations)
	}
}

// ---------------------------------------------------------------------------
// impedance
// ---------------------------------------------------------------------------

func TestImpedanceOnTargetIsClean(t *testing.T) {
	b := board4L()
	// 0.34 mm on 0.21 mm FR-4 microstrip ≈ 50 Ω.
	addTrace(b, "CLK", 0, 5, 5, 20, 5, 0.34)
	addPour(b, "GND", 1) // In1.Cu reference plane, full outline
	sch := schWithClass("CLK", "hs", core.NetClass{ImpedanceOhms: 50})
	rep := Check(b, sch, DefaultOptions())
	if countKind(rep, KindImpedanceDeviation) != 0 {
		t.Fatalf("expected no deviation, got %+v", rep.Violations)
	}
	if rep.Errors != 0 || rep.Warnings != 0 {
		t.Fatalf("expected a clean report, got %s %+v", rep.Summary(), rep.Violations)
	}
}

func TestImpedanceTooNarrowWarnsThenErrors(t *testing.T) {
	b := board4L()
	addTrace(b, "CLK", 0, 5, 5, 20, 5, 0.25)  // ≈58 Ω → +16% → warning
	addTrace(b, "CLK", 0, 20, 5, 30, 5, 0.22) // ≈62 Ω → +23% → error (>2×tol)
	addPour(b, "GND", 1)
	sch := schWithClass("CLK", "hs", core.NetClass{ImpedanceOhms: 50})
	rep := Check(b, sch, DefaultOptions())
	if n := countKind(rep, KindImpedanceDeviation); n != 2 {
		t.Fatalf("expected 2 deviations, got %d: %+v", n, rep.Violations)
	}
	if rep.Errors != 1 || rep.Warnings != 1 {
		t.Fatalf("expected 1 error + 1 warning, got %s: %+v", rep.Summary(), rep.Violations)
	}
	v := firstOfKind(t, rep, KindImpedanceDeviation)
	if v.Net != "CLK" || v.XMM == 0 {
		t.Fatalf("violation should carry net and location: %+v", v)
	}
}

// A routed net keeps one width for hundreds of segments: that is one fact,
// not one finding per segment.
func TestImpedanceCoalescesSegmentsOfEqualWidth(t *testing.T) {
	b := board4L()
	addPour(b, "GND", 1)
	for i := 0; i < 40; i++ {
		x := 5 + 0.5*float64(i)
		addTrace(b, "SCK", 0, x, 5, x+0.5, 5, 0.22)
	}
	sch := schWithClass("SCK", "hs", core.NetClass{ImpedanceOhms: 50})
	rep := Check(b, sch, DefaultOptions())
	if n := countKind(rep, KindImpedanceDeviation); n != 1 {
		t.Fatalf("40 same-width segments must coalesce into 1 finding, got %d: %+v", n, rep.Violations)
	}
	v := firstOfKind(t, rep, KindImpedanceDeviation)
	if !strings.Contains(v.Message, "(40 segments)") {
		t.Fatalf("message should carry the segment count: %s", v.Message)
	}
	if v.XMM != 5.25 { // midpoint of the first offending segment
		t.Fatalf("group should sit on the first offender, got %+v", v)
	}
}

// Different widths (or layers) are different facts and stay separate.
func TestImpedanceGroupsSplitByWidthAndLayer(t *testing.T) {
	b := board4L()
	addPour(b, "GND", 1)
	addTrace(b, "SCK", 0, 5, 5, 10, 5, 0.22)
	addTrace(b, "SCK", 0, 10, 5, 15, 5, 0.22) // same group as above
	addTrace(b, "SCK", 0, 15, 5, 20, 5, 0.25) // different width
	addTrace(b, "SCK", 3, 5, 8, 10, 8, 0.22)  // same width, other layer
	sch := schWithClass("SCK", "hs", core.NetClass{ImpedanceOhms: 50})
	rep := Check(b, sch, DefaultOptions())
	if n := countKind(rep, KindImpedanceDeviation); n != 3 {
		t.Fatalf("expected 3 groups (0.22/L1, 0.25/L1, 0.22/L4), got %d: %+v", n, rep.Violations)
	}
	// The worst segment sets the group severity; both 0.22 groups are errors.
	if rep.Errors < 2 {
		t.Fatalf("expected the 0.22 mm groups to be errors: %s %+v", rep.Summary(), rep.Violations)
	}
}

func TestImpedanceToleranceOptionWidens(t *testing.T) {
	b := board4L()
	addTrace(b, "CLK", 0, 5, 5, 20, 5, 0.25) // +16%
	addPour(b, "GND", 1)
	sch := schWithClass("CLK", "hs", core.NetClass{ImpedanceOhms: 50})
	opts := DefaultOptions()
	opts.Tolerance = 0.20
	rep := Check(b, sch, opts)
	if countKind(rep, KindImpedanceDeviation) != 0 {
		t.Fatalf("tol=0.20 should absorb +16%%: %+v", rep.Violations)
	}
}

// An escape neck: 0.5 mm of narrow copper on a 20 mm net. Far off target, but
// too short to matter — a warning, never an error.
func TestImpedanceShortNeckIsOnlyAWarning(t *testing.T) {
	b := board4L()
	addPour(b, "GND", 1)
	addTrace(b, "CLK", 0, 5, 5, 5.5, 5, 0.15) // escape neck
	addTrace(b, "CLK", 0, 5.5, 5, 25.5, 5, 0.34)
	sch := schWithClass("CLK", "hs", core.NetClass{ImpedanceOhms: 50})
	rep := Check(b, sch, DefaultOptions())
	v := firstOfKind(t, rep, KindImpedanceDeviation)
	if v.Severity != SeverityWarning {
		t.Fatalf("a 0.5 mm neck must not be an error: %+v", v)
	}
	if !strings.Contains(v.Message, "(short neck)") {
		t.Fatalf("message should name the neck: %s", v.Message)
	}
	if rep.Errors != 0 {
		t.Fatalf("expected no errors, got %s: %+v", rep.Summary(), rep.Violations)
	}
}

// The other side of the threshold: 3 mm of the same width is a mis-sized line,
// not a neck, and stays the error it always was.
func TestImpedanceLongDeviationStaysAnError(t *testing.T) {
	b := board4L()
	addPour(b, "GND", 1)
	addTrace(b, "CLK", 0, 5, 5, 8, 5, 0.15)
	addTrace(b, "CLK", 0, 8, 5, 28, 5, 0.34)
	sch := schWithClass("CLK", "hs", core.NetClass{ImpedanceOhms: 50})
	rep := Check(b, sch, DefaultOptions())
	v := firstOfKind(t, rep, KindImpedanceDeviation)
	if v.Severity != SeverityError {
		t.Fatalf("a 3 mm deviation must stay an error: %+v", v)
	}
	if strings.Contains(v.Message, "short neck") {
		t.Fatalf("3 mm is not a neck: %s", v.Message)
	}
}

// Short is relative too: 1.5 mm is most of a 5 mm net, so it is the line, not
// an escape from it.
func TestImpedanceNeckMustAlsoBeAShareOfTheNet(t *testing.T) {
	b := board4L()
	addPour(b, "GND", 1)
	addTrace(b, "CLK", 0, 5, 5, 6.5, 5, 0.15)
	addTrace(b, "CLK", 0, 6.5, 5, 10, 5, 0.34)
	sch := schWithClass("CLK", "hs", core.NetClass{ImpedanceOhms: 50})
	rep := Check(b, sch, DefaultOptions())
	v := firstOfKind(t, rep, KindImpedanceDeviation)
	if v.Severity != SeverityError {
		t.Fatalf("30%% of the net is not a neck: %+v", v)
	}
}

// A stackup with no Er cannot feed the closed form: one warning for the net,
// never a failed run and never a silent FR-4 guess.
func TestMissingStackupDataWarnsOncePerNet(t *testing.T) {
	b := board2L()
	s := core.Default2Layer()
	s.Dielectrics = []core.Dielectric{{ThicknessMM: 1.5}} // Er unset
	b.Stackup = &s
	addTrace(b, "CLK", 0, 5, 5, 20, 5, 0.25)
	addTrace(b, "CLK", 0, 20, 5, 30, 5, 0.25)
	sch := schWithClass("CLK", "hs", core.NetClass{ImpedanceOhms: 50})
	rep := Check(b, sch, DefaultOptions())
	if n := countKind(rep, KindStackupIncomplete); n != 1 {
		t.Fatalf("expected exactly 1 stackup warning, got %d: %+v", n, rep.Violations)
	}
	if countKind(rep, KindImpedanceDeviation) != 0 {
		t.Fatalf("no impedance verdict without stackup data: %+v", rep.Violations)
	}
	if rep.Errors != 0 {
		t.Fatalf("missing stackup data is a warning, not an error: %+v", rep.Violations)
	}
}

// ---------------------------------------------------------------------------
// return path
// ---------------------------------------------------------------------------

func TestReturnPathCoveredPlaneIsClean(t *testing.T) {
	b := board4L()
	addTrace(b, "CLK", 0, 5, 5, 20, 5, 0.34)
	addPour(b, "GND", 1, [2]float64{0, 0}, [2]float64{40, 0}, [2]float64{40, 30}, [2]float64{0, 30})
	sch := schWithClass("CLK", "hs", core.NetClass{ImpedanceOhms: 50})
	rep := Check(b, sch, DefaultOptions())
	if countKind(rep, KindReferencePlaneGap) != 0 {
		t.Fatalf("plane covers the trace: %+v", rep.Violations)
	}
}

func TestReturnPathGapUnderTrace(t *testing.T) {
	b := board4L()
	// Trace runs x=5→20 at y=5; the plane pour stops at x=10.
	addTrace(b, "CLK", 0, 5, 5, 20, 5, 0.34)
	addPour(b, "GND", 1, [2]float64{0, 0}, [2]float64{10, 0}, [2]float64{10, 30}, [2]float64{0, 30})
	sch := schWithClass("CLK", "hs", core.NetClass{ImpedanceOhms: 50})
	rep := Check(b, sch, DefaultOptions())
	if n := countKind(rep, KindReferencePlaneGap); n != 1 {
		t.Fatalf("expected 1 plane gap, got %d: %+v", n, rep.Violations)
	}
	v := firstOfKind(t, rep, KindReferencePlaneGap)
	if v.Severity != SeverityError {
		t.Fatalf("plane gap must be an error: %+v", v)
	}
	if v.XMM < 10 || v.XMM > 20 {
		t.Fatalf("gap should be reported past x=10: %+v", v)
	}
}

// Gap locations are per-segment, but a net routed entirely off the plane is
// capped so it cannot bury the rest of the report.
func TestReturnPathGapsAreCappedPerNet(t *testing.T) {
	b := board4L()
	// Plane pour stops at x=10; 15 segments live past it.
	addPour(b, "GND", 1, [2]float64{0, 0}, [2]float64{10, 0}, [2]float64{10, 30}, [2]float64{0, 30})
	for i := 0; i < 15; i++ {
		y := 3 + float64(i)
		addTrace(b, "CLK", 0, 12, y, 18, y, 0.34)
	}
	sch := schWithClass("CLK", "hs", core.NetClass{ImpedanceOhms: 50})
	rep := Check(b, sch, DefaultOptions())
	// 10 located gaps + 1 summary line.
	if n := countKind(rep, KindReferencePlaneGap); n != 11 {
		t.Fatalf("expected 10 gaps + 1 summary, got %d: %+v", n, rep.Violations)
	}
	last := rep.Violations[len(rep.Violations)-1]
	if !strings.Contains(last.Message, "and 5 more gaps on CLK") {
		t.Fatalf("expected the suppressed-gap summary last, got %q", last.Message)
	}
	if last.XMM != 0 || last.YMM != 0 {
		t.Fatalf("the summary line has no single location: %+v", last)
	}
}

func TestReturnPathNoPourOnPlaneLayer(t *testing.T) {
	b := board4L()
	addTrace(b, "CLK", 0, 5, 5, 20, 5, 0.34)
	addTrace(b, "CLK", 0, 20, 5, 30, 5, 0.34)
	sch := schWithClass("CLK", "hs", core.NetClass{ImpedanceOhms: 50})
	rep := Check(b, sch, DefaultOptions())
	// One per net, not one per segment.
	if n := countKind(rep, KindReferencePlaneGap); n != 1 {
		t.Fatalf("expected 1 no-pour violation, got %d: %+v", n, rep.Violations)
	}
	if rep.Errors != 1 {
		t.Fatalf("expected 1 error: %s %+v", rep.Summary(), rep.Violations)
	}
}

// No plane and no pour anywhere: nothing to reference against, one warning
// per net rather than per segment.
func TestReturnPathNoPlaneInStackup(t *testing.T) {
	b := board2L() // Default2Layer: both layers are signal
	addTrace(b, "CLK", 0, 5, 5, 20, 5, 2.7)
	addTrace(b, "CLK", 0, 20, 5, 30, 5, 2.7)
	sch := schWithClass("CLK", "hs", core.NetClass{ImpedanceOhms: 50})
	rep := Check(b, sch, DefaultOptions())
	if n := countKind(rep, KindNoReferencePlane); n != 1 {
		t.Fatalf("expected 1 no-reference warning, got %d: %+v", n, rep.Violations)
	}
	v := firstOfKind(t, rep, KindNoReferencePlane)
	if v.Severity != SeverityWarning {
		t.Fatalf("no-reference is a warning: %+v", v)
	}
	if !strings.Contains(v.Message, "no reference plane or pour") {
		t.Fatalf("message must mention pours too: %q", v.Message)
	}
	if countKind(rep, KindReferencePlaneGap) != 0 {
		t.Fatalf("nothing to check against, so no gaps: %+v", rep.Violations)
	}
}

// A pour on the trace's own layer is not a reference — the return current
// needs the copper on the other side of the dielectric.
func TestReturnPathPourOnSameLayerIsNotAReference(t *testing.T) {
	b := board2L()
	addTrace(b, "CLK", 0, 5, 5, 20, 5, 2.7)
	addPour(b, "GND", 0) // outline pour on F.Cu, same layer as the trace
	sch := schWithClass("CLK", "hs", core.NetClass{ImpedanceOhms: 50})
	rep := Check(b, sch, DefaultOptions())
	if n := countKind(rep, KindNoReferencePlane); n != 1 {
		t.Fatalf("expected 1 no-reference warning, got %d: %+v", n, rep.Violations)
	}
}

// The 2-layer case that matters: no plane in the stackup, but a full GND pour
// on the opposite copper is the de-facto reference and covers the trace.
func TestReturnPathPourIsReferenceWithoutPlane(t *testing.T) {
	b := board2L()
	addTrace(b, "CLK", 0, 5, 5, 20, 5, 2.7)
	addPour(b, "GND", 1, [2]float64{0, 0}, [2]float64{40, 0}, [2]float64{40, 30}, [2]float64{0, 30})
	sch := schWithClass("CLK", "hs", core.NetClass{ImpedanceOhms: 50})
	rep := Check(b, sch, DefaultOptions())
	if n := countKind(rep, KindNoReferencePlane); n != 0 {
		t.Fatalf("the pour is the reference, no warning expected: %+v", rep.Violations)
	}
	if n := countKind(rep, KindReferencePlaneGap); n != 0 {
		t.Fatalf("the pour covers the whole trace: %+v", rep.Violations)
	}
}

// Same 2-layer setup, but the pour stops short of the trace.
func TestReturnPathPourGapWithoutPlane(t *testing.T) {
	b := board2L()
	// Trace runs x=5→20 at y=5; the B.Cu pour stops at x=10.
	addTrace(b, "CLK", 0, 5, 5, 20, 5, 2.7)
	addPour(b, "GND", 1, [2]float64{0, 0}, [2]float64{10, 0}, [2]float64{10, 30}, [2]float64{0, 30})
	sch := schWithClass("CLK", "hs", core.NetClass{ImpedanceOhms: 50})
	rep := Check(b, sch, DefaultOptions())
	if n := countKind(rep, KindNoReferencePlane); n != 0 {
		t.Fatalf("a pour exists, so no no-reference warning: %+v", rep.Violations)
	}
	if n := countKind(rep, KindReferencePlaneGap); n != 1 {
		t.Fatalf("expected 1 return-path gap, got %d: %+v", n, rep.Violations)
	}
	v := firstOfKind(t, rep, KindReferencePlaneGap)
	if v.Severity != SeverityError {
		t.Fatalf("a return-path gap is an error: %+v", v)
	}
	if v.XMM < 10 || v.XMM > 20 {
		t.Fatalf("gap should be reported past x=10: %+v", v)
	}
	if !strings.Contains(v.Message, "reference pour on B.Cu") {
		t.Fatalf("message must name the reference pour layer: %q", v.Message)
	}
}

// A cutout removes the plane copper under the trace.
func TestReturnPathCutoutIsAGap(t *testing.T) {
	b := board4L()
	addTrace(b, "CLK", 0, 5, 5, 20, 5, 0.34)
	addPour(b, "GND", 1) // outline pour
	b.Cutouts = append(b.Cutouts, core.Cutout{ID: core.NewID(), Polygon: []core.Point{
		core.NewPoint(core.FromMM(12), core.FromMM(3)),
		core.NewPoint(core.FromMM(16), core.FromMM(3)),
		core.NewPoint(core.FromMM(16), core.FromMM(7)),
		core.NewPoint(core.FromMM(12), core.FromMM(7)),
	}})
	sch := schWithClass("CLK", "hs", core.NetClass{ImpedanceOhms: 50})
	rep := Check(b, sch, DefaultOptions())
	v := firstOfKind(t, rep, KindReferencePlaneGap)
	if v.XMM < 12 || v.XMM > 16 {
		t.Fatalf("gap should land inside the cutout: %+v", v)
	}
}

// ---------------------------------------------------------------------------
// diff-pair skew
// ---------------------------------------------------------------------------

func diffPairSchematic(tolMM float64) *core.Schematic {
	sch := core.NewSchematic()
	sch.NetClasses["usb"] = &core.NetClass{Name: "usb", LengthToleranceMM: tolMM}
	sch.Nets["D_P"] = &core.Net{Name: "D_P", Class: "usb", DiffPair: "D_N"}
	sch.Nets["D_N"] = &core.Net{Name: "D_N", Class: "usb", DiffPair: "D_P"}
	return sch
}

func TestDiffPairSkewWithinToleranceIsClean(t *testing.T) {
	b := board4L()
	addPour(b, "GND", 1)
	addTrace(b, "D_P", 0, 5, 5, 15, 5, 0.2)
	addTrace(b, "D_N", 0, 5, 6, 15.2, 6, 0.2)
	rep := Check(b, diffPairSchematic(0.5), DefaultOptions())
	if countKind(rep, KindDiffPairSkew) != 0 {
		t.Fatalf("0.2 mm skew under 0.5 mm tolerance: %+v", rep.Violations)
	}
}

func TestDiffPairSkewReportedOncePerPair(t *testing.T) {
	b := board4L()
	addPour(b, "GND", 1)
	addTrace(b, "D_P", 0, 5, 5, 15, 5, 0.2) // 10 mm
	addTrace(b, "D_N", 0, 5, 6, 16, 6, 0.2) // 11 mm → skew 1.0 mm
	rep := Check(b, diffPairSchematic(0.5), DefaultOptions())
	if n := countKind(rep, KindDiffPairSkew); n != 1 {
		t.Fatalf("expected 1 skew finding for the pair, got %d: %+v", n, rep.Violations)
	}
	if v := firstOfKind(t, rep, KindDiffPairSkew); v.Severity != SeverityWarning {
		t.Fatalf("1.0 mm vs 0.5 mm tolerance is a warning: %+v", v)
	}
}

func TestDiffPairSkewBeyondTwiceToleranceIsError(t *testing.T) {
	b := board4L()
	addPour(b, "GND", 1)
	addTrace(b, "D_P", 0, 5, 5, 15, 5, 0.2) // 10 mm
	addTrace(b, "D_N", 0, 5, 6, 18, 6, 0.2) // 13 mm → skew 3.0 mm
	rep := Check(b, diffPairSchematic(0.5), DefaultOptions())
	if v := firstOfKind(t, rep, KindDiffPairSkew); v.Severity != SeverityError {
		t.Fatalf("3.0 mm vs 0.5 mm tolerance is an error: %+v", v)
	}
}

// No class tolerance ⇒ the 0.5 mm default applies.
func TestDiffPairSkewDefaultTolerance(t *testing.T) {
	b := board4L()
	addPour(b, "GND", 1)
	addTrace(b, "D_P", 0, 5, 5, 15, 5, 0.2)
	addTrace(b, "D_N", 0, 5, 6, 15.8, 6, 0.2) // skew 0.8 mm
	rep := Check(b, diffPairSchematic(0), DefaultOptions())
	if n := countKind(rep, KindDiffPairSkew); n != 1 {
		t.Fatalf("expected the 0.5 mm default to fire, got %d: %+v", n, rep.Violations)
	}
}

func TestDiffPairUnroutedIsSkipped(t *testing.T) {
	b := board4L()
	addPour(b, "GND", 1)
	rep := Check(b, diffPairSchematic(0.5), DefaultOptions())
	if countKind(rep, KindDiffPairSkew) != 0 {
		t.Fatalf("nothing routed yet: %+v", rep.Violations)
	}
}

// ---------------------------------------------------------------------------
// via budget + net selection
// ---------------------------------------------------------------------------

func TestViaBudget(t *testing.T) {
	b := board4L()
	addPour(b, "GND", 1)
	addTrace(b, "CLK", 0, 5, 5, 20, 5, 0.34)
	for i := 0; i < 3; i++ {
		addVia(b, "CLK", 6+float64(i), 5)
	}
	sch := schWithClass("CLK", "hs", core.NetClass{ImpedanceOhms: 50})

	opts := DefaultOptions()
	if rep := Check(b, sch, opts); countKind(rep, KindViaBudget) != 0 {
		t.Fatalf("max_vias=0 disables the budget: %+v", rep.Violations)
	}
	opts.MaxVias = 2
	rep := Check(b, sch, opts)
	if n := countKind(rep, KindViaBudget); n != 1 {
		t.Fatalf("expected 1 via-budget warning, got %d: %+v", n, rep.Violations)
	}
	if v := firstOfKind(t, rep, KindViaBudget); v.Severity != SeverityWarning {
		t.Fatalf("via budget is a warning: %+v", v)
	}
}

// Without args the scope is class-impedance / diff-pair nets only.
func TestNetSelectionIgnoresPlainNets(t *testing.T) {
	b := board4L()
	addPour(b, "GND", 1)
	addTrace(b, "CLK", 0, 5, 5, 20, 5, 0.25)   // in scope (impedance class)
	addTrace(b, "LED", 0, 5, 10, 20, 10, 0.25) // no class → out of scope
	sch := schWithClass("CLK", "hs", core.NetClass{ImpedanceOhms: 50})
	sch.Nets["LED"] = &core.Net{Name: "LED"}
	rep := Check(b, sch, DefaultOptions())
	for _, v := range rep.Violations {
		if v.Net == "LED" {
			t.Fatalf("LED is out of scope: %+v", rep.Violations)
		}
	}
	if countKind(rep, KindImpedanceDeviation) != 1 {
		t.Fatalf("expected the CLK deviation: %+v", rep.Violations)
	}
}

func TestExplicitNetsRestrictScope(t *testing.T) {
	b := board4L()
	addPour(b, "GND", 1)
	addTrace(b, "CLK", 0, 5, 5, 20, 5, 0.25)
	addTrace(b, "D_P", 0, 5, 8, 15, 8, 0.2)
	addTrace(b, "D_N", 0, 5, 9, 18, 9, 0.2)
	sch := diffPairSchematic(0.5)
	sch.NetClasses["hs"] = &core.NetClass{Name: "hs", ImpedanceOhms: 50}
	sch.Nets["CLK"] = &core.Net{Name: "CLK", Class: "hs"}

	opts := DefaultOptions()
	opts.Nets = []string{"CLK"}
	rep := Check(b, sch, opts)
	if countKind(rep, KindDiffPairSkew) != 0 {
		t.Fatalf("D_P/D_N are out of the requested scope: %+v", rep.Violations)
	}
	if countKind(rep, KindImpedanceDeviation) != 1 {
		t.Fatalf("expected the CLK deviation: %+v", rep.Violations)
	}
}

func TestUnknownExplicitNetWarns(t *testing.T) {
	b := board4L()
	addPour(b, "GND", 1)
	opts := DefaultOptions()
	opts.Nets = []string{"TYPO"}
	rep := Check(b, core.NewSchematic(), opts)
	if n := countKind(rep, KindUnknownNet); n != 1 {
		t.Fatalf("expected an unknown-net warning, got %+v", rep.Violations)
	}
}

func TestNilBoardAndEmptyScope(t *testing.T) {
	if rep := Check(nil, nil, DefaultOptions()); len(rep.Violations) != 0 {
		t.Fatalf("nil board must be empty: %+v", rep)
	}
	rep := Check(board4L(), nil, DefaultOptions())
	if len(rep.Violations) != 0 {
		t.Fatalf("no schematic ⇒ nothing in scope: %+v", rep)
	}
	if rep.Summary() != "si: 0 errors, 0 warnings (0 findings)" {
		t.Fatalf("summary: %s", rep.Summary())
	}
}
