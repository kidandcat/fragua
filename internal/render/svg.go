// Package render produces board/schematic SVG (and later PNG) for UI and agents.
package render

import (
	"fmt"
	"strings"

	"github.com/mentasystems/fragua/internal/core"
)

// BoardSVG renders a simple top-view SVG of the board copper and pads.
func BoardSVG(board *core.Board) string {
	if board == nil {
		return `<svg xmlns="http://www.w3.org/2000/svg" width="400" height="300"/>`
	}
	var minX, minY, maxX, maxY float64
	if board.Outline != nil {
		minX = board.Outline.Min.X.ToMM()
		minY = board.Outline.Min.Y.ToMM()
		maxX = board.Outline.Max.X.ToMM()
		maxY = board.Outline.Max.Y.ToMM()
	} else {
		maxX, maxY = 40, 30
	}
	// padding
	pad := 2.0
	w := maxX - minX + 2*pad
	h := maxY - minY + 2*pad
	if w < 1 {
		w = 40
	}
	if h < 1 {
		h = 30
	}
	scale := 10.0 // 10 px per mm
	vw := w * scale
	vh := h * scale
	tx := func(x float64) float64 { return (x - minX + pad) * scale }
	ty := func(y float64) float64 { return (maxY - y + pad) * scale } // flip Y for screen

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %.1f %.1f" width="%.0f" height="%.0f">`, vw, vh, vw, vh)
	b.WriteString(`<rect width="100%" height="100%" fill="#1a1a1a"/>`)
	// substrate
	fmt.Fprintf(&b, `<rect x="%.2f" y="%.2f" width="%.2f" height="%.2f" fill="#2d4a3e" stroke="#4a7c59" stroke-width="1"/>`,
		tx(minX), ty(maxY), (maxX-minX)*scale, (maxY-minY)*scale)

	// traces
	for _, tr := range board.Traces {
		color := "#c9a227"
		if tr.Layer.Index == 1 {
			color = "#4a90d9"
		}
		fmt.Fprintf(&b, `<line x1="%.2f" y1="%.2f" x2="%.2f" y2="%.2f" stroke="%s" stroke-width="%.2f" stroke-linecap="round"/>`,
			tx(tr.Start.X.ToMM()), ty(tr.Start.Y.ToMM()),
			tx(tr.End.X.ToMM()), ty(tr.End.Y.ToMM()),
			color, tr.Width.ToMM()*scale)
	}
	// vias
	for _, v := range board.Vias {
		fmt.Fprintf(&b, `<circle cx="%.2f" cy="%.2f" r="%.2f" fill="#888" stroke="#ccc" stroke-width="0.5"/>`,
			tx(v.Position.X.ToMM()), ty(v.Position.Y.ToMM()), v.Diameter.ToMM()*scale/2)
	}
	// pads
	for _, fp := range board.Footprints {
		if fp == nil {
			continue
		}
		for i := range fp.Pads {
			pad := &fp.Pads[i]
			aabb := core.PadWorldAABB(fp, pad)
			fmt.Fprintf(&b, `<rect x="%.2f" y="%.2f" width="%.2f" height="%.2f" fill="#d4af37" opacity="0.9"/>`,
				tx(aabb.Min.X.ToMM()), ty(aabb.Max.Y.ToMM()),
				aabb.Width().ToMM()*scale, aabb.Height().ToMM()*scale)
		}
		// ref designator
		fmt.Fprintf(&b, `<text x="%.2f" y="%.2f" fill="#eee" font-size="8" font-family="monospace">%s</text>`,
			tx(fp.Position.X.ToMM()), ty(fp.Position.Y.ToMM())-4, escape(fp.Reference))
	}
	b.WriteString(`</svg>`)
	return b.String()
}

func escape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}
