package core

import "math"

// TextPolylines converts silk text into world-space polylines (Hershey strokes).
// Matches pcb_core::hershey::text_polylines.
func TextPolylines(text string, origin Point, size Length, rotationDeg float64, anchor SilkAnchor) [][]Point {
	if text == "" {
		return nil
	}
	scale := size.ToMM() / hersheyCapHeight
	n := 0
	for range text {
		n++
	}
	totalW := float64(n*hersheyAdvance) * scale
	anchorDx := 0.0
	switch anchor {
	case SilkAnchorMiddle:
		anchorDx = -totalW / 2
	case SilkAnchorEnd:
		anchorDx = -totalW
	}
	theta := rotationDeg * math.Pi / 180
	sinT, cosT := math.Sin(theta), math.Cos(theta)
	ox, oy := origin.X.ToMM(), origin.Y.ToMM()
	toWorld := func(xu, yu float64) Point {
		lx := anchorDx + xu*scale
		ly := yu * scale
		wx := lx*cosT - ly*sinT + ox
		wy := lx*sinT + ly*cosT + oy
		return Point{X: FromMM(wx), Y: FromMM(wy)}
	}

	var out [][]Point
	penX := 0.0
	for _, ch := range text {
		g, ok := hersheyGlyphs[ch]
		if !ok {
			g = [][][2]int8{{{4, 0}, {20, 0}, {20, 28}, {4, 28}, {4, 0}}}
		}
		for _, stroke := range g {
			if len(stroke) < 2 {
				continue
			}
			poly := make([]Point, len(stroke))
			for i, p := range stroke {
				poly[i] = toWorld(penX+float64(p[0]), float64(p[1]))
			}
			out = append(out, poly)
		}
		penX += float64(hersheyAdvance)
	}
	return out
}

// DefaultSilkStroke is size/8 (KiCad-ish), matching Rust default_silk_stroke.
func DefaultSilkStroke(size Length) Length {
	return Length(size / 8)
}
