package parts

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/mentasystems/fragua/internal/core"
)

// ParseKicadMod reads a .kicad_mod footprint. Both dialects are accepted: the
// v5 `(module NAME …)` form with `(layer F.SilkS) (width 0.12)` siblings, and
// the v6+ `(footprint "NAME" …)` form with quoted layers and `(stroke (width))`.
//
// KiCad footprints are mm with Y growing downwards; core.LibraryPad is mm Y-up,
// so every Y is negated. F.SilkS/B.SilkS graphics become silk; F.CrtYd is taken
// as the courtyard and folded into BodyRect (Fragua has no courtyard layer, and
// BodyRect is exactly the placement keep-out).
func ParseKicadMod(src []byte, key string) (*Part, error) {
	nodes, err := parseSexp(string(src))
	if err != nil {
		return nil, fmt.Errorf("kicad_mod: %w", err)
	}
	var root sexp
	for _, n := range nodes {
		if n.name() == "footprint" || n.name() == "module" {
			root = n
			break
		}
	}
	if !root.isL {
		return nil, fmt.Errorf("kicad_mod: no (footprint …) / (module …) node")
	}
	name := unquote(root.arg(0))
	if key == "" {
		key = name
	}

	var pads []core.LibraryPad
	var silk []core.LibrarySilk
	courtyard := newBoundsAcc()
	seen := map[string]int{}
	npth := 0

	for _, node := range root.list {
		if !node.isL {
			continue
		}
		switch node.name() {
		case "pad":
			pad, ok, isNPTH := kicadPad(node)
			if isNPTH {
				npth++
				continue
			}
			if !ok {
				continue
			}
			if n, dup := seen[pad.Number]; dup {
				seen[pad.Number] = n + 1
				pad.Number = fmt.Sprintf("%s_%d", pad.Number, n+1)
			} else {
				seen[pad.Number] = 1
			}
			pads = append(pads, pad)
		case "fp_line", "fp_arc", "fp_circle", "fp_rect", "fp_poly":
			layer := kicadGraphicLayer(node)
			pts := kicadGraphicPoints(node)
			if layer == "F.CrtYd" || layer == "B.CrtYd" {
				for _, p := range pts {
					courtyard.addPoint(p[0], -p[1])
				}
				continue
			}
			sl, ok := kicadSilkLayer(layer)
			if !ok {
				continue
			}
			silk = append(silk, polylineSilk(pts, kicadStrokeWidth(node), sl,
				func(x, y float64) (float64, float64) { return x, -y })...)
		}
	}
	if len(pads) == 0 {
		return nil, fmt.Errorf("kicad_mod: %s has no copper pads", name)
	}
	for _, p := range pads {
		courtyard.addPoint(p.XMM-p.WMM/2, p.YMM-p.HMM/2)
		courtyard.addPoint(p.XMM+p.WMM/2, p.YMM+p.HMM/2)
	}

	desc := name
	if d, ok := root.child("descr"); ok {
		if v := unquote(d.arg(0)); v != "" {
			desc = v
		}
	}
	tags := ""
	if t, ok := root.child("tags"); ok {
		tags = unquote(strings.Join(t.args(), " "))
	}
	part := &Part{
		Entry: core.LibraryEntry{
			Key:         key,
			Description: strings.TrimSpace(desc),
			Pads:        pads,
			Silk:        silk,
			BodyRect:    courtyard.rect(),
		},
		Kind: inferKind(tags+" "+name, pads),
	}
	part.Pins = genericPins(pads)
	part.finish(SourceKiCad)
	if npth > 0 {
		// NPTH slots have no copper and no home in LibraryEntry; the courtyard
		// still covers them for placement.
		part.Entry.Description = strings.TrimSpace(fmt.Sprintf("%s (%d NPTH dropped)", part.Entry.Description, npth))
	}
	return part, nil
}

// kicadPad converts one (pad …) node. Returns isNPTH for mechanical holes,
// which carry no copper and are skipped rather than faked as pads.
func kicadPad(node sexp) (core.LibraryPad, bool, bool) {
	args := node.args()
	if len(args) < 3 {
		return core.LibraryPad{}, false, false
	}
	number := unquote(args[0])
	kind := args[1]  // smd | thru_hole | np_thru_hole | connect
	shape := args[2] // rect | roundrect | circle | oval | trapezoid | custom
	if kind == "np_thru_hole" {
		return core.LibraryPad{}, false, true
	}
	x, y, ok := node.xy("at")
	if !ok {
		return core.LibraryPad{}, false, false
	}
	at, _ := node.child("at")
	rot := at.argF(2)
	w, h, ok := node.xy("size")
	if !ok || w <= 0 || h <= 0 {
		return core.LibraryPad{}, false, false
	}
	// `custom` pads carry primitives; approximate by the declared size box.
	if shape == "custom" {
		if prim, has := node.child("primitives"); has {
			b := newBoundsAcc()
			for _, p := range kicadPrimitivePoints(prim) {
				b.addPoint(p[0], p[1])
			}
			if b.any {
				pw, ph := b.size()
				w, h = math.Max(w, pw), math.Max(h, ph)
			}
		}
	}
	w, h = rotatedExtent(w, h, rot)
	if number == "" {
		return core.LibraryPad{}, false, false
	}
	pad := core.LibraryPad{Number: number, XMM: x, YMM: -y, WMM: w, HMM: h}
	if d, has := node.child("drill"); has {
		pad.DrillMM = kicadDrill(d, w, h)
	}
	return pad, true, false
}

// kicadDrill reads (drill D) or (drill oval DX DY). Only one round drill fits
// in core.LibraryPad, so an oval takes its smaller axis: that is the real bit
// diameter, and widening it would fake a negative annular ring.
func kicadDrill(d sexp, padW, padH float64) *float64 {
	args := d.args()
	if len(args) == 0 {
		return nil
	}
	var v float64
	if strings.EqualFold(args[0], "oval") && len(args) >= 3 {
		v = math.Min(parseFloat(args[1]), parseFloat(args[2]))
	} else {
		v = parseFloat(args[0])
	}
	if v <= 0 {
		return nil
	}
	if lim := math.Min(padW, padH); lim > 0 && v > lim {
		v = lim
	}
	return &v
}

// kicadGraphicLayer reads (layer X) in either dialect (quoted or bare).
func kicadGraphicLayer(node sexp) string {
	if l, ok := node.child("layer"); ok {
		return unquote(l.arg(0))
	}
	if l, ok := node.child("layers"); ok {
		return unquote(l.arg(0))
	}
	return ""
}

func kicadSilkLayer(layer string) (core.SilkLayer, bool) {
	switch layer {
	case "F.SilkS":
		return core.SilkTop, true
	case "B.SilkS":
		return core.SilkBottom, true
	}
	return "", false
}

// kicadStrokeWidth reads (width W) (v5) or (stroke (width W)) (v6+).
func kicadStrokeWidth(node sexp) float64 {
	if s, ok := node.child("stroke"); ok {
		if w, ok := s.child("width"); ok {
			return w.argF(0)
		}
	}
	if w, ok := node.child("width"); ok {
		return w.argF(0)
	}
	return 0.12
}

// kicadGraphicPoints flattens a graphic node into a polyline (KiCad mm, Y-down).
// Arcs and circles are approximated by segments; polygons close back to their
// first point.
func kicadGraphicPoints(node sexp) [][2]float64 {
	switch node.name() {
	case "fp_line":
		sx, sy, ok1 := node.xy("start")
		ex, ey, ok2 := node.xy("end")
		if !ok1 || !ok2 {
			return nil
		}
		return [][2]float64{{sx, sy}, {ex, ey}}
	case "fp_rect":
		sx, sy, ok1 := node.xy("start")
		ex, ey, ok2 := node.xy("end")
		if !ok1 || !ok2 {
			return nil
		}
		return [][2]float64{{sx, sy}, {ex, sy}, {ex, ey}, {sx, ey}, {sx, sy}}
	case "fp_circle":
		cx, cy, ok1 := node.xy("center")
		ex, ey, ok2 := node.xy("end")
		if !ok1 || !ok2 {
			return nil
		}
		return circlePoints(cx, cy, math.Hypot(ex-cx, ey-cy), 16)
	case "fp_arc":
		return kicadArcPoints(node)
	case "fp_poly":
		pts, ok := node.child("pts")
		if !ok {
			return nil
		}
		var out [][2]float64
		for _, xy := range pts.children("xy") {
			out = append(out, [2]float64{xy.argF(0), xy.argF(1)})
		}
		if len(out) > 2 {
			out = append(out, out[0])
		}
		return out
	}
	return nil
}

// kicadArcPoints handles both arc dialects: v6+ start/mid/end (three points on
// the circle) and v5 start=centre + end + (angle A).
func kicadArcPoints(node sexp) [][2]float64 {
	sx, sy, ok1 := node.xy("start")
	ex, ey, ok2 := node.xy("end")
	if !ok1 || !ok2 {
		return nil
	}
	if mx, my, ok := node.xy("mid"); ok {
		cx, cy, r, ok := circleThrough(sx, sy, mx, my, ex, ey)
		if !ok {
			return [][2]float64{{sx, sy}, {mx, my}, {ex, ey}}
		}
		a0 := math.Atan2(sy-cy, sx-cx)
		am := math.Atan2(my-cy, mx-cx)
		a1 := math.Atan2(ey-cy, ex-cx)
		// Walk the direction that passes through the mid point.
		if !angleBetween(a0, am, a1) {
			a1 -= 2 * math.Pi * math.Copysign(1, a1-a0)
		}
		return arcPoints(cx, cy, r, a0, a1, 12)
	}
	if a, ok := node.child("angle"); ok {
		cx, cy := sx, sy
		r := math.Hypot(ex-cx, ey-cy)
		a0 := math.Atan2(ey-cy, ex-cx)
		return arcPoints(cx, cy, r, a0, a0+a.argF(0)*math.Pi/180, 12)
	}
	return [][2]float64{{sx, sy}, {ex, ey}}
}

// circleThrough returns the circle through three points.
func circleThrough(x1, y1, x2, y2, x3, y3 float64) (cx, cy, r float64, ok bool) {
	d := 2 * (x1*(y2-y3) + x2*(y3-y1) + x3*(y1-y2))
	if math.Abs(d) < 1e-12 {
		return 0, 0, 0, false
	}
	s1 := x1*x1 + y1*y1
	s2 := x2*x2 + y2*y2
	s3 := x3*x3 + y3*y3
	cx = (s1*(y2-y3) + s2*(y3-y1) + s3*(y1-y2)) / d
	cy = (s1*(x3-x2) + s2*(x1-x3) + s3*(x2-x1)) / d
	return cx, cy, math.Hypot(x1-cx, y1-cy), true
}

// angleBetween reports whether m lies on the a→b sweep taken the short way.
func angleBetween(a, m, b float64) bool {
	norm := func(v float64) float64 {
		for v < 0 {
			v += 2 * math.Pi
		}
		return math.Mod(v, 2*math.Pi)
	}
	return norm(m-a) <= norm(b-a)
}

func arcPoints(cx, cy, r, a0, a1 float64, steps int) [][2]float64 {
	out := make([][2]float64, 0, steps+1)
	for i := 0; i <= steps; i++ {
		a := a0 + (a1-a0)*float64(i)/float64(steps)
		out = append(out, [2]float64{cx + r*math.Cos(a), cy + r*math.Sin(a)})
	}
	return out
}

// kicadPrimitivePoints collects the vertices of custom-pad primitives.
func kicadPrimitivePoints(prim sexp) [][2]float64 {
	var out [][2]float64
	for _, c := range prim.list {
		if !c.isL {
			continue
		}
		switch c.name() {
		case "gr_line", "gr_rect", "gr_circle", "gr_arc":
			if x, y, ok := c.xy("start"); ok {
				out = append(out, [2]float64{x, y})
			}
			if x, y, ok := c.xy("center"); ok {
				out = append(out, [2]float64{x, y})
			}
			if x, y, ok := c.xy("end"); ok {
				out = append(out, [2]float64{x, y})
			}
		case "gr_poly":
			if pts, ok := c.child("pts"); ok {
				for _, xy := range pts.children("xy") {
					out = append(out, [2]float64{xy.argF(0), xy.argF(1)})
				}
			}
		}
	}
	return out
}

// LoadKicadMod reads and parses a .kicad_mod file. An empty key defaults to the
// footprint name inside the file.
func LoadKicadMod(path, key string) (*Part, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	p, err := ParseKicadMod(src, key)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return p, nil
}
