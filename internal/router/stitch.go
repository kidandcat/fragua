package router

import (
	"math"

	"github.com/mentasystems/fragua/internal/core"
)

// StitchIsolatedPads is the script-facing pour stitch.
func StitchIsolatedPads(board *core.Board, opts Options) int {
	n := stitchIsolatedPads(board, opts)
	n += stitchPourGrid(board, opts, false)
	return n
}

func stitchIsolatedPads(board *core.Board, opts Options) int {
	nGrid := stitchPourGrid(board, opts, true) // only pours that asked for stitching
	// net → layers that already have a pour. A pad sitting in a same-layer
	// pour is already connected; we only via-stitch to a pour on another layer.
	pourLayers := map[string][]uint8{}
	for _, p := range board.Pours {
		if p.Net != "" {
			pourLayers[p.Net] = append(pourLayers[p.Net], p.Layer.Index)
		}
	}
	if len(pourLayers) == 0 {
		return nGrid
	}
	added := nGrid
	for _, fp := range footprintsStable(board) {
		for i := range fp.Pads {
			pad := &fp.Pads[i]
			if pad.Net == nil {
				continue
			}
			layers, ok := pourLayers[*pad.Net]
			if !ok {
				continue
			}
			// A drilled pad is copper on every layer: a same-net pour on
			// any layer already ties it, and a stitch via would land on
			// top of the pad's own hole.
			if pad.Drill != nil && *pad.Drill > 0 {
				continue
			}
			sameLayer := false
			for _, L := range layers {
				if L == pad.Layer.Index {
					sameLayer = true
					break
				}
			}
			if sameLayer {
				continue
			}
			c := core.PadWorldCenter(fp, pad)
			// A QFN dogbone sits ~0.7–1.4 mm off the pad and already
			// ties it to an inner plane; don't drop a second via on top.
			if hasNearbyCopper(board, *pad.Net, c, 1.8) {
				continue
			}
			net := *pad.Net
			cx, cy := c.X.ToMM(), c.Y.ToMM()
			sites := [][2]float64{{cx, cy}}
			if pad.Drill == nil {
				bx, by := 0.0, 0.0
				if board.Outline != nil {
					bx = (board.Outline.Min.X.ToMM() + board.Outline.Max.X.ToMM()) / 2
					by = (board.Outline.Min.Y.ToMM() + board.Outline.Max.Y.ToMM()) / 2
				}
				dx, dy := bx-cx, by-cy
				d := math.Hypot(dx, dy)
				if d < 1e-6 {
					dx, dy, d = 1, 0, 1
				}
				ux, uy := dx/d, dy/d
				px, py := -uy, ux
				w, h := core.PadWorldSize(fp, pad)
				base := math.Max(w.ToMM(), h.ToMM())/2 + 0.45
				for _, r := range []float64{base, base + 0.35, base + 0.70} {
					for _, lat := range []float64{0, 0.35, -0.35} {
						sites = append(sites, [2]float64{cx + ux*r + px*lat, cy + uy*r + py*lat})
					}
				}
			}
			need := opts.TraceWidthMM/2 + 0.13
			placed := false
			for _, s := range sites {
				vx, vy := s[0], s[1]
				if !outlineContains(board.Outline, vx, vy, 0.4) {
					continue
				}
				if stubHitsForeignPad(board, cx, cy, vx, vy, net, need, fp, i) {
					continue
				}
				if fanoutHitsPad(board, vx, vy, opts.ViaDiameterMM/2+0.13, fp, i) {
					continue
				}
				if !holeSiteOK(board, vx, vy, opts.ViaDrillMM) {
					continue
				}
				snapT, snapV := len(board.Traces), len(board.Vias)
				vp := core.NewPoint(core.FromMM(vx), core.FromMM(vy))
				board.Vias = append(board.Vias, core.Via{
					ID: core.NewID(), Net: net, Position: vp,
					Drill: core.FromMM(opts.ViaDrillMM), Diameter: core.FromMM(opts.ViaDiameterMM),
				})
				if pad.Drill == nil {
					board.Traces = append(board.Traces, core.Trace{
						ID: core.NewID(), Layer: pad.Layer, Net: net,
						Width: core.FromMM(opts.TraceWidthMM), Start: c, End: vp,
					})
				}
				if !copperClearanceFrom(board, snapT, commitClearance(board)) ||
					!viaClearanceFrom(board, snapV, commitClearance(board)) {
					board.Traces = board.Traces[:snapT]
					board.Vias = board.Vias[:snapV]
					continue
				}
				placed = true
				break
			}
			if placed {
				added++
			}
		}
	}
	return added
}

// stitchPourGrid drops a via lattice inside pours that requested stitching
// (including empty `stitching: {}`) or that have same-net copper on another
// layer with no via in the island. Vias actually tie the plane through.
func stitchPourGrid(board *core.Board, opts Options, onlyRequested bool) int {
	if board.Outline == nil && len(board.Pours) == 0 {
		return 0
	}
	fab := core.ActiveFabRules(board)
	added := 0
	for i := range board.Pours {
		pr := &board.Pours[i]
		need := pr.StitchRequested() || (!onlyRequested && pourNeedsViaTie(board, pr))
		if !need {
			continue
		}
		if viaInPour(board, pr) {
			continue
		}
		pitch := 2.54
		if pr.Stitching != nil && pr.Stitching.PitchMM > 0 {
			pitch = pr.Stitching.PitchMM
		}
		drill := opts.ViaDrillMM
		dia := opts.ViaDiameterMM
		if pr.Stitching != nil && pr.Stitching.DrillMM > 0 {
			drill = pr.Stitching.DrillMM
		}
		if pr.Stitching != nil && pr.Stitching.Diameter > 0 {
			dia = pr.Stitching.Diameter
		}
		if drill <= 0 {
			drill = fab.MinViaDrillMM
		}
		if dia <= 0 {
			dia = fab.MinViaDiameterMM
		}
		if drill <= 0 {
			drill = 0.30
		}
		if dia <= 0 {
			dia = 0.60
		}
		xmin, ymin, xmax, ymax := pourBoundsMM(board, pr)
		margin := math.Max(dia/2+0.4, 0.8)
		for x := xmin + margin; x <= xmax-margin+1e-9; x += pitch {
			for y := ymin + margin; y <= ymax-margin+1e-9; y += pitch {
				if !pointInPourRegion(board, pr, x, y) {
					continue
				}
				// The barrel, not just the centre, has to clear the void.
				if pointInPourVoidMargin(board, x, y, dia/2) {
					continue
				}
				if !outlineContains(board.Outline, x, y, 0.4) {
					continue
				}
				if fanoutHitsPad(board, x, y, dia/2+0.15, nil, -1) {
					continue
				}
				if !holeSiteOK(board, x, y, drill) {
					continue
				}
				if viaNear(board, x, y, pitch*0.45) {
					continue
				}
				snapV := len(board.Vias)
				board.Vias = append(board.Vias, core.Via{
					ID:       core.NewID(),
					Net:      pr.Net,
					Position: core.NewPoint(core.FromMM(x), core.FromMM(y)),
					Drill:    core.FromMM(drill),
					Diameter: core.FromMM(dia),
				})
				// The stitch adds a via and no trace, so the trace-only
				// check could never fail: validate the barrel itself.
				if !viaClearanceFrom(board, snapV, commitClearance(board)) {
					board.Vias = board.Vias[:snapV]
					continue
				}
				added++
			}
		}
	}
	return added
}

func pourNeedsViaTie(board *core.Board, pr *core.Pour) bool {
	if pr == nil || pr.Net == "" {
		return false
	}
	if viaInPour(board, pr) {
		return false
	}
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
				c := core.PadWorldCenter(fp, pad)
				if pointInPourRegion(board, pr, c.X.ToMM(), c.Y.ToMM()) {
					return false
				}
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

func viaInPour(board *core.Board, pr *core.Pour) bool {
	for _, v := range board.Vias {
		if v.Net != pr.Net {
			continue
		}
		if pointInPourRegion(board, pr, v.Position.X.ToMM(), v.Position.Y.ToMM()) {
			return true
		}
	}
	return false
}

func viaNear(board *core.Board, x, y, rMM float64) bool {
	r2 := rMM * rMM
	for _, v := range board.Vias {
		dx := v.Position.X.ToMM() - x
		dy := v.Position.Y.ToMM() - y
		if dx*dx+dy*dy <= r2 {
			return true
		}
	}
	return false
}

func pourBoundsMM(board *core.Board, pr *core.Pour) (xmin, ymin, xmax, ymax float64) {
	if len(pr.Polygon) >= 3 {
		xmin, ymin = pr.Polygon[0].X.ToMM(), pr.Polygon[0].Y.ToMM()
		xmax, ymax = xmin, ymin
		for _, p := range pr.Polygon[1:] {
			x, y := p.X.ToMM(), p.Y.ToMM()
			if x < xmin {
				xmin = x
			}
			if y < ymin {
				ymin = y
			}
			if x > xmax {
				xmax = x
			}
			if y > ymax {
				ymax = y
			}
		}
		return
	}
	if board.Outline != nil {
		return board.Outline.Min.X.ToMM(), board.Outline.Min.Y.ToMM(),
			board.Outline.Max.X.ToMM(), board.Outline.Max.Y.ToMM()
	}
	return 0, 0, 0, 0
}

func pointInPourRegion(board *core.Board, pr *core.Pour, x, y float64) bool {
	if pointInPourVoid(board, x, y) {
		return false
	}
	if len(pr.Polygon) >= 3 {
		return pointInPoly(pr.Polygon, x, y)
	}
	if board.Outline == nil {
		return false
	}
	r := *board.Outline
	inset := 0.3
	return x >= r.Min.X.ToMM()+inset && x <= r.Max.X.ToMM()-inset &&
		y >= r.Min.Y.ToMM()+inset && y <= r.Max.Y.ToMM()-inset
}

// pointInPourVoid reports whether a point falls somewhere the pour is cleared
// out of — an internal cutout, plus the same setback a milled edge gets, or a
// no-copper keepout. Mirrors the gerber export, so stitching vias land where
// there will actually be copper. A non-rectangular cutout is tested by its
// bounding box, which errs towards stitching less.
func pointInPourVoid(board *core.Board, x, y float64) bool {
	return pointInPourVoidMargin(board, x, y, 0)
}

// pointInPourVoidMargin is pointInPourVoid with the void grown by marginMM, so
// a via can be tested by its barrel rather than its centre.
func pointInPourVoidMargin(board *core.Board, x, y, marginMM float64) bool {
	const cutoutSetbackMM = 0.3
	for i := range board.Cutouts {
		poly := board.Cutouts[i].Polygon
		if len(poly) < 3 {
			continue
		}
		xmin, ymin, xmax, ymax := polyBoundsMM(poly)
		if x >= xmin-cutoutSetbackMM-marginMM && x <= xmax+cutoutSetbackMM+marginMM &&
			y >= ymin-cutoutSetbackMM-marginMM && y <= ymax+cutoutSetbackMM+marginMM {
			return true
		}
	}
	for i := range board.Keepouts {
		k := &board.Keepouts[i]
		if !k.NoCopper {
			continue
		}
		if k.Rect != nil {
			r := *k.Rect
			if x >= r.Min.X.ToMM()-marginMM && x <= r.Max.X.ToMM()+marginMM &&
				y >= r.Min.Y.ToMM()-marginMM && y <= r.Max.Y.ToMM()+marginMM {
				return true
			}
			continue
		}
		if len(k.Polygon) >= 3 && pointInPoly(k.Polygon, x, y) {
			return true
		}
	}
	return false
}

func polyBoundsMM(poly []core.Point) (xmin, ymin, xmax, ymax float64) {
	xmin, ymin = poly[0].X.ToMM(), poly[0].Y.ToMM()
	xmax, ymax = xmin, ymin
	for _, p := range poly[1:] {
		xmin = math.Min(xmin, p.X.ToMM())
		ymin = math.Min(ymin, p.Y.ToMM())
		xmax = math.Max(xmax, p.X.ToMM())
		ymax = math.Max(ymax, p.Y.ToMM())
	}
	return
}

func pointInPoly(poly []core.Point, x, y float64) bool {
	n := len(poly)
	if n < 3 {
		return false
	}
	inside := false
	j := n - 1
	for i := 0; i < n; i++ {
		xi, yi := poly[i].X.ToMM(), poly[i].Y.ToMM()
		xj, yj := poly[j].X.ToMM(), poly[j].Y.ToMM()
		if (yi > y) != (yj > y) && x < (xj-xi)*(y-yi)/(yj-yi+1e-18)+xi {
			inside = !inside
		}
		j = i
	}
	return inside
}

func hasNearbyCopper(board *core.Board, net string, p core.Point, rMM float64) bool {
	r2 := rMM * rMM
	near := func(q core.Point) bool {
		dx := q.X.ToMM() - p.X.ToMM()
		dy := q.Y.ToMM() - p.Y.ToMM()
		return dx*dx+dy*dy <= r2
	}
	for _, v := range board.Vias {
		if v.Net == net && near(v.Position) {
			return true
		}
	}
	for _, t := range board.Traces {
		if t.Net == net && (near(t.Start) || near(t.End)) {
			return true
		}
	}
	return false
}
