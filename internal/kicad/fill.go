package kicad

import (
	"math"

	"github.com/mentasystems/fragua/internal/core"
)

// The pour geometry Fragua already computes for the gerber pack: same
// clearances, same edge setback, same default thermal relief.
const (
	pourClearanceMM     = 0.2
	pourEdgeClearanceMM = 0.3
	spokeOverlapMM      = 0.05
	defaultSpokeMM      = 0.4
	defaultThermalGapMM = 0.4
)

// maxFillRects caps a zone's fill. Past it the grid is coarsened rather than
// writing a ten-megabyte file; a fill is a convenience, never the truth.
const maxFillRects = 20000

const (
	voidRect = iota
	voidSeg
	voidDisc
)

// shape is one piece of copper the pour must keep off. A soft shape is a
// thermal ring: its own spokes may bridge it. A hard shape is another net.
type shape struct {
	kind int
	min  [2]float64
	max  [2]float64
	a, b [2]float64
	rad  float64
	soft bool
}

func (s shape) bbox() (minx, miny, maxx, maxy float64) {
	switch s.kind {
	case voidRect:
		return s.min[0], s.min[1], s.max[0], s.max[1]
	case voidSeg:
		return math.Min(s.a[0], s.b[0]) - s.rad, math.Min(s.a[1], s.b[1]) - s.rad,
			math.Max(s.a[0], s.b[0]) + s.rad, math.Max(s.a[1], s.b[1]) + s.rad
	default:
		return s.a[0] - s.rad, s.a[1] - s.rad, s.a[0] + s.rad, s.a[1] + s.rad
	}
}

// hit reports whether the point is inside the shape grown by margin.
func (s shape) hit(x, y, margin float64) bool {
	switch s.kind {
	case voidRect:
		return x >= s.min[0]-margin && x <= s.max[0]+margin &&
			y >= s.min[1]-margin && y <= s.max[1]+margin
	case voidSeg:
		return segDist(x, y, s.a, s.b) <= s.rad+margin
	default:
		return math.Hypot(x-s.a[0], y-s.a[1]) <= s.rad+margin
	}
}

func segDist(x, y float64, a, b [2]float64) float64 {
	dx, dy := b[0]-a[0], b[1]-a[1]
	l2 := dx*dx + dy*dy
	if l2 < 1e-18 {
		return math.Hypot(x-a[0], y-a[1])
	}
	t := ((x-a[0])*dx + (y-a[1])*dy) / l2
	t = math.Max(0, math.Min(1, t))
	return math.Hypot(x-(a[0]+t*dx), y-(a[1]+t*dy))
}

// index is a uniform bucket grid over shape bounding boxes, so a fill cell
// only tests the handful of shapes that can possibly reach it.
type index struct {
	cell             float64
	minX, minY       float64
	nx, ny           int
	buckets          [][]int
	shapes           []shape
	anyHard, anySoft bool
}

func newIndex(shapes []shape, minX, minY, maxX, maxY, margin float64) *index {
	ix := &index{cell: 2.0, minX: minX - 1, minY: minY - 1, shapes: shapes}
	ix.nx = int((maxX-ix.minX)/ix.cell) + 2
	ix.ny = int((maxY-ix.minY)/ix.cell) + 2
	if ix.nx < 1 {
		ix.nx = 1
	}
	if ix.ny < 1 {
		ix.ny = 1
	}
	ix.buckets = make([][]int, ix.nx*ix.ny)
	for i, s := range shapes {
		if s.soft {
			ix.anySoft = true
		} else {
			ix.anyHard = true
		}
		x0, y0, x1, y1 := s.bbox()
		cx0, cy0 := ix.cellOf(x0-margin, y0-margin)
		cx1, cy1 := ix.cellOf(x1+margin, y1+margin)
		for cy := cy0; cy <= cy1; cy++ {
			for cx := cx0; cx <= cx1; cx++ {
				ix.buckets[cy*ix.nx+cx] = append(ix.buckets[cy*ix.nx+cx], i)
			}
		}
	}
	return ix
}

func (ix *index) cellOf(x, y float64) (int, int) {
	cx := int((x - ix.minX) / ix.cell)
	cy := int((y - ix.minY) / ix.cell)
	return clampInt(cx, 0, ix.nx-1), clampInt(cy, 0, ix.ny-1)
}

// query reports whether the point is inside a hard shape, and inside a soft one.
func (ix *index) query(x, y, margin float64) (hard, soft bool) {
	cx, cy := ix.cellOf(x, y)
	for _, i := range ix.buckets[cy*ix.nx+cx] {
		s := ix.shapes[i]
		if !s.hit(x, y, margin) {
			continue
		}
		if s.soft {
			soft = true
		} else {
			hard = true
			return
		}
	}
	return
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ---------------------------------------------------------------- zone fill

// zoneFill returns the copper of one pour as axis-aligned rectangles in
// Fragua coordinates. The rasteriser is conservative on purpose: a cell only
// becomes copper when its whole footprint clears every void, so the exported
// fill never claims copper closer to another net than the pour's clearance.
func (e *exporter) zoneFill(pour *core.Pour, outline []core.Point) []core.Rect {
	if len(outline) < 3 {
		return nil
	}
	minX, minY, maxX, maxY := polyBBox(outline)
	shapes, keeps := e.pourShapes(pour)
	grid := e.opts.FillGridMM
	for attempt := 0; attempt < 4; attempt++ {
		rects := rasterise(outline, shapes, keeps, minX, minY, maxX, maxY, grid)
		if len(rects) <= maxFillRects {
			return rects
		}
		grid *= 2
	}
	return nil
}

func rasterise(outline []core.Point, shapes []shape, keeps []shape,
	minX, minY, maxX, maxY, grid float64) []core.Rect {
	nx := int(math.Ceil((maxX-minX)/grid)) + 1
	ny := int(math.Ceil((maxY-minY)/grid)) + 1
	if nx <= 0 || ny <= 0 || nx*ny > 8_000_000 {
		return nil
	}
	margin := grid * math.Sqrt2 / 2
	ix := newIndex(shapes, minX, minY, maxX, maxY, margin)
	kx := newIndex(keeps, minX, minY, maxX, maxY, margin)

	rows := make([][]bool, ny)
	for iy := 0; iy < ny; iy++ {
		row := make([]bool, nx)
		cy := minY + (float64(iy)+0.5)*grid
		for i := 0; i < nx; i++ {
			cx := minX + (float64(i)+0.5)*grid
			if !insideWithMargin(outline, cx, cy, pourEdgeClearanceMM+margin) {
				continue
			}
			hard, soft := ix.query(cx, cy, margin)
			if hard {
				continue
			}
			if soft {
				if k, _ := kx.query(cx, cy, 0); !k {
					continue
				}
			}
			row[i] = true
		}
		rows[iy] = row
	}
	return mergeRects(rows, nx, ny, minX, minY, grid)
}

// mergeRects turns the cell grid into maximal rectangles: horizontal runs
// first, then runs stacked while their span is unchanged.
func mergeRects(rows [][]bool, nx, ny int, minX, minY, grid float64) []core.Rect {
	type run struct{ x0, x1, y0, y1 int }
	var open []run
	var out []core.Rect
	flush := func(r run) {
		out = append(out, core.Rect{
			Min: core.Point{X: core.FromMM(minX + float64(r.x0)*grid), Y: core.FromMM(minY + float64(r.y0)*grid)},
			Max: core.Point{X: core.FromMM(minX + float64(r.x1)*grid), Y: core.FromMM(minY + float64(r.y1)*grid)},
		})
	}
	for iy := 0; iy < ny; iy++ {
		var cur []run
		for i := 0; i < nx; {
			if !rows[iy][i] {
				i++
				continue
			}
			j := i
			for j < nx && rows[iy][j] {
				j++
			}
			cur = append(cur, run{x0: i, x1: j, y0: iy, y1: iy + 1})
			i = j
		}
		var next []run
		ci := 0
		for _, o := range open {
			for ci < len(cur) && cur[ci].x0 < o.x0 {
				next = append(next, cur[ci])
				ci++
			}
			if ci < len(cur) && cur[ci].x0 == o.x0 && cur[ci].x1 == o.x1 {
				o.y1 = iy + 1
				next = append(next, o)
				ci++
				continue
			}
			flush(o)
		}
		next = append(next, cur[ci:]...)
		open = next
	}
	for _, o := range open {
		flush(o)
	}
	return out
}

// pourShapes collects everything the pour must clear (hard), the thermal
// rings it may bridge (soft), and the spokes that bridge them (keeps).
func (e *exporter) pourShapes(pour *core.Pour) (shapes []shape, keeps []shape) {
	layer := pour.Layer
	relief := core.ThermalRelief{Kind: "spokes4", SpokeWidthMM: defaultSpokeMM, GapMM: defaultThermalGapMM}
	if pour.ThermalRelief != nil {
		relief = *pour.ThermalRelief
	}
	gap := relief.GapMM
	if gap <= 0 {
		gap = defaultThermalGapMM
	}
	spoke := relief.SpokeWidthMM
	if spoke <= 0 {
		spoke = defaultSpokeMM
	}

	for _, id := range e.board.FootprintOrder {
		fp := e.board.Footprints[id]
		if fp == nil {
			continue
		}
		for i := range fp.Pads {
			pad := &fp.Pads[i]
			if !core.PadOccupiesLayer(pad, layer) {
				continue
			}
			net := ""
			if pad.Net != nil {
				net = *pad.Net
			}
			r := core.PadWorldAABB(fp, pad)
			if net == pour.Net {
				if !relief.IsSpokes4() {
					continue // solid: the pad melts into the pour
				}
				shapes = append(shapes, rectShape(r, gap, true))
				keeps = append(keeps, spokes(r, spoke, gap)...)
				continue
			}
			shapes = append(shapes, rectShape(r, pourClearanceMM, false))
		}
	}
	for i := range e.board.Traces {
		t := &e.board.Traces[i]
		if t.Layer.Index != layer.Index || t.Net == pour.Net {
			continue
		}
		shapes = append(shapes, shape{
			kind: voidSeg,
			a:    [2]float64{t.Start.X.ToMM(), t.Start.Y.ToMM()},
			b:    [2]float64{t.End.X.ToMM(), t.End.Y.ToMM()},
			rad:  t.Width.ToMM()/2 + pourClearanceMM,
		})
	}
	for i := range e.board.Vias {
		v := &e.board.Vias[i]
		if v.Net == pour.Net {
			continue
		}
		shapes = append(shapes, shape{
			kind: voidDisc,
			a:    [2]float64{v.Position.X.ToMM(), v.Position.Y.ToMM()},
			rad:  v.Diameter.ToMM()/2 + pourClearanceMM,
		})
	}
	for i := range e.board.Cutouts {
		if r, ok := core.PolygonBBox(e.board.Cutouts[i].Polygon); ok {
			shapes = append(shapes, rectShape(r, pourEdgeClearanceMM, false))
		}
	}
	for i := range e.board.Keepouts {
		k := &e.board.Keepouts[i]
		if !k.NoCopper {
			continue
		}
		if len(k.Polygon) >= 3 {
			if r, ok := core.PolygonBBox(k.Polygon); ok {
				shapes = append(shapes, rectShape(r, 0, false))
			}
			continue
		}
		if k.Rect != nil {
			shapes = append(shapes, rectShape(*k.Rect, 0, false))
		}
	}
	return shapes, keeps
}

func rectShape(r core.Rect, growMM float64, soft bool) shape {
	return shape{
		kind: voidRect,
		min:  [2]float64{r.Min.X.ToMM() - growMM, r.Min.Y.ToMM() - growMM},
		max:  [2]float64{r.Max.X.ToMM() + growMM, r.Max.Y.ToMM() + growMM},
		soft: soft,
	}
}

// spokes are the four thermal bridges that tie a same-net pad to the pour.
func spokes(r core.Rect, widthMM, gapMM float64) []shape {
	cx := (r.Min.X.ToMM() + r.Max.X.ToMM()) / 2
	cy := (r.Min.Y.ToMM() + r.Max.Y.ToMM()) / 2
	half := widthMM / 2
	reach := gapMM + spokeOverlapMM
	return []shape{
		{kind: voidRect, min: [2]float64{cx - half, r.Max.Y.ToMM()}, max: [2]float64{cx + half, r.Max.Y.ToMM() + reach}},
		{kind: voidRect, min: [2]float64{cx - half, r.Min.Y.ToMM() - reach}, max: [2]float64{cx + half, r.Min.Y.ToMM()}},
		{kind: voidRect, min: [2]float64{r.Max.X.ToMM(), cy - half}, max: [2]float64{r.Max.X.ToMM() + reach, cy + half}},
		{kind: voidRect, min: [2]float64{r.Min.X.ToMM() - reach, cy - half}, max: [2]float64{r.Min.X.ToMM(), cy + half}},
	}
}

// ---------------------------------------------------------------- polygons

func polyBBox(poly []core.Point) (minX, minY, maxX, maxY float64) {
	minX, minY = math.Inf(1), math.Inf(1)
	maxX, maxY = math.Inf(-1), math.Inf(-1)
	for _, p := range poly {
		x, y := p.X.ToMM(), p.Y.ToMM()
		minX, minY = math.Min(minX, x), math.Min(minY, y)
		maxX, maxY = math.Max(maxX, x), math.Max(maxY, y)
	}
	return
}

// insideWithMargin is point-in-polygon plus a real setback from every edge.
func insideWithMargin(poly []core.Point, x, y, margin float64) bool {
	if !pointInPoly(poly, x, y) {
		return false
	}
	for i := range poly {
		a := [2]float64{poly[i].X.ToMM(), poly[i].Y.ToMM()}
		j := (i + 1) % len(poly)
		b := [2]float64{poly[j].X.ToMM(), poly[j].Y.ToMM()}
		if segDist(x, y, a, b) < margin {
			return false
		}
	}
	return true
}

func pointInPoly(poly []core.Point, x, y float64) bool {
	in := false
	n := len(poly)
	for i, j := 0, n-1; i < n; j, i = i, i+1 {
		xi, yi := poly[i].X.ToMM(), poly[i].Y.ToMM()
		xj, yj := poly[j].X.ToMM(), poly[j].Y.ToMM()
		if (yi > y) != (yj > y) && x < (xj-xi)*(y-yi)/(yj-yi)+xi {
			in = !in
		}
	}
	return in
}
