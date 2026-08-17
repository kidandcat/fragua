package core

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Layer is a copper layer index into the board stackup.
// Index 0 = top, last = bottom. JSON accepts "Top"/"Bottom" or {"index":N}.
type Layer struct {
	Index uint8 `json:"index"`
}

// Common layers.
var (
	LayerTop    = Layer{Index: 0}
	LayerBottom = Layer{Index: 1}
)

// IsTop reports index 0.
func (l Layer) IsTop() bool { return l.Index == 0 }

// LegacyName returns a short display name.
func (l Layer) LegacyName() string {
	switch l.Index {
	case 0:
		return "Top"
	case 1:
		return "Bottom"
	default:
		return fmt.Sprintf("In%d", l.Index-1)
	}
}

// MarshalJSON emits "Top"/"Bottom" for outer layers (byte-stable with Rust).
func (l Layer) MarshalJSON() ([]byte, error) {
	switch l.Index {
	case 0:
		return []byte(`"Top"`), nil
	case 1:
		return []byte(`"Bottom"`), nil
	default:
		return json.Marshal(struct {
			Index uint8 `json:"index"`
		}{Index: l.Index})
	}
}

// UnmarshalJSON accepts "Top"/"Bottom"/"F.Cu"/"B.Cu"/"InN", {index:N}, or bare int.
func (l *Layer) UnmarshalJSON(b []byte) error {
	// string form
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		switch strings.ToLower(s) {
		case "top", "f.cu":
			*l = LayerTop
			return nil
		case "bottom", "b.cu":
			*l = LayerBottom
			return nil
		}
		if strings.HasPrefix(s, "In") || strings.HasPrefix(s, "in") {
			rest := s[2:]
			if i := strings.IndexByte(rest, '.'); i >= 0 {
				rest = rest[:i]
			}
			n, err := strconv.Atoi(rest)
			if err != nil || n < 1 {
				return fmt.Errorf("unknown layer %q", s)
			}
			*l = Layer{Index: uint8(n)}
			return nil
		}
		return fmt.Errorf("unknown layer %q", s)
	}
	// bare number
	var n int
	if err := json.Unmarshal(b, &n); err == nil {
		if n < 0 || n > 255 {
			return fmt.Errorf("layer index out of range: %d", n)
		}
		*l = Layer{Index: uint8(n)}
		return nil
	}
	// object
	var obj struct {
		Index *uint8 `json:"index"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		return err
	}
	if obj.Index == nil {
		return fmt.Errorf("layer missing index")
	}
	*l = Layer{Index: *obj.Index}
	return nil
}

// SilkLayer is the silkscreen side.
type SilkLayer string

const (
	SilkTop    SilkLayer = "Top"
	SilkBottom SilkLayer = "Bottom"
)

// SilkAnchor is horizontal text anchor (SVG text-anchor semantics).
type SilkAnchor string

const (
	SilkAnchorStart  SilkAnchor = "Start"
	SilkAnchorMiddle SilkAnchor = "Middle"
	SilkAnchorEnd    SilkAnchor = "End"
)

// LayerKind classifies a stackup entry.
type LayerKind string

const (
	LayerKindSignal     LayerKind = "signal"
	LayerKindPower      LayerKind = "power"
	LayerKindPlane      LayerKind = "plane"
	LayerKindMixed      LayerKind = "mixed"
	LayerKindDielectric LayerKind = "dielectric"
)

// LayerSpec is one copper/dielectric entry in the stackup.
type LayerSpec struct {
	Name           string    `json:"name,omitempty"`
	Kind           LayerKind `json:"kind,omitempty"`
	ThicknessUM    float64   `json:"thickness_um,omitempty"`
	CopperWeightOz float64   `json:"copper_oz,omitempty"`
	DielectricEr   float64   `json:"dielectric_er,omitempty"`
	// AssignedNet is the pour/plane net (e.g. GND) — not a layer name.
	AssignedNet string `json:"assigned_net,omitempty"`
}

// UnmarshalJSON accepts copper_oz and the older copper_weight_oz tag.
func (l *LayerSpec) UnmarshalJSON(b []byte) error {
	type alias LayerSpec
	aux := struct {
		*alias
		LegacyOz float64 `json:"copper_weight_oz,omitempty"`
	}{alias: (*alias)(l)}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	if l.CopperWeightOz == 0 && aux.LegacyOz > 0 {
		l.CopperWeightOz = aux.LegacyOz
	}
	return nil
}

// Dielectric describes a dielectric sheet.
type Dielectric struct {
	ThicknessMM float64 `json:"thickness_mm,omitempty"`
	Er          float64 `json:"er,omitempty"`
}

// LayerStackup is the board layer stack.
type LayerStackup struct {
	Layers      []LayerSpec  `json:"layers,omitempty"`
	Dielectrics []Dielectric `json:"dielectrics,omitempty"`
}

// Default2Layer returns a standard 2-layer FR-4 stackup.
func Default2Layer() LayerStackup {
	return LayerStackup{
		Layers: []LayerSpec{
			{Name: "F.Cu", Kind: LayerKindSignal, CopperWeightOz: 1},
			{Name: "B.Cu", Kind: LayerKindSignal, CopperWeightOz: 1},
		},
		Dielectrics: []Dielectric{{ThicknessMM: 1.5, Er: 4.5}},
	}
}

// Default4Layer is a plausible JLCPCB 1.6 mm 4-layer stack:
// F.Cu / In1.Cu / In2.Cu / B.Cu. Inner layers are planes; assigned nets
// (GND / a power rail) are filled in by Apply4Layer when those nets exist.
func Default4Layer() LayerStackup {
	return LayerStackup{
		Layers: []LayerSpec{
			{Name: "F.Cu", Kind: LayerKindSignal, CopperWeightOz: 1},
			{Name: "In1.Cu", Kind: LayerKindPlane, CopperWeightOz: 1},
			{Name: "In2.Cu", Kind: LayerKindPlane, CopperWeightOz: 1},
			{Name: "B.Cu", Kind: LayerKindSignal, CopperWeightOz: 1},
		},
		Dielectrics: []Dielectric{
			{ThicknessMM: 0.21, Er: 4.5},
			{ThicknessMM: 1.065, Er: 4.5},
			{ThicknessMM: 0.21, Er: 4.5},
		},
	}
}

// IsPlane reports whether copper index i is a power/plane layer.
func (s LayerStackup) IsPlane(i int) bool {
	if i < 0 || i >= len(s.Layers) {
		return false
	}
	k := s.Layers[i].Kind
	return k == LayerKindPower || k == LayerKindPlane
}

// CopperOz returns the copper weight in oz for layer i (default 1).
func (s LayerStackup) CopperOz(i int) float64 {
	if i < 0 || i >= len(s.Layers) {
		return 1
	}
	oz := s.Layers[i].CopperWeightOz
	if oz <= 0 {
		return 1
	}
	return oz
}

// TotalThicknessMM is copper + dielectric, approximating 1 oz = 0.035 mm.
func (s LayerStackup) TotalThicknessMM() float64 {
	t := 0.0
	for i, l := range s.Layers {
		if l.Kind == LayerKindDielectric {
			continue
		}
		t += s.CopperOz(i) * 0.035
	}
	for _, d := range s.Dielectrics {
		t += d.ThicknessMM
	}
	return t
}

// CopperCount returns the number of copper layers (defaults to 2).
func (s LayerStackup) CopperCount() int {
	n := 0
	for _, l := range s.Layers {
		if l.Kind != LayerKindDielectric {
			n++
		}
	}
	if n == 0 {
		return 2
	}
	return n
}

// DielectricThicknessMM returns the first dielectric thickness or 1.5.
func (s LayerStackup) DielectricThicknessMM() float64 {
	if len(s.Dielectrics) > 0 && s.Dielectrics[0].ThicknessMM > 0 {
		return s.Dielectrics[0].ThicknessMM
	}
	return 1.5
}

// DielectricEr returns relative permittivity or 4.5.
func (s LayerStackup) DielectricEr() float64 {
	if len(s.Dielectrics) > 0 && s.Dielectrics[0].Er > 0 {
		return s.Dielectrics[0].Er
	}
	return 4.5
}

// CopperThicknessMM returns approx thickness from first copper weight (1 oz ≈ 0.035 mm).
func (s LayerStackup) CopperThicknessMM() float64 {
	for _, l := range s.Layers {
		if l.Kind != LayerKindDielectric {
			oz := l.CopperWeightOz
			if oz <= 0 {
				oz = 1
			}
			return oz * 0.035
		}
	}
	return 0.035
}

// BottomLayer returns the bottom copper layer handle.
func (s LayerStackup) BottomLayer() Layer {
	n := s.CopperCount()
	if n < 2 {
		return LayerBottom
	}
	return Layer{Index: uint8(n - 1)}
}
