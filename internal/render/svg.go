// Package render produces board/schematic SVG matching Rust pcb-render.
package render

import (
	"fmt"
	"math"
	"strings"

	"github.com/mentasystems/fragua/internal/core"
)

var padPalette = []string{
	"#c97a2b", "#2b6cc9", "#3aa66c", "#a63a8c",
	"#d6b500", "#b0303a", "#3aa6a6", "#9c6b3a",
}
var tracePalette = []string{
	"#ffd166", "#4ec9ff", "#84e8b3", "#e495d2",
	"#ffe89a", "#ff95a0", "#9ce5e5", "#deb887",
}

// BoardSVG renders a top-view SVG in the same visual language as
// crates/pcb-render (dark canvas, mm grid, orange/cyan copper, pad
// names, drills, body outlines, dimension labels).
func BoardSVG(board *core.Board) string {
	if board == nil {
		return `<svg xmlns="http://www.w3.org/2000/svg" width="400" height="300"/>`
	}
	vx, vy, vw, vh := viewMM(board)
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="%.3f %.3f %.3f %.3f" width="100%%" height="100%%">`,
		vx, -(vy + vh), vw, vh)
	b.WriteString(`<g transform="scale(1,-1)">`)
	fmt.Fprintf(&b, `<rect x="%.3f" y="%.3f" width="%.3f" height="%.3f" fill="#0e1116"/>`, vx, vy, vw, vh)
	writeGrid(&b, vx, vy, vw, vh)

	if o := board.Outline; o != nil {
		rad := board.OutlineCornerRadius.ToMM()
		fmt.Fprintf(&b, `<rect x="%.3f" y="%.3f" width="%.3f" height="%.3f" rx="%.3f" ry="%.3f" fill="#5a3a1f" fill-opacity="0.72"/>`,
			o.Min.X.ToMM(), o.Min.Y.ToMM(), o.Width().ToMM(), o.Height().ToMM(), rad, rad)
		for _, pour := range board.Pours {
			writePour(&b, board, pour, *o)
		}
		fmt.Fprintf(&b, `<rect x="%.3f" y="%.3f" width="%.3f" height="%.3f" rx="%.3f" ry="%.3f" fill="none" stroke="#d6905b" stroke-width="0.4"/>`,
			o.Min.X.ToMM(), o.Min.Y.ToMM(), o.Width().ToMM(), o.Height().ToMM(), rad, rad)
		cx := (o.Min.X.ToMM() + o.Max.X.ToMM()) / 2
		cy := (o.Min.Y.ToMM() + o.Max.Y.ToMM()) / 2
		fmt.Fprintf(&b, `<g transform="translate(%.3f,%.3f) scale(1,-1)"><text x="0" y="0" text-anchor="middle" font-family="ui-monospace, monospace" font-size="1.4" fill="#d6905b">%.1f mm</text></g>`,
			cx, o.Max.Y.ToMM()+1.8, o.Width().ToMM())
		fmt.Fprintf(&b, `<g transform="translate(%.3f,%.3f) scale(1,-1) rotate(-90)"><text x="0" y="0" text-anchor="middle" font-family="ui-monospace, monospace" font-size="1.4" fill="#d6905b">%.1f mm</text></g>`,
			o.Min.X.ToMM()-1.8, cy, o.Height().ToMM())
	}

	b.WriteString(`<g pointer-events="none" stroke="#d6905b" stroke-width="0.08" opacity="0.6"><line x1="-1.5" y1="0" x2="1.5" y2="0"/><line x1="0" y1="-1.5" x2="0" y2="1.5"/></g>`)
	b.WriteString(`<g transform="translate(0.4,0.4) scale(1,-1)"><text x="0" y="0" font-family="ui-monospace, monospace" font-size="0.9" fill="#d6905b" opacity="0.7">0,0</text></g>`)

	// Bottom copper first, then footprints (pads + names), then top copper, then vias.
	for _, tr := range board.Traces {
		if tr.Layer.Index != 0 {
			writeTrace(&b, tr)
		}
	}
	for _, id := range board.FootprintOrder {
		if fp := board.Footprints[id]; fp != nil {
			writeFootprint(&b, fp)
		}
	}
	for _, fp := range board.Footprints {
		if fp == nil {
			continue
		}
		seen := false
		for _, id := range board.FootprintOrder {
			if board.Footprints[id] == fp {
				seen = true
				break
			}
		}
		if !seen {
			writeFootprint(&b, fp)
		}
	}
	for _, tr := range board.Traces {
		if tr.Layer.Index == 0 {
			writeTrace(&b, tr)
		}
	}
	for _, v := range board.Vias {
		writeVia(&b, v)
	}
	writeSilk(&b, board)
	if o := board.Outline; o != nil {
		fmt.Fprintf(&b, `<rect x="%.3f" y="%.3f" width="%.3f" height="%.3f" fill="none" stroke="#e8a86a" stroke-width="0.35"/>`,
			o.Min.X.ToMM(), o.Min.Y.ToMM(), o.Width().ToMM(), o.Height().ToMM())
	}
	b.WriteString(`</g></svg>`)
	return b.String()
}

func viewMM(board *core.Board) (x, y, w, h float64) {
	if o := board.Outline; o != nil {
		px := o.Width().ToMM() / 10
		py := o.Height().ToMM() / 10
		return o.Min.X.ToMM() - px, o.Min.Y.ToMM() - py, o.Width().ToMM() + 2*px, o.Height().ToMM() + 2*py
	}
	return -5, -5, 60, 60
}

func writeGrid(b *strings.Builder, vx, vy, vw, vh float64) {
	if vw > 400 || vh > 400 {
		return
	}
	b.WriteString(`<g pointer-events="none" stroke-width="0.03" fill="none">`)
	for x := math.Floor(vx); x <= vx+vw+1e-9; x++ {
		stroke := "#161b22"
		if int(math.Round(x))%5 == 0 {
			stroke = "#222a35"
		}
		fmt.Fprintf(b, `<line x1="%.3f" y1="%.3f" x2="%.3f" y2="%.3f" stroke="%s"/>`, x, vy, x, vy+vh, stroke)
	}
	for y := math.Floor(vy); y <= vy+vh+1e-9; y++ {
		stroke := "#161b22"
		if int(math.Round(y))%5 == 0 {
			stroke = "#222a35"
		}
		fmt.Fprintf(b, `<line x1="%.3f" y1="%.3f" x2="%.3f" y2="%.3f" stroke="%s"/>`, vx, y, vx+vw, y, stroke)
	}
	b.WriteString(`</g>`)
	for x := math.Ceil(vx/5) * 5; x <= vx+vw+1e-9; x += 5 {
		fmt.Fprintf(b, `<g transform="translate(%.3f,%.3f) scale(1,-1)"><text x="0" y="0" font-family="ui-monospace, monospace" font-size="0.9" fill="#3a4452">%d</text></g>`, x, vy+0.4, int(x))
	}
	for y := math.Ceil(vy/5) * 5; y <= vy+vh+1e-9; y += 5 {
		fmt.Fprintf(b, `<g transform="translate(%.3f,%.3f) scale(1,-1)"><text x="0" y="0" font-family="ui-monospace, monospace" font-size="0.9" fill="#3a4452">%d</text></g>`, vx+0.3, y, int(y))
	}
}

func writePour(b *strings.Builder, board *core.Board, pour core.Pour, outline core.Rect) {
	inset := 0.3
	x := outline.Min.X.ToMM() + inset
	y := outline.Min.Y.ToMM() + inset
	w := outline.Width().ToMM() - 2*inset
	h := outline.Height().ToMM() - 2*inset
	fill := padFill(pour.Layer)
	// evenodd: board rect minus clearance holes around foreign pads
	fmt.Fprintf(b, `<path fill="%s" fill-opacity="0.16" fill-rule="evenodd" d="M%.3f,%.3f h%.3f v%.3f h%.3f z`,
		fill, x, y, w, h, -w)
	halo := 0.55
	for _, fp := range board.Footprints {
		if fp == nil {
			continue
		}
		for i := range fp.Pads {
			pad := &fp.Pads[i]
			if !core.PadOccupiesLayer(pad, pour.Layer) {
				continue
			}
			if pad.Net != nil && *pad.Net == pour.Net {
				continue
			}
			aa := core.PadWorldAABB(fp, pad)
			fmt.Fprintf(b, ` M%.3f,%.3f h%.3f v%.3f h%.3f z`,
				aa.Min.X.ToMM()-halo, aa.Min.Y.ToMM()-halo,
				aa.Width().ToMM()+2*halo, aa.Height().ToMM()+2*halo,
				-(aa.Width().ToMM() + 2*halo))
		}
	}
	b.WriteString(`"/>`)
}

func writeTrace(b *strings.Builder, tr core.Trace) {
	fmt.Fprintf(b, `<line x1="%.3f" y1="%.3f" x2="%.3f" y2="%.3f" stroke="%s" stroke-width="%.3f" stroke-linecap="round"/>`,
		tr.Start.X.ToMM(), tr.Start.Y.ToMM(), tr.End.X.ToMM(), tr.End.Y.ToMM(),
		traceStroke(tr.Layer), tr.Width.ToMM())
}

func writeVia(b *strings.Builder, v core.Via) {
	cx, cy := v.Position.X.ToMM(), v.Position.Y.ToMM()
	outer, inner := v.Diameter.ToMM()/2, v.Drill.ToMM()/2
	if inner < 0.05 {
		inner = outer * 0.4
	}
	fmt.Fprintf(b, `<circle cx="%.3f" cy="%.3f" r="%.3f" fill="#7d8590"/><circle cx="%.3f" cy="%.3f" r="%.3f" fill="#0e1116"/>`,
		cx, cy, outer, cx, cy, inner)
}

func writeFootprint(b *strings.Builder, fp *core.Footprint) {
	fmt.Fprintf(b, `<g data-board-ref="%s" transform="translate(%.3f,%.3f) rotate(%.2f)">`,
		escape(fp.Reference), fp.Position.X.ToMM(), fp.Position.Y.ToMM(), fp.Rotation)
	// local pad bbox + 0.4 mm body
	if len(fp.Pads) > 0 {
		minX, minY := 1e9, 1e9
		maxX, maxY := -1e9, -1e9
		for i := range fp.Pads {
			p := &fp.Pads[i]
			ox, oy := p.Offset.X.ToMM(), p.Offset.Y.ToMM()
			hw, hh := p.Size[0].ToMM()/2, p.Size[1].ToMM()/2
			if ox-hw < minX {
				minX = ox - hw
			}
			if oy-hh < minY {
				minY = oy - hh
			}
			if ox+hw > maxX {
				maxX = ox + hw
			}
			if oy+hh > maxY {
				maxY = oy + hh
			}
		}
		minX -= 0.4
		minY -= 0.4
		maxX += 0.4
		maxY += 0.4
		fmt.Fprintf(b, `<rect x="%.3f" y="%.3f" width="%.3f" height="%.3f" fill="rgba(255,255,255,0.01)" stroke="#8b949e" stroke-width="0.1"/>`,
			minX, minY, maxX-minX, maxY-minY)
	}
	for i := range fp.Pads {
		writePad(b, &fp.Pads[i])
	}
	b.WriteString(`<g transform="scale(1,-1)" pointer-events="none">`)
	for i := range fp.Pads {
		p := &fp.Pads[i]
		pw, ph := p.Size[0].ToMM(), p.Size[1].ToMM()
		if pw < 0.8 || ph < 0.8 {
			continue
		}
		label := p.Name
		if label == "" {
			label = p.Number
		}
		chars := float64(len([]rune(label)))
		if chars < 1 {
			chars = 1
		}
		cap := math.Min(pw, ph) * 0.55
		if cap < 0.30 {
			cap = 0.30
		}
		if cap > 1.0 {
			cap = 1.0
		}
		byW := pw / chars * 1.4
		if byW < 0.30 {
			byW = 0.30
		}
		if byW > 1.0 {
			byW = 1.0
		}
		sz := math.Min(cap, byW)
		fmt.Fprintf(b, `<text x="%.3f" y="%.3f" text-anchor="middle" dominant-baseline="middle" font-family="ui-monospace, monospace" font-size="%.2f" fill="#0e1116">%s</text>`,
			p.Offset.X.ToMM(), -p.Offset.Y.ToMM(), sz, escape(label))
	}
	b.WriteString(`</g></g>`)
}

func writePad(b *strings.Builder, pad *core.Pad) {
	cx, cy := pad.Offset.X.ToMM(), pad.Offset.Y.ToMM()
	w, h := pad.Size[0].ToMM(), pad.Size[1].ToMM()
	fmt.Fprintf(b, `<rect x="%.3f" y="%.3f" width="%.3f" height="%.3f" fill="%s"/>`,
		cx-w/2, cy-h/2, w, h, padFill(pad.Layer))
	if pad.Net != nil && isGND(*pad.Net) {
		fmt.Fprintf(b, `<rect x="%.3f" y="%.3f" width="%.3f" height="%.3f" fill="none" stroke="#ff2bd6" stroke-width="0.15"/>`,
			cx-w/2, cy-h/2, w, h)
	}
	if pad.Drill != nil && *pad.Drill > 0 {
		fmt.Fprintf(b, `<circle cx="%.3f" cy="%.3f" r="%.3f" fill="#0e1116"/>`,
			cx, cy, pad.Drill.ToMM()/2)
	}
}

func writeSilk(b *strings.Builder, board *core.Board) {
	b.WriteString(`<g pointer-events="none" stroke="#e6edf3" stroke-linecap="round" fill="none">`)
	for _, ln := range board.SilkLines {
		fmt.Fprintf(b, `<line x1="%.3f" y1="%.3f" x2="%.3f" y2="%.3f" stroke-width="%.3f"/>`,
			ln.Start.X.ToMM(), ln.Start.Y.ToMM(), ln.End.X.ToMM(), ln.End.Y.ToMM(), ln.Width.ToMM())
	}
	b.WriteString(`</g>`)
	for _, t := range board.SilkTexts {
		sz := t.Size.ToMM()
		if sz < 0.4 {
			sz = 0.9
		}
		fmt.Fprintf(b, `<g transform="translate(%.3f,%.3f) scale(1,-1)"><text x="0" y="0" text-anchor="middle" dominant-baseline="middle" font-family="ui-monospace, monospace" font-size="%.2f" fill="#e6edf3">%s</text></g>`,
			t.Position.X.ToMM(), t.Position.Y.ToMM(), sz, escape(t.Text))
	}
	// Default REF if the footprint has no silk text.
	for _, fp := range board.Footprints {
		if fp == nil || fp.Reference == "" {
			continue
		}
		has := false
		for _, s := range fp.Silk {
			if s.Kind == "text" && strings.Contains(s.Text, "{REF}") {
				has = true
				break
			}
		}
		if has {
			// still draw resolved REF from silk items
			for _, s := range fp.Silk {
				if s.Kind != "text" {
					continue
				}
				txt := strings.ReplaceAll(s.Text, "{REF}", fp.Reference)
				txt = strings.ReplaceAll(txt, "{VAL}", fp.Value)
				wx := fp.Position.X.ToMM() + s.Position.X.ToMM()
				wy := fp.Position.Y.ToMM() + s.Position.Y.ToMM()
				sz := s.Size.ToMM()
				if sz < 0.4 {
					sz = 0.9
				}
				fmt.Fprintf(b, `<g transform="translate(%.3f,%.3f) scale(1,-1)"><text x="0" y="0" text-anchor="middle" font-family="ui-monospace, monospace" font-size="%.2f" fill="#e6edf3">%s</text></g>`,
					wx, wy, sz, escape(txt))
			}
			continue
		}
		fmt.Fprintf(b, `<g transform="translate(%.3f,%.3f) scale(1,-1)"><text x="0" y="0" text-anchor="middle" font-family="ui-monospace, monospace" font-size="0.9" fill="#e6edf3">%s</text></g>`,
			fp.Position.X.ToMM(), fp.Position.Y.ToMM()+2.2, escape(fp.Reference))
	}
}

func padFill(l core.Layer) string {
	i := int(l.Index)
	if i >= 0 && i < len(padPalette) {
		return padPalette[i]
	}
	return "#888"
}

func traceStroke(l core.Layer) string {
	i := int(l.Index)
	if i >= 0 && i < len(tracePalette) {
		return tracePalette[i]
	}
	return "#aaa"
}

func isGND(n string) bool {
	u := strings.ToUpper(strings.TrimSpace(n))
	return u == "GND" || u == "GROUND" || u == "VSS" || u == "0V" || u == "AGND" || strings.HasPrefix(u, "GND")
}

func escape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, `"`, "&quot;")
	return s
}
