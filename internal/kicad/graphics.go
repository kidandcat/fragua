package kicad

import (
	"fmt"
	"math"

	"github.com/mentasystems/fragua/internal/core"
)

// mountHoles returns the NPTH list under either field name.
func (e *exporter) mountHoles() []core.MountHole {
	if len(e.board.MountHoles) > 0 {
		return e.board.MountHoles
	}
	return e.board.Holes
}

// emitHoles writes each NPTH mount hole as its own footprint with one
// np_thru_hole pad — the shape KiCad's own MountingHole footprints have.
func (e *exporter) emitHoles() {
	for i, h := range e.mountHoles() {
		d := num(h.Diameter.ToMM())
		label := h.Label
		if label == "" {
			label = fmt.Sprintf("H%d", i+1)
		}
		e.line(1, "(footprint %q", "fragua:MountingHole")
		e.line(2, "(layer %q)", "F.Cu")
		e.line(2, "(uuid %q)", e.uuid("hole/"+h.ID.String()))
		e.line(2, "(at %s %s)", e.x(h.Center.X), e.y(h.Center.Y))
		e.line(2, "(descr %q)", "NPTH mounting hole")
		e.line(2, "(attr exclude_from_pos_files exclude_from_bom)")
		e.field2(e.uuid("holeref/"+h.ID.String()), "Reference", label, "F.SilkS")
		e.field2(e.uuid("holeval/"+h.ID.String()), "Value", "NPTH "+d+"mm", "F.Fab")
		e.line(2, "(pad %q np_thru_hole circle", "")
		e.line(3, "(at 0 0)")
		e.line(3, "(size %s %s)", d, d)
		e.line(3, "(drill %s)", d)
		e.line(3, "(layers %q %q)", "*.Cu", "*.Mask")
		e.line(3, "(uuid %q)", e.uuid("holepad/"+h.ID.String()))
		e.line(2, ")")
		e.line(2, "(embedded_fonts no)")
		e.line(1, ")")
	}
}

func (e *exporter) field2(uid, name, value, layer string) {
	e.line(2, "(property %q %q", name, value)
	e.line(3, "(at 0 0 0)")
	e.line(3, "(layer %q)", layer)
	e.line(3, "(uuid %q)", uid)
	e.line(3, "(effects")
	e.line(4, "(font (size 1 1) (thickness 0.15))")
	e.line(3, ")")
	e.line(2, ")")
}

// ---------------------------------------------------------------- edge cuts

const edgeStrokeMM = 0.1

// emitEdge writes the board outline and every internal cutout on Edge.Cuts as
// closed loops — the same outer path the gerber writer uses, so the two packs
// cannot disagree about where the board ends.
func (e *exporter) emitEdge() {
	switch {
	case len(e.board.OutlinePoly) >= 3:
		e.edgeLoop("outline", e.board.OutlinePoly)
	case e.board.Outline != nil && e.board.OutlineCornerRadius > 0:
		e.edgeRounded(*e.board.Outline, e.board.OutlineCornerRadius)
	case e.board.Outline != nil:
		r := e.board.Outline
		e.edgeLoop("outline", []core.Point{
			{X: r.Min.X, Y: r.Min.Y}, {X: r.Max.X, Y: r.Min.Y},
			{X: r.Max.X, Y: r.Max.Y}, {X: r.Min.X, Y: r.Max.Y},
		})
	}
	for i, c := range e.board.Cutouts {
		if len(c.Polygon) >= 3 {
			e.edgeLoop(fmt.Sprintf("cutout/%d/%s", i, c.ID.String()), c.Polygon)
		}
	}
}

func (e *exporter) edgeLoop(key string, poly []core.Point) {
	for i := range poly {
		a, b := poly[i], poly[(i+1)%len(poly)]
		e.grLine(fmt.Sprintf("edge/%s/%d", key, i), a, b, edgeStrokeMM, "Edge.Cuts")
	}
}

// edgeRounded emits four straight edges plus four quarter arcs, matching the
// gerber writer's rounded outline.
func (e *exporter) edgeRounded(r core.Rect, rad core.Length) {
	x0, y0, x1, y1 := r.Min.X, r.Min.Y, r.Max.X, r.Max.Y
	if 2*rad > r.Width() || 2*rad > r.Height() {
		rad = core.Min(r.Width()/2, r.Height()/2)
	}
	pt := func(x, y core.Length) core.Point { return core.Point{X: x, Y: y} }
	segs := [][2]core.Point{
		{pt(x0+rad, y0), pt(x1-rad, y0)},
		{pt(x1, y0+rad), pt(x1, y1-rad)},
		{pt(x1-rad, y1), pt(x0+rad, y1)},
		{pt(x0, y1-rad), pt(x0, y0+rad)},
	}
	for i, s := range segs {
		e.grLine(fmt.Sprintf("edge/round/%d", i), s[0], s[1], edgeStrokeMM, "Edge.Cuts")
	}
	// Each arc turns a straight edge into the next one around a corner centre.
	corners := [][3]core.Point{
		{pt(x1-rad, y0), pt(x1, y0+rad), pt(x1-rad, y0+rad)},
		{pt(x1, y1-rad), pt(x1-rad, y1), pt(x1-rad, y1-rad)},
		{pt(x0+rad, y1), pt(x0, y1-rad), pt(x0+rad, y1-rad)},
		{pt(x0, y0+rad), pt(x0+rad, y0), pt(x0+rad, y0+rad)},
	}
	for i, c := range corners {
		e.grArc(fmt.Sprintf("edge/arc/%d", i), c[0], c[1], c[2], edgeStrokeMM, "Edge.Cuts")
	}
}

func (e *exporter) grLine(key string, a, b core.Point, widthMM float64, layer string) {
	e.line(1, "(gr_line")
	e.line(2, "(start %s %s)", e.x(a.X), e.y(a.Y))
	e.line(2, "(end %s %s)", e.x(b.X), e.y(b.Y))
	e.line(2, "(stroke (width %s) (type solid))", num(widthMM))
	e.line(2, "(layer %q)", layer)
	e.line(2, "(uuid %q)", e.uuid(key))
	e.line(1, ")")
}

// grArc writes the KiCad start/mid/end arc for a quarter turn about centre.
func (e *exporter) grArc(key string, start, end, centre core.Point, widthMM float64, layer string) {
	a0 := math.Atan2(float64(start.Y-centre.Y), float64(start.X-centre.X))
	a1 := math.Atan2(float64(end.Y-centre.Y), float64(end.X-centre.X))
	for a1-a0 > math.Pi {
		a1 -= 2 * math.Pi
	}
	for a1-a0 < -math.Pi {
		a1 += 2 * math.Pi
	}
	rad := math.Hypot(float64(start.X-centre.X), float64(start.Y-centre.Y))
	am := (a0 + a1) / 2
	mid := core.Point{
		X: centre.X + core.Length(math.Round(rad*math.Cos(am))),
		Y: centre.Y + core.Length(math.Round(rad*math.Sin(am))),
	}
	e.line(1, "(gr_arc")
	e.line(2, "(start %s %s)", e.x(start.X), e.y(start.Y))
	e.line(2, "(mid %s %s)", e.x(mid.X), e.y(mid.Y))
	e.line(2, "(end %s %s)", e.x(end.X), e.y(end.Y))
	e.line(2, "(stroke (width %s) (type solid))", num(widthMM))
	e.line(2, "(layer %q)", layer)
	e.line(2, "(uuid %q)", e.uuid(key))
	e.line(1, ")")
}

// ---------------------------------------------------------------- board silk

func (e *exporter) emitBoardSilk() {
	for i, s := range e.board.SilkLines {
		w := s.Width.ToMM()
		if w <= 0 {
			w = 0.12
		}
		e.grLine(fmt.Sprintf("silkline/%d", i), s.Start, s.End, w, silkLayerName(s.Layer))
	}
	for i, t := range e.board.SilkTexts {
		size := t.Size
		if size <= 0 {
			size = core.FromMM(1)
		}
		th := t.Width
		if th <= 0 {
			th = size / 8
		}
		e.line(1, "(gr_text %q", t.Text)
		e.line(2, "(at %s %s%s)", e.x(t.Position.X), e.y(t.Position.Y), angleSuffix(t.Rotation))
		e.line(2, "(layer %q)", silkLayerName(t.Layer))
		e.line(2, "(uuid %q)", e.uuid(fmt.Sprintf("silktext/%d", i)))
		e.line(2, "(effects")
		e.line(3, "(font (size %s %s) (thickness %s))", num(size.ToMM()), num(size.ToMM()), num(th.ToMM()))
		if j := justify(t.Anchor); j != "" {
			e.line(3, "(justify %s)", j)
		}
		e.line(2, ")")
		e.line(1, ")")
	}
}

// ---------------------------------------------------------------- copper

func (e *exporter) emitTracks() {
	for i := range e.board.Traces {
		t := &e.board.Traces[i]
		e.line(1, "(segment")
		e.line(2, "(start %s %s)", e.x(t.Start.X), e.y(t.Start.Y))
		e.line(2, "(end %s %s)", e.x(t.End.X), e.y(t.End.Y))
		e.line(2, "(width %s)", num(t.Width.ToMM()))
		e.line(2, "(layer %q)", e.layerName(t.Layer))
		e.line(2, "(net %d)", e.nets.idx(t.Net))
		e.line(2, "(uuid %q)", e.uuid("seg/"+t.ID.String()))
		e.line(1, ")")
	}
	n := e.stack.CopperCount()
	for i := range e.board.Vias {
		v := &e.board.Vias[i]
		from, to := 0, n-1
		kind := ""
		if v.FromLayer != nil {
			from = int(v.FromLayer.Index)
		}
		if v.ToLayer != nil {
			to = int(v.ToLayer.Index)
		}
		if from > to {
			from, to = to, from
		}
		if from != 0 || to != n-1 {
			kind = " blind"
		}
		e.line(1, "(via%s", kind)
		e.line(2, "(at %s %s)", e.x(v.Position.X), e.y(v.Position.Y))
		e.line(2, "(size %s)", num(v.Diameter.ToMM()))
		e.line(2, "(drill %s)", num(v.Drill.ToMM()))
		e.line(2, "(layers %q %q)", e.cuName(from), e.cuName(to))
		e.line(2, "(net %d)", e.nets.idx(v.Net))
		e.line(2, "(uuid %q)", e.uuid("via/"+v.ID.String()))
		e.line(1, ")")
	}
}
