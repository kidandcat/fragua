package gerber

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/mentasystems/fragua/internal/core"
)

// Generation software version stamped into X2 attributes.
// Matches crates/pcb-gerber workspace version when possible.
const genVersion = "1.1.0"

// Mask clearance applied per side when expanding pad apertures (0.05 mm).
const maskClearance = core.Length(50_000)

// Edge.Cuts stroke width (0.05 mm).
const edgeStroke = core.Length(50_000)

type side int

const (
	sideTop side = iota
	sideBottom
)

func (s side) maskLabel() string {
	if s == sideTop {
		return "F.Mask"
	}
	return "B.Mask"
}

func (s side) silkLabel() string {
	if s == sideTop {
		return "F.SilkS"
	}
	return "B.SilkS"
}

func (s side) silkLayer() core.SilkLayer {
	if s == sideTop {
		return core.SilkTop
	}
	return core.SilkBottom
}

// copperLayerForSide returns the copper layer index used for mask/silk
// filtering. Top is always 0; bottom is the stackup's last copper layer
// (index 1 on the default 2-layer board).
func copperLayerForSide(board *core.Board, s side) core.Layer {
	if s == sideTop {
		return core.LayerTop
	}
	return board.StackupOrDefault().BottomLayer()
}

// --- aperture table ---------------------------------------------------------

type apertureKind int

const (
	apRect apertureKind = iota
	apRound
)

type aperture struct {
	kind apertureKind
	w, h core.Length // rect
	d    core.Length // round
}

type apertureTable struct {
	// key → D-code (starting at 10)
	ids  map[aperture]uint32
	list []aperture
}

func (t *apertureTable) intern(ap aperture) uint32 {
	if t.ids == nil {
		t.ids = make(map[aperture]uint32)
	}
	if id, ok := t.ids[ap]; ok {
		return id
	}
	id := uint32(10 + len(t.list))
	t.ids[ap] = id
	t.list = append(t.list, ap)
	return id
}

func rectAp(w, h core.Length) aperture { return aperture{kind: apRect, w: w, h: h} }
func roundAp(d core.Length) aperture   { return aperture{kind: apRound, d: d} }

// --- header / primitives ----------------------------------------------------

func writeHeader(label string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "G04 pcb %s*\n", label)
	fmt.Fprintf(&b, "%%TF.GenerationSoftware,pcb,pcb-gerber,%s*%%\n", genVersion)
	if fn, ok := fileFunction(label); ok {
		fmt.Fprintf(&b, "%%TF.FileFunction,%s*%%\n", fn)
		b.WriteString("%TF.FilePolarity,Positive*%\n")
	}
	b.WriteString("%FSLAX46Y46*%\n")
	b.WriteString("%MOMM*%\n")
	b.WriteString("%LPD*%\n")
	return b.String()
}

func fileFunction(label string) (string, bool) {
	switch label {
	case "F.Cu":
		return "Copper,L1,Top", true
	case "B.Cu":
		return "Copper,L2,Bot", true
	case "F.Mask":
		return "Soldermask,Top", true
	case "B.Mask":
		return "Soldermask,Bot", true
	case "F.SilkS":
		return "Legend,Top", true
	case "B.SilkS":
		return "Legend,Bot", true
	case "Edge.Cuts":
		return "Profile,NP", true
	default:
		return "", false
	}
}

func writeApertures(t *apertureTable) string {
	var b strings.Builder
	for i, ap := range t.list {
		id := 10 + i
		switch ap.kind {
		case apRect:
			fmt.Fprintf(&b, "%%ADD%dR,%.6fX%.6f*%%\n", id, ap.w.ToMM(), ap.h.ToMM())
		case apRound:
			fmt.Fprintf(&b, "%%ADD%dC,%.6f*%%\n", id, ap.d.ToMM())
		}
	}
	return b.String()
}

func coord(l core.Length) int64 { return int64(l) }

func flash(p core.Point) string {
	return fmt.Sprintf("X%dY%dD03*\n", coord(p.X), coord(p.Y))
}

func moveTo(p core.Point) string {
	return fmt.Sprintf("X%dY%dD02*\n", coord(p.X), coord(p.Y))
}

func lineTo(p core.Point) string {
	return fmt.Sprintf("X%dY%dD01*\n", coord(p.X), coord(p.Y))
}

func selectAp(id uint32) string {
	return fmt.Sprintf("D%d*\n", id)
}

func footer() string { return "M02*\n" }

// --- pad helpers ------------------------------------------------------------

// padOccupiesLayer: SMD pads only on their assigned layer; PTH (drill set)
// occupy every copper layer.
func padOccupiesLayer(pad *core.Pad, target core.Layer) bool {
	if pad.Drill != nil {
		return true
	}
	return pad.Layer.Index == target.Index
}

// padWorldSize swaps w/h for 90°/270° rotations (matches Rust).
func padWorldSize(fp *core.Footprint, pad *core.Pad) (core.Length, core.Length) {
	rot := math.Mod(fp.Rotation, 360)
	if rot < 0 {
		rot += 360
	}
	w, h := pad.Size[0], pad.Size[1]
	if (rot > 45 && rot < 135) || (rot > 225 && rot < 315) {
		return h, w
	}
	return w, h
}

// footprintsInOrder walks FootprintOrder, falling back to sorted map keys.
func footprintsInOrder(board *core.Board) []*core.Footprint {
	var out []*core.Footprint
	seen := make(map[string]bool)
	for _, id := range board.FootprintOrder {
		fp := board.Footprints[id]
		if fp == nil {
			continue
		}
		out = append(out, fp)
		seen[id] = true
	}
	if len(out) == 0 && len(board.Footprints) > 0 {
		keys := make([]string, 0, len(board.Footprints))
		for id := range board.Footprints {
			keys = append(keys, id)
		}
		sort.Strings(keys)
		for _, id := range keys {
			if fp := board.Footprints[id]; fp != nil {
				out = append(out, fp)
			}
		}
	}
	return out
}

// --- copper -----------------------------------------------------------------

func writeCopperLayer(board *core.Board, layer core.Layer, label string) string {
	// Outer layers use the standard F.Cu / B.Cu labels for X2 attributes.
	bottom := board.StackupOrDefault().BottomLayer()
	if layer.IsTop() {
		return writeCopper(board, layer, "F.Cu")
	}
	if layer.Index == bottom.Index {
		return writeCopper(board, layer, "B.Cu")
	}
	if label == "" {
		label = fmt.Sprintf("In%d.Cu", layer.Index)
	}
	return writeCopper(board, layer, label)
}

const pourClearance = core.Length(200_000)     // 0.2 mm
const pourEdgeClearance = core.Length(300_000) // 0.3 mm
const spokeOverlap = core.Length(50_000)       // 0.05 mm

func writeCopper(board *core.Board, layer core.Layer, label string) string {
	var b strings.Builder
	b.WriteString(writeHeader(label))

	var table apertureTable
	type flashOp struct {
		id uint32
		p  core.Point
	}
	type drawOp struct {
		id   uint32
		a, b core.Point
	}

	orphanT := orphanTraceIDs(board)
	orphanV := orphanViaIDs(board)

	pourNets := map[string]bool{}
	pourRelief := map[string]core.ThermalRelief{}
	for _, p := range board.Pours {
		if p.Layer.Index != layer.Index {
			continue
		}
		pourNets[p.Net] = true
		if p.ThermalRelief != nil {
			pourRelief[p.Net] = *p.ThermalRelief
		} else {
			pourRelief[p.Net] = core.ThermalRelief{Kind: "spokes4", SpokeWidthMM: 0.4, GapMM: 0.4}
		}
	}
	hasPour := len(pourNets) > 0

	var voidFlashes []flashOp
	var voidDraws []drawOp
	var spokeDraws []drawOp
	if hasPour {
		cl := pourClearance
		for _, fp := range footprintsInOrder(board) {
			for i := range fp.Pads {
				pad := &fp.Pads[i]
				if !padOccupiesLayer(pad, layer) {
					continue
				}
				net := ""
				if pad.Net != nil {
					net = *pad.Net
				}
				center := core.PadWorldCenter(fp, pad)
				pw, ph := core.PadWorldSize(fp, pad)
				if rel, ok := pourRelief[net]; ok && rel.IsSpokes4() {
					gap := core.FromMM(rel.GapMM)
					if gap == 0 {
						gap = core.FromMM(0.4)
					}
					sw := rel.SpokeWidthMM
					if sw == 0 {
						sw = 0.4
					}
					ringID := table.intern(rectAp(pw+gap+gap, ph+gap+gap))
					voidFlashes = append(voidFlashes, flashOp{ringID, center})
					spokeID := table.intern(roundAp(core.FromMM(sw)))
					reach := core.Length(gap + 100_000)
					for _, sp := range selectSpokes(center, pw, ph, core.FromMM(sw)/2, cl, reach, net, layer, board, orphanT, orphanV) {
						spokeDraws = append(spokeDraws, drawOp{spokeID, sp[0], sp[1]})
					}
					continue
				}
				if _, same := pourNets[net]; same {
					continue // solid: pad melts into pour
				}
				id := table.intern(rectAp(pw+cl+cl, ph+cl+cl))
				voidFlashes = append(voidFlashes, flashOp{id, center})
			}
		}
		for i := range board.Traces {
			t := &board.Traces[i]
			if t.Layer.Index != layer.Index || pourNets[t.Net] || orphanT[t.ID] {
				continue
			}
			id := table.intern(roundAp(t.Width + cl + cl))
			voidDraws = append(voidDraws, drawOp{id, t.Start, t.End})
		}
		for i := range board.Vias {
			v := &board.Vias[i]
			if pourNets[v.Net] || orphanV[v.ID] {
				continue
			}
			id := table.intern(roundAp(v.Diameter + cl + cl))
			voidFlashes = append(voidFlashes, flashOp{id, v.Position})
		}
	}

	var flashes []flashOp
	var draws []drawOp
	for _, fp := range footprintsInOrder(board) {
		for i := range fp.Pads {
			pad := &fp.Pads[i]
			if !padOccupiesLayer(pad, layer) {
				continue
			}
			pw, ph := core.PadWorldSize(fp, pad)
			id := table.intern(rectAp(pw, ph))
			flashes = append(flashes, flashOp{id, core.PadWorldCenter(fp, pad)})
		}
	}
	for i := range board.Traces {
		t := &board.Traces[i]
		if t.Layer.Index != layer.Index || orphanT[t.ID] {
			continue
		}
		id := table.intern(roundAp(t.Width))
		draws = append(draws, drawOp{id, t.Start, t.End})
	}
	for i := range board.Vias {
		v := &board.Vias[i]
		if orphanV[v.ID] {
			continue
		}
		id := table.intern(roundAp(v.Diameter))
		flashes = append(flashes, flashOp{id, v.Position})
	}

	b.WriteString(writeApertures(&table))

	if hasPour && board.Outline != nil {
		inset := pourEdgeClearance
		x0, y0 := board.Outline.Min.X+inset, board.Outline.Min.Y+inset
		x1, y1 := board.Outline.Max.X-inset, board.Outline.Max.Y-inset
		fmt.Fprintf(&b, "G36*\nX%dY%dD02*\nX%dY%dD01*\nX%dY%dD01*\nX%dY%dD01*\nX%dY%dD01*\nG37*\n",
			coord(x0), coord(y0), coord(x1), coord(y0), coord(x1), coord(y1), coord(x0), coord(y1), coord(x0), coord(y0))
		b.WriteString("%LPC*%\n")
		var cur uint32
		for _, f := range voidFlashes {
			if f.id != cur {
				b.WriteString(selectAp(f.id))
				cur = f.id
			}
			b.WriteString(flash(f.p))
		}
		for _, d := range voidDraws {
			if d.id != cur {
				b.WriteString(selectAp(d.id))
				cur = d.id
			}
			b.WriteString(moveTo(d.a))
			b.WriteString(lineTo(d.b))
		}
		b.WriteString("%LPD*%\n")
		cur = 0
		for _, d := range spokeDraws {
			if d.id != cur {
				b.WriteString(selectAp(d.id))
				cur = d.id
			}
			b.WriteString(moveTo(d.a))
			b.WriteString(lineTo(d.b))
		}
	}

	var current uint32
	for _, f := range flashes {
		if f.id != current {
			b.WriteString(selectAp(f.id))
			current = f.id
		}
		b.WriteString(flash(f.p))
	}
	for _, d := range draws {
		if d.id != current {
			b.WriteString(selectAp(d.id))
			current = d.id
		}
		b.WriteString(moveTo(d.a))
		b.WriteString(lineTo(d.b))
	}
	b.WriteString(footer())
	return b.String()
}

// --- mask -------------------------------------------------------------------

func writeMask(board *core.Board, s side) string {
	var b strings.Builder
	b.WriteString(writeHeader(s.maskLabel()))
	layer := copperLayerForSide(board, s)

	var table apertureTable
	type flashOp struct {
		id uint32
		p  core.Point
	}
	var flashes []flashOp

	for _, fp := range footprintsInOrder(board) {
		for i := range fp.Pads {
			pad := &fp.Pads[i]
			if !padOccupiesLayer(pad, layer) {
				continue
			}
			pw, ph := padWorldSize(fp, pad)
			id := table.intern(rectAp(pw+maskClearance+maskClearance, ph+maskClearance+maskClearance))
			flashes = append(flashes, flashOp{id, core.PadWorldCenter(fp, pad)})
		}
	}

	b.WriteString(writeApertures(&table))
	var current uint32
	for _, f := range flashes {
		if f.id != current {
			b.WriteString(selectAp(f.id))
			current = f.id
		}
		b.WriteString(flash(f.p))
	}
	b.WriteString(footer())
	return b.String()
}

// --- silk (Hershey text + footprint lines) ----------------------------------

func writeSilk(board *core.Board, s side) string {
	var b strings.Builder
	b.WriteString(writeHeader(s.silkLabel()))
	sl := s.silkLayer()

	var table apertureTable
	type strokeOp struct {
		id   uint32
		poly []core.Point // 2+ points
	}
	var draws []strokeOp

	pushSeg := func(id uint32, a, e core.Point) {
		draws = append(draws, strokeOp{id, []core.Point{a, e}})
	}

	for i := range board.SilkLines {
		line := &board.SilkLines[i]
		if line.Layer != sl {
			continue
		}
		id := table.intern(roundAp(line.Width))
		pushSeg(id, line.Start, line.End)
	}
	for i := range board.SilkTexts {
		txt := &board.SilkTexts[i]
		if txt.Layer != sl {
			continue
		}
		w := txt.Width
		if w == 0 {
			w = core.DefaultSilkStroke(txt.Size)
		}
		id := table.intern(roundAp(w))
		for _, poly := range core.TextPolylines(txt.Text, txt.Position, txt.Size, txt.Rotation, txt.Anchor) {
			if len(poly) >= 2 {
				draws = append(draws, strokeOp{id, poly})
			}
		}
	}
	for _, fp := range footprintsInOrder(board) {
		padRects := make([]core.Rect, 0, len(fp.Pads))
		for i := range fp.Pads {
			c := core.PadWorldCenter(fp, &fp.Pads[i])
			pw, ph := core.PadWorldSize(fp, &fp.Pads[i])
			padRects = append(padRects, core.RectFromCenter(c, pw, ph))
		}
		if len(fp.Silk) == 0 {
			// Default {REF} label above body (Rust write_silk).
			def := core.SilkTop
			if !fp.Layer.IsTop() {
				def = core.SilkBottom
			}
			if def != sl {
				continue
			}
			primary := fp.Reference
			if fp.Key != "" {
				primary = fp.Key
			}
			if primary == "" {
				continue
			}
			body, ok := footprintBodyLocal(fp)
			if !ok {
				continue
			}
			anchorLocal := core.Point{Y: body.maxY + core.FromMM(0.6)}
			pos := core.LocalToWorld(fp, anchorLocal)
			size := core.FromMM(0.9)
			id := table.intern(roundAp(core.DefaultSilkStroke(size)))
			for _, poly := range core.TextPolylines(primary, pos, size, fp.Rotation, core.SilkAnchorMiddle) {
				if len(poly) >= 2 {
					draws = append(draws, strokeOp{id, poly})
				}
			}
			continue
		}
		for _, item := range fp.Silk {
			if item.Layer != sl {
				continue
			}
			if item.Kind == "text" {
				pos := core.LocalToWorld(fp, item.Position)
				text := core.ResolveSilkText(fp, item.Text)
				w := item.Width
				if w == 0 {
					w = core.DefaultSilkStroke(item.Size)
				}
				id := table.intern(roundAp(w))
				for _, poly := range core.TextPolylines(text, pos, item.Size, item.Rotation+fp.Rotation, item.Anchor) {
					if len(poly) < 2 {
						continue
					}
					if len(padRects) == 0 || polylineMissesAll(poly, padRects) {
						draws = append(draws, strokeOp{id, poly})
						continue
					}
					for i := 0; i+1 < len(poly); i++ {
						for _, piece := range clipSegment(poly[i], poly[i+1], padRects) {
							pushSeg(id, piece[0], piece[1])
						}
					}
				}
				continue
			}
			// line
			id := table.intern(roundAp(item.Width))
			a := core.LocalToWorld(fp, item.Start)
			e := core.LocalToWorld(fp, item.End)
			for _, piece := range clipSegment(a, e, padRects) {
				pushSeg(id, piece[0], piece[1])
			}
		}
	}

	b.WriteString(writeApertures(&table))
	var current uint32
	for _, d := range draws {
		if d.id != current {
			b.WriteString(selectAp(d.id))
			current = d.id
		}
		b.WriteString(moveTo(d.poly[0]))
		for _, p := range d.poly[1:] {
			b.WriteString(lineTo(p))
		}
	}
	b.WriteString(footer())
	return b.String()
}

type localBody struct{ maxY core.Length }

func polylineMissesAll(poly []core.Point, rects []core.Rect) bool {
	for i := 0; i+1 < len(poly); i++ {
		pieces := clipSegment(poly[i], poly[i+1], rects)
		if len(pieces) != 1 || pieces[0][0] != poly[i] || pieces[0][1] != poly[i+1] {
			return false
		}
	}
	return true
}

func clipSegment(start, end core.Point, rects []core.Rect) [][2]core.Point {
	pieces := [][2]core.Point{{start, end}}
	if len(rects) == 0 {
		return pieces
	}
	for _, rect := range rects {
		var next [][2]core.Point
		for _, p := range pieces {
			next = append(next, subtractRect(p[0], p[1], rect)...)
		}
		pieces = next
		if len(pieces) == 0 {
			return pieces
		}
	}
	return pieces
}

func subtractRect(a, b core.Point, rect core.Rect) [][2]core.Point {
	ax, ay := a.X.ToMM(), a.Y.ToMM()
	bx, by := b.X.ToMM(), b.Y.ToMM()
	rminX, rminY := rect.Min.X.ToMM(), rect.Min.Y.ToMM()
	rmaxX, rmaxY := rect.Max.X.ToMM(), rect.Max.Y.ToMM()
	dx, dy := bx-ax, by-ay
	t0, t1 := 0.0, 1.0
	edges := [4][2]float64{
		{-dx, ax - rminX},
		{dx, rmaxX - ax},
		{-dy, ay - rminY},
		{dy, rmaxY - ay},
	}
	for _, e := range edges {
		p, q := e[0], e[1]
		if math.Abs(p) < 1e-12 {
			if q < 0 {
				return [][2]core.Point{{a, b}}
			}
			continue
		}
		r := q / p
		if p < 0 {
			if r > t1 {
				return [][2]core.Point{{a, b}}
			}
			if r > t0 {
				t0 = r
			}
		} else {
			if r < t0 {
				return [][2]core.Point{{a, b}}
			}
			if r < t1 {
				t1 = r
			}
		}
	}
	if t0 >= t1 {
		return [][2]core.Point{{a, b}}
	}
	var out [][2]core.Point
	at := func(t float64) core.Point {
		return core.Point{X: core.FromMM(ax + dx*t), Y: core.FromMM(ay + dy*t)}
	}
	if t0 > 1e-9 {
		out = append(out, [2]core.Point{a, at(t0)})
	}
	if 1-t1 > 1e-9 {
		out = append(out, [2]core.Point{at(t1), b})
	}
	return out
}

func footprintBodyLocal(fp *core.Footprint) (localBody, bool) {
	if len(fp.Pads) == 0 {
		return localBody{}, false
	}
	maxY := fp.Pads[0].Offset.Y + fp.Pads[0].Size[1]/2
	for i := range fp.Pads {
		t := fp.Pads[i].Offset.Y + fp.Pads[i].Size[1]/2
		if t > maxY {
			maxY = t
		}
	}
	// Mimic the 0.4 mm body expand the renderer uses (Rust).
	return localBody{maxY: maxY + core.FromMM(0.4)}, true
}

func localToWorld(fp *core.Footprint, p core.Point) core.Point {
	ox, oy := float64(p.X), float64(p.Y)
	rad := fp.Rotation * math.Pi / 180
	c, s := math.Cos(rad), math.Sin(rad)
	rx := ox*c - oy*s
	ry := ox*s + oy*c
	return core.Point{
		X: fp.Position.X + core.Length(math.Round(rx)),
		Y: fp.Position.Y + core.Length(math.Round(ry)),
	}
}

// --- edge cuts --------------------------------------------------------------

func writeEdgeCuts(board *core.Board) string {
	var b strings.Builder
	b.WriteString(writeHeader("Edge.Cuts"))
	var table apertureTable
	id := table.intern(roundAp(edgeStroke))
	b.WriteString(writeApertures(&table))
	b.WriteString(selectAp(id))

	// Rust write_edge_cuts: if outer_path() has ≥3 points, emit that
	// closed poly and return — even when a corner radius is set.
	// outer_path() for a rectangular outline is the 4 sharp corners.
	if outer := outerPath(board); len(outer) >= 3 {
		writeClosedPoly(&b, outer)
		for _, cut := range board.Cutouts {
			if len(cut.Polygon) >= 3 {
				writeClosedPoly(&b, cut.Polygon)
			}
		}
		b.WriteString(footer())
		return b.String()
	}

	rect := board.Outline
	if rect == nil {
		// No outline: empty but well-formed gerber.
		b.WriteString(footer())
		return b.String()
	}

	r := board.OutlineCornerRadius
	if r == 0 {
		p00 := core.Point{X: rect.Min.X, Y: rect.Min.Y}
		p10 := core.Point{X: rect.Max.X, Y: rect.Min.Y}
		p11 := core.Point{X: rect.Max.X, Y: rect.Max.Y}
		p01 := core.Point{X: rect.Min.X, Y: rect.Max.Y}
		b.WriteString(moveTo(p00))
		b.WriteString(lineTo(p10))
		b.WriteString(lineTo(p11))
		b.WriteString(lineTo(p01))
		b.WriteString(lineTo(p00))
		for _, cut := range board.Cutouts {
			if len(cut.Polygon) >= 3 {
				writeClosedPoly(&b, cut.Polygon)
			}
		}
		b.WriteString(footer())
		return b.String()
	}

	// Rounded corners: 4 straight edges + 4 CCW quarter-arcs (G75 multi-quadrant).
	b.WriteString("G75*\n")
	xmin, ymin := rect.Min.X, rect.Min.Y
	xmax, ymax := rect.Max.X, rect.Max.Y
	pBottomStart := core.Point{X: xmin + r, Y: ymin}
	pBottomEnd := core.Point{X: xmax - r, Y: ymin}
	pRightStart := core.Point{X: xmax, Y: ymin + r}
	pRightEnd := core.Point{X: xmax, Y: ymax - r}
	pTopStart := core.Point{X: xmax - r, Y: ymax}
	pTopEnd := core.Point{X: xmin + r, Y: ymax}
	pLeftStart := core.Point{X: xmin, Y: ymax - r}
	pLeftEnd := core.Point{X: xmin, Y: ymin + r}

	b.WriteString(moveTo(pBottomStart))
	b.WriteString(lineTo(pBottomEnd))
	fmt.Fprintf(&b, "G03X%dY%dI%dJ%dD01*\n", coord(pRightStart.X), coord(pRightStart.Y), 0, coord(r))
	b.WriteString(lineTo(pRightEnd))
	fmt.Fprintf(&b, "G03X%dY%dI%dJ%dD01*\n", coord(pTopStart.X), coord(pTopStart.Y), coord(-r), 0)
	b.WriteString(lineTo(pTopEnd))
	fmt.Fprintf(&b, "G03X%dY%dI%dJ%dD01*\n", coord(pLeftStart.X), coord(pLeftStart.Y), 0, coord(-r))
	b.WriteString(lineTo(pLeftEnd))
	fmt.Fprintf(&b, "G03X%dY%dI%dJ%dD01*\n", coord(pBottomStart.X), coord(pBottomStart.Y), coord(r), 0)

	for _, cut := range board.Cutouts {
		if len(cut.Polygon) >= 3 {
			writeClosedPoly(&b, cut.Polygon)
		}
	}
	b.WriteString(footer())
	return b.String()
}

func outerPath(board *core.Board) []core.Point {
	if len(board.OutlinePoly) >= 3 {
		return board.OutlinePoly
	}
	if board.Outline != nil {
		r := board.Outline
		return []core.Point{
			{X: r.Min.X, Y: r.Min.Y},
			{X: r.Max.X, Y: r.Min.Y},
			{X: r.Max.X, Y: r.Max.Y},
			{X: r.Min.X, Y: r.Max.Y},
		}
	}
	return nil
}

func writeClosedPoly(b *strings.Builder, poly []core.Point) {
	if len(poly) == 0 {
		return
	}
	b.WriteString(moveTo(poly[0]))
	for _, p := range poly[1:] {
		b.WriteString(lineTo(p))
	}
	b.WriteString(lineTo(poly[0]))
}

func orphanTraceIDs(board *core.Board) map[core.ID]bool {
	out := map[core.ID]bool{}
	for i := range board.Traces {
		t := &board.Traces[i]
		tol := t.Width.ToMM()/2 + 1e-3
		if !endpointTouches(board, t, t.Start, tol) && !endpointTouches(board, t, t.End, tol) {
			out[t.ID] = true
		}
	}
	return out
}

func endpointTouches(board *core.Board, t *core.Trace, p core.Point, tol float64) bool {
	x, y := p.X.ToMM(), p.Y.ToMM()
	for _, fp := range footprintsInOrder(board) {
		for i := range fp.Pads {
			pad := &fp.Pads[i]
			if !padOccupiesLayer(pad, t.Layer) {
				continue
			}
			if pad.Net == nil || *pad.Net != t.Net {
				continue
			}
			c := core.PadWorldCenter(fp, pad)
			pw, ph := core.PadWorldSize(fp, pad)
			dx := math.Abs(x-c.X.ToMM()) - pw.ToMM()/2
			dy := math.Abs(y-c.Y.ToMM()) - ph.ToMM()/2
			if dx <= tol && dy <= tol {
				return true
			}
		}
	}
	for _, v := range board.Vias {
		if v.Net != t.Net {
			continue
		}
		r := v.Diameter.ToMM()/2 + tol
		dx, dy := x-v.Position.X.ToMM(), y-v.Position.Y.ToMM()
		if dx*dx+dy*dy <= r*r {
			return true
		}
	}
	for j := range board.Traces {
		o := &board.Traces[j]
		if o.ID == t.ID || o.Net != t.Net || o.Layer.Index != t.Layer.Index {
			continue
		}
		for _, q := range []core.Point{o.Start, o.End} {
			dx, dy := x-q.X.ToMM(), y-q.Y.ToMM()
			if dx*dx+dy*dy <= tol*tol {
				return true
			}
		}
	}
	return false
}

func orphanViaIDs(board *core.Board) map[core.ID]bool {
	dropped := orphanTraceIDs(board)
	out := map[core.ID]bool{}
	for i := range board.Vias {
		v := &board.Vias[i]
		cx, cy := v.Position.X.ToMM(), v.Position.Y.ToMM()
		r := v.Diameter.ToMM()/2 + 1e-3
		ok := false
		for j := range board.Traces {
			t := &board.Traces[j]
			if dropped[t.ID] || t.Net != v.Net {
				continue
			}
			for _, q := range []core.Point{t.Start, t.End} {
				dx, dy := cx-q.X.ToMM(), cy-q.Y.ToMM()
				if dx*dx+dy*dy <= r*r {
					ok = true
					break
				}
			}
			if ok {
				break
			}
		}
		if !ok {
			out[v.ID] = true
		}
	}
	return out
}

func selectSpokes(center core.Point, padW, padH, spokeHalf, clearance, reach core.Length, net string, layer core.Layer, board *core.Board, orphanT, orphanV map[core.ID]bool) [][2]core.Point {
	hw, hh := int64(padW/2), int64(padH/2)
	cx, cy := int64(center.X), int64(center.Y)
	o := int64(spokeOverlap)
	rch := int64(reach)
	ortho := [][2]core.Point{
		{{X: core.Length(cx - hw - rch), Y: center.Y}, {X: core.Length(cx - hw + o), Y: center.Y}},
		{{X: core.Length(cx + hw - o), Y: center.Y}, {X: core.Length(cx + hw + rch), Y: center.Y}},
		{{X: center.X, Y: core.Length(cy - hh - rch)}, {X: center.X, Y: core.Length(cy - hh + o)}},
		{{X: center.X, Y: core.Length(cy + hh - o)}, {X: center.X, Y: core.Length(cy + hh + rch)}},
	}
	var kept [][2]core.Point
	for _, s := range ortho {
		if spokeClear(s[0], s[1], spokeHalf, clearance, net, layer, board, orphanT, orphanV) {
			kept = append(kept, s)
		}
	}
	if len(kept) > 0 {
		return kept
	}
	diag := [][2]core.Point{
		{{X: core.Length(cx - hw + o), Y: core.Length(cy - hh + o)}, {X: core.Length(cx - hw - rch), Y: core.Length(cy - hh - rch)}},
		{{X: core.Length(cx + hw - o), Y: core.Length(cy - hh + o)}, {X: core.Length(cx + hw + rch), Y: core.Length(cy - hh - rch)}},
		{{X: core.Length(cx - hw + o), Y: core.Length(cy + hh - o)}, {X: core.Length(cx - hw - rch), Y: core.Length(cy + hh + rch)}},
		{{X: core.Length(cx + hw - o), Y: core.Length(cy + hh - o)}, {X: core.Length(cx + hw + rch), Y: core.Length(cy + hh + rch)}},
	}
	for _, s := range diag {
		if spokeClear(s[0], s[1], spokeHalf, clearance, net, layer, board, orphanT, orphanV) {
			kept = append(kept, s)
		}
	}
	return kept
}

func spokeClear(a, b core.Point, spokeHalf, clr core.Length, net string, layer core.Layer, board *core.Board, orphanT, orphanV map[core.ID]bool) bool {
	need := spokeHalf.ToMM() + clr.ToMM()
	ax, ay := a.X.ToMM(), a.Y.ToMM()
	bx, by := b.X.ToMM(), b.Y.ToMM()
	for _, fp := range footprintsInOrder(board) {
		for i := range fp.Pads {
			pad := &fp.Pads[i]
			if pad.Net != nil && *pad.Net == net {
				continue
			}
			if !padOccupiesLayer(pad, layer) {
				continue
			}
			c := core.PadWorldCenter(fp, pad)
			pw, ph := core.PadWorldSize(fp, pad)
			min := [2]float64{c.X.ToMM() - pw.ToMM()/2, c.Y.ToMM() - ph.ToMM()/2}
			max := [2]float64{c.X.ToMM() + pw.ToMM()/2, c.Y.ToMM() + ph.ToMM()/2}
			if segRectDistMM([2]float64{ax, ay}, [2]float64{bx, by}, min, max) < need {
				return false
			}
		}
	}
	for _, t := range board.Traces {
		if orphanT[t.ID] || t.Net == net || t.Layer.Index != layer.Index {
			continue
		}
		d := segSegDistMM([2]float64{ax, ay}, [2]float64{bx, by}, [2]float64{t.Start.X.ToMM(), t.Start.Y.ToMM()}, [2]float64{t.End.X.ToMM(), t.End.Y.ToMM()})
		if d-t.Width.ToMM()/2 < need {
			return false
		}
	}
	for _, v := range board.Vias {
		if orphanV[v.ID] || v.Net == net {
			continue
		}
		d := pointSegDistMM([2]float64{v.Position.X.ToMM(), v.Position.Y.ToMM()}, [2]float64{ax, ay}, [2]float64{bx, by})
		if d-v.Diameter.ToMM()/2 < need {
			return false
		}
	}
	return true
}

func pointSegDistMM(p, a, b [2]float64) float64 {
	dx, dy := b[0]-a[0], b[1]-a[1]
	len2 := dx*dx + dy*dy
	if len2 < 1e-18 {
		return math.Hypot(p[0]-a[0], p[1]-a[1])
	}
	t := ((p[0]-a[0])*dx + (p[1]-a[1])*dy) / len2
	if t < 0 {
		t = 0
	} else if t > 1 {
		t = 1
	}
	return math.Hypot(p[0]-(a[0]+t*dx), p[1]-(a[1]+t*dy))
}

func segsIntersectMM(a, b, c, d [2]float64) bool {
	orient := func(p, q, r [2]float64) float64 {
		return (q[0]-p[0])*(r[1]-p[1]) - (q[1]-p[1])*(r[0]-p[0])
	}
	o1, o2 := orient(a, b, c), orient(a, b, d)
	o3, o4 := orient(c, d, a), orient(c, d, b)
	return ((o1 > 0) != (o2 > 0)) && ((o3 > 0) != (o4 > 0))
}

func segSegDistMM(a, b, c, d [2]float64) float64 {
	if segsIntersectMM(a, b, c, d) {
		return 0
	}
	return math.Min(
		math.Min(pointSegDistMM(a, c, d), pointSegDistMM(b, c, d)),
		math.Min(pointSegDistMM(c, a, b), pointSegDistMM(d, a, b)),
	)
}

func segRectDistMM(a, b, min, max [2]float64) float64 {
	inside := func(p [2]float64) bool {
		return p[0] >= min[0] && p[0] <= max[0] && p[1] >= min[1] && p[1] <= max[1]
	}
	if inside(a) || inside(b) {
		return 0
	}
	corners := [4][2]float64{
		{min[0], min[1]}, {max[0], min[1]}, {max[0], max[1]}, {min[0], max[1]},
	}
	best := math.Inf(1)
	for i := 0; i < 4; i++ {
		d := segSegDistMM(a, b, corners[i], corners[(i+1)%4])
		if d < best {
			best = d
		}
	}
	return best
}
