package placer

import (
	"math"
	"strings"

	"github.com/mentasystems/fragua/internal/core"
)

// EdgeDrillMarginMM is the extra distance, on top of the fab copper-to-edge
// clearance, that a drilled barrel keeps from the routed outline. The routing
// bit wanders more than the copper etch does and a hole that breaks out of the
// edge scraps the panel, so a barrel gets a wider berth than a pad.
const EdgeDrillMarginMM = 0.2

// EdgeClearances is the copper/drill-to-edge floor edge-place respects.
type EdgeClearances struct {
	CopperMM float64
	DrillMM  float64
}

// EdgeClearancesFor reads the board's fab profile (JLCPCB 2-layer is the
// documented default) and derives the two edge floors from it.
func EdgeClearancesFor(b *core.Board) EdgeClearances {
	cl := core.ActiveFabRules(b).MinEdgeClearanceMM
	if cl <= 0 {
		cl = core.DefaultRules().EdgeClearance.ToMM()
	}
	return EdgeClearances{CopperMM: cl, DrillMM: cl + EdgeDrillMarginMM}
}

// hasDrilledPad reports a through-hole footprint: at least one pad is a
// plated barrel rather than a surface pad.
func hasDrilledPad(fp *core.Footprint) bool {
	for i := range fp.Pads {
		if d := fp.Pads[i].Drill; d != nil && *d > 0 {
			return true
		}
	}
	return false
}

// edgeSnapRect is the outward-facing extent edge-place kisses against the
// outline, and the reason `edge-place` means two different things.
//
// A footprint with no drilled pad is a castellated / edge-launch part: its
// copper is *designed* to reach the routed edge — half holes on a module, an
// edge-launch SMA, a card-edge finger — which is why DRC exempts edge-mounted
// pads from the edge-clearance check. Its extent is the pad bbox and its
// declared body is allowed to overhang, and that overhang is the point: it is
// what puts a module's USB-C shell out where a cable can reach it.
//
// Anything with a hole in it is through-hole. There the body, not the copper,
// is what has to fit on the board: a screw terminal's wire mouth must end at
// the edge with the whole block inboard, and the barrels must stay a fab
// clearance away from the router bit. Its extent is the declared courtyard
// when the library has one (unioned with the pads, so a courtyard smaller than
// the copper can never push a pad off the board) and the pad bbox otherwise.
func edgeSnapRect(fp *core.Footprint) (core.Rect, bool) {
	pads, ok := footprintBounds(fp)
	if !ok {
		return core.Rect{}, false
	}
	if !hasDrilledPad(fp) {
		return pads, true
	}
	if fp.BodyRect != nil || !fp.PlacementMargin.IsZero() {
		if c, ok := core.CourtyardWorld(fp); ok {
			return pads.Union(c), true
		}
	}
	return pads, true
}

// clearanceShift is the translation that pulls a through-hole footprint back
// inboard until every pad's copper and every barrel clears the outline by the
// fab floor. Surface-mount parts get no shift: see edgeSnapRect.
func clearanceShift(fp *core.Footprint, o core.Rect, cl EdgeClearances) (core.Length, core.Length) {
	if !hasDrilledPad(fp) {
		return 0, 0
	}
	var minX, maxX, minY, maxY float64
	consider := func(r core.Rect, need float64) {
		minX = math.Max(minX, need-(r.Min.X-o.Min.X).ToMM())
		maxX = math.Max(maxX, need-(o.Max.X-r.Max.X).ToMM())
		minY = math.Max(minY, need-(r.Min.Y-o.Min.Y).ToMM())
		maxY = math.Max(maxY, need-(o.Max.Y-r.Max.Y).ToMM())
	}
	for i := range fp.Pads {
		pad := &fp.Pads[i]
		consider(core.PadWorldAABB(fp, pad), cl.CopperMM)
		if d := pad.Drill; d != nil && *d > 0 {
			consider(core.RectFromCenter(core.PadWorldCenter(fp, pad), *d, *d), cl.DrillMM)
		}
	}
	return core.FromMM(minX - maxX), core.FromMM(minY - maxY)
}

func translate(fp *core.Footprint, dx, dy core.Length) {
	if dx == 0 && dy == 0 {
		return
	}
	fp.Position = core.NewPoint(fp.Position.X+dx, fp.Position.Y+dy)
}

// snapToSide slides fp perpendicular to the named outline side until its
// outward extent kisses that side, then pulls it back inboard by whatever the
// fab edge clearance demands.
func snapToSide(fp *core.Footprint, outline core.Rect, side string, cl EdgeClearances) {
	r, ok := edgeSnapRect(fp)
	if !ok {
		return
	}
	var dx, dy core.Length
	switch strings.ToLower(side) {
	case "left":
		dx = outline.Min.X - r.Min.X
	case "right":
		dx = outline.Max.X - r.Max.X
	case "top":
		dy = outline.Max.Y - r.Max.Y
	default: // bottom
		dy = outline.Min.Y - r.Min.Y
	}
	translate(fp, dx, dy)
	cx, cy := clearanceShift(fp, outline, cl)
	translate(fp, cx, cy)
}

// SnapToNearestEdge translates an edge-mounted footprint onto the outline side
// it is already closest to, using the same extent rule as EdgePlace. It is a
// fixed point: running it on an already-snapped part does not move it.
func SnapToNearestEdge(fp *core.Footprint, outline core.Rect, cl EdgeClearances) {
	snapToSide(fp, outline, nearestSide(fp, outline), cl)
}

// nearestSide names the outline side the footprint's extent is closest to.
func nearestSide(fp *core.Footprint, outline core.Rect) string {
	b, ok := edgeSnapRect(fp)
	if !ok {
		return "bottom"
	}
	d := map[string]float64{
		"left":   math.Abs((b.Min.X - outline.Min.X).ToMM()),
		"right":  math.Abs((outline.Max.X - b.Max.X).ToMM()),
		"bottom": math.Abs((b.Min.Y - outline.Min.Y).ToMM()),
		"top":    math.Abs((outline.Max.Y - b.Max.Y).ToMM()),
	}
	// Ordered so ties resolve deterministically, matching the old switch.
	best := "left"
	for _, s := range []string{"right", "bottom", "top"} {
		if d[s] < d[best] {
			best = s
		}
	}
	return best
}

// EdgePlace snaps fp onto the named outline side. alongMM is the coordinate
// along that edge (Y for left/right, X for top/bottom). Rotation is set so the
// part's mating face points out of the board.
func EdgePlace(fp *core.Footprint, outline core.Rect, side string, alongMM *float64, cl EdgeClearances) {
	if fp == nil {
		return
	}
	fp.Rotation = outwardRotation(fp.EdgeSide, side)
	if alongMM != nil {
		if r, ok := edgeSnapRect(fp); ok {
			cx := (r.Min.X + r.Max.X) / 2
			cy := (r.Min.Y + r.Max.Y) / 2
			switch strings.ToLower(side) {
			case "left", "right":
				translate(fp, 0, core.FromMM(*alongMM)-cy)
			default:
				translate(fp, core.FromMM(*alongMM)-cx, 0)
			}
		}
	} else if r, ok := edgeSnapRect(fp); ok {
		// No `along` given: centre the part on the edge it is going to.
		cx := (r.Min.X + r.Max.X) / 2
		cy := (r.Min.Y + r.Max.Y) / 2
		midX := (outline.Min.X + outline.Max.X) / 2
		midY := (outline.Min.Y + outline.Max.Y) / 2
		switch strings.ToLower(side) {
		case "left", "right":
			translate(fp, 0, midY-cy)
		default:
			translate(fp, midX-cx, 0)
		}
	}
	snapToSide(fp, outline, side, cl)
}

// edgeSideAngle is the footprint-local direction, in degrees CCW from +X,
// that a footprint's declared EdgeSide points at rotation 0. A footprint
// that does not declare one is assumed to face -Y, like a connector drawn
// with its mating face at the bottom of the sheet.
func edgeSideAngle(s *core.EdgeSide) float64 {
	if s == nil {
		return 270
	}
	switch *s {
	case core.EdgeRight:
		return 0
	case core.EdgeTop:
		return 90
	case core.EdgeLeft:
		return 180
	default: // core.EdgeBottom
		return 270
	}
}

// outwardRotation is the rotation that turns a footprint's mating face
// toward the outside of the named board edge. Rotating by r maps a local
// direction at angle a to a+r, so r = (edge normal) - (local face).
func outwardRotation(s *core.EdgeSide, side string) float64 {
	var normal float64
	switch strings.ToLower(side) {
	case "left":
		normal = 180
	case "right":
		normal = 0
	case "top":
		normal = 90
	default: // bottom
		normal = 270
	}
	r := math.Mod(normal-edgeSideAngle(s), 360)
	if r < 0 {
		r += 360
	}
	return r
}

// PlanEdges assigns each named edge-mounted footprint to the outline
// side that minimises the HPWL of its nets (wiring pull).
func PlanEdges(board *core.Board, refs []string) int {
	if board.Outline == nil {
		return 0
	}
	o := *board.Outline
	cl := EdgeClearancesFor(board)
	moved := 0
	sides := []string{"left", "right", "top", "bottom"}
	for _, ref := range refs {
		fp := board.FootprintByRef(ref)
		if fp == nil {
			continue
		}
		fp.EdgeMounted = true
		bestSide := "bottom"
		best := math.Inf(1)
		old := pos{fp.Position.X, fp.Position.Y, fp.Rotation}
		for _, s := range sides {
			EdgePlace(fp, o, s, nil, cl)
			h := rawHPWL(board)
			if h < best {
				best = h
				bestSide = s
			}
		}
		EdgePlace(fp, o, bestSide, nil, cl)
		if fp.Position.X != old.x || fp.Position.Y != old.y || fp.Rotation != old.rot {
			moved++
		}
	}
	return moved
}
