package kicad

import (
	"fmt"
	"strings"

	"github.com/mentasystems/fragua/internal/core"
)

func (e *exporter) emitZones() {
	for i := range e.board.Pours {
		e.emitPour(i, &e.board.Pours[i])
	}
	for i := range e.board.Keepouts {
		k := &e.board.Keepouts[i]
		poly := k.Polygon
		if len(poly) < 3 {
			if k.Rect == nil {
				continue
			}
			poly = rectPoly(*k.Rect)
		}
		e.emitKeepout(uuidFor("keepout/"+k.ID.String()), fmt.Sprintf("keepout%d", i+1), poly,
			k.NoCopper || (!k.NoCopper && !k.NoPlace), k.NoPlace)
	}
	// A rule area is a local override, not a keepout; KiCad has no matching
	// object, so it is exported as a documented placement keepout that keeps
	// copper allowed — the constraint itself stays in the Fragua project.
	for i := range e.board.RuleAreas {
		ra := &e.board.RuleAreas[i]
		name := ra.Name
		if name == "" {
			name = fmt.Sprintf("rule-area%d", i+1)
		}
		e.emitKeepout(uuidFor("rulearea/"+ra.ID.String()), "fragua:"+name, rectPoly(ra.Rect), false, true)
	}
}

func rectPoly(r core.Rect) []core.Point {
	return []core.Point{
		{X: r.Min.X, Y: r.Min.Y}, {X: r.Max.X, Y: r.Min.Y},
		{X: r.Max.X, Y: r.Max.Y}, {X: r.Min.X, Y: r.Max.Y},
	}
}

// pourOutline is the pour's own polygon, or the board outline when the pour
// covers the whole board.
func (e *exporter) pourOutline(p *core.Pour) []core.Point {
	if len(p.Polygon) >= 3 {
		return p.Polygon
	}
	if len(e.board.OutlinePoly) >= 3 {
		return e.board.OutlinePoly
	}
	if e.board.Outline != nil {
		return rectPoly(*e.board.Outline)
	}
	return nil
}

func (e *exporter) emitPour(i int, p *core.Pour) {
	outline := e.pourOutline(p)
	if len(outline) < 3 {
		return
	}
	layer := e.layerName(p.Layer)
	gap, spoke := defaultThermalGapMM, defaultSpokeMM
	if p.ThermalRelief != nil {
		if p.ThermalRelief.GapMM > 0 {
			gap = p.ThermalRelief.GapMM
		}
		if p.ThermalRelief.SpokeWidthMM > 0 {
			spoke = p.ThermalRelief.SpokeWidthMM
		}
	}
	key := p.ID.String()
	if key == "" {
		key = fmt.Sprintf("%d/%s/%s", i, p.Net, layer)
	}
	e.line(1, "(zone")
	e.line(2, "(net %d)", e.nets.idx(p.Net))
	e.line(2, "(net_name %q)", p.Net)
	e.line(2, "(layer %q)", layer)
	e.line(2, "(uuid %q)", uuidFor("zone/"+key))
	e.line(2, "(name %q)", p.Net)
	e.line(2, "(hatch edge 0.5)")
	e.line(2, "(connect_pads")
	e.line(3, "(clearance %s)", num(pourClearanceMM))
	e.line(2, ")")
	e.line(2, "(min_thickness %s)", num(e.opts.FillGridMM))
	e.line(2, "(filled_areas_thickness no)")
	e.line(2, "(fill yes")
	if p.ThermalRelief != nil && !p.ThermalRelief.IsSpokes4() {
		e.line(3, "(thermal_gap 0)")
		e.line(3, "(thermal_bridge_width %s)", num(spoke))
	} else {
		e.line(3, "(thermal_gap %s)", num(gap))
		e.line(3, "(thermal_bridge_width %s)", num(spoke))
	}
	e.line(2, ")")
	e.polygon(2, "polygon", outline)
	if e.opts.FillZones {
		for _, r := range e.zoneFill(p, outline) {
			e.filledRect(layer, r)
		}
	}
	e.line(1, ")")
}

func (e *exporter) emitKeepout(uid, name string, poly []core.Point, noCopper, noPlace bool) {
	if len(poly) < 3 {
		return
	}
	var layers []string
	for i := 0; i < e.stack.CopperCount(); i++ {
		layers = append(layers, fmt.Sprintf("%q", e.cuName(i)))
	}
	allow := func(b bool) string {
		if b {
			return "not_allowed"
		}
		return "allowed"
	}
	e.line(1, "(zone")
	e.line(2, "(net 0)")
	e.line(2, "(net_name %q)", "")
	e.line(2, "(layers %s)", strings.Join(layers, " "))
	e.line(2, "(uuid %q)", uid)
	e.line(2, "(name %q)", name)
	e.line(2, "(hatch edge 0.5)")
	e.line(2, "(connect_pads")
	e.line(3, "(clearance 0)")
	e.line(2, ")")
	e.line(2, "(min_thickness 0.25)")
	e.line(2, "(keepout")
	e.line(3, "(tracks %s)", allow(noCopper))
	e.line(3, "(vias %s)", allow(noCopper))
	e.line(3, "(pads %s)", allow(noCopper))
	e.line(3, "(copperpour %s)", allow(noCopper))
	e.line(3, "(footprints %s)", allow(noPlace))
	e.line(2, ")")
	e.line(2, "(fill")
	e.line(3, "(thermal_gap 0.5)")
	e.line(3, "(thermal_bridge_width 0.5)")
	e.line(2, ")")
	e.polygon(2, "polygon", poly)
	e.line(1, ")")
}

func (e *exporter) polygon(depth int, tag string, poly []core.Point) {
	e.line(depth, "(%s", tag)
	e.line(depth+1, "(pts")
	for _, p := range poly {
		e.line(depth+2, "(xy %s %s)", e.x(p.X), e.y(p.Y))
	}
	e.line(depth+1, ")")
	e.line(depth, ")")
}

// filledRect writes one rectangle of precomputed pour copper. The fill is kept
// on one line: a plane can be thousands of these.
func (e *exporter) filledRect(layer string, r core.Rect) {
	x0, y0 := e.x(r.Min.X), e.y(r.Min.Y)
	x1, y1 := e.x(r.Max.X), e.y(r.Max.Y)
	e.line(2, "(filled_polygon (layer %q) (pts (xy %s %s) (xy %s %s) (xy %s %s) (xy %s %s)))",
		layer, x0, y0, x1, y0, x1, y1, x0, y1)
}
