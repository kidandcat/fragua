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
	var flashes []flashOp
	var draws []drawOp

	// Pads.
	for _, fp := range footprintsInOrder(board) {
		for i := range fp.Pads {
			pad := &fp.Pads[i]
			if !padOccupiesLayer(pad, layer) {
				continue
			}
			pw, ph := padWorldSize(fp, pad)
			id := table.intern(rectAp(pw, ph))
			flashes = append(flashes, flashOp{id, core.PadWorldCenter(fp, pad)})
		}
	}
	// Traces (no orphan filter yet — full connectivity analysis is a
	// known gap vs Rust; emit all traces on this layer).
	for i := range board.Traces {
		t := &board.Traces[i]
		if t.Layer.Index != layer.Index {
			continue
		}
		id := table.intern(roundAp(t.Width))
		draws = append(draws, drawOp{id, t.Start, t.End})
	}
	// Vias: copper pad on every layer.
	for i := range board.Vias {
		v := &board.Vias[i]
		id := table.intern(roundAp(v.Diameter))
		flashes = append(flashes, flashOp{id, v.Position})
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

// --- silk (board-level lines; no Hershey text yet) --------------------------

func writeSilk(board *core.Board, s side) string {
	var b strings.Builder
	b.WriteString(writeHeader(s.silkLabel()))
	sl := s.silkLayer()

	var table apertureTable
	type drawOp struct {
		id   uint32
		a, b core.Point
	}
	var draws []drawOp

	for i := range board.SilkLines {
		line := &board.SilkLines[i]
		if line.Layer != sl {
			continue
		}
		id := table.intern(roundAp(line.Width))
		draws = append(draws, drawOp{id, line.Start, line.End})
	}
	// Footprint silk line segments (library-local → world).
	for _, fp := range footprintsInOrder(board) {
		for _, group := range fp.Silk {
			for i := range group.Lines {
				line := &group.Lines[i]
				if line.Layer != sl {
					continue
				}
				id := table.intern(roundAp(line.Width))
				a := localToWorld(fp, line.Start)
				e := localToWorld(fp, line.End)
				draws = append(draws, drawOp{id, a, e})
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
		b.WriteString(moveTo(d.a))
		b.WriteString(lineTo(d.b))
	}
	b.WriteString(footer())
	return b.String()
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
