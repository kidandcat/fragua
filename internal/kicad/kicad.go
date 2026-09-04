// Package kicad writes a KiCad 9 board file (.kicad_pcb, s-expression,
// format version 20241229) from a Fragua board. This is the trust bridge: an
// EE opens the export in KiCad and checks Fragua's copper with their own eyes.
//
// Geometry is exact, not re-derived. Fragua is Y-up with the origin at the
// board's lower-left; KiCad is Y-down, so the whole board is mirrored about
// the outline's top edge — which turns Fragua's CCW rotation into KiCad's own
// CCW-on-screen rotation with the same sign, so angles pass through unchanged.
package kicad

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/mentasystems/fragua/internal/core"
)

// FormatVersion is the KiCad board file format this writer emits (KiCad 9).
const FormatVersion = "20241229"

// Generator identifies Fragua in the file header.
const Generator = "fragua"

// Options tunes the export.
type Options struct {
	// Name is the board name; it seeds the deterministic UUIDs so the same
	// board always exports byte-identically.
	Name string
	// FillZones emits (filled_polygon …) for every pour so KiCad shows the
	// copper without a refill. Off means zone outlines only.
	FillZones bool
	// FillGridMM is the rasteriser cell for the zone fill (0 → 0.1 mm).
	FillGridMM float64
}

// DefaultOptions exports filled zones on a 0.1 mm fill grid.
func DefaultOptions() Options {
	return Options{FillZones: true, FillGridMM: 0.1}
}

// WriteFile exports board to path.
func WriteFile(board *core.Board, path string, opts Options) error {
	s, err := Export(board, opts)
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(s), 0o644)
}

// Export renders the board as .kicad_pcb text.
func Export(board *core.Board, opts Options) (string, error) {
	if board == nil {
		return "", fmt.Errorf("kicad: nil board")
	}
	if opts.FillGridMM <= 0 {
		opts.FillGridMM = 0.1
	}
	e := &exporter{board: board, opts: opts, stack: board.StackupOrDefault()}
	e.flipY = e.originY()
	e.nets = buildNets(board)
	e.emit()
	return e.b.String(), nil
}

type exporter struct {
	board *core.Board
	opts  Options
	stack core.LayerStackup
	nets  *netTable
	flipY core.Length
	b     strings.Builder
}

// originY is the Y the board is mirrored about: the top of everything the
// board contains, so the export lands in KiCad's positive quadrant.
func (e *exporter) originY() core.Length {
	top := core.Length(math.MinInt64)
	seen := false
	note := func(y core.Length) {
		if !seen || y > top {
			top, seen = y, true
		}
	}
	if e.board.Outline != nil {
		note(e.board.Outline.Max.Y)
	}
	for _, p := range e.board.OutlinePoly {
		note(p.Y)
	}
	for _, id := range e.board.FootprintOrder {
		if fp := e.board.Footprints[id]; fp != nil {
			note(fp.Position.Y)
		}
	}
	if !seen {
		return 0
	}
	return top
}

// x / y map Fragua board coordinates into KiCad page coordinates (mm).
func (e *exporter) x(v core.Length) string { return num(v.ToMM()) }
func (e *exporter) y(v core.Length) string { return num((e.flipY - v).ToMM()) }

// ---------------------------------------------------------------- nets

type netTable struct {
	names []string       // index → name; 0 is the unconnected net
	index map[string]int // name → index
}

func (n *netTable) idx(name string) int {
	if i, ok := n.index[name]; ok {
		return i
	}
	return 0
}

func buildNets(b *core.Board) *netTable {
	set := map[string]bool{}
	for _, fp := range b.Footprints {
		if fp == nil {
			continue
		}
		for i := range fp.Pads {
			if fp.Pads[i].Net != nil && *fp.Pads[i].Net != "" {
				set[*fp.Pads[i].Net] = true
			}
		}
	}
	for _, t := range b.Traces {
		if t.Net != "" {
			set[t.Net] = true
		}
	}
	for _, v := range b.Vias {
		if v.Net != "" {
			set[v.Net] = true
		}
	}
	for _, p := range b.Pours {
		if p.Net != "" {
			set[p.Net] = true
		}
	}
	names := make([]string, 0, len(set)+1)
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	t := &netTable{names: append([]string{""}, names...), index: map[string]int{"": 0}}
	for i, n := range t.names {
		t.index[n] = i
	}
	return t
}

// ---------------------------------------------------------------- layers

// kicadCopperOrdinal is KiCad 9's PCB_LAYER_ID for copper index i on a stack
// of n: F.Cu 0, B.Cu 2, In1.Cu 4, In2.Cu 6 … (not the pre-9 0..31 numbering).
func kicadCopperOrdinal(i, n int) int {
	switch {
	case i <= 0:
		return 0
	case n >= 2 && i >= n-1:
		return 2
	default:
		return 2 + 2*i
	}
}

// cuName is the canonical KiCad name of copper index i. A stackup may name a
// layer anything it likes; KiCad only knows F.Cu / InN.Cu / B.Cu.
func (e *exporter) cuName(i int) string {
	return core.CopperLayerName(i, e.stack.CopperCount())
}

func (e *exporter) layerName(l core.Layer) string { return e.cuName(int(l.Index)) }

func silkLayerName(s core.SilkLayer) string {
	if s == core.SilkBottom {
		return "B.SilkS"
	}
	return "F.SilkS"
}

// techLayers is the fixed non-copper set KiCad expects in every board.
var techLayers = []struct {
	ord  int
	name string
	user string
}{
	{9, "F.Adhes", "F.Adhesive"},
	{11, "B.Adhes", "B.Adhesive"},
	{13, "F.Paste", ""},
	{15, "B.Paste", ""},
	{5, "F.SilkS", "F.Silkscreen"},
	{7, "B.SilkS", "B.Silkscreen"},
	{1, "F.Mask", ""},
	{3, "B.Mask", ""},
	{17, "Dwgs.User", "User.Drawings"},
	{19, "Cmts.User", "User.Comments"},
	{21, "Eco1.User", "User.Eco1"},
	{23, "Eco2.User", "User.Eco2"},
	{25, "Edge.Cuts", ""},
	{27, "Margin", ""},
	{31, "F.CrtYd", "F.Courtyard"},
	{29, "B.CrtYd", "B.Courtyard"},
	{35, "F.Fab", ""},
	{33, "B.Fab", ""},
}

// ---------------------------------------------------------------- emit

func (e *exporter) line(depth int, format string, args ...any) {
	e.b.WriteString(strings.Repeat("\t", depth))
	fmt.Fprintf(&e.b, format, args...)
	e.b.WriteByte('\n')
}

func (e *exporter) emit() {
	e.line(0, "(kicad_pcb")
	e.line(1, "(version %s)", FormatVersion)
	e.line(1, "(generator %q)", Generator)
	e.line(1, "(generator_version %q)", core.Version)
	e.emitGeneral()
	e.emitLayers()
	e.emitSetup()
	e.emitNets()
	e.emitFootprints()
	e.emitHoles()
	e.emitEdge()
	e.emitBoardSilk()
	e.emitTracks()
	e.emitZones()
	e.line(1, "(embedded_fonts no)")
	e.line(0, ")")
}

func (e *exporter) emitGeneral() {
	e.line(1, "(general")
	e.line(2, "(thickness %s)", num(e.stack.TotalThicknessMM()))
	e.line(2, "(legacy_teardrops no)")
	e.line(1, ")")
	e.line(1, "(paper %q)", "A4")
}

func (e *exporter) emitLayers() {
	n := e.stack.CopperCount()
	e.line(1, "(layers")
	for i := 0; i < n; i++ {
		kind := "signal"
		if e.stack.IsPlane(i) {
			kind = "power"
		}
		e.line(2, "(%d %q %s)", kicadCopperOrdinal(i, n), e.cuName(i), kind)
	}
	for _, t := range techLayers {
		if t.user != "" {
			e.line(2, "(%d %q user %q)", t.ord, t.name, t.user)
		} else {
			e.line(2, "(%d %q user)", t.ord, t.name)
		}
	}
	e.line(1, ")")
}

func (e *exporter) emitSetup() {
	e.line(1, "(setup")
	if e.stack.CopperCount() >= 4 {
		e.emitStackup()
	}
	e.line(2, "(pad_to_mask_clearance 0)")
	e.line(2, "(allow_soldermask_bridges_in_footprints no)")
	e.line(1, ")")
}

// emitStackup mirrors the Fragua stackup: copper thickness from the copper
// weight, dielectric thickness and Er from the declared slabs.
func (e *exporter) emitStackup() {
	e.line(2, "(stackup")
	e.line(3, "(layer %q (type %q))", "F.SilkS", "Top Silk Screen")
	e.line(3, "(layer %q (type %q))", "F.Paste", "Top Solder Paste")
	e.line(3, "(layer %q (type %q) (thickness 0.01))", "F.Mask", "Top Solder Mask")
	n := e.stack.CopperCount()
	for i := 0; i < n; i++ {
		e.line(3, "(layer %q (type %q) (thickness %s))", e.cuName(i), "copper", num(e.stack.CopperOz(i)*0.035))
		if i == n-1 {
			break
		}
		d := core.Dielectric{ThicknessMM: 0.2, Er: 4.5}
		if i < len(e.stack.Dielectrics) {
			d = e.stack.Dielectrics[i]
		}
		if d.ThicknessMM <= 0 {
			d.ThicknessMM = 0.2
		}
		if d.Er <= 0 {
			d.Er = 4.5
		}
		e.line(3, "(layer %q (type %q) (thickness %s) (material %q) (epsilon_r %s) (loss_tangent 0.02))",
			fmt.Sprintf("dielectric %d", i+1), "core", num(d.ThicknessMM), "FR4", num(d.Er))
	}
	e.line(3, "(layer %q (type %q) (thickness 0.01))", "B.Mask", "Bottom Solder Mask")
	e.line(3, "(layer %q (type %q))", "B.Paste", "Bottom Solder Paste")
	e.line(3, "(layer %q (type %q))", "B.SilkS", "Bottom Silk Screen")
	e.line(3, "(copper_finish %q)", "None")
	e.line(3, "(dielectric_constraints no)")
	e.line(2, ")")
}

func (e *exporter) emitNets() {
	for i, n := range e.nets.names {
		e.line(1, "(net %d %q)", i, n)
	}
}

// ---------------------------------------------------------------- footprints

func (e *exporter) emitFootprints() {
	for _, id := range e.board.FootprintOrder {
		fp := e.board.Footprints[id]
		if fp == nil {
			continue
		}
		e.emitFootprint(fp)
	}
}

// local maps a footprint-local Fragua offset into KiCad's local frame: the
// same reflection the whole board gets, so the footprint angle is unchanged.
func local(p core.Point) (string, string) { return num(p.X.ToMM()), num(-p.Y.ToMM()) }

func (e *exporter) emitFootprint(fp *core.Footprint) {
	key := libID(fp)
	e.line(1, "(footprint %q", key)
	e.line(2, "(layer %q)", e.layerName(fp.Layer))
	e.line(2, "(uuid %q)", uuidFor("fp/"+fp.Reference+"/"+fp.ID.String()))
	e.line(2, "(at %s %s%s)", e.x(fp.Position.X), e.y(fp.Position.Y), angleSuffix(fp.Rotation))
	if fp.Description != "" {
		e.line(2, "(descr %q)", fp.Description)
	}
	e.line(2, "(attr %s)", footprintAttr(fp))
	silkSide := "F.SilkS"
	fabSide := "F.Fab"
	if !fp.Layer.IsTop() {
		silkSide, fabSide = "B.SilkS", "B.Fab"
	}
	ry := refOffsetMM(fp)
	e.field(fp, "Reference", fp.Reference, 0, -ry, silkSide)
	e.field(fp, "Value", fp.Value, 0, ry, fabSide)
	e.emitCourtyard(fp)
	for i := range fp.Silk {
		e.emitFootprintSilk(fp, &fp.Silk[i], i)
	}
	for i := range fp.Pads {
		e.emitPad(fp, &fp.Pads[i])
	}
	e.line(2, "(embedded_fonts no)")
	e.line(1, ")")
}

// refOffsetMM keeps the reference text clear of the pads.
func refOffsetMM(fp *core.Footprint) float64 {
	h := 1.0
	for i := range fp.Pads {
		if half := fp.Pads[i].Size[1].ToMM() / 2; half+0.8 > h {
			h = half + 0.8
		}
	}
	return h
}

func (e *exporter) field(fp *core.Footprint, name, value string, dx, dy float64, layer string) {
	e.line(2, "(property %q %q", name, value)
	e.line(3, "(at %s %s 0)", num(dx), num(dy))
	e.line(3, "(layer %q)", layer)
	e.line(3, "(uuid %q)", uuidFor("field/"+fp.ID.String()+"/"+name))
	e.line(3, "(effects")
	e.line(4, "(font (size 1 1) (thickness 0.15))")
	e.line(3, ")")
	e.line(2, ")")
}

func (e *exporter) emitCourtyard(fp *core.Footprint) {
	r, ok := courtyardLocal(fp)
	if !ok {
		return
	}
	layer := "F.CrtYd"
	if !fp.Layer.IsTop() {
		layer = "B.CrtYd"
	}
	e.line(2, "(fp_rect")
	e.line(3, "(start %s %s)", num(r[0]), num(-r[3]))
	e.line(3, "(end %s %s)", num(r[2]), num(-r[1]))
	e.line(3, "(stroke (width 0.05) (type solid))")
	e.line(3, "(fill no)")
	e.line(3, "(layer %q)", layer)
	e.line(3, "(uuid %q)", uuidFor("crtyd/"+fp.ID.String()))
	e.line(2, ")")
}

// courtyardLocal returns the footprint-local courtyard as minX,minY,maxX,maxY
// in mm (Fragua Y-up): the library body rect, or the pad union plus margin.
func courtyardLocal(fp *core.Footprint) ([4]float64, bool) {
	if fp.BodyRect != nil {
		return [4]float64{fp.BodyRect.MinXMM, fp.BodyRect.MinYMM, fp.BodyRect.MaxXMM, fp.BodyRect.MaxYMM}, true
	}
	if len(fp.Pads) == 0 {
		return [4]float64{}, false
	}
	out := [4]float64{math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)}
	for i := range fp.Pads {
		p := &fp.Pads[i]
		cx, cy := p.Offset.X.ToMM(), p.Offset.Y.ToMM()
		hw, hh := p.Size[0].ToMM()/2, p.Size[1].ToMM()/2
		out[0] = math.Min(out[0], cx-hw)
		out[1] = math.Min(out[1], cy-hh)
		out[2] = math.Max(out[2], cx+hw)
		out[3] = math.Max(out[3], cy+hh)
	}
	m := core.CourtyardMarginMM
	return [4]float64{out[0] - m, out[1] - m, out[2] + m, out[3] + m}, true
}

func (e *exporter) emitFootprintSilk(fp *core.Footprint, s *core.FootprintSilkItem, i int) {
	layer := silkLayerName(s.Layer)
	if !fp.Layer.IsTop() && s.Layer != core.SilkBottom {
		layer = "B.SilkS"
	}
	uid := uuidFor(fmt.Sprintf("fpsilk/%s/%d", fp.ID.String(), i))
	switch s.Kind {
	case "line":
		sx, sy := local(s.Start)
		ex, ey := local(s.End)
		w := s.Width
		if w <= 0 {
			w = core.FromMM(0.12)
		}
		e.line(2, "(fp_line")
		e.line(3, "(start %s %s)", sx, sy)
		e.line(3, "(end %s %s)", ex, ey)
		e.line(3, "(stroke (width %s) (type solid))", num(w.ToMM()))
		e.line(3, "(layer %q)", layer)
		e.line(3, "(uuid %q)", uid)
		e.line(2, ")")
	case "text":
		px, py := local(s.Position)
		size := s.Size
		if size <= 0 {
			size = core.FromMM(1)
		}
		th := s.Width
		if th <= 0 {
			th = size / 8
		}
		e.line(2, "(fp_text user %q", core.ResolveSilkText(fp, s.Text))
		e.line(3, "(at %s %s%s)", px, py, angleSuffix(s.Rotation))
		e.line(3, "(layer %q)", layer)
		e.line(3, "(uuid %q)", uid)
		e.line(3, "(effects")
		e.line(4, "(font (size %s %s) (thickness %s))", num(size.ToMM()), num(size.ToMM()), num(th.ToMM()))
		if j := justify(s.Anchor); j != "" {
			e.line(4, "(justify %s)", j)
		}
		e.line(3, ")")
		e.line(2, ")")
	}
}

func (e *exporter) emitPad(fp *core.Footprint, pad *core.Pad) {
	px, py := local(pad.Offset)
	kind, shape := "smd", "rect"
	if pad.Drill != nil && *pad.Drill > 0 {
		kind = "thru_hole"
		shape = "oval"
		if pad.Size[0] == pad.Size[1] {
			shape = "circle"
		}
	}
	number := pad.Number
	if number == "" {
		number = pad.Name
	}
	e.line(2, "(pad %q %s %s", number, kind, shape)
	e.line(3, "(at %s %s%s)", px, py, angleSuffix(fp.Rotation))
	e.line(3, "(size %s %s)", num(pad.Size[0].ToMM()), num(pad.Size[1].ToMM()))
	if pad.Drill != nil && *pad.Drill > 0 {
		e.line(3, "(drill %s)", num(pad.Drill.ToMM()))
	}
	e.line(3, "(layers %s)", e.padLayers(pad))
	if pad.Net != nil && *pad.Net != "" {
		e.line(3, "(net %d %q)", e.nets.idx(*pad.Net), *pad.Net)
	}
	e.line(3, "(uuid %q)", uuidFor("pad/"+fp.ID.String()+"/"+pad.Number))
	e.line(2, ")")
}

func (e *exporter) padLayers(pad *core.Pad) string {
	if pad.Drill != nil && *pad.Drill > 0 {
		return `"*.Cu" "*.Mask"`
	}
	name := e.layerName(pad.Layer)
	switch name {
	case "F.Cu":
		return `"F.Cu" "F.Mask" "F.Paste"`
	case "B.Cu":
		return `"B.Cu" "B.Mask" "B.Paste"`
	default:
		return strconv.Quote(name)
	}
}

func footprintAttr(fp *core.Footprint) string {
	for i := range fp.Pads {
		if fp.Pads[i].Drill != nil && *fp.Pads[i].Drill > 0 {
			return "through_hole"
		}
	}
	return "smd"
}

// libID is a KiCad LIB_ID: a library nickname and a footprint name.
func libID(fp *core.Footprint) string {
	name := fp.Key
	if name == "" {
		name = fp.Library
	}
	if i := strings.LastIndex(name, ":"); i >= 0 {
		name = name[i+1:]
	}
	if name == "" {
		name = "unnamed"
	}
	return "fragua:" + sanitizeLibName(name)
}

func sanitizeLibName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '_' || r == '-' || r == '.' || r == '+':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "unnamed"
	}
	return b.String()
}

// ---------------------------------------------------------------- text

func justify(a core.SilkAnchor) string {
	switch a {
	case core.SilkAnchorStart:
		return "left"
	case core.SilkAnchorEnd:
		return "right"
	}
	return ""
}

func angleSuffix(deg float64) string {
	d := math.Mod(deg, 360)
	if d < 0 {
		d += 360
	}
	if d == 0 {
		return ""
	}
	return " " + num(d)
}

// ---------------------------------------------------------------- helpers

// num formats a millimetre value the way KiCad does: fixed point, no trailing
// zeros, never "-0".
func num(v float64) string {
	if v == 0 || math.Abs(v) < 5e-7 {
		return "0"
	}
	s := strconv.FormatFloat(v, 'f', 6, 64)
	s = strings.TrimRight(s, "0")
	s = strings.TrimSuffix(s, ".")
	if s == "" || s == "-" {
		return "0"
	}
	return s
}

// uuidFor is a deterministic v4-shaped UUID: the same board exports the same
// file every time, so diffs are meaningful and tests are stable.
func uuidFor(key string) string {
	h := sha256.Sum256([]byte("fragua-kicad:" + key))
	b := h[:16]
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	s := hex.EncodeToString(b)
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}
