// Package drc implements geometric design rule checking.
package drc

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/mentasystems/fragua/internal/core"
	"github.com/mentasystems/fragua/internal/impedance"
)

// Severity of a violation.
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Kind classifies a DRC violation.
type Kind string

const (
	KindPadPadClearance        Kind = "pad_pad_clearance"
	KindTraceTraceClearance    Kind = "trace_trace_clearance"
	KindTracePadClearance      Kind = "trace_pad_clearance"
	KindEdgeClearance          Kind = "edge_clearance"
	KindUnconnectedPad         Kind = "unconnected_pad"
	KindNetSplit               Kind = "net_split"
	KindNarrowTrace            Kind = "narrow_trace"
	KindSmallDrill             Kind = "small_drill"
	KindSmallComponentDangling Kind = "small_component_dangling"
	KindBodyOverlap            Kind = "body_overlap"
	KindBodyOffBoard           Kind = "body_off_board"
	KindRoutingInefficient     Kind = "routing_inefficient"
	KindRuleBelowFabLimit      Kind = "rule_below_fab_limit"
	KindAnnularRing            Kind = "annular_ring"
	KindViaPadClearance        Kind = "via_pad_clearance"
	KindViaTraceClearance      Kind = "via_trace_clearance"
	KindCourtyardOverlap       Kind = "courtyard_overlap"
	KindIsolatedPour           Kind = "isolated_pour"
	KindUnstitchedPour         Kind = "unstitched_pour"
	KindHoleToHole             Kind = "hole_to_hole"
	KindCopperSliver           Kind = "copper_sliver"
	KindImpedanceMismatch      Kind = "impedance_mismatch"
	KindTeardropClearance      Kind = "teardrop_clearance"
)

// SMALL_COMPONENT_PAD_LIMIT matches Rust.
const smallComponentPadLimit = 8

// TouchTolMM is the copper-contact slack (mm). Same spirit as Rust TOUCH_TOL_MM.
const TouchTolMM = 1e-6

// Violation is one DRC finding.
type Violation struct {
	Kind     Kind     `json:"kind"`
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
	Net      string   `json:"net,omitempty"`
	XMM      float64  `json:"x_mm,omitempty"`
	YMM      float64  `json:"y_mm,omitempty"`
}

// Options configures a DRC run.
type Options struct {
	MinClearance            core.Length
	EdgeClearance           core.Length
	MinTraceWidth           core.Length
	MinDrill                core.Length
	MinAnnularRing          core.Length
	MinHoleToHole           core.Length
	MinSliver               core.Length
	RoutingInefficientRatio float64
	FabProfile              *core.FabProfileHandle
}

// DefaultOptions: 0.2 mm clearance, 0.3 mm min drill (JLCPCB standard via).
func DefaultOptions() Options {
	return Options{
		MinClearance:            core.FromMM(0.2),
		EdgeClearance:           core.FromMM(0.3),
		MinTraceWidth:           core.FromMM(0.1),
		MinDrill:                core.FromMM(0.3),
		MinAnnularRing:          core.FromMM(0.15),
		MinHoleToHole:           core.FromMM(0.50),
		MinSliver:               core.FromMM(0.15),
		RoutingInefficientRatio: 1.5,
	}
}

// Report is the full DRC result.
type Report struct {
	Violations []Violation `json:"violations"`
	Errors     int         `json:"errors"`
	Warnings   int         `json:"warnings"`
}

// Summary is a one-line agent-friendly summary.
func (r Report) Summary() string {
	return fmt.Sprintf("drc: %d errors, %d warnings (%d findings)", r.Errors, r.Warnings, len(r.Violations))
}

func (r *Report) add(v Violation) {
	r.Violations = append(r.Violations, v)
	if v.Severity == SeverityError {
		r.Errors++
	} else {
		r.Warnings++
	}
}

// padGeom is world-space pad geometry for clearance / connectivity.
type padGeom struct {
	ref, number string
	layer       core.Layer
	throughHole bool
	rect        core.Rect
	net         string // empty if unassigned
	edgeMounted bool
}

func (p padGeom) label() string {
	return p.ref + "." + p.number
}

func (p padGeom) occupiesLayer(l core.Layer) bool {
	return p.throughHole || p.layer == l
}

func (p padGeom) sharesLayerWith(o padGeom) bool {
	return p.throughHole || o.throughHole || p.layer == o.layer
}

func collectPads(board *core.Board) []padGeom {
	var out []padGeom
	for _, id := range board.FootprintOrder {
		fp := board.Footprints[id]
		if fp == nil {
			continue
		}
		for i := range fp.Pads {
			pad := &fp.Pads[i]
			net := ""
			if pad.Net != nil {
				net = *pad.Net
			}
			out = append(out, padGeom{
				ref:         fp.Reference,
				number:      pad.Number,
				layer:       pad.Layer,
				throughHole: pad.Drill != nil,
				rect:        core.PadWorldAABB(fp, pad),
				net:         net,
				edgeMounted: fp.EdgeMounted,
			})
		}
	}
	// Footprints not in order (defensive)
	seen := make(map[string]bool, len(board.FootprintOrder))
	for _, id := range board.FootprintOrder {
		seen[id] = true
	}
	var extras []string
	for id := range board.Footprints {
		if !seen[id] {
			extras = append(extras, id)
		}
	}
	sort.Strings(extras)
	for _, id := range extras {
		fp := board.Footprints[id]
		if fp == nil {
			continue
		}
		for i := range fp.Pads {
			pad := &fp.Pads[i]
			net := ""
			if pad.Net != nil {
				net = *pad.Net
			}
			out = append(out, padGeom{
				ref:         fp.Reference,
				number:      pad.Number,
				layer:       pad.Layer,
				throughHole: pad.Drill != nil,
				rect:        core.PadWorldAABB(fp, pad),
				net:         net,
				edgeMounted: fp.EdgeMounted,
			})
		}
	}
	return out
}

// Check runs geometric DRC on board (schematic optional for net classes).
// Stage order mirrors pcb_drc::run.
func Check(board *core.Board, sch *core.Schematic, opts Options) Report {
	var rep Report
	if board == nil {
		return rep
	}
	resolver := buildResolver(board, sch, opts)
	edgeClMM := opts.EdgeClearance.ToMM()
	minDrillMM := opts.MinDrill.ToMM()
	if opts.FabProfile != nil && opts.FabProfile.MinEdgeClearanceMM > 0 {
		if opts.FabProfile.MinEdgeClearanceMM > edgeClMM {
			edgeClMM = opts.FabProfile.MinEdgeClearanceMM
		}
	}
	if board.FabRules != nil && board.FabRules.MinEdgeClearanceMM > 0 {
		if board.FabRules.MinEdgeClearanceMM > edgeClMM {
			edgeClMM = board.FabRules.MinEdgeClearanceMM
		}
	}

	pads := collectPads(board)

	minAnnularMM := opts.MinAnnularRing.ToMM()
	if opts.FabProfile != nil && opts.FabProfile.MinAnnularRingMM > 0 {
		if opts.FabProfile.MinAnnularRingMM > minAnnularMM {
			minAnnularMM = opts.FabProfile.MinAnnularRingMM
		}
	}
	if board.FabRules != nil && board.FabRules.MinAnnularRingMM > 0 {
		if board.FabRules.MinAnnularRingMM > minAnnularMM {
			minAnnularMM = board.FabRules.MinAnnularRingMM
		}
	}
	if board.FabRules != nil && board.FabRules.MinViaDrillMM > 0 && board.FabRules.MinViaDrillMM > minDrillMM {
		minDrillMM = board.FabRules.MinViaDrillMM
	}

	checkPadPad(pads, resolver, &rep)
	checkTraceTrace(board, resolver, &rep)
	checkTracePad(board, pads, resolver, &rep)
	checkViaPad(board, pads, resolver, &rep)
	checkViaTrace(board, resolver, &rep)
	if board.Outline != nil {
		checkEdge(board, pads, *board.Outline, edgeClMM, &rep)
		checkBodyOffBoard(board, *board.Outline, &rep)
	}
	checkCourtyardOverlap(board, &rep)
	checkUnconnectedPads(board, pads, &rep)
	checkSmallComponentDangling(board, pads, &rep)
	checkNetContinuity(board, pads, &rep)
	checkNarrowTraces(board, resolver, &rep)
	checkSmallDrills(board, minDrillMM, &rep)
	checkAnnularRing(board, minAnnularMM, &rep)
	checkIsolatedPours(board, &rep)
	minH2H := opts.MinHoleToHole.ToMM()
	if minH2H <= 0 {
		minH2H = 0.50
	}
	if board.FabRules != nil && board.FabRules.MinHoleToHoleMM > 0 {
		minH2H = board.FabRules.MinHoleToHoleMM
	}
	if opts.FabProfile != nil && opts.FabProfile.MinHoleToHoleMM > 0 {
		minH2H = opts.FabProfile.MinHoleToHoleMM
	}
	checkHoleToHole(board, minH2H, &rep)
	minSliver := opts.MinSliver.ToMM()
	if minSliver <= 0 {
		minSliver = 0.15
	}
	if board.FabRules != nil && board.FabRules.MinSliverMM > 0 {
		minSliver = board.FabRules.MinSliverMM
	}
	checkCopperSliver(board, pads, minSliver, &rep)
	checkRoutingInefficient(board, opts, &rep)
	checkRuleAreasVsFab(board, &rep)
	checkTeardropClearance(board, pads, resolver, &rep)
	checkImpedance(board, sch, &rep)

	return rep
}

func buildResolver(board *core.Board, sch *core.Schematic, opts Options) *core.RuleResolver {
	r := &core.RuleResolver{
		Defaults: core.RuleDefaults{
			Clearance:  opts.MinClearance,
			TraceWidth: opts.MinTraceWidth,
		},
		Areas: append([]core.RuleArea(nil), board.RuleAreas...),
	}
	if sch != nil {
		r.NetClass = map[string]core.NetClass{}
		for k, c := range sch.NetClasses {
			if c != nil {
				r.NetClass[k] = *c
			}
		}
		r.NetToClass = map[string]string{}
		for name, net := range sch.Nets {
			if net == nil {
				continue
			}
			if net.Class != "" {
				r.NetToClass[name] = net.Class
			}
		}
		for k, v := range sch.NetToClass {
			r.NetToClass[k] = v
		}
	}
	return r
}

func clearanceMM(res *core.RuleResolver, netA, netB string, siteX, siteY float64) float64 {
	p := core.NewPoint(core.FromMM(siteX), core.FromMM(siteY))
	return res.ClearanceBetween(p, netA, netB).ToMM()
}

func checkPadPad(pads []padGeom, res *core.RuleResolver, rep *Report) {
	for i := 0; i < len(pads); i++ {
		for j := i + 1; j < len(pads); j++ {
			a, b := pads[i], pads[j]
			if !a.sharesLayerWith(b) {
				continue
			}
			if a.net != "" && a.net == b.net {
				continue
			}
			gap := aabbGapMM(a.rect, b.rect)
			sx, sy := rectRectClosestSite(a.rect, b.rect)
			clr := clearanceMM(res, a.net, b.net, sx, sy)
			if gap+0.001 < clr {
				rep.add(Violation{
					Kind: KindPadPadClearance, Severity: SeverityError,
					Message: fmt.Sprintf("pad %s – pad %s: %.3f mm < %.3f mm", a.label(), b.label(), gap, clr),
					Net:     a.net, XMM: sx, YMM: sy,
				})
			}
		}
	}
}

func checkTraceTrace(board *core.Board, res *core.RuleResolver, rep *Report) {
	trs := board.Traces
	for i := 0; i < len(trs); i++ {
		for j := i + 1; j < len(trs); j++ {
			a, b := trs[i], trs[j]
			if a.Layer != b.Layer || a.Net == b.Net {
				continue
			}
			a0, a1 := ptMM(a.Start), ptMM(a.End)
			b0, b1 := ptMM(b.Start), ptMM(b.End)
			halfA := a.Width.ToMM() / 2
			halfB := b.Width.ToMM() / 2
			center := segmentSegmentDistance(a0, a1, b0, b1)
			gap := center - halfA - halfB
			sx, sy := segSegClosestSite(a0, a1, b0, b1)
			clr := clearanceMM(res, a.Net, b.Net, sx, sy)
			if gap+0.001 < clr {
				rep.add(Violation{
					Kind: KindTraceTraceClearance, Severity: SeverityError,
					Message: fmt.Sprintf("trace %s – trace %s: %.3f mm < %.3f mm", a.Net, b.Net, gap, clr),
					Net:     a.Net, XMM: sx, YMM: sy,
				})
			}
		}
	}
}

func checkViaPad(board *core.Board, pads []padGeom, res *core.RuleResolver, rep *Report) {
	for _, v := range board.Vias {
		vr := v.Diameter.ToMM() / 2
		c := ptMM(v.Position)
		for _, pad := range pads {
			if pad.net != "" && pad.net == v.Net {
				continue // same net may touch
			}
			gap := pointRectDistance(c, pad.rect) - vr
			sx, sy := rectCenterMM(pad.rect)
			clr := clearanceMM(res, v.Net, pad.net, c[0], c[1])
			if gap+0.001 < clr {
				rep.add(Violation{
					Kind: KindViaPadClearance, Severity: SeverityError,
					Message: fmt.Sprintf("via %s – pad %s: %.3f mm < %.3f mm", v.Net, pad.label(), gap, clr),
					Net:     v.Net, XMM: sx, YMM: sy,
				})
			}
		}
	}
}

func checkViaTrace(board *core.Board, res *core.RuleResolver, rep *Report) {
	for _, v := range board.Vias {
		vr := v.Diameter.ToMM() / 2
		c := ptMM(v.Position)
		for _, tr := range board.Traces {
			if tr.Net == v.Net {
				continue
			}
			gap := pointSegmentDistance(c, ptMM(tr.Start), ptMM(tr.End)) - vr - tr.Width.ToMM()/2
			clr := clearanceMM(res, v.Net, tr.Net, c[0], c[1])
			if gap+0.001 < clr {
				rep.add(Violation{
					Kind: KindViaTraceClearance, Severity: SeverityError,
					Message: fmt.Sprintf("via %s – trace %s: %.3f mm < %.3f mm", v.Net, tr.Net, gap, clr),
					Net:     v.Net, XMM: c[0], YMM: c[1],
				})
			}
		}
	}
}

func checkAnnularRing(board *core.Board, minAnnularMM float64, rep *Report) {
	if minAnnularMM <= 0 {
		return
	}
	for _, v := range board.Vias {
		ring := (v.Diameter.ToMM() - v.Drill.ToMM()) / 2
		if ring+1e-6 < minAnnularMM {
			rep.add(Violation{
				Kind: KindAnnularRing, Severity: SeverityError,
				Message: fmt.Sprintf("via %s annular %.3f mm < min %.3f mm", v.Net, ring, minAnnularMM),
				Net:     v.Net, XMM: v.Position.X.ToMM(), YMM: v.Position.Y.ToMM(),
			})
		}
	}
	for _, id := range board.FootprintOrder {
		fp := board.Footprints[id]
		if fp == nil {
			continue
		}
		for i := range fp.Pads {
			pad := &fp.Pads[i]
			if pad.Drill == nil {
				continue
			}
			w, h := core.PadWorldSize(fp, pad)
			minDim := w.ToMM()
			if h.ToMM() < minDim {
				minDim = h.ToMM()
			}
			ring := (minDim - pad.Drill.ToMM()) / 2
			if ring+1e-6 < minAnnularMM {
				c := core.PadWorldCenter(fp, pad)
				net := ""
				if pad.Net != nil {
					net = *pad.Net
				}
				rep.add(Violation{
					Kind: KindAnnularRing, Severity: SeverityError,
					Message: fmt.Sprintf("PTH %s.%s annular %.3f mm < min %.3f mm", fp.Reference, pad.Number, ring, minAnnularMM),
					Net:     net, XMM: c.X.ToMM(), YMM: c.Y.ToMM(),
				})
			}
		}
	}
}

func checkCourtyardOverlap(board *core.Board, rep *Report) {
	type body struct {
		ref      string
		rect     core.Rect
		elevated bool
	}
	var bodies []body
	for _, id := range board.FootprintOrder {
		fp := board.Footprints[id]
		if fp == nil {
			continue
		}
		r, ok := core.CourtyardWorld(fp)
		if !ok {
			continue
		}
		bodies = append(bodies, body{ref: fp.Reference, rect: r, elevated: fp.Elevated})
	}
	seen := map[string]bool{}
	for _, id := range board.FootprintOrder {
		seen[id] = true
	}
	var extras []string
	for id := range board.Footprints {
		if !seen[id] {
			extras = append(extras, id)
		}
	}
	sort.Strings(extras)
	for _, id := range extras {
		fp := board.Footprints[id]
		if fp == nil {
			continue
		}
		r, ok := core.CourtyardWorld(fp)
		if !ok {
			continue
		}
		bodies = append(bodies, body{ref: fp.Reference, rect: r, elevated: fp.Elevated})
	}
	for i := 0; i < len(bodies); i++ {
		for j := i + 1; j < len(bodies); j++ {
			a, b := bodies[i], bodies[j]
			if a.elevated != b.elevated {
				continue // elevated body may overlap a low one
			}
			if !a.rect.Intersects(b.rect) {
				continue
			}
			cx := (a.rect.Min.X.ToMM() + a.rect.Max.X.ToMM() + b.rect.Min.X.ToMM() + b.rect.Max.X.ToMM()) / 4
			cy := (a.rect.Min.Y.ToMM() + a.rect.Max.Y.ToMM() + b.rect.Min.Y.ToMM() + b.rect.Max.Y.ToMM()) / 4
			rep.add(Violation{
				Kind: KindCourtyardOverlap, Severity: SeverityError,
				Message: fmt.Sprintf("courtyard overlap %s – %s", a.ref, b.ref),
				XMM:     cx, YMM: cy,
			})
		}
	}
}

func checkTracePad(board *core.Board, pads []padGeom, res *core.RuleResolver, rep *Report) {
	for _, tr := range board.Traces {
		half := tr.Width.ToMM() / 2
		a, b := ptMM(tr.Start), ptMM(tr.End)
		for _, pad := range pads {
			if !pad.occupiesLayer(tr.Layer) {
				continue
			}
			if pad.net != "" && pad.net == tr.Net {
				continue
			}
			gap := segmentAABBDistance(a, b, pad.rect) - half
			// Resolve clearance at the closest approach site (Rust), not pad centre —
			// so a segment that dips into a fine-pitch rule area is legal there.
			sx, sy := segRectClosestSite(a, b, pad.rect)
			clr := clearanceMM(res, tr.Net, pad.net, sx, sy)
			if gap+0.001 < clr {
				cx, cy := rectCenterMM(pad.rect)
				rep.add(Violation{
					Kind: KindTracePadClearance, Severity: SeverityError,
					Message: fmt.Sprintf("trace %s – pad %s: %.3f mm < %.3f mm", tr.Net, pad.label(), gap, clr),
					Net:     tr.Net, XMM: cx, YMM: cy,
				})
			}
		}
	}
}

func closestPointOnSegment(p, a, b [2]float64) [2]float64 {
	dx, dy := b[0]-a[0], b[1]-a[1]
	len2 := dx*dx + dy*dy
	if len2 < 1e-24 {
		return a
	}
	t := ((p[0]-a[0])*dx + (p[1]-a[1])*dy) / len2
	if t < 0 {
		t = 0
	} else if t > 1 {
		t = 1
	}
	return [2]float64{a[0] + t*dx, a[1] + t*dy}
}

func clampPointToRect(p [2]float64, r core.Rect) [2]float64 {
	x := math.Min(math.Max(p[0], r.Min.X.ToMM()), r.Max.X.ToMM())
	y := math.Min(math.Max(p[1], r.Min.Y.ToMM()), r.Max.Y.ToMM())
	return [2]float64{x, y}
}

func segRectClosestSite(a, b [2]float64, rect core.Rect) (float64, float64) {
	c := [2]float64{}
	c[0], c[1] = rectCenterMM(rect)
	onSeg := closestPointOnSegment(c, a, b)
	onRect := clampPointToRect(onSeg, rect)
	onSeg = closestPointOnSegment(onRect, a, b)
	return (onSeg[0] + onRect[0]) / 2, (onSeg[1] + onRect[1]) / 2
}

func segSegClosestSite(a0, a1, b0, b1 [2]float64) (float64, float64) {
	type pair struct{ p, q [2]float64 }
	cands := []pair{
		{a0, closestPointOnSegment(a0, b0, b1)},
		{a1, closestPointOnSegment(a1, b0, b1)},
		{closestPointOnSegment(b0, a0, a1), b0},
		{closestPointOnSegment(b1, a0, a1), b1},
	}
	best := cands[0]
	bestD := math.Inf(1)
	for _, c := range cands {
		d := (c.p[0]-c.q[0])*(c.p[0]-c.q[0]) + (c.p[1]-c.q[1])*(c.p[1]-c.q[1])
		if d < bestD {
			bestD = d
			best = c
		}
	}
	return (best.p[0] + best.q[0]) / 2, (best.p[1] + best.q[1]) / 2
}

func rectRectClosestSite(a, b core.Rect) (float64, float64) {
	acx, acy := rectCenterMM(a)
	bcx, bcy := rectCenterMM(b)
	pa := clampPointToRect([2]float64{bcx, bcy}, a)
	pb := clampPointToRect([2]float64{acx, acy}, b)
	return (pa[0] + pb[0]) / 2, (pa[1] + pb[1]) / 2
}

func checkEdge(board *core.Board, pads []padGeom, outline core.Rect, edgeClMM float64, rep *Report) {
	ox0, oy0 := outline.Min.X.ToMM(), outline.Min.Y.ToMM()
	ox1, oy1 := outline.Max.X.ToMM(), outline.Max.Y.ToMM()
	edgeGap := func(x0, y0, x1, y1 float64) float64 {
		return math.Min(
			math.Min(x0-ox0, ox1-x1),
			math.Min(y0-oy0, oy1-y1),
		)
	}
	for _, pad := range pads {
		if pad.edgeMounted {
			continue
		}
		r := pad.rect
		gap := edgeGap(r.Min.X.ToMM(), r.Min.Y.ToMM(), r.Max.X.ToMM(), r.Max.Y.ToMM())
		if gap+1e-6 < edgeClMM {
			cx, cy := rectCenterMM(r)
			rep.add(Violation{
				Kind: KindEdgeClearance, Severity: SeverityError,
				Message: fmt.Sprintf("pad %s touches edge: %.3f mm < %.3f mm", pad.label(), gap, edgeClMM),
				Net:     pad.net, XMM: cx, YMM: cy,
			})
		}
	}
	for _, tr := range board.Traces {
		half := tr.Width.ToMM() / 2
		sx, sy := tr.Start.X.ToMM(), tr.Start.Y.ToMM()
		ex, ey := tr.End.X.ToMM(), tr.End.Y.ToMM()
		xmin := math.Min(sx, ex) - half
		xmax := math.Max(sx, ex) + half
		ymin := math.Min(sy, ey) - half
		ymax := math.Max(sy, ey) + half
		gap := edgeGap(xmin, ymin, xmax, ymax)
		if gap+1e-6 < edgeClMM {
			rep.add(Violation{
				Kind: KindEdgeClearance, Severity: SeverityError,
				Message: fmt.Sprintf("trace %s touches edge: %.3f mm < %.3f mm", tr.Net, gap, edgeClMM),
				Net:     tr.Net, XMM: (sx + ex) / 2, YMM: (sy + ey) / 2,
			})
		}
	}
	for _, v := range board.Vias {
		r := v.Diameter.ToMM() / 2
		cx, cy := v.Position.X.ToMM(), v.Position.Y.ToMM()
		gap := edgeGap(cx-r, cy-r, cx+r, cy+r)
		if gap+1e-6 < edgeClMM {
			rep.add(Violation{
				Kind: KindEdgeClearance, Severity: SeverityError,
				Message: fmt.Sprintf("via %s touches edge: %.3f mm < %.3f mm", v.Net, gap, edgeClMM),
				Net:     v.Net, XMM: cx, YMM: cy,
			})
		}
	}
}

// checkBodyOffBoard flags footprints whose pad-union AABB leaves the outline.
// Without library placement margins this is the body proxy available in core.
func checkBodyOffBoard(board *core.Board, outline core.Rect, rep *Report) {
	for _, id := range board.FootprintOrder {
		fp := board.Footprints[id]
		if fp == nil || len(fp.Pads) == 0 {
			continue
		}
		var body core.Rect
		for i := range fp.Pads {
			aabb := core.PadWorldAABB(fp, &fp.Pads[i])
			if i == 0 {
				body = aabb
			} else {
				body = body.Union(aabb)
			}
		}
		if rectInsideOutline(body, outline) {
			continue
		}
		cx, cy := rectCenterMM(body)
		rep.add(Violation{
			Kind: KindBodyOffBoard, Severity: SeverityError,
			Message: fmt.Sprintf("%s body extends past board outline", fp.Reference),
			XMM:     cx, YMM: cy,
		})
	}
}

func rectInsideOutline(r, outline core.Rect) bool {
	return r.Min.X >= outline.Min.X && r.Min.Y >= outline.Min.Y &&
		r.Max.X <= outline.Max.X && r.Max.Y <= outline.Max.Y
}

func checkNarrowTraces(board *core.Board, res *core.RuleResolver, rep *Report) {
	// Rust uses area override or defaults.trace_width only — not net-class
	// preferred width (class width is a router target, not a DRC floor).
	for _, tr := range board.Traces {
		mx := (tr.Start.X.ToMM() + tr.End.X.ToMM()) / 2
		my := (tr.Start.Y.ToMM() + tr.End.Y.ToMM()) / 2
		p := core.NewPoint(core.FromMM(mx), core.FromMM(my))
		minW := res.Defaults.TraceWidth.ToMM()
		// highest-priority area with TraceWidthMM wins (absolute)
		bestPrio := -1 << 30
		bestArea := -1.0
		for _, a := range res.Areas {
			if a.TraceWidthMM == nil || !a.Rect.ContainsPoint(p) {
				continue
			}
			area := a.Rect.Width().ToMM() * a.Rect.Height().ToMM()
			if a.Priority > bestPrio || (a.Priority == bestPrio && (bestArea < 0 || area < bestArea)) {
				bestPrio = a.Priority
				bestArea = area
				minW = *a.TraceWidthMM
			}
		}
		w := tr.Width.ToMM()
		if w+1e-6 < minW {
			rep.add(Violation{
				Kind: KindNarrowTrace, Severity: SeverityWarning,
				Message: fmt.Sprintf("trace %s is %.3f mm < min %.3f mm", tr.Net, w, minW),
				Net:     tr.Net, XMM: mx, YMM: my,
			})
		}
	}
}

func checkSmallDrills(board *core.Board, minDrillMM float64, rep *Report) {
	for _, v := range board.Vias {
		if v.Drill.ToMM()+1e-6 < minDrillMM {
			rep.add(Violation{
				Kind: KindSmallDrill, Severity: SeverityWarning,
				Message: fmt.Sprintf("via %s drill %.3f mm < min %.3f mm", v.Net, v.Drill.ToMM(), minDrillMM),
				Net:     v.Net, XMM: v.Position.X.ToMM(), YMM: v.Position.Y.ToMM(),
			})
		}
	}
}

func checkSmallComponentDangling(board *core.Board, pads []padGeom, rep *Report) {
	// Build lookup pad geom by ref.number
	byKey := map[string]padGeom{}
	for _, p := range pads {
		byKey[p.label()] = p
	}
	var refs []string
	fps := map[string]*core.Footprint{}
	for _, id := range board.FootprintOrder {
		fp := board.Footprints[id]
		if fp == nil {
			continue
		}
		refs = append(refs, fp.Reference)
		fps[fp.Reference] = fp
	}
	// extras
	for _, fp := range board.Footprints {
		if fp == nil {
			continue
		}
		if _, ok := fps[fp.Reference]; !ok {
			refs = append(refs, fp.Reference)
			fps[fp.Reference] = fp
		}
	}
	sort.Strings(refs)
	for _, ref := range refs {
		fp := fps[ref]
		if len(fp.Pads) >= smallComponentPadLimit {
			continue
		}
		var dangling []string
		for i := range fp.Pads {
			pad := &fp.Pads[i]
			key := ref + "." + pad.Number
			pg, ok := byKey[key]
			connected := false
			if pad.Net != nil && *pad.Net != "" && ok {
				net := *pad.Net
				connected = padHasSameNetNeighbour(pads, pg) ||
					traceTouchesPad(board, pg, net) ||
					viaTouchesPad(board, pg, net) ||
					pourCoversPad(board, pg, net)
			}
			if !connected {
				dangling = append(dangling, key)
			}
		}
		if len(dangling) == 0 {
			continue
		}
		rep.add(Violation{
			Kind: KindSmallComponentDangling, Severity: SeverityWarning,
			Message: fmt.Sprintf("%s (%d pads) has dangling pad(s): %s", ref, len(fp.Pads), strings.Join(dangling, ", ")),
			XMM:     fp.Position.X.ToMM(), YMM: fp.Position.Y.ToMM(),
		})
	}
}

func checkRoutingInefficient(board *core.Board, opts Options, rep *Report) {
	pourNets := map[string]bool{}
	for _, p := range board.Pours {
		pourNets[p.Net] = true
	}
	netPads := map[string][][2]float64{}
	for _, fp := range board.Footprints {
		if fp == nil {
			continue
		}
		for i := range fp.Pads {
			pad := &fp.Pads[i]
			if pad.Net == nil || *pad.Net == "" || pourNets[*pad.Net] {
				continue
			}
			c := core.PadWorldCenter(fp, pad)
			netPads[*pad.Net] = append(netPads[*pad.Net], [2]float64{c.X.ToMM(), c.Y.ToMM()})
		}
	}
	netLen := map[string]float64{}
	for _, tr := range board.Traces {
		dx := tr.End.X.ToMM() - tr.Start.X.ToMM()
		dy := tr.End.Y.ToMM() - tr.Start.Y.ToMM()
		netLen[tr.Net] += math.Hypot(dx, dy)
	}
	threshold := opts.RoutingInefficientRatio
	if threshold <= 0 {
		threshold = 1.5
	}
	nets := make([]string, 0, len(netPads))
	for n := range netPads {
		nets = append(nets, n)
	}
	sort.Strings(nets)
	for _, net := range nets {
		pads := netPads[net]
		if len(pads) < 2 {
			continue
		}
		actual := netLen[net]
		if actual <= 1e-6 {
			continue
		}
		minX, minY := pads[0][0], pads[0][1]
		maxX, maxY := minX, minY
		cx, cy := 0.0, 0.0
		for _, p := range pads {
			if p[0] < minX {
				minX = p[0]
			}
			if p[0] > maxX {
				maxX = p[0]
			}
			if p[1] < minY {
				minY = p[1]
			}
			if p[1] > maxY {
				maxY = p[1]
			}
			cx += p[0]
			cy += p[1]
		}
		hpwl := (maxX - minX) + (maxY - minY)
		if hpwl < 1e-3 {
			continue
		}
		ratio := actual / hpwl
		if ratio > threshold {
			rep.add(Violation{
				Kind: KindRoutingInefficient, Severity: SeverityWarning,
				Message: fmt.Sprintf("net %s routing ratio %.2f > %.2f (actual %.2f / HPWL %.2f)", net, ratio, threshold, actual, hpwl),
				Net:     net, XMM: cx / float64(len(pads)), YMM: cy / float64(len(pads)),
			})
		}
	}
}

func checkRuleAreasVsFab(board *core.Board, rep *Report) {
	fr := board.FabRules
	if fr == nil {
		return
	}
	for _, area := range board.RuleAreas {
		cx := (area.Rect.Min.X.ToMM() + area.Rect.Max.X.ToMM()) / 2
		cy := (area.Rect.Min.Y.ToMM() + area.Rect.Max.Y.ToMM()) / 2
		warn := func(what string, got, min float64) {
			rep.add(Violation{
				Kind: KindRuleBelowFabLimit, Severity: SeverityWarning,
				Message: fmt.Sprintf("rule area `%s` %s %.3f mm < %s min %.3f mm", area.Name, what, got, fr.Preset, min),
				XMM:     cx, YMM: cy,
			})
		}
		if area.ClearanceMM != nil && *area.ClearanceMM+1e-9 < fr.MinClearanceMM {
			warn("clearance", *area.ClearanceMM, fr.MinClearanceMM)
		}
		if area.TraceWidthMM != nil && *area.TraceWidthMM+1e-9 < fr.MinTraceWidthMM {
			warn("trace width", *area.TraceWidthMM, fr.MinTraceWidthMM)
		}
		if area.ViaDrillMM != nil && *area.ViaDrillMM+1e-9 < fr.MinViaDrillMM {
			warn("via drill", *area.ViaDrillMM, fr.MinViaDrillMM)
		}
		if area.ViaDiameterMM != nil && *area.ViaDiameterMM+1e-9 < fr.MinViaDiameterMM {
			warn("via diameter", *area.ViaDiameterMM, fr.MinViaDiameterMM)
		}
	}
}

func checkIsolatedPours(board *core.Board, rep *Report) {
	for i := range board.Pours {
		pr := &board.Pours[i]
		hasVia := pourHasTieVia(board, pr)
		otherLayer := pourHasOtherLayerCopper(board, pr)
		if pr.StitchRequested() && !hasVia {
			cx, cy := pourMarker(board, pr)
			rep.add(Violation{
				Kind: KindUnstitchedPour, Severity: SeverityError,
				Message: fmt.Sprintf("pour %s on layer %d requested stitching but has no stitch via", pr.Net, pr.Layer.Index),
				Net:     pr.Net, XMM: cx, YMM: cy,
			})
		}
		if otherLayer && !hasVia {
			cx, cy := pourMarker(board, pr)
			rep.add(Violation{
				Kind: KindIsolatedPour, Severity: SeverityError,
				Message: fmt.Sprintf("pour %s on layer %d is isolated (no via to the rest of the net)", pr.Net, pr.Layer.Index),
				Net:     pr.Net, XMM: cx, YMM: cy,
			})
		}
	}
}

func pourHasTieVia(board *core.Board, pr *core.Pour) bool {
	for _, v := range board.Vias {
		if v.Net != pr.Net {
			continue
		}
		if pointInPour(board, *pr, v.Position.X.ToMM(), v.Position.Y.ToMM()) {
			return true
		}
	}
	for _, id := range board.FootprintOrder {
		fp := board.Footprints[id]
		if fp == nil {
			continue
		}
		for i := range fp.Pads {
			pad := &fp.Pads[i]
			if pad.Drill == nil || pad.Net == nil || *pad.Net != pr.Net {
				continue
			}
			c := core.PadWorldCenter(fp, pad)
			if pointInPour(board, *pr, c.X.ToMM(), c.Y.ToMM()) {
				return true
			}
		}
	}
	return false
}

func pourHasOtherLayerCopper(board *core.Board, pr *core.Pour) bool {
	for _, fp := range board.Footprints {
		if fp == nil {
			continue
		}
		for i := range fp.Pads {
			pad := &fp.Pads[i]
			if pad.Net == nil || *pad.Net != pr.Net {
				continue
			}
			if pad.Drill != nil {
				continue
			}
			if pad.Layer.Index != pr.Layer.Index {
				return true
			}
		}
	}
	for _, t := range board.Traces {
		if t.Net == pr.Net && t.Layer.Index != pr.Layer.Index {
			return true
		}
	}
	return false
}

func pourMarker(board *core.Board, pr *core.Pour) (float64, float64) {
	if len(pr.Polygon) > 0 {
		return pr.Polygon[0].X.ToMM(), pr.Polygon[0].Y.ToMM()
	}
	if board.Outline != nil {
		return (board.Outline.Min.X.ToMM() + board.Outline.Max.X.ToMM()) / 2,
			(board.Outline.Min.Y.ToMM() + board.Outline.Max.Y.ToMM()) / 2
	}
	return 0, 0
}

type hole struct {
	x, y, r float64
	label   string
}

func checkHoleToHole(board *core.Board, minGapMM float64, rep *Report) {
	var holes []hole
	for _, v := range board.Vias {
		holes = append(holes, hole{v.Position.X.ToMM(), v.Position.Y.ToMM(), v.Drill.ToMM() / 2, "via " + v.Net})
	}
	for _, id := range board.FootprintOrder {
		fp := board.Footprints[id]
		if fp == nil {
			continue
		}
		for i := range fp.Pads {
			pad := &fp.Pads[i]
			if pad.Drill == nil {
				continue
			}
			c := core.PadWorldCenter(fp, pad)
			holes = append(holes, hole{c.X.ToMM(), c.Y.ToMM(), pad.Drill.ToMM() / 2, fp.Reference + "." + pad.Number})
		}
	}
	for _, h := range board.MountHoles {
		holes = append(holes, hole{h.Center.X.ToMM(), h.Center.Y.ToMM(), h.Diameter.ToMM() / 2, "NPTH"})
	}
	for _, h := range board.Holes {
		holes = append(holes, hole{h.Center.X.ToMM(), h.Center.Y.ToMM(), h.Diameter.ToMM() / 2, "NPTH"})
	}
	for i := 0; i < len(holes); i++ {
		for j := i + 1; j < len(holes); j++ {
			a, b := holes[i], holes[j]
			gap := math.Hypot(a.x-b.x, a.y-b.y) - a.r - b.r
			if gap+1e-6 < minGapMM {
				rep.add(Violation{
					Kind: KindHoleToHole, Severity: SeverityError,
					Message: fmt.Sprintf("hole-to-hole %s – %s: %.3f mm < %.3f mm", a.label, b.label, gap, minGapMM),
					XMM:     (a.x + b.x) / 2, YMM: (a.y + b.y) / 2,
				})
			}
		}
	}
}

func checkCopperSliver(board *core.Board, pads []padGeom, minSliverMM float64, rep *Report) {
	// Same-net copper with a positive gap thinner than min sliver (acid trap).
	for i := 0; i < len(pads); i++ {
		for j := i + 1; j < len(pads); j++ {
			a, b := pads[i], pads[j]
			if a.net == "" || a.net != b.net || !a.sharesLayerWith(b) {
				continue
			}
			gap := aabbGapMM(a.rect, b.rect)
			if gap > TouchTolMM && gap+1e-6 < minSliverMM {
				sx, sy := rectRectClosestSite(a.rect, b.rect)
				rep.add(Violation{
					Kind: KindCopperSliver, Severity: SeverityWarning,
					Message: fmt.Sprintf("copper sliver %s – %s: %.3f mm < %.3f mm", a.label(), b.label(), gap, minSliverMM),
					Net:     a.net, XMM: sx, YMM: sy,
				})
			}
		}
	}
	for _, v := range board.Vias {
		c := ptMM(v.Position)
		vr := v.Diameter.ToMM() / 2
		for _, pad := range pads {
			if pad.net == "" || pad.net != v.Net {
				continue
			}
			gap := pointRectDistance(c, pad.rect) - vr
			if gap > TouchTolMM && gap+1e-6 < minSliverMM {
				rep.add(Violation{
					Kind: KindCopperSliver, Severity: SeverityWarning,
					Message: fmt.Sprintf("copper sliver via %s – pad %s: %.3f mm < %.3f mm", v.Net, pad.label(), gap, minSliverMM),
					Net:     v.Net, XMM: c[0], YMM: c[1],
				})
			}
		}
	}
}

func checkUnconnectedPads(board *core.Board, pads []padGeom, rep *Report) {
	for _, pad := range pads {
		if pad.net == "" {
			continue
		}
		if padHasSameNetNeighbour(pads, pad) ||
			traceTouchesPad(board, pad, pad.net) ||
			viaTouchesPad(board, pad, pad.net) ||
			pourCoversPad(board, pad, pad.net) {
			continue
		}
		cx, cy := rectCenterMM(pad.rect)
		rep.add(Violation{
			Kind: KindUnconnectedPad, Severity: SeverityWarning,
			Message: fmt.Sprintf("pad %s on net %s has no copper", pad.label(), pad.net),
			Net:     pad.net, XMM: cx, YMM: cy,
		})
	}
}

func padHasSameNetNeighbour(pads []padGeom, pad padGeom) bool {
	for _, o := range pads {
		if o.ref == pad.ref && o.number == pad.number {
			continue
		}
		if !o.sharesLayerWith(pad) || o.net != pad.net {
			continue
		}
		if aabbGapMM(pad.rect, o.rect) <= 0 {
			return true
		}
	}
	return false
}

func traceTouchesPad(board *core.Board, pad padGeom, net string) bool {
	for _, tr := range board.Traces {
		if tr.Net != net || !pad.occupiesLayer(tr.Layer) {
			continue
		}
		d := segmentAABBDistance(ptMM(tr.Start), ptMM(tr.End), pad.rect)
		if d-tr.Width.ToMM()/2 <= TouchTolMM {
			return true
		}
	}
	return false
}

func viaTouchesPad(board *core.Board, pad padGeom, net string) bool {
	for _, v := range board.Vias {
		if v.Net != net {
			continue
		}
		r := v.Diameter.ToMM() / 2
		if pointRectDistance(ptMM(v.Position), pad.rect)-r <= TouchTolMM {
			return true
		}
	}
	return false
}

func pourCoversPad(board *core.Board, pad padGeom, net string) bool {
	cx, cy := rectCenterMM(pad.rect)
	for _, p := range board.Pours {
		if p.Net != net || !pad.occupiesLayer(p.Layer) {
			continue
		}
		if pointInPour(board, p, cx, cy) {
			return true
		}
	}
	return false
}

// pourEdgeInsetMM matches gerber pourEdgeClearance (0.3 mm) for outline pours.
const pourEdgeInsetMM = 0.3

func pointInPour(board *core.Board, p core.Pour, x, y float64) bool {
	if len(p.Polygon) >= 3 {
		return pointInPolygonMM(p.Polygon, x, y)
	}
	if board.Outline == nil {
		return false
	}
	r := *board.Outline
	inset := pourEdgeInsetMM
	return x >= r.Min.X.ToMM()+inset && x <= r.Max.X.ToMM()-inset &&
		y >= r.Min.Y.ToMM()+inset && y <= r.Max.Y.ToMM()-inset
}

func pointInPolygonMM(poly []core.Point, x, y float64) bool {
	n := len(poly)
	if n < 3 {
		return false
	}
	inside := false
	j := n - 1
	for i := 0; i < n; i++ {
		xi, yi := poly[i].X.ToMM(), poly[i].Y.ToMM()
		xj, yj := poly[j].X.ToMM(), poly[j].Y.ToMM()
		if (yi > y) != (yj > y) && x < (xj-xi)*(y-yi)/(yj-yi)+xi {
			inside = !inside
		}
		j = i
	}
	return inside
}

// ---------------------------------------------------------------------------
// Net continuity — union-find over copper elements (Rust parity).
// ---------------------------------------------------------------------------

type copperKind int

const (
	cuPad copperKind = iota
	cuTrace
	cuVia
	cuPour
)

type copperElem struct {
	net   string
	layer *core.Layer // nil ⇒ all layers (via / PTH)
	kind  copperKind
	// pad
	rect core.Rect
	// trace
	a, b [2]float64
	half float64
	// via
	c [2]float64
	r float64
	// pour
	pour *core.Pour
	// meta
	isPad  bool
	label  string
	center [2]float64
}

func layersOverlap(a, b *core.Layer) bool {
	if a == nil || b == nil {
		return true
	}
	return *a == *b
}

func buildCopperElems(board *core.Board, pads []padGeom) []copperElem {
	var elems []copperElem
	for _, p := range pads {
		if p.net == "" {
			continue
		}
		var layer *core.Layer
		if !p.throughHole {
			l := p.layer
			layer = &l
		}
		cx, cy := rectCenterMM(p.rect)
		elems = append(elems, copperElem{
			net: p.net, layer: layer, kind: cuPad, rect: p.rect,
			isPad: true, label: p.label(), center: [2]float64{cx, cy},
		})
	}
	for _, tr := range board.Traces {
		a, b := ptMM(tr.Start), ptMM(tr.End)
		l := tr.Layer
		elems = append(elems, copperElem{
			net: tr.Net, layer: &l, kind: cuTrace, a: a, b: b, half: tr.Width.ToMM() / 2,
			label: "trace", center: [2]float64{(a[0] + b[0]) / 2, (a[1] + b[1]) / 2},
		})
	}
	for _, v := range board.Vias {
		c := ptMM(v.Position)
		elems = append(elems, copperElem{
			net: v.Net, layer: nil, kind: cuVia, c: c, r: v.Diameter.ToMM() / 2,
			label: "via", center: c,
		})
	}
	for i := range board.Pours {
		pr := &board.Pours[i]
		l := pr.Layer
		elems = append(elems, copperElem{
			net: pr.Net, layer: &l, kind: cuPour, pour: pr, label: "pour",
		})
	}
	return elems
}

func pourTouchesElem(board *core.Board, pour *copperElem, o *copperElem) bool {
	if pour.pour == nil {
		return false
	}
	switch o.kind {
	case cuPad:
		cx, cy := rectCenterMM(o.rect)
		return pointInPour(board, *pour.pour, cx, cy)
	case cuVia:
		return pointInPour(board, *pour.pour, o.c[0], o.c[1])
	case cuTrace:
		return pointInPour(board, *pour.pour, o.a[0], o.a[1]) ||
			pointInPour(board, *pour.pour, o.b[0], o.b[1])
	case cuPour:
		// Two outline pours on the same layer/net overlap.
		return true
	}
	return false
}

func elemsTouch(board *core.Board, a, b *copperElem) bool {
	if !layersOverlap(a.layer, b.layer) {
		return false
	}
	if a.kind == cuPour {
		return pourTouchesElem(board, a, b)
	}
	if b.kind == cuPour {
		return pourTouchesElem(board, b, a)
	}
	switch {
	case a.kind == cuPad && b.kind == cuPad:
		return aabbGapMM(a.rect, b.rect) <= TouchTolMM
	case a.kind == cuPad && b.kind == cuTrace:
		return segmentAABBDistance(b.a, b.b, a.rect)-b.half <= TouchTolMM
	case a.kind == cuTrace && b.kind == cuPad:
		return segmentAABBDistance(a.a, a.b, b.rect)-a.half <= TouchTolMM
	case a.kind == cuPad && b.kind == cuVia:
		return pointRectDistance(b.c, a.rect)-b.r <= TouchTolMM
	case a.kind == cuVia && b.kind == cuPad:
		return pointRectDistance(a.c, b.rect)-a.r <= TouchTolMM
	case a.kind == cuTrace && b.kind == cuTrace:
		return segmentSegmentDistance(a.a, a.b, b.a, b.b)-(a.half+b.half) <= TouchTolMM
	case a.kind == cuTrace && b.kind == cuVia:
		return pointSegmentDistance(b.c, a.a, a.b)-(a.half+b.r) <= TouchTolMM
	case a.kind == cuVia && b.kind == cuTrace:
		return pointSegmentDistance(a.c, b.a, b.b)-(b.half+a.r) <= TouchTolMM
	case a.kind == cuVia && b.kind == cuVia:
		dx, dy := a.c[0]-b.c[0], a.c[1]-b.c[1]
		return math.Hypot(dx, dy)-(a.r+b.r) <= TouchTolMM
	}
	return false
}

type unionFind struct {
	parent []int
	rank   []uint8
}

func newUF(n int) *unionFind {
	p := make([]int, n)
	for i := range p {
		p[i] = i
	}
	return &unionFind{parent: p, rank: make([]uint8, n)}
}

func (u *unionFind) find(x int) int {
	for u.parent[x] != x {
		u.parent[x] = u.parent[u.parent[x]]
		x = u.parent[x]
	}
	return x
}

func (u *unionFind) union(a, b int) {
	ra, rb := u.find(a), u.find(b)
	if ra == rb {
		return
	}
	switch {
	case u.rank[ra] < u.rank[rb]:
		u.parent[ra] = rb
	case u.rank[ra] > u.rank[rb]:
		u.parent[rb] = ra
	default:
		u.parent[rb] = ra
		u.rank[ra]++
	}
}

func checkNetContinuity(board *core.Board, pads []padGeom, rep *Report) {
	elems := buildCopperElems(board, pads)
	n := len(elems)
	if n == 0 {
		return
	}

	// Group indices by net (deterministic: sort net names later).
	byNet := map[string][]int{}
	for i, e := range elems {
		byNet[e.net] = append(byNet[e.net], i)
	}
	nets := make([]string, 0, len(byNet))
	for net := range byNet {
		nets = append(nets, net)
	}
	sort.Strings(nets)

	for _, net := range nets {
		idxs := byNet[net]
		var padIdxs []int
		for _, i := range idxs {
			if elems[i].isPad {
				padIdxs = append(padIdxs, i)
			}
		}
		if len(padIdxs) < 2 {
			continue
		}

		uf := newUF(n)
		touched := make([]bool, n)
		for ai := 0; ai < len(idxs); ai++ {
			for bi := ai + 1; bi < len(idxs); bi++ {
				ia, ib := idxs[ai], idxs[bi]
				if elemsTouch(board, &elems[ia], &elems[ib]) {
					uf.union(ia, ib)
					touched[ia] = true
					touched[ib] = true
				}
			}
		}

		// Islands of pads that touch *some* same-net copper.
		islands := map[int][]int{}
		for _, pi := range padIdxs {
			if !touched[pi] {
				continue
			}
			root := uf.find(pi)
			islands[root] = append(islands[root], pi)
		}
		if len(islands) < 2 {
			continue
		}

		// Deterministic group listing: sort roots, then pad labels within.
		roots := make([]int, 0, len(islands))
		for r := range islands {
			roots = append(roots, r)
		}
		sort.Slice(roots, func(i, j int) bool {
			// Smallest island first (culprit); then root id.
			if len(islands[roots[i]]) != len(islands[roots[j]]) {
				return len(islands[roots[i]]) < len(islands[roots[j]])
			}
			return roots[i] < roots[j]
		})
		groupStrs := make([]string, 0, len(roots))
		for _, r := range roots {
			names := make([]string, len(islands[r]))
			for i, pi := range islands[r] {
				names[i] = elems[pi].label
			}
			sort.Strings(names)
			s := "{"
			for i, nm := range names {
				if i > 0 {
					s += ", "
				}
				s += nm
			}
			s += "}"
			groupStrs = append(groupStrs, s)
		}
		marker := elems[islands[roots[0]][0]].center
		rep.add(Violation{
			Kind: KindNetSplit, Severity: SeverityError,
			Message: fmt.Sprintf("net %q is split into %d isolated copper islands: %s",
				net, len(roots), joinPipe(groupStrs)),
			Net: net, XMM: marker[0], YMM: marker[1],
		})
	}
}

func joinPipe(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	s := parts[0]
	for i := 1; i < len(parts); i++ {
		s += " | " + parts[i]
	}
	return s
}

// ---------------------------------------------------------------------------
// Geometry helpers (mm)
// ---------------------------------------------------------------------------

func ptMM(p core.Point) [2]float64 {
	return [2]float64{p.X.ToMM(), p.Y.ToMM()}
}

func rectCenterMM(r core.Rect) (float64, float64) {
	return (r.Min.X.ToMM() + r.Max.X.ToMM()) / 2, (r.Min.Y.ToMM() + r.Max.Y.ToMM()) / 2
}

func aabbGapMM(a, b core.Rect) float64 {
	ax0, ay0 := a.Min.X.ToMM(), a.Min.Y.ToMM()
	ax1, ay1 := a.Max.X.ToMM(), a.Max.Y.ToMM()
	bx0, by0 := b.Min.X.ToMM(), b.Min.Y.ToMM()
	bx1, by1 := b.Max.X.ToMM(), b.Max.Y.ToMM()
	gapX := math.Max(0, math.Max(bx0-ax1, ax0-bx1))
	gapY := math.Max(0, math.Max(by0-ay1, ay0-by1))
	return math.Hypot(gapX, gapY)
}

func pointRectDistance(p [2]float64, r core.Rect) float64 {
	dx := math.Max(0, math.Max(r.Min.X.ToMM()-p[0], p[0]-r.Max.X.ToMM()))
	dy := math.Max(0, math.Max(r.Min.Y.ToMM()-p[1], p[1]-r.Max.Y.ToMM()))
	return math.Hypot(dx, dy)
}

func pointSegmentDistance(p, a, b [2]float64) float64 {
	dx, dy := b[0]-a[0], b[1]-a[1]
	len2 := dx*dx + dy*dy
	if len2 < 1e-24 {
		return math.Hypot(p[0]-a[0], p[1]-a[1])
	}
	t := ((p[0]-a[0])*dx + (p[1]-a[1])*dy) / len2
	if t < 0 {
		t = 0
	} else if t > 1 {
		t = 1
	}
	cx, cy := a[0]+t*dx, a[1]+t*dy
	return math.Hypot(p[0]-cx, p[1]-cy)
}

func segmentsIntersect(a0, a1, b0, b1 [2]float64) bool {
	orient := func(p, q, r [2]float64) float64 {
		return (q[0]-p[0])*(r[1]-p[1]) - (q[1]-p[1])*(r[0]-p[0])
	}
	o1 := orient(a0, a1, b0)
	o2 := orient(a0, a1, b1)
	o3 := orient(b0, b1, a0)
	o4 := orient(b0, b1, a1)
	return o1*o2 < 0 && o3*o4 < 0
}

func segmentSegmentDistance(a0, a1, b0, b1 [2]float64) float64 {
	if segmentsIntersect(a0, a1, b0, b1) {
		return 0
	}
	return math.Min(
		math.Min(pointSegmentDistance(a0, b0, b1), pointSegmentDistance(a1, b0, b1)),
		math.Min(pointSegmentDistance(b0, a0, a1), pointSegmentDistance(b1, a0, a1)),
	)
}

func segmentAABBDistance(a, b [2]float64, rect core.Rect) float64 {
	rx0, ry0 := rect.Min.X.ToMM(), rect.Min.Y.ToMM()
	rx1, ry1 := rect.Max.X.ToMM(), rect.Max.Y.ToMM()
	inside := func(p [2]float64) bool {
		return p[0] >= rx0 && p[0] <= rx1 && p[1] >= ry0 && p[1] <= ry1
	}
	if inside(a) || inside(b) {
		return 0
	}
	edges := [4][2][2]float64{
		{{rx0, ry0}, {rx1, ry0}},
		{{rx1, ry0}, {rx1, ry1}},
		{{rx1, ry1}, {rx0, ry1}},
		{{rx0, ry1}, {rx0, ry0}},
	}
	best := math.Inf(1)
	for _, e := range edges {
		d := segmentSegmentDistance(a, b, e[0], e[1])
		if d < best {
			best = d
		}
	}
	return best
}

// checkTeardropClearance treats teardrop copper as same-net: it never
// flags a teardrop against its own pad/via/trace. Other nets still see
// the extra copper.
func checkTeardropClearance(board *core.Board, pads []padGeom, res *core.RuleResolver, rep *Report) {
	tears := core.BuildTeardrops(board)
	if len(tears) == 0 {
		return
	}
	for _, td := range tears {
		aabb := td.AABB()
		cx := (aabb.Min.X.ToMM() + aabb.Max.X.ToMM()) / 2
		cy := (aabb.Min.Y.ToMM() + aabb.Max.Y.ToMM()) / 2
		for _, pad := range pads {
			if pad.net != "" && pad.net == td.Net {
				continue
			}
			if !pad.occupiesLayer(td.Layer) {
				continue
			}
			gap := polyRectGapFull(td.Poly, pad.rect)
			clr := clearanceMM(res, td.Net, pad.net, cx, cy)
			if gap+0.001 < clr {
				rep.add(Violation{
					Kind: KindTeardropClearance, Severity: SeverityError,
					Message: fmt.Sprintf("teardrop %s – pad %s: %.3f mm < %.3f mm", td.Net, pad.label(), gap, clr),
					Net:     td.Net, XMM: cx, YMM: cy,
				})
			}
		}
		for _, tr := range board.Traces {
			if tr.Net == td.Net || tr.Layer.Index != td.Layer.Index {
				continue
			}
			gap := polySegGap(td.Poly, ptMM(tr.Start), ptMM(tr.End)) - tr.Width.ToMM()/2
			clr := clearanceMM(res, td.Net, tr.Net, cx, cy)
			if gap+0.001 < clr {
				rep.add(Violation{
					Kind: KindTeardropClearance, Severity: SeverityError,
					Message: fmt.Sprintf("teardrop %s – trace %s: %.3f mm < %.3f mm", td.Net, tr.Net, gap, clr),
					Net:     td.Net, XMM: cx, YMM: cy,
				})
			}
		}
		for _, v := range board.Vias {
			if v.Net == td.Net {
				continue
			}
			gap := polyCircleGap(td.Poly, ptMM(v.Position), v.Diameter.ToMM()/2)
			clr := clearanceMM(res, td.Net, v.Net, cx, cy)
			if gap+0.001 < clr {
				rep.add(Violation{
					Kind: KindTeardropClearance, Severity: SeverityError,
					Message: fmt.Sprintf("teardrop %s – via %s: %.3f mm < %.3f mm", td.Net, v.Net, gap, clr),
					Net:     td.Net, XMM: cx, YMM: cy,
				})
			}
		}
	}
}

func polySegGap(poly []core.Point, a, b [2]float64) float64 {
	best := math.Inf(1)
	n := len(poly)
	for i := 0; i < n; i++ {
		p0, p1 := ptMM(poly[i]), ptMM(poly[(i+1)%n])
		d := segmentSegmentDistance(p0, p1, a, b)
		if d < best {
			best = d
		}
	}
	if pointInPolygonMM(poly, a[0], a[1]) || pointInPolygonMM(poly, b[0], b[1]) {
		return 0
	}
	return best
}

func polyCircleGap(poly []core.Point, c [2]float64, radius float64) float64 {
	if pointInPolygonMM(poly, c[0], c[1]) {
		return -radius
	}
	best := math.Inf(1)
	n := len(poly)
	for i := 0; i < n; i++ {
		d := pointSegmentDistance(c, ptMM(poly[i]), ptMM(poly[(i+1)%n]))
		if d < best {
			best = d
		}
	}
	return best - radius
}

func polyRectGapFull(poly []core.Point, r core.Rect) float64 {
	if len(poly) < 3 {
		return math.Inf(1)
	}
	if pointInPolygonMM(poly, r.Min.X.ToMM(), r.Min.Y.ToMM()) ||
		pointInPolygonMM(poly, r.Max.X.ToMM(), r.Min.Y.ToMM()) ||
		pointInPolygonMM(poly, r.Max.X.ToMM(), r.Max.Y.ToMM()) ||
		pointInPolygonMM(poly, r.Min.X.ToMM(), r.Max.Y.ToMM()) {
		return 0
	}
	for _, p := range poly {
		if r.ContainsPoint(p) {
			return 0
		}
	}
	best := math.Inf(1)
	n := len(poly)
	for i := 0; i < n; i++ {
		d := segmentAABBDistance(ptMM(poly[i]), ptMM(poly[(i+1)%n]), r)
		if d < best {
			best = d
		}
	}
	return best
}

func checkImpedance(board *core.Board, sch *core.Schematic, rep *Report) {
	if sch == nil {
		return
	}
	seen := map[string]bool{}
	consider := func(net string) {
		if net == "" || seen[net] {
			return
		}
		clsName := ""
		if n := sch.Nets[net]; n != nil && n.Class != "" {
			clsName = n.Class
		}
		if clsName == "" && sch.NetToClass != nil {
			clsName = sch.NetToClass[net]
		}
		if clsName == "" {
			return
		}
		cls := sch.NetClasses[clsName]
		if cls == nil || cls.ImpedanceOhms <= 0 {
			return
		}
		seen[net] = true
		r, err := impedance.AnalyzeNet(board, sch, net)
		if err != nil {
			rep.add(Violation{
				Kind: KindImpedanceMismatch, Severity: SeverityWarning,
				Message: err.Error(),
				Net:     net,
			})
			return
		}
		if r.WidthMM <= 0 || r.RequiredWMM <= 0 {
			return
		}
		pct := math.Abs(r.WidthMM-r.RequiredWMM) / r.RequiredWMM
		if pct > 0.10 {
			rep.add(Violation{
				Kind: KindImpedanceMismatch, Severity: SeverityWarning,
				Message: fmt.Sprintf("net %s width %.3f mm vs %.3f mm for %.0f Ω (%+.0f%%)",
					net, r.WidthMM, r.RequiredWMM, r.TargetOhms, 100*(r.WidthMM-r.RequiredWMM)/r.RequiredWMM),
				Net: net,
			})
		}
	}
	for name := range sch.Nets {
		consider(name)
	}
	for name := range sch.NetToClass {
		consider(name)
	}
	if board != nil {
		for i := range board.Traces {
			consider(board.Traces[i].Net)
		}
	}
}
