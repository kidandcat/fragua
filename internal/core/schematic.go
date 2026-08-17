package core

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// PinSide is which side of a symbol body a pin exits.
type PinSide string

const (
	PinLeft   PinSide = "left"
	PinRight  PinSide = "right"
	PinTop    PinSide = "top"
	PinBottom PinSide = "bottom"
)

// PinRole is the electrical role used by ERC.
type PinRole string

const (
	PinPassive  PinRole = "passive"
	PinInput    PinRole = "input"
	PinOutput   PinRole = "output"
	PinBidir    PinRole = "bidir"
	PinPowerOut PinRole = "power_out"
	PinPowerIn  PinRole = "power_in"
	PinNC       PinRole = "nc"
)

// IsNC reports a no-connect / explicitly unused pin.
func (p SchPin) IsNC() bool {
	return p.Role == PinNC || p.NC
}

// SchPin is one pin on a generic IC symbol.
type SchPin struct {
	Number string  `json:"number"`
	Name   string  `json:"name"`
	Side   PinSide `json:"side"`
	Role   PinRole `json:"role"`
	NC     bool    `json:"nc,omitempty"`
}

// SymbolKind tags the symbol type. JSON uses {"kind":"resistor"} or generic_ic with pins.
type SymbolKind struct {
	Kind   string   `json:"kind"`
	ICPins []SchPin `json:"pins,omitempty"`
}

// UnmarshalJSON accepts tagged form from Rust serde.
func (k *SymbolKind) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		// plain string?
		var s string
		if err2 := json.Unmarshal(b, &s); err2 == nil {
			k.Kind = s
			return nil
		}
		return err
	}
	if v, ok := raw["kind"]; ok {
		_ = json.Unmarshal(v, &k.Kind)
	}
	if v, ok := raw["pins"]; ok {
		_ = json.Unmarshal(v, &k.ICPins)
	}
	return nil
}

// MarshalJSON emits {"kind":"..."} form.
func (k SymbolKind) MarshalJSON() ([]byte, error) {
	if len(k.ICPins) > 0 {
		return json.Marshal(struct {
			Kind string   `json:"kind"`
			Pins []SchPin `json:"pins"`
		}{Kind: k.Kind, Pins: k.ICPins})
	}
	return json.Marshal(struct {
		Kind string `json:"kind"`
	}{Kind: k.Kind})
}

// Pins returns pin definitions for discretes or explicit IC pins.
func (k SymbolKind) Pins() []SchPin {
	switch strings.ToLower(k.Kind) {
	case "resistor", "capacitor", "inductor":
		return []SchPin{
			{Number: "1", Side: PinLeft, Role: PinPassive},
			{Number: "2", Side: PinRight, Role: PinPassive},
		}
	case "led", "diode":
		return []SchPin{
			{Number: "A", Name: "A", Side: PinLeft, Role: PinPassive},
			{Number: "K", Name: "K", Side: PinRight, Role: PinPassive},
		}
	case "generic_ic", "genericic":
		return k.ICPins
	default:
		if len(k.ICPins) > 0 {
			return k.ICPins
		}
		return nil
	}
}

// Symbol is a schematic symbol instance.
type Symbol struct {
	ID           ID         `json:"id"`
	Reference    string     `json:"reference"`
	Value        string     `json:"value"`
	Kind         SymbolKind `json:"kind"`
	Position     Point      `json:"position"`
	Rotation     float64    `json:"rotation"`
	Key          string     `json:"key"`
	Description  string     `json:"description"`
	LcscID       string     `json:"lcsc_id,omitempty"`
	MPN          string     `json:"mpn,omitempty"`
	Manufacturer string     `json:"manufacturer,omitempty"`
}

// NetConnection links a symbol pin to a net.
type NetConnection struct {
	SymbolID  ID     `json:"symbol_id"`
	PinNumber string `json:"pin_number"`
}

// Net is a named electrical net.
type Net struct {
	Name        string          `json:"name"`
	Connections []NetConnection `json:"connections"`
	Class       string          `json:"class,omitempty"`
	DiffPair    string          `json:"diff_pair,omitempty"`
}

// NetClass holds physical rules for a class of nets.
// ImpedanceOhms is stored for later; DRC does not field-solve it.
type NetClass struct {
	Name               string  `json:"name"`
	TraceWidthMM       float64 `json:"trace_width_mm,omitempty"`
	ClearanceMM        float64 `json:"clearance_mm,omitempty"`
	LengthToleranceMM  float64 `json:"length_tolerance_mm,omitempty"`
	ViaDrillMM         float64 `json:"via_drill_mm,omitempty"`
	ViaDiameterMM      float64 `json:"via_diameter_mm,omitempty"`
	ImpedanceOhms      float64 `json:"impedance_ohms,omitempty"`
	DiffPair           string  `json:"diff_pair,omitempty"`
}

// Schematic is the netlist + layout hints.
type Schematic struct {
	Symbols      map[string]*Symbol   `json:"symbols"`
	SymbolOrder  []string             `json:"symbol_order"`
	Nets         map[string]*Net      `json:"nets"`
	NetClasses   map[string]*NetClass `json:"net_classes"`
	NetToClass   map[string]string    `json:"net_to_class"`
}

// NewSchematic returns an empty schematic.
func NewSchematic() *Schematic {
	return &Schematic{
		Symbols:    make(map[string]*Symbol),
		Nets:       make(map[string]*Net),
		NetClasses: make(map[string]*NetClass),
		NetToClass: make(map[string]string),
	}
}

// UnmarshalJSON ensures maps are non-nil.
func (s *Schematic) UnmarshalJSON(data []byte) error {
	type alias Schematic
	aux := (*alias)(s)
	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}
	if s.Symbols == nil {
		s.Symbols = make(map[string]*Symbol)
	}
	if s.Nets == nil {
		s.Nets = make(map[string]*Net)
	}
	if s.NetClasses == nil {
		s.NetClasses = make(map[string]*NetClass)
	}
	if s.NetToClass == nil {
		s.NetToClass = make(map[string]string)
	}
	return nil
}

// IsPowerNamedNet reports names that look like power rails.
func IsPowerNamedNet(name string) bool {
	n := strings.ToUpper(strings.TrimSpace(name))
	if n == "GND" || n == "VCC" || n == "VDD" || n == "VSS" || n == "VGND" {
		return true
	}
	if strings.HasPrefix(n, "+") || strings.HasPrefix(n, "-") {
		return true
	}
	if strings.Contains(n, "VBUS") || strings.Contains(n, "VBAT") {
		return true
	}
	// +3V3 style without plus
	if len(n) >= 2 && (n[0] == 'V' || strings.Contains(n, "V")) {
		if strings.Contains(n, "3V3") || strings.Contains(n, "1V8") || strings.Contains(n, "5V") || strings.Contains(n, "1V1") {
			return true
		}
	}
	return false
}

// ParseBusRange parses D[0:7] → ("D", 0, 7).
func ParseBusRange(name string) (prefix string, lo, hi int, ok bool) {
	i := strings.Index(name, "[")
	j := strings.Index(name, "]")
	if i < 0 || j <= i {
		return "", 0, 0, false
	}
	inner := name[i+1 : j]
	parts := strings.Split(inner, ":")
	if len(parts) != 2 {
		return "", 0, 0, false
	}
	a, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	b, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil {
		return "", 0, 0, false
	}
	if a > b {
		a, b = b, a
	}
	return name[:i], a, b, true
}

// ExpandBus expands bus net names into individual nets.
func ExpandBus(name string) []string {
	prefix, lo, hi, ok := ParseBusRange(name)
	if !ok {
		return []string{name}
	}
	out := make([]string, 0, hi-lo+1)
	for i := lo; i <= hi; i++ {
		out = append(out, fmt.Sprintf("%s%d", prefix, i))
	}
	return out
}
