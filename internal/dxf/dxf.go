// Package dxf writes the mechanical view of a board as an ASCII DXF (R12):
// the outline, milled cutouts, holes and the component envelopes an
// enclosure has to clear. Coordinates are millimetres in board space (Y up,
// same as the Gerbers), so a CAD tool can import the file as a sketch.
package dxf

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/mentasystems/fragua/internal/core"
)

// Layer names in the DXF. Stable: enclosure scripts select by them.
const (
	LayerOutline    = "OUTLINE"
	LayerCutouts    = "CUTOUTS"
	LayerNPTH       = "HOLES_NPTH"
	LayerPTH        = "HOLES_PTH"
	LayerCompTop    = "COMP_TOP"
	LayerCompBottom = "COMP_BOTTOM"
)

// layers lists every layer with its AutoCAD colour index.
var layers = []struct {
	name  string
	color int
}{
	{LayerOutline, 7},
	{LayerCutouts, 1},
	{LayerNPTH, 5},
	{LayerPTH, 4},
	{LayerCompTop, 3},
	{LayerCompBottom, 6},
}

// Stats counts what Write emitted, so callers can report it.
type Stats struct {
	OutlineMM  [2]float64 // width, height of the outline bounding box
	Cutouts    int
	NPTH       int
	PTH        int
	CompTop    int
	CompBottom int
}

// Write returns the DXF text for board.
func Write(board *core.Board) (string, Stats) {
	w := &writer{}
	var st Stats
	w.header(board)
	w.tables()
	w.line("0", "SECTION", "2", "ENTITIES")

	if outer := outlinePoly(board); len(outer) >= 3 {
		w.poly(LayerOutline, outer)
	} else if board.Outline != nil {
		w.roundedRect(LayerOutline, *board.Outline, board.OutlineCornerRadius.ToMM())
	}
	if bb, ok := outlineBBox(board); ok {
		st.OutlineMM = [2]float64{bb.Width().ToMM(), bb.Height().ToMM()}
	}

	for _, cut := range board.Cutouts {
		if len(cut.Polygon) >= 3 {
			w.poly(LayerCutouts, cut.Polygon)
			st.Cutouts++
		}
	}
	for _, h := range board.NPTHHoles() {
		if h.Diameter <= 0 {
			continue
		}
		w.circle(LayerNPTH, h.Center, h.Diameter.ToMM()/2)
		st.NPTH++
	}

	bottom := board.StackupOrDefault().BottomLayer().Index
	margin := board.ComponentRules().CourtyardMarginMM
	for _, fp := range footprints(board) {
		for i := range fp.Pads {
			pad := &fp.Pads[i]
			if pad.Drill == nil || *pad.Drill <= 0 {
				continue
			}
			w.circle(LayerPTH, core.PadWorldCenter(fp, pad), pad.Drill.ToMM()/2)
			st.PTH++
		}
		_, body, ok := core.PackageOutlines(fp, margin)
		if !ok {
			continue
		}
		layer := LayerCompTop
		if fp.Layer.Index == bottom {
			layer = LayerCompBottom
			st.CompBottom++
		} else {
			st.CompTop++
		}
		w.rect(layer, body)
		if fp.Reference != "" {
			c := core.Point{X: (body.Min.X + body.Max.X) / 2, Y: (body.Min.Y + body.Max.Y) / 2}
			h := math.Max(0.3, math.Min(1.0, 0.4*math.Min(body.Width().ToMM(), body.Height().ToMM())))
			w.text(layer, c, h, fp.Reference)
		}
	}

	w.line("0", "ENDSEC", "0", "EOF")
	return w.b.String(), st
}

// footprints returns the footprints in a stable order (FootprintOrder first,
// then any stragglers by reference) so the file is reproducible.
func footprints(board *core.Board) []*core.Footprint {
	seen := map[string]bool{}
	var out []*core.Footprint
	for _, id := range board.FootprintOrder {
		if fp := board.Footprints[id]; fp != nil && !seen[id] {
			seen[id] = true
			out = append(out, fp)
		}
	}
	var rest []string
	for id := range board.Footprints {
		if !seen[id] {
			rest = append(rest, id)
		}
	}
	sort.Strings(rest)
	for _, id := range rest {
		if fp := board.Footprints[id]; fp != nil {
			out = append(out, fp)
		}
	}
	return out
}

func outlinePoly(board *core.Board) []core.Point {
	if len(board.OutlinePoly) >= 3 {
		return board.OutlinePoly
	}
	return nil
}

func outlineBBox(board *core.Board) (core.Rect, bool) {
	if pts := outlinePoly(board); len(pts) >= 3 {
		r := core.Rect{Min: pts[0], Max: pts[0]}
		for _, p := range pts[1:] {
			r = r.Union(core.Rect{Min: p, Max: p})
		}
		return r, true
	}
	if board.Outline != nil {
		return *board.Outline, true
	}
	return core.Rect{}, false
}

type writer struct{ b strings.Builder }

// line writes alternating group-code / value lines.
func (w *writer) line(kv ...string) {
	for _, s := range kv {
		w.b.WriteString(s)
		w.b.WriteByte('\n')
	}
}

func num(v float64) string {
	s := fmt.Sprintf("%.6f", v)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if s == "-0" || s == "" {
		return "0"
	}
	return s
}

func (w *writer) header(board *core.Board) {
	w.line("0", "SECTION", "2", "HEADER")
	w.line("9", "$ACADVER", "1", "AC1009")
	w.line("9", "$INSUNITS", "70", "4") // millimetres
	w.line("9", "$MEASUREMENT", "70", "1")
	if bb, ok := outlineBBox(board); ok {
		w.line("9", "$EXTMIN", "10", num(bb.Min.X.ToMM()), "20", num(bb.Min.Y.ToMM()), "30", "0")
		w.line("9", "$EXTMAX", "10", num(bb.Max.X.ToMM()), "20", num(bb.Max.Y.ToMM()), "30", "0")
	}
	w.line("0", "ENDSEC")
}

func (w *writer) tables() {
	w.line("0", "SECTION", "2", "TABLES")
	w.line("0", "TABLE", "2", "LAYER", "70", fmt.Sprint(len(layers)))
	for _, l := range layers {
		w.line("0", "LAYER", "2", l.name, "70", "0", "62", fmt.Sprint(l.color), "6", "CONTINUOUS")
	}
	w.line("0", "ENDTAB", "0", "ENDSEC")
}

func (w *writer) seg(layer string, x0, y0, x1, y1 float64) {
	w.line("0", "LINE", "8", layer,
		"10", num(x0), "20", num(y0), "30", "0",
		"11", num(x1), "21", num(y1), "31", "0")
}

func (w *writer) poly(layer string, pts []core.Point) {
	for i := range pts {
		a, b := pts[i], pts[(i+1)%len(pts)]
		if a == b {
			continue
		}
		w.seg(layer, a.X.ToMM(), a.Y.ToMM(), b.X.ToMM(), b.Y.ToMM())
	}
}

func (w *writer) rect(layer string, r core.Rect) {
	w.poly(layer, []core.Point{
		{X: r.Min.X, Y: r.Min.Y}, {X: r.Max.X, Y: r.Min.Y},
		{X: r.Max.X, Y: r.Max.Y}, {X: r.Min.X, Y: r.Max.Y},
	})
}

// roundedRect draws r with corner radius rad (mm) as 4 lines + 4 CCW arcs.
func (w *writer) roundedRect(layer string, r core.Rect, rad float64) {
	x0, y0, x1, y1 := r.Min.X.ToMM(), r.Min.Y.ToMM(), r.Max.X.ToMM(), r.Max.Y.ToMM()
	rad = math.Min(rad, math.Min(x1-x0, y1-y0)/2)
	if rad <= 0 {
		w.rect(layer, r)
		return
	}
	w.seg(layer, x0+rad, y0, x1-rad, y0)
	w.seg(layer, x1, y0+rad, x1, y1-rad)
	w.seg(layer, x1-rad, y1, x0+rad, y1)
	w.seg(layer, x0, y1-rad, x0, y0+rad)
	w.arc(layer, x1-rad, y0+rad, rad, 270, 360)
	w.arc(layer, x1-rad, y1-rad, rad, 0, 90)
	w.arc(layer, x0+rad, y1-rad, rad, 90, 180)
	w.arc(layer, x0+rad, y0+rad, rad, 180, 270)
}

func (w *writer) arc(layer string, cx, cy, r, a0, a1 float64) {
	w.line("0", "ARC", "8", layer,
		"10", num(cx), "20", num(cy), "30", "0",
		"40", num(r), "50", num(a0), "51", num(a1))
}

func (w *writer) circle(layer string, c core.Point, r float64) {
	w.line("0", "CIRCLE", "8", layer,
		"10", num(c.X.ToMM()), "20", num(c.Y.ToMM()), "30", "0",
		"40", num(r))
}

// text writes a middle-centred label (72=1 centre, 73=2 middle).
func (w *writer) text(layer string, c core.Point, h float64, s string) {
	x, y := num(c.X.ToMM()), num(c.Y.ToMM())
	w.line("0", "TEXT", "8", layer,
		"10", x, "20", y, "30", "0",
		"40", num(h), "1", s,
		"72", "1", "73", "2",
		"11", x, "21", y, "31", "0")
}
