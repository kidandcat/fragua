package parts

import (
	"fmt"
	"os"
	"strings"

	"github.com/mentasystems/fragua/internal/core"
)

// KicadSymbol is one parsed .kicad_sym entry.
type KicadSymbol struct {
	Name        string
	Description string
	RefPrefix   string
	Pins        []core.SchPin
}

// ParseKicadSym reads a .kicad_sym library. Graphics are ignored; only
// `(pin TYPE STYLE (at x y rot) (name "…") (number "…"))` matters. Unit
// sub-symbols ("R_Small_1_1") are walked, and their pins folded into the
// parent. Pins with the same number across units are kept once.
func ParseKicadSym(src []byte) ([]KicadSymbol, error) {
	nodes, err := parseSexp(string(src))
	if err != nil {
		return nil, fmt.Errorf("kicad_sym: %w", err)
	}
	var out []KicadSymbol
	for _, n := range nodes {
		switch n.name() {
		case "kicad_symbol_lib":
			for _, s := range n.children("symbol") {
				out = append(out, kicadSymbol(s))
			}
		case "symbol":
			out = append(out, kicadSymbol(n))
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("kicad_sym: no (symbol …) nodes")
	}
	return out, nil
}

func kicadSymbol(node sexp) KicadSymbol {
	sym := KicadSymbol{Name: unquote(node.arg(0))}
	for _, p := range node.children("property") {
		switch unquote(p.arg(0)) {
		case "Reference":
			sym.RefPrefix = RefPrefixFromEasyEDA(unquote(p.arg(1)))
		case "Description":
			sym.Description = unquote(p.arg(1))
		}
	}
	seen := map[string]bool{}
	var walk func(sexp)
	walk = func(n sexp) {
		for _, pin := range n.children("pin") {
			sp, ok := kicadPin(pin)
			if !ok || seen[sp.Number] {
				continue
			}
			seen[sp.Number] = true
			sym.Pins = append(sym.Pins, sp)
		}
		for _, unit := range n.children("symbol") {
			walk(unit)
		}
	}
	walk(node)
	return sym
}

func kicadPin(node sexp) (core.SchPin, bool) {
	pin := core.SchPin{Role: kicadPinRole(node.arg(0))}
	if n, ok := node.child("number"); ok {
		pin.Number = unquote(n.arg(0))
	}
	if pin.Number == "" {
		return core.SchPin{}, false
	}
	if n, ok := node.child("name"); ok {
		name := unquote(n.arg(0))
		if name != "~" && name != pin.Number {
			pin.Name = name
		}
	}
	at, _ := node.child("at")
	pin.Side = kicadPinSide(at.argF(2))
	if pin.Role == core.PinNC {
		pin.NC = true
	}
	return pin, true
}

// kicadPinRole maps the KiCad electrical type onto core.PinRole.
func kicadPinRole(s string) core.PinRole {
	switch strings.ToLower(s) {
	case "input":
		return core.PinInput
	case "output":
		return core.PinOutput
	case "bidirectional", "tri_state":
		return core.PinBidir
	case "power_in":
		return core.PinPowerIn
	case "power_out", "open_collector", "open_emitter":
		return core.PinPowerOut
	case "no_connect", "unconnected":
		return core.PinNC
	default: // passive, free, unspecified
		return core.PinPassive
	}
}

// kicadPinSide maps a pin's `at` angle to a symbol side. In KiCad the angle is
// the direction the pin line runs *from* its connection point towards the body,
// so the pin sits on the opposite edge: 0 (→ +X) means a left-hand pin.
func kicadPinSide(rot float64) core.PinSide {
	switch mod360(rot) {
	case 90:
		return core.PinBottom
	case 180:
		return core.PinRight
	case 270:
		return core.PinTop
	default:
		return core.PinLeft
	}
}

// LoadKicadSym reads and parses a .kicad_sym file.
func LoadKicadSym(path string) ([]KicadSymbol, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseKicadSym(src)
}

// MergeSymbolPins copies pin names/roles from a KiCad symbol onto an entry
// whose pads already carry the right numbers. Returns how many pins matched.
func MergeSymbolPins(entry *core.LibraryEntry, sym KicadSymbol) int {
	byNum := map[string]int{}
	for i := range entry.Pads {
		byNum[entry.Pads[i].Number] = i
	}
	var pins []core.SchPin
	matched := 0
	for _, p := range sym.Pins {
		i, ok := byNum[p.Number]
		if !ok {
			continue
		}
		matched++
		if p.Name != "" {
			entry.Pads[i].Name = p.Name
		}
		pins = append(pins, p)
	}
	if matched == 0 {
		return 0
	}
	entry.Pins = pins
	if entry.SymbolKindName == "" || entry.SymbolKindName == "generic_ic" {
		entry.SymbolKindName = "generic_ic"
	}
	return matched
}
