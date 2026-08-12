package core

import (
	"encoding/json"
	"math"
)

// Minimum hand-solder gap between footprint pad AABBs (mm).
const MinFootprintGapMM = 2.0

// MountHoleClearanceMM is keepout around NPTH mount holes.
const MountHoleClearanceMM = 0.5

// CastellatedPadInsetMM: pads may sit this far outside the outline when castellated.
const CastellatedPadInsetMM = 0.15

// EdgeSide indicates which board/footprint edge an edge-mounted part faces.
// JSON values match Rust: top/right/bottom/left (lowercase).
type EdgeSide string

const (
	EdgeTop    EdgeSide = "top"
	EdgeRight  EdgeSide = "right"
	EdgeBottom EdgeSide = "bottom"
	EdgeLeft   EdgeSide = "left"
)

// Pad is a copper pad on a footprint.
type Pad struct {
	Number string  `json:"number"`
	Name   string  `json:"name"`
	Offset Point   `json:"offset"`
	Size   [2]Length `json:"size"`
	Layer  Layer   `json:"layer"`
	Net    *string `json:"net"`
	Drill  *Length `json:"drill,omitempty"`
}

// FootprintSilk is silk geometry owned by a footprint (library-local coords).
type FootprintSilk struct {
	Lines []SilkLine `json:"lines,omitempty"`
	Texts []SilkText `json:"texts,omitempty"`
}

// Footprint is a placed (or palette) component instance.
type Footprint struct {
	ID          ID             `json:"id"`
	Reference   string         `json:"reference"`
	Value       string         `json:"value"`
	Library     string         `json:"library"`
	Position    Point          `json:"position"`
	Rotation    float64        `json:"rotation"`
	Layer       Layer          `json:"layer"`
	Pads        []Pad          `json:"pads"`
	Key         string         `json:"key"`
	Description string         `json:"description"`
	EdgeMounted bool           `json:"edge_mounted"`
	EdgeSide    *EdgeSide      `json:"edge_side"`
	Silk        []FootprintSilk `json:"silk"`
}

// Trace is a copper segment.
type Trace struct {
	ID     ID     `json:"id"`
	Layer  Layer  `json:"layer"`
	Start  Point  `json:"start"`
	End    Point  `json:"end"`
	Width  Length `json:"width"`
	Net    string `json:"net"`
}

// Via is a plated through-hole connecting layers.
type Via struct {
	ID       ID     `json:"id"`
	Position Point  `json:"position"`
	Drill    Length `json:"drill"`
	Diameter Length `json:"diameter"`
	Net      string `json:"net"`
	// FromLayer/ToLayer optional for multi-layer; omitted means through-all.
	FromLayer *Layer `json:"from_layer,omitempty"`
	ToLayer   *Layer `json:"to_layer,omitempty"`
}

// ThermalRelief settings for a pour.
type ThermalRelief struct {
	Enabled      bool    `json:"enabled,omitempty"`
	GapMM        float64 `json:"gap_mm,omitempty"`
	SpokeWidthMM float64 `json:"spoke_width_mm,omitempty"`
}

// StitchPolicy for pour via stitching.
type StitchPolicy struct {
	Enabled  bool    `json:"enabled,omitempty"`
	PitchMM  float64 `json:"pitch_mm,omitempty"`
	DrillMM  float64 `json:"drill_mm,omitempty"`
	Diameter float64 `json:"diameter_mm,omitempty"`
}

// Pour is a copper pour region (simplified: full-board or rect).
type Pour struct {
	ID             ID              `json:"id,omitempty"`
	Net            string          `json:"net"`
	Layer          Layer           `json:"layer"`
	ThermalRelief  *ThermalRelief  `json:"thermal_relief,omitempty"`
	Stitching      *StitchPolicy   `json:"stitching,omitempty"`
	// Optional polygon; empty means board outline.
	Polygon []Point `json:"polygon,omitempty"`
}

// Keepout forbids copper/placement.
type Keepout struct {
	ID      ID     `json:"id"`
	Rect    *Rect  `json:"rect,omitempty"`
	Polygon []Point `json:"polygon,omitempty"`
	NoCopper bool  `json:"no_copper,omitempty"`
	NoPlace  bool  `json:"no_place,omitempty"`
}

// SilkLine is a board-level silkscreen segment.
type SilkLine struct {
	Layer SilkLayer `json:"layer"`
	Start Point     `json:"start"`
	End   Point     `json:"end"`
	Width Length    `json:"width"`
}

// SilkText is a board-level silkscreen text run.
type SilkText struct {
	Layer    SilkLayer  `json:"layer"`
	Position Point      `json:"position"`
	Text     string     `json:"text"`
	Size     Length     `json:"size"`
	Rotation float64    `json:"rotation"`
	Anchor   SilkAnchor `json:"anchor"`
	Width    Length     `json:"width"`
}

// Cutout is a milled board cutout (polygon).
type Cutout struct {
	ID      ID      `json:"id"`
	Polygon []Point `json:"polygon"`
}

// MountHole is an NPTH hole.
type MountHole struct {
	ID       ID     `json:"id"`
	Position Point  `json:"position"`
	Drill    Length `json:"drill"`
}

// Board is the physical layout model.
type Board struct {
	Outline             *Rect           `json:"outline"`
	OutlinePoly         []Point         `json:"outline_poly,omitempty"`
	OutlineCornerRadius Length          `json:"outline_corner_radius,omitempty"`
	Footprints          map[string]*Footprint `json:"footprints"`
	FootprintOrder      []string        `json:"footprint_order"`
	Traces              []Trace         `json:"traces"`
	Vias                []Via           `json:"vias"`
	Pours               []Pour          `json:"pours"`
	Keepouts            []Keepout       `json:"keepouts"`
	SilkLines           []SilkLine      `json:"silk_lines"`
	SilkTexts           []SilkText      `json:"silk_texts"`
	RuleAreas           []RuleArea      `json:"rule_areas"`
	FabRules            *FabRules       `json:"fab_rules,omitempty"`
	Cutouts             []Cutout        `json:"cutouts,omitempty"`
	Holes               []MountHole     `json:"holes,omitempty"`
	Stackup             *LayerStackup   `json:"stackup,omitempty"`
}

// NewBoard returns an empty 2-layer board.
func NewBoard() *Board {
	return &Board{
		Footprints:     make(map[string]*Footprint),
		FootprintOrder: nil,
		Traces:         nil,
		Vias:           nil,
	}
}

// StackupOrDefault returns stackup or Default2Layer.
func (b *Board) StackupOrDefault() LayerStackup {
	if b.Stackup != nil {
		return *b.Stackup
	}
	return Default2Layer()
}

// AddFootprint inserts a footprint and appends to order.
func (b *Board) AddFootprint(fp *Footprint) {
	if b.Footprints == nil {
		b.Footprints = make(map[string]*Footprint)
	}
	if fp.ID.IsZero() {
		fp.ID = NewID()
	}
	key := fp.ID.String()
	b.Footprints[key] = fp
	b.FootprintOrder = append(b.FootprintOrder, key)
}

// FootprintByRef returns the footprint with the given reference designator.
func (b *Board) FootprintByRef(ref string) *Footprint {
	for _, id := range b.FootprintOrder {
		if fp := b.Footprints[id]; fp != nil && fp.Reference == ref {
			return fp
		}
	}
	for _, fp := range b.Footprints {
		if fp != nil && fp.Reference == ref {
			return fp
		}
	}
	return nil
}

// ClearRoute removes all traces and vias.
func (b *Board) ClearRoute() {
	b.Traces = nil
	b.Vias = nil
}

// PadWorldCenter returns the world-space center of a pad on a footprint.
func PadWorldCenter(fp *Footprint, pad *Pad) Point {
	ox, oy := float64(pad.Offset.X), float64(pad.Offset.Y)
	rad := fp.Rotation * math.Pi / 180
	c, s := math.Cos(rad), math.Sin(rad)
	rx := ox*c - oy*s
	ry := ox*s + oy*c
	return Point{
		X: fp.Position.X + Length(math.Round(rx)),
		Y: fp.Position.Y + Length(math.Round(ry)),
	}
}

// PadWorldAABB returns the axis-aligned bounding box of a pad (90° rotations).
func PadWorldAABB(fp *Footprint, pad *Pad) Rect {
	// For non-orthogonal rotations use center + max extent; for 0/90/180/270
	// swap width/height when needed.
	rot := math.Mod(fp.Rotation, 360)
	if rot < 0 {
		rot += 360
	}
	w, h := pad.Size[0], pad.Size[1]
	// near 90 or 270 → swap
	if (rot > 45 && rot < 135) || (rot > 225 && rot < 315) {
		w, h = h, w
	}
	c := PadWorldCenter(fp, pad)
	return RectFromCenter(c, w, h)
}

// size is stored as [w,h] Length array in JSON — custom unmarshal if needed.
// encoding/json handles [2]Length when JSON is [n,n].

// Ensure JSON maps for footprints deserialize even when null.
func (b *Board) UnmarshalJSON(data []byte) error {
	type alias Board
	aux := &struct {
		*alias
	}{alias: (*alias)(b)}
	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}
	if b.Footprints == nil {
		b.Footprints = make(map[string]*Footprint)
	}
	return nil
}
