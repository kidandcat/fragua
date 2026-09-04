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
	area, widest := 0.0, 0.0
	for i, sym := range syms {
		cells[i] = layoutSymbol(sym, nets)
		area += cells[i].w * cells[i].h
		widest = math.Max(widest, cells[i].w)
	}
	// Shelf-pack, not a uniform grid: one 57-pin QFN next to thirty 0603s
	// would size every cell to the QFN and shrink the sheet to nothing.
	shelfW := math.Max(widest, math.Sqrt(area*1.9))
	x, y, rowH, totalW := schMargin, schMargin, 0.0, 0.0
	for i := range cells {
		if x > schMargin && x+cells[i].w > shelfW+schMargin {
			x = schMargin
			y += rowH + schGutter
			rowH = 0
		}
		cells[i].x = x + cells[i].w/2
		cells[i].y = y + cells[i].h/2
		x += cells[i].w + schGutter
		totalW = math.Max(totalW, x-schGutter)
		rowH = math.Max(rowH, cells[i].h)
	}
	totalW += schMargin
	totalH := y + rowH + schMargin

	var b strings.Builder
	pw, ph := svgPixelSize(totalW, totalH)
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %.3f %.3f" width="%d" height="%d" preserveAspectRatio="xMidYMid meet" data-view-mm="0 0 %.3f %.3f">`,
		totalW, totalH, pw, ph, totalW, totalH)
	b.WriteString(`<g data-root="1">`)
	fmt.Fprintf(&b, `<g data-layer="background"><rect x="0" y="0" width="%.3f" height="%.3f" fill="#0e1116"/>`, totalW, totalH)
	fmt.Fprintf(&b, `<rect x="%.3f" y="%.3f" width="%.3f" height="%.3f" fill="none" stroke="#222a35" stroke-width="0.12"/></g>`,
		schMargin/2, schMargin/2, totalW-schMargin, totalH-schMargin)
	b.WriteString(`<g data-layer="symbols">`)
	for _, c := range cells {
		fmt.Fprintf(&b, `<g data-sym="%s" data-kind="%s" data-value="%s" data-key="%s" transform="translate(%.3f,%.3f)">`,
			escape(c.sym.Reference), escape(strings.ToLower(c.sym.Kind.Kind)), escape(c.sym.Value), escape(c.sym.Key), c.x, c.y)
		b.WriteString(c.body)
		b.WriteString(`</g>`)
	}
	b.WriteString(`</g></g></svg>`)
	return b.String()
}

type schCell struct {
	sym  *core.Symbol
	w, h float64
	x, y float64 // sheet position of the symbol origin
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

// pinPlan is one pin with the flag it will carry, sized before anything is
// drawn: the sheet has to reserve the room the flags need, not discover it.
type pinPlan struct {
	pin   core.SchPin
	net   string
	label string
	width float64
}

func planPin(sym *core.Symbol, p core.SchPin, nets map[string]string) pinPlan {
	net := nets[sym.ID.String()+"."+p.Number]
	label := net
	if label == "" {
		label = "?"
		if p.IsNC() {
			label = "NC"
		}
	}
	return pinPlan{pin: p, net: net, label: label,
		width: float64(len([]rune(label)))*schFont*schCharW + 1.2}
}

func maxFlagWidth(groups ...[]pinPlan) float64 {
	w := 0.0
	for _, g := range groups {
		for _, p := range g {
			w = math.Max(w, p.width)
		}
	}
	return w
}

func layoutSymbol(sym *core.Symbol, nets map[string]string) schCell {
	var left, right, top, bottom []pinPlan
	longestName := 0.0
	for _, p := range sym.Kind.Pins() {
		pp := planPin(sym, p, nets)
		longestName = math.Max(longestName, float64(len([]rune(pinLabel(p)))))
		switch p.Side {
		case core.PinRight:
			right = append(right, pp)
		case core.PinTop:
			top = append(top, pp)
		case core.PinBottom:
			bottom = append(bottom, pp)
		default:
			left = append(left, pp)
		}
	}
	flagLR := maxFlagWidth(left, right)
	flagTB := maxFlagWidth(top, bottom)

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
	// Reference above the body, value below — clear of the flags on those
	// sides, which is where a bottom-pinned connector used to bury its value.
	refY, valY := -bodyH/2-1.1, bodyH/2+1.8
	if len(top) > 0 {
		refY = -bodyH/2 - schLead - flagTB - 1.4
	}
	if len(bottom) > 0 {
		valY = bodyH/2 + schLead + flagTB + 2.6
	}
	fmt.Fprintf(&g, `<text x="0" y="%.3f" text-anchor="middle" font-family="ui-monospace, monospace" font-size="%.2f" fill="#d6905b">%s</text>`,
		refY, schRefFont, escape(sym.Reference))
	if v := symValue(sym); v != "" {
		fmt.Fprintf(&g, `<text x="0" y="%.3f" text-anchor="middle" font-family="ui-monospace, monospace" font-size="%.2f" fill="#9aa3b2">%s</text>`,
			valY, schFont, escape(v))
	}

	place := func(list []pinPlan, side core.PinSide) {
		n := len(list)
		for i, pp := range list {
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
			p := pp.pin
			fmt.Fprintf(&g, `<g data-pin="%s" data-pin-name="%s" data-role="%s" data-net="%s" data-sym="%s">`,
				escape(p.Number), escape(pinLabel(p)), escape(string(p.Role)), escape(pp.net), escape(sym.Reference))
			fmt.Fprintf(&g, `<line x1="%.3f" y1="%.3f" x2="%.3f" y2="%.3f" stroke="%s" stroke-width="0.14"/>`,
				px, py, lx, ly, pinStroke(p, pp.net))
			fmt.Fprintf(&g, `<circle cx="%.3f" cy="%.3f" r="0.24" fill="%s"/>`, lx, ly, pinStroke(p, pp.net))
			writeNetFlag(&g, lx, ly, side, pp)
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

	// Reserve room where the pins actually stick out. A 2-pin passive with
	// left/right flags needs width, not the height of its widest flag.
	hReach := 2.0
	if len(left)+len(right) > 0 {
		hReach = schLead + flagLR + 1.5
	}
	vReach := 4.0 // reference above, value below
	if len(top)+len(bottom) > 0 {
		vReach = schLead + flagTB + 4.0
	}
	return schCell{sym: sym, w: bodyW + 2*hReach, h: bodyH + 2*vReach, body: g.String()}
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

// writeNetFlag draws the net-name tag at the end of a pin lead. Top and
// bottom flags are turned 90°: laid flat they are wider than the pin pitch,
// so a connector's bottom row wrote its net names over each other.
func writeNetFlag(g *strings.Builder, x, y float64, side core.PinSide, pp pinPlan) {
	fill, stroke, text := "#1b222c", "#3a4452", "#e6edf3"
	switch {
	case pp.net == "" && pp.pin.IsNC():
		text = "#7d8590"
	case pp.net == "":
		fill, stroke, text = "#2a1416", "#b0303a", "#ff9d9d"
	case core.IsPowerNamedNet(pp.net):
		fill, stroke, text = "#2a1c10", "#d6905b", "#f0c49a"
	}
	tw, th := pp.width, schFont+0.9
	// Each flag is drawn in a local frame whose +x runs away from the body.
	frame := fmt.Sprintf(`translate(%.3f,%.3f)`, x, y)
	switch side {
	case core.PinTop:
		frame += ` rotate(-90)`
	case core.PinBottom:
		frame += ` rotate(90)`
	case core.PinLeft:
		frame += ` rotate(180)`
	}
	fmt.Fprintf(g, `<g transform="%s">`, frame)
	fmt.Fprintf(g, `<rect class="net-flag" data-net="%s" x="0.3" y="%.3f" width="%.3f" height="%.3f" rx="0.4" fill="%s" stroke="%s" stroke-width="0.1"/>`,
		escape(pp.net), -th/2, tw, th, fill, stroke)
	if side == core.PinLeft {
		// The 180° frame would mirror the lettering: counter-rotate the text
		// and anchor it at the far end of its own tag.
		fmt.Fprintf(g, `<g transform="translate(%.3f,0) rotate(180)"><text x="0" y="0" text-anchor="start" dominant-baseline="middle" font-family="ui-monospace, monospace" font-size="%.2f" fill="%s">%s</text></g>`,
			tw-0.3, schFont, text, escape(pp.label))
	} else {
		fmt.Fprintf(g, `<text x="0.9" y="0" text-anchor="start" dominant-baseline="middle" font-family="ui-monospace, monospace" font-size="%.2f" fill="%s">%s</text>`,
			schFont, text, escape(pp.label))
	}
	g.WriteString(`</g>`)
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
