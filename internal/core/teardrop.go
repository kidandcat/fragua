package core

import "math"

// TeardropLengthFactor is teardrop length as a multiple of the pad/via
// copper radius (half the pad dimension perpendicular to the trace, or
// via diameter/2). 1.25× is a typical CAM default.
const TeardropLengthFactor = 1.25

// Teardrop is a tapered copper region at a trace↔pad or trace↔via joint.
// Poly is a 4-vertex flare (wide at the pad/via, narrow at the trace).
type Teardrop struct {
	Net   string
	Layer Layer
	Poly  []Point
}

// BuildTeardrops returns copper flares for every trace endpoint that
// lands on a same-net pad or via. Empty when board.Teardrops is false.
func BuildTeardrops(b *Board) []Teardrop {
	if b == nil || !b.Teardrops {
		return nil
	}
	var out []Teardrop
	for i := range b.Traces {
		t := &b.Traces[i]
		if t.Width <= 0 {
			continue
		}
		if td, ok := teardropAtEnd(b, t, t.Start, t.End); ok {
			out = append(out, td)
		}
		if td, ok := teardropAtEnd(b, t, t.End, t.Start); ok {
			out = append(out, td)
		}
	}
	return out
}

func teardropAtEnd(b *Board, t *Trace, end, other Point) (Teardrop, bool) {
	jx, jy, padHalf, ok := junctionAt(b, t, end)
	if !ok {
		return Teardrop{}, false
	}
	traceHalf := t.Width.ToMM() / 2
	if padHalf <= traceHalf+1e-6 {
		return Teardrop{}, false
	}
	dx := other.X.ToMM() - jx
	dy := other.Y.ToMM() - jy
	seg := math.Hypot(dx, dy)
	if seg < 1e-6 {
		return Teardrop{}, false
	}
	ux, uy := dx/seg, dy/seg
	length := padHalf * TeardropLengthFactor
	if length > 0.8*seg {
		length = 0.8 * seg
	}
	if length < 0.05 {
		return Teardrop{}, false
	}
	poly := flareQuad(jx, jy, ux, uy, padHalf, traceHalf, length)
	return Teardrop{Net: t.Net, Layer: t.Layer, Poly: poly}, true
}

// junctionAt finds a same-net pad or via that the trace endpoint touches.
// Returns the copper centre (mm), the half-width of that copper perpendicular
// to the trace (mm), and whether a junction exists.
func junctionAt(b *Board, t *Trace, end Point) (cx, cy, padHalf float64, ok bool) {
	ex, ey := end.X.ToMM(), end.Y.ToMM()
	tol := t.Width.ToMM()/2 + 1e-3
	bestHalf := 0.0
	found := false

	for _, id := range b.FootprintOrder {
		fp := b.Footprints[id]
		if fp == nil {
			continue
		}
		for i := range fp.Pads {
			pad := &fp.Pads[i]
			if !PadOccupiesLayer(pad, t.Layer) {
				continue
			}
			if pad.Net == nil || *pad.Net != t.Net {
				continue
			}
			c := PadWorldCenter(fp, pad)
			pw, ph := PadWorldSize(fp, pad)
			dx := math.Abs(ex-c.X.ToMM()) - pw.ToMM()/2
			dy := math.Abs(ey-c.Y.ToMM()) - ph.ToMM()/2
			if dx > tol || dy > tol {
				continue
			}
			// Half-width perpendicular to the leaving trace: use the
			// pad extent across the trace direction (max of the two
			// half-sizes so a rectangular pad still fillets).
			half := math.Max(pw.ToMM(), ph.ToMM()) / 2
			if !found || half > bestHalf {
				cx, cy, bestHalf, found = c.X.ToMM(), c.Y.ToMM(), half, true
			}
		}
	}
	// Footprints not in order (defensive, same as DRC).
	seen := make(map[string]bool, len(b.FootprintOrder))
	for _, id := range b.FootprintOrder {
		seen[id] = true
	}
	for id, fp := range b.Footprints {
		if fp == nil || seen[id] {
			continue
		}
		for i := range fp.Pads {
			pad := &fp.Pads[i]
			if !PadOccupiesLayer(pad, t.Layer) {
				continue
			}
			if pad.Net == nil || *pad.Net != t.Net {
				continue
			}
			c := PadWorldCenter(fp, pad)
			pw, ph := PadWorldSize(fp, pad)
			dx := math.Abs(ex-c.X.ToMM()) - pw.ToMM()/2
			dy := math.Abs(ey-c.Y.ToMM()) - ph.ToMM()/2
			if dx > tol || dy > tol {
				continue
			}
			half := math.Max(pw.ToMM(), ph.ToMM()) / 2
			if !found || half > bestHalf {
				cx, cy, bestHalf, found = c.X.ToMM(), c.Y.ToMM(), half, true
			}
		}
	}

	for i := range b.Vias {
		v := &b.Vias[i]
		if v.Net != t.Net {
			continue
		}
		if v.FromLayer != nil && v.ToLayer != nil {
			lo, hi := v.FromLayer.Index, v.ToLayer.Index
			if lo > hi {
				lo, hi = hi, lo
			}
			if t.Layer.Index < lo || t.Layer.Index > hi {
				continue
			}
		}
		r := v.Diameter.ToMM() / 2
		dx, dy := ex-v.Position.X.ToMM(), ey-v.Position.Y.ToMM()
		if dx*dx+dy*dy > (r+tol)*(r+tol) {
			continue
		}
		if !found || r > bestHalf {
			cx, cy, bestHalf, found = v.Position.X.ToMM(), v.Position.Y.ToMM(), r, true
		}
	}
	return cx, cy, bestHalf, found
}

// flareQuad is a two-segment taper: width 2*wideHalf at the junction,
// 2*narrowHalf at distance `length` along (ux,uy).
func flareQuad(jx, jy, ux, uy, wideHalf, narrowHalf, length float64) []Point {
	px, py := -uy, ux
	farX, farY := jx+ux*length, jy+uy*length
	return []Point{
		NewPoint(FromMM(jx+px*wideHalf), FromMM(jy+py*wideHalf)),
		NewPoint(FromMM(farX+px*narrowHalf), FromMM(farY+py*narrowHalf)),
		NewPoint(FromMM(farX-px*narrowHalf), FromMM(farY-py*narrowHalf)),
		NewPoint(FromMM(jx-px*wideHalf), FromMM(jy-py*wideHalf)),
	}
}

// TeardropAABB is the axis-aligned box of a teardrop polygon.
func (t Teardrop) AABB() Rect {
	if len(t.Poly) == 0 {
		return Rect{}
	}
	r := Rect{Min: t.Poly[0], Max: t.Poly[0]}
	for _, p := range t.Poly[1:] {
		if p.X < r.Min.X {
			r.Min.X = p.X
		}
		if p.Y < r.Min.Y {
			r.Min.Y = p.Y
		}
		if p.X > r.Max.X {
			r.Max.X = p.X
		}
		if p.Y > r.Max.Y {
			r.Max.Y = p.Y
		}
	}
	return r
}
