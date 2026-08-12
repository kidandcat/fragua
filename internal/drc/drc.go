// Package drc implements geometric design rule checking.
package drc

import (
	"fmt"
	"math"
	"sort"

	"github.com/mentasystems/fragua/internal/core"
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
	KindPadPadClearance     Kind = "pad_pad_clearance"
	KindTraceTraceClearance Kind = "trace_trace_clearance"
	KindTracePadClearance   Kind = "trace_pad_clearance"
	KindViaPadClearance     Kind = "via_pad_clearance"
	KindEdgeClearance       Kind = "edge_clearance"
	KindUnconnectedPad      Kind = "unconnected_pad"
	KindNetSplit            Kind = "net_split"
	KindMinTraceWidth       Kind = "min_trace_width"
	KindMinDrill            Kind = "min_drill"
	KindBodyOverlap         Kind = "body_overlap"
	KindBodyOffBoard        Kind = "body_off_board"
	KindRoutingInefficient  Kind = "routing_inefficient"
	KindAnnularRing         Kind = "annular_ring"
)

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
	RoutingInefficientRatio float64
	FabProfile              *core.FabProfileHandle
}

// DefaultOptions matches Rust baseline (0.2 mm clearance).
func DefaultOptions() Options {
	return Options{
		MinClearance:            core.FromMM(0.2),
		EdgeClearance:           core.FromMM(0.3),
		MinTraceWidth:           core.FromMM(0.1),
		MinDrill:                core.FromMM(0.2),
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
func Check(board *core.Board, sch *core.Schematic, opts Options) Report {
	var rep Report
	if board == nil {
		return rep
	}
	minCl := opts.MinClearance
	edgeCl := opts.EdgeClearance
	minW := opts.MinTraceWidth
	minDrill := opts.MinDrill
	if opts.FabProfile != nil {
		if c := core.FromMM(opts.FabProfile.MinClearanceMM); c > minCl {
			minCl = c
		}
		if c := core.FromMM(opts.FabProfile.MinEdgeClearanceMM); c > edgeCl {
			edgeCl = c
		}
		if c := core.FromMM(opts.FabProfile.MinTraceWidthMM); c > minW {
			minW = c
		}
		if c := core.FromMM(opts.FabProfile.MinDrillMM); c > minDrill {
			minDrill = c
		}
	}
	minClMM := minCl.ToMM()
	edgeClMM := edgeCl.ToMM()
	minWMM := minW.ToMM()
	minDrillMM := minDrill.ToMM()
	minRing := core.FromMM(0.1)
	if opts.FabProfile != nil {
		minRing = core.FromMM(opts.FabProfile.MinAnnularRingMM)
	}

	pads := collectPads(board)

	checkPadPad(pads, minClMM, &rep)
	checkTraceTrace(board, minClMM, &rep)
	checkTracePad(board, pads, minClMM, &rep)
	checkViaPad(board, pads, minClMM, &rep)
	if board.Outline != nil {
		checkEdge(board, pads, *board.Outline, edgeClMM, &rep)
		checkBodyOffBoard(board, *board.Outline, &rep)
	}
	checkMinTraceWidth(board, minWMM, &rep)
	checkVias(board, minDrillMM, minRing, &rep)
	checkUnconnectedPads(board, pads, &rep)
	checkNetContinuity(board, pads, &rep)

	return rep
}

func checkPadPad(pads []padGeom, minClMM float64, rep *Report) {
	for i := 0; i < len(pads); i++ {
		for j := i + 1; j < len(pads); j++ {
			a, b := pads[i], pads[j]
			if !a.sharesLayerWith(b) {
				continue
			}
			// Same assigned net: not a clearance violation. Unassigned pads still clear.
			if a.net != "" && a.net == b.net {
				continue
			}
			gap := aabbGapMM(a.rect, b.rect)
			if gap+1e-6 < minClMM {
				cx, cy := rectCenterMM(a.rect)
				rep.add(Violation{
					Kind: KindPadPadClearance, Severity: SeverityError,
					Message: fmt.Sprintf("pad %s – pad %s: %.3f mm < %.3f mm", a.label(), b.label(), gap, minClMM),
					Net:     a.net, XMM: cx, YMM: cy,
				})
			}
		}
	}
}

func checkTraceTrace(board *core.Board, minClMM float64, rep *Report) {
	trs := board.Traces
	for i := 0; i < len(trs); i++ {
		for j := i + 1; j < len(trs); j++ {
			a, b := trs[i], trs[j]
			if a.Layer != b.Layer || a.Net == b.Net {
				continue
			}
			a0 := ptMM(a.Start)
			a1 := ptMM(a.End)
			b0 := ptMM(b.Start)
			b1 := ptMM(b.End)
			halfA := a.Width.ToMM() / 2
			halfB := b.Width.ToMM() / 2
			gap := segmentSegmentDistance(a0, a1, b0, b1) - halfA - halfB
			if gap+1e-6 < minClMM {
				mx := (a0[0] + a1[0] + b0[0] + b1[0]) / 4
				my := (a0[1] + a1[1] + b0[1] + b1[1]) / 4
				rep.add(Violation{
					Kind: KindTraceTraceClearance, Severity: SeverityError,
					Message: fmt.Sprintf("trace %s – trace %s: %.3f mm < %.3f mm", a.Net, b.Net, gap, minClMM),
					Net:     a.Net, XMM: mx, YMM: my,
				})
			}
		}
	}
}

func checkTracePad(board *core.Board, pads []padGeom, minClMM float64, rep *Report) {
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
			if gap+1e-6 < minClMM {
				cx, cy := rectCenterMM(pad.rect)
				rep.add(Violation{
					Kind: KindTracePadClearance, Severity: SeverityError,
					Message: fmt.Sprintf("trace %s – pad %s: %.3f mm < %.3f mm", tr.Net, pad.label(), gap, minClMM),
					Net:     tr.Net, XMM: cx, YMM: cy,
				})
			}
		}
	}
}

func checkViaPad(board *core.Board, pads []padGeom, minClMM float64, rep *Report) {
	for _, v := range board.Vias {
		r := v.Diameter.ToMM() / 2
		c := ptMM(v.Position)
		for _, pad := range pads {
			if pad.net != "" && pad.net == v.Net {
				continue
			}
			// Via is through-all copper; always shares a layer with the pad.
			gap := pointRectDistance(c, pad.rect) - r
			if gap+1e-6 < minClMM {
				cx, cy := rectCenterMM(pad.rect)
				rep.add(Violation{
					Kind: KindViaPadClearance, Severity: SeverityError,
					Message: fmt.Sprintf("via %s – pad %s: %.3f mm < %.3f mm", v.Net, pad.label(), gap, minClMM),
					Net:     v.Net, XMM: cx, YMM: cy,
				})
			}
		}
	}
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

func checkMinTraceWidth(board *core.Board, minWMM float64, rep *Report) {
	for _, tr := range board.Traces {
		if tr.Width.ToMM()+1e-6 < minWMM {
			rep.add(Violation{
				Kind: KindMinTraceWidth, Severity: SeverityError,
				Message: fmt.Sprintf("trace net %s width %.3f < %.3f mm", tr.Net, tr.Width.ToMM(), minWMM),
				Net:     tr.Net, XMM: tr.Start.X.ToMM(), YMM: tr.Start.Y.ToMM(),
			})
		}
	}
}

func checkVias(board *core.Board, minDrillMM float64, minRing core.Length, rep *Report) {
	for _, v := range board.Vias {
		if v.Drill.ToMM()+1e-6 < minDrillMM {
			rep.add(Violation{
				Kind: KindMinDrill, Severity: SeverityError,
				Message: fmt.Sprintf("via net %s drill %.3f < %.3f mm", v.Net, v.Drill.ToMM(), minDrillMM),
				Net:     v.Net, XMM: v.Position.X.ToMM(), YMM: v.Position.Y.ToMM(),
			})
		}
		ring := (v.Diameter - v.Drill) / 2
		if ring < minRing {
			rep.add(Violation{
				Kind: KindAnnularRing, Severity: SeverityError,
				Message: fmt.Sprintf("via net %s annular %.3f mm", v.Net, ring.ToMM()),
				Net:     v.Net, XMM: v.Position.X.ToMM(), YMM: v.Position.Y.ToMM(),
			})
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
	for _, p := range board.Pours {
		if p.Net == net && pad.occupiesLayer(p.Layer) {
			return true
		}
	}
	return false
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
	for _, pr := range board.Pours {
		l := pr.Layer
		elems = append(elems, copperElem{
			net: pr.Net, layer: &l, kind: cuPour, label: "pour",
		})
	}
	return elems
}

func elemsTouch(a, b *copperElem) bool {
	if !layersOverlap(a.layer, b.layer) {
		return false
	}
	// Normalize so a.kind <= b.kind for fewer cases — handle pairs explicitly.
	if a.kind == cuPour || b.kind == cuPour {
		return true
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
				if elemsTouch(&elems[ia], &elems[ib]) {
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
