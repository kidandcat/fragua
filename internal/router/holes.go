package router

import (
	"github.com/mentasystems/fragua/internal/core"
)

// Drill clearance is a mechanical rule, not a copper rule: two holes closer
// than the fab's hole-to-hole gap break the drill bit no matter which nets
// they belong to, so the router has to honour it while it *places* vias
// instead of letting DRC find them afterwards.

type holePt struct {
	x, y float64 // mm
	r    float64 // drill radius, mm
}

// holeMap answers "may a via be drilled here?" using the same rule the DRC
// hole_to_hole check applies.
type holeMap struct {
	pts    []holePt
	minGap float64
}

// newHoleMap collects every drilled feature already on the board: through-hole
// pads, existing vias, NPTH mount holes.
func newHoleMap(board *core.Board) *holeMap {
	h := &holeMap{minGap: 0.50}
	if board == nil {
		return h
	}
	if fab := core.ActiveFabRules(board); fab.MinHoleToHoleMM > 0 {
		h.minGap = fab.MinHoleToHoleMM
	}
	for _, fp := range footprintsStable(board) {
		for i := range fp.Pads {
			pad := &fp.Pads[i]
			if pad.Drill == nil || *pad.Drill <= 0 {
				continue
			}
			c := core.PadWorldCenter(fp, pad)
			h.pts = append(h.pts, holePt{c.X.ToMM(), c.Y.ToMM(), pad.Drill.ToMM() / 2})
		}
	}
	// Existing vias are registered by newGrid through blockViaObstacle,
	// so they are deliberately absent here.
	for _, m := range board.MountHoles {
		h.pts = append(h.pts, holePt{m.Center.X.ToMM(), m.Center.Y.ToMM(), m.Diameter.ToMM() / 2})
	}
	for _, m := range board.Holes {
		h.pts = append(h.pts, holePt{m.Center.X.ToMM(), m.Center.Y.ToMM(), m.Diameter.ToMM() / 2})
	}
	return h
}

// ok reports whether a drill of diameter drillMM centred on (xMM,yMM) keeps
// the fab's hole-to-hole gap to every hole already known.
func (h *holeMap) ok(xMM, yMM, drillMM float64) bool {
	if h == nil {
		return true
	}
	r := drillMM / 2
	for _, p := range h.pts {
		need := h.minGap + r + p.r
		dx, dy := p.x-xMM, p.y-yMM
		if dx*dx+dy*dy < need*need {
			return false
		}
	}
	return true
}

// add registers a freshly placed drill so later vias in the same run keep
// their distance from it too.
func (h *holeMap) add(xMM, yMM, drillMM float64) {
	if h == nil {
		return
	}
	h.pts = append(h.pts, holePt{xMM, yMM, drillMM / 2})
}

func (h *holeMap) mark() int {
	if h == nil {
		return 0
	}
	return len(h.pts)
}

func (h *holeMap) truncate(n int) {
	if h == nil || n > len(h.pts) {
		return
	}
	h.pts = h.pts[:n]
}

// viaSiteOK reports whether a via may be drilled at a grid cell: the fab
// hole-to-hole gap to every other drill, and enough room for the barrel's
// annulus on every layer. The lateral search only ever cleared a trace
// half-width, and a via pad is usually more than twice that.
func (g *grid) viaSiteOK(cx, cy int, net string) bool {
	wx, wy := g.cellToWorld(cx, cy)
	if g.holes != nil && !g.holes.ok(wx.ToMM(), wy.ToMM(), g.opts.ViaDrillMM) {
		return false
	}
	hw := g.opts.ViaDiameterMM / 2
	if hw <= g.opts.TraceWidthMM/2 {
		return true // the lateral clearance already covers the barrel
	}
	need := hw + g.opts.padClear()
	xm, ym := wx.ToMM(), wy.ToMM()
	for L := uint8(0); L < uint8(g.layers); L++ {
		if !g.clearanceForHalf(cx, cy, L, net, need) {
			return false
		}
		if g.nearForeignPad(xm, ym, L, net, need) {
			return false
		}
	}
	return true
}

// padSources are the grid cells a pad connects to: its own layer, or every
// copper layer when the pad is drilled.
func (g *grid) padSources(p padLoc) []cellKey {
	cx, cy, ok := g.worldToCell(p.p.X, p.p.Y)
	if !ok {
		cx, cy = clampCell(p.p, g)
	}
	if !p.through {
		return []cellKey{{cx, cy, p.layer}}
	}
	out := make([]cellKey, 0, g.layers)
	for L := uint8(0); L < uint8(g.layers); L++ {
		out = append(out, cellKey{cx, cy, L})
	}
	return out
}

// holeSiteOK is the allocation-light form used by the fanout/stitch passes,
// which place a handful of vias outside the grid search.
func holeSiteOK(board *core.Board, xMM, yMM, drillMM float64) bool {
	if board == nil {
		return true
	}
	minGap := 0.50
	if fab := core.ActiveFabRules(board); fab.MinHoleToHoleMM > 0 {
		minGap = fab.MinHoleToHoleMM
	}
	r := drillMM / 2
	clash := func(hx, hy, hr float64) bool {
		need := minGap + r + hr
		dx, dy := hx-xMM, hy-yMM
		return dx*dx+dy*dy < need*need
	}
	for _, id := range board.FootprintOrder {
		fp := board.Footprints[id]
		if fp == nil {
			continue
		}
		for i := range fp.Pads {
			pad := &fp.Pads[i]
			if pad.Drill == nil || *pad.Drill <= 0 {
				continue
			}
			c := core.PadWorldCenter(fp, pad)
			if clash(c.X.ToMM(), c.Y.ToMM(), pad.Drill.ToMM()/2) {
				return false
			}
		}
	}
	for _, v := range board.Vias {
		if clash(v.Position.X.ToMM(), v.Position.Y.ToMM(), v.Drill.ToMM()/2) {
			return false
		}
	}
	for _, m := range board.MountHoles {
		if clash(m.Center.X.ToMM(), m.Center.Y.ToMM(), m.Diameter.ToMM()/2) {
			return false
		}
	}
	for _, m := range board.Holes {
		if clash(m.Center.X.ToMM(), m.Center.Y.ToMM(), m.Diameter.ToMM()/2) {
			return false
		}
	}
	return true
}
