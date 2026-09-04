package render

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/mentasystems/fragua/internal/core"
)

// Schematic sheet metrics, in sheet units (1 unit ≈ 1 mm on screen).
const (
	schPitch    = 3.2  // pin-to-pin spacing along a body side
	schLead     = 3.4  // lead length from body edge to the net flag
	schFont     = 1.25 // pin / flag text
	schRefFont  = 1.7  // reference designator
	schCharW    = 0.62 // width of one monospace char at font size 1
	schGutter   = 5.0  // space between grid cells
	schMargin   = 4.0  // sheet border
	schMinBodyW = 7.0
	schMinBodyH = 6.0
)

// SchematicSVG draws a readable, deterministic schematic sheet: symbols as
// bodies with pins on their sides and a net flag per pin. Flags, not wires —
// a net label next to every pin always reads, where auto-routed wiring on a
// grid layout does not.
func SchematicSVG(s *core.Schematic) string {
	syms := schSymbols(s)
	if len(syms) == 0 {
		return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 400 300" width="400" height="300"><rect width="400" height="300" fill="#0e1116"/><text x="200" y="150" text-anchor="middle" dominant-baseline="middle" font-family="ui-monospace, monospace" font-size="18" fill="#9aa3b2">empty schematic</text></svg>`
	}
	nets := schPinNets(s)

	cells := make([]schCell, len(syms))
	cw, ch := 0.0, 0.0
	for i, sym := range syms {
		cells[i] = layoutSymbol(sym, nets)
		cw = math.Max(cw, cells[i].w)
		ch = math.Max(ch, cells[i].h)
	}
	cols := int(math.Ceil(math.Sqrt(float64(len(cells)) * 1.35)))
	if cols < 1 {
		cols = 1
	}
	rows := (len(cells) + cols - 1) / cols
	totalW := schMargin*2 + float64(cols)*cw + float64(cols-1)*schGutter
	totalH := schMargin*2 + float64(rows)*ch + float64(rows-1)*schGutter

	var b strings.Builder
	pw, ph := svgPixelSize(totalW, totalH)
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %.3f %.3f" width="%d" height="%d" preserveAspectRatio="xMidYMid meet" data-view-mm="0 0 %.3f %.3f">`,
		totalW, totalH, pw, ph, totalW, totalH)
	b.WriteString(`<g data-root="1">`)
	fmt.Fprintf(&b, `<g data-layer="background"><rect x="0" y="0" width="%.3f" height="%.3f" fill="#0e1116"/>`, totalW, totalH)
	fmt.Fprintf(&b, `<rect x="%.3f" y="%.3f" width="%.3f" height="%.3f" fill="none" stroke="#222a35" stroke-width="0.12"/></g>`,
		schMargin/2, schMargin/2, totalW-schMargin, totalH-schMargin)
	b.WriteString(`<g data-layer="symbols">`)
	for i, c := range cells {
		col, row := i%cols, i/cols
		x := schMargin + float64(col)*(cw+schGutter) + cw/2
		y := schMargin + float64(row)*(ch+schGutter) + ch/2
		fmt.Fprintf(&b, `<g data-sym="%s" data-kind="%s" data-value="%s" data-key="%s" transform="translate(%.3f,%.3f)">`,
			escape(c.sym.Reference), escape(strings.ToLower(c.sym.Kind.Kind)), escape(c.sym.Value), escape(c.sym.Key), x, y)
		b.WriteString(c.body)
		b.WriteString(`</g>`)
	}
	b.WriteString(`</g></g></svg>`)
	return b.String()
}

type schCell struct {
	sym  *core.Symbol
	w, h float64
	body string
}

// schSymbols orders symbols so the sheet is stable: ICs first, then discretes,
// each group in natural reference order (R2 before R10).
func schSymbols(s *core.Schematic) []*core.Symbol {
	if s == nil {
		return nil
	}
	out := make([]*core.Symbol, 0, len(s.Symbols))
	seen := map[string]bool{}
	for _, id := range s.SymbolOrder {
		if sym := s.Symbols[id]; sym != nil {
			out = append(out, sym)
			seen[id] = true
		}
	}
	rest := make([]string, 0)
	for id, sym := range s.Symbols {
		if sym != nil && !seen[id] {
			rest = append(rest, id)
		}
	}
	sort.Strings(rest)
	for _, id := range rest {
		out = append(out, s.Symbols[id])
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := schRank(out[i]), schRank(out[j])
		if ri != rj {
			return ri < rj
		}
		return refLess(out[i].Reference, out[j].Reference)
	})
	return out
}

func schRank(s *core.Symbol) int {
	switch strings.ToLower(s.Kind.Kind) {
	case "resistor", "capacitor", "inductor", "led", "diode":
		return 1
	default:
		return 0
	}
}

// refLess compares reference designators naturally: R2 before R10.
func refLess(a, b string) bool {
	pa, na := splitRef(a)
	pb, nb := splitRef(b)
	if pa != pb {
		return pa < pb
	}
	if na != nb {
		return na < nb
	}
	return a < b
}

func splitRef(r string) (string, int) {
	i := len(r)
	for i > 0 && r[i-1] >= '0' && r[i-1] <= '9' {
		i--
	}
	n, err := strconv.Atoi(r[i:])
	if err != nil {
		return r, -1
	}
	return r[:i], n
}

// schPinNets maps "<symbolID>.<pin>" to its net name.
func schPinNets(s *core.Schematic) map[string]string {
	out := map[string]string{}
	if s == nil {
		return out
	}
	names := make([]string, 0, len(s.Nets))
	for n := range s.Nets {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		net := s.Nets[n]
		if net == nil {
			continue
		}
		for _, c := range net.Connections {
			out[c.SymbolID.String()+"."+c.PinNumber] = net.Name
		}
	}
	return out
}

func layoutSymbol(sym *core.Symbol, nets map[string]string) schCell {
	pins := sym.Kind.Pins()
	var left, right, top, bottom []core.SchPin
	for _, p := range pins {
		switch p.Side {
		case core.PinRight:
			right = append(right, p)
		case core.PinTop:
			top = append(top, p)
		case core.PinBottom:
			bottom = append(bottom, p)
		default:
			left = append(left, p)
		}
	}
	longestName := 0.0
	for _, p := range pins {
		longestName = math.Max(longestName, float64(len([]rune(pinLabel(p)))))
	}
	bodyW := math.Max(schMinBodyW, longestName*schFont*schCharW*2+4)
	if n := math.Max(float64(len(top)), float64(len(bottom))); n > 0 {
		bodyW = math.Max(bodyW, n*schPitch+2)
	}
	bodyH := math.Max(schMinBodyH, math.Max(float64(len(left)), float64(len(right)))*schPitch+3)

	var g strings.Builder
	discrete := schRank(sym) == 1
	if discrete {
		bodyW, bodyH = 4.6, 5.0
		writeDiscreteGlyph(&g, sym)
	} else {
		fmt.Fprintf(&g, `<rect class="sym-body" x="%.3f" y="%.3f" width="%.3f" height="%.3f" rx="0.5" fill="#151a22" stroke="#8b949e" stroke-width="0.16"/>`,
			-bodyW/2, -bodyH/2, bodyW, bodyH)
	}
	// Reference above the body, value below: the two things a human scans for.
	fmt.Fprintf(&g, `<text x="0" y="%.3f" text-anchor="middle" font-family="ui-monospace, monospace" font-size="%.2f" fill="#d6905b">%s</text>`,
		-bodyH/2-1.1, schRefFont, escape(sym.Reference))
	if v := symValue(sym); v != "" {
		fmt.Fprintf(&g, `<text x="0" y="%.3f" text-anchor="middle" font-family="ui-monospace, monospace" font-size="%.2f" fill="#9aa3b2">%s</text>`,
			bodyH/2+1.8, schFont, escape(v))
	}

	flagW := 0.0
	place := func(list []core.SchPin, side core.PinSide) {
		n := len(list)
		for i, p := range list {
			off := (float64(i) - float64(n-1)/2) * schPitch
			var px, py, lx, ly float64
			switch side {
			case core.PinRight:
				px, py = bodyW/2, off
				lx, ly = px+schLead, off
			case core.PinTop:
				px, py = off, -bodyH/2
				lx, ly = off, py-schLead
			case core.PinBottom:
				px, py = off, bodyH/2
				lx, ly = off, py+schLead
			default:
				px, py = -bodyW/2, off
				lx, ly = px-schLead, off
			}
			net := nets[sym.ID.String()+"."+p.Number]
			fmt.Fprintf(&g, `<g data-pin="%s" data-pin-name="%s" data-role="%s" data-net="%s" data-sym="%s">`,
				escape(p.Number), escape(pinLabel(p)), escape(string(p.Role)), escape(net), escape(sym.Reference))
			fmt.Fprintf(&g, `<line x1="%.3f" y1="%.3f" x2="%.3f" y2="%.3f" stroke="%s" stroke-width="0.14"/>`,
				px, py, lx, ly, pinStroke(p, net))
			fmt.Fprintf(&g, `<circle cx="%.3f" cy="%.3f" r="0.24" fill="%s"/>`, lx, ly, pinStroke(p, net))
			w := writeNetFlag(&g, lx, ly, side, net, p)
			flagW = math.Max(flagW, w)
			if !discrete && pinLabel(p) != "" {
				writePinLabel(&g, px, py, side, pinLabel(p))
			}
			g.WriteString(`</g>`)
		}
	}
	place(left, core.PinLeft)
	place(right, core.PinRight)
	place(top, core.PinTop)
	place(bottom, core.PinBottom)

	reach := schLead + flagW + 1.5
	w := bodyW + 2*reach
	h := bodyH + 2*math.Max(reach, 4.0)
	return schCell{sym: sym, w: w, h: h, body: g.String()}
}

func symValue(sym *core.Symbol) string {
	if sym.Value != "" {
		return sym.Value
	}
	return sym.Key
}

func pinLabel(p core.SchPin) string {
	if p.Name != "" {
		return p.Name
	}
	return p.Number
}

func pinStroke(p core.SchPin, net string) string {
	switch {
	case p.IsNC():
		return "#5b6472"
	case net == "":
		return "#b0303a"
	case core.IsPowerNamedNet(net):
		return "#d6905b"
	default:
		return "#8b949e"
	}
}

// writeNetFlag draws the net-name tag at the end of a pin lead and returns
// its width, so the sheet can leave room for the longest one.
func writeNetFlag(g *strings.Builder, x, y float64, side core.PinSide, net string, p core.SchPin) float64 {
	label := net
	fill := "#1b222c"
	stroke := "#3a4452"
	text := "#e6edf3"
	if label == "" {
		if p.IsNC() {
			label = "NC"
			text = "#7d8590"
		} else {
			label = "?"
			fill = "#2a1416"
			stroke = "#b0303a"
			text = "#ff9d9d"
		}
	} else if core.IsPowerNamedNet(net) {
		fill = "#2a1c10"
		stroke = "#d6905b"
		text = "#f0c49a"
	}
	tw := float64(len([]rune(label)))*schFont*schCharW + 1.2
	th := schFont + 0.9
	var rx, ry, tx float64
	anchor := "middle"
	switch side {
	case core.PinRight:
		rx, ry = x+0.3, y-th/2
		tx, anchor = rx+0.6, "start"
	case core.PinTop, core.PinBottom:
		rx, ry = x-tw/2, y-th/2
		tx = x
	default:
		rx, ry = x-0.3-tw, y-th/2
		tx, anchor = rx+tw-0.6, "end"
	}
	fmt.Fprintf(g, `<rect class="net-flag" data-net="%s" x="%.3f" y="%.3f" width="%.3f" height="%.3f" rx="0.4" fill="%s" stroke="%s" stroke-width="0.1"/>`,
		escape(net), rx, ry, tw, th, fill, stroke)
	fmt.Fprintf(g, `<text x="%.3f" y="%.3f" text-anchor="%s" dominant-baseline="middle" font-family="ui-monospace, monospace" font-size="%.2f" fill="%s">%s</text>`,
		tx, y+0.05, anchor, schFont, text, escape(label))
	return tw
}

func writePinLabel(g *strings.Builder, x, y float64, side core.PinSide, label string) {
	var tx, ty float64
	anchor := "middle"
	switch side {
	case core.PinRight:
		tx, ty, anchor = x-0.5, y+0.05, "end"
	case core.PinTop:
		tx, ty = x, y+1.2
	case core.PinBottom:
		tx, ty = x, y-0.7
	default:
		tx, ty, anchor = x+0.5, y+0.05, "start"
	}
	fmt.Fprintf(g, `<text x="%.3f" y="%.3f" text-anchor="%s" dominant-baseline="middle" font-family="ui-monospace, monospace" font-size="%.2f" fill="#9aa3b2">%s</text>`,
		tx, ty, anchor, schFont*0.85, escape(label))
}

// writeDiscreteGlyph draws the standard 2-pin symbol for a passive, pins on
// the left and right at y=0.
func writeDiscreteGlyph(g *strings.Builder, sym *core.Symbol) {
	const stroke = `stroke="#e6edf3" stroke-width="0.18" fill="none"`
	switch strings.ToLower(sym.Kind.Kind) {
	case "capacitor":
		fmt.Fprintf(g, `<line x1="-0.45" y1="-1.6" x2="-0.45" y2="1.6" %s/><line x1="0.45" y1="-1.6" x2="0.45" y2="1.6" %s/>`, stroke, stroke)
		fmt.Fprintf(g, `<line x1="-2.3" y1="0" x2="-0.45" y2="0" %s/><line x1="0.45" y1="0" x2="2.3" y2="0" %s/>`, stroke, stroke)
	case "inductor":
		fmt.Fprintf(g, `<path d="M-1.8,0 a0.6,0.6 0 0 1 1.2,0 a0.6,0.6 0 0 1 1.2,0 a0.6,0.6 0 0 1 1.2,0" %s/>`, stroke)
		fmt.Fprintf(g, `<line x1="-2.3" y1="0" x2="-1.8" y2="0" %s/><line x1="1.8" y1="0" x2="2.3" y2="0" %s/>`, stroke, stroke)
	case "led", "diode":
		fmt.Fprintf(g, `<path d="M-1.1,-1.2 L1.1,0 L-1.1,1.2 Z" stroke="#e6edf3" stroke-width="0.16" fill="#e6edf3" fill-opacity="0.18"/>`)
		fmt.Fprintf(g, `<line x1="1.1" y1="-1.2" x2="1.1" y2="1.2" %s/>`, stroke)
		fmt.Fprintf(g, `<line x1="-2.3" y1="0" x2="-1.1" y2="0" %s/><line x1="1.1" y1="0" x2="2.3" y2="0" %s/>`, stroke, stroke)
		if strings.EqualFold(sym.Kind.Kind, "led") {
			fmt.Fprintf(g, `<path d="M0.4,-1.5 L1.5,-2.6 M1.1,-1.5 L2.2,-2.6" stroke="#d6905b" stroke-width="0.14" fill="none"/>`)
		}
	default: // resistor
		fmt.Fprintf(g, `<rect x="-1.5" y="-0.9" width="3.0" height="1.8" rx="0.2" %s/>`, stroke)
		fmt.Fprintf(g, `<line x1="-2.3" y1="0" x2="-1.5" y2="0" %s/><line x1="1.5" y1="0" x2="2.3" y2="0" %s/>`, stroke, stroke)
	}
}
