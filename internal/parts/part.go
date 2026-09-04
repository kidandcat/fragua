// Package parts turns real-world component sources into library entries.
//
// Three sources, all producing the same core.LibraryEntry + schematic pins:
//   - LCSC / JLCPCB via the EasyEDA public API  (easyeda.go, online + cached)
//   - KiCad .kicad_mod / .kicad_sym             (kicad_mod.go, kicad_sym.go, offline)
//   - IPC-7351B land-pattern generators         (ipc.go, always available)
//
// Every entry is footprint-local mm, Y-up, matching core.LibraryPad.
package parts

import (
	"strings"

	"github.com/mentasystems/fragua/internal/core"
)

// Source tags where an entry came from (stored in LibraryEntry.Source).
const (
	SourceLCSC  = "lcsc"
	SourceKiCad = "kicad"
	SourceIPC   = "ipc"
)

// Part is one resolved component: a library footprint plus the schematic
// pins that go with it. Entry.Pins/Entry.SymbolKindName mirror Pins/Kind so
// the whole thing survives a round-trip through the on-disk library.
type Part struct {
	Entry core.LibraryEntry
	Pins  []core.SchPin
	// Kind is the schematic symbol kind ("generic_ic", "resistor", …).
	Kind string
	// RefPrefix is the natural reference letter ("U", "R", "C", "J", …).
	RefPrefix string
}

// finish stamps the derived fields onto the entry. Call before returning a Part.
func (p *Part) finish(source string) {
	if p.Kind == "" {
		p.Kind = "generic_ic"
	}
	if p.RefPrefix == "" {
		p.RefPrefix = refPrefixForKind(p.Kind)
	}
	p.Entry.Source = source
	p.Entry.Pins = p.Pins
	p.Entry.SymbolKindName = p.Kind
	if p.Entry.Silk == nil {
		p.Entry.Silk = []core.LibrarySilk{}
	}
	if p.Entry.Attachments == nil {
		p.Entry.Attachments = []core.Attachment{}
	}
}

func refPrefixForKind(kind string) string {
	switch kind {
	case "resistor":
		return "R"
	case "capacitor":
		return "C"
	case "inductor":
		return "L"
	case "led", "diode":
		return "D"
	default:
		return "U"
	}
}

// RefPrefixFromEasyEDA normalises EasyEDA's `pre` field ("U?", "R?") to a letter.
func RefPrefixFromEasyEDA(pre string) string {
	pre = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(pre), "?"))
	pre = strings.TrimSuffix(pre, "*")
	out := make([]rune, 0, 4)
	for _, r := range pre {
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return ""
	}
	return strings.ToUpper(string(out))
}

// twoPadNumbers reports the pad numbers when the footprint has exactly two pads.
func twoPadNumbers(pads []core.LibraryPad) (string, string, bool) {
	if len(pads) != 2 {
		return "", "", false
	}
	return pads[0].Number, pads[1].Number, true
}

// inferKind picks a discrete symbol kind from a category/package hint when the
// footprint is a plain two-terminal part; everything else stays generic_ic so
// that pin numbers (and therefore `net REF.PIN`) always resolve.
//
// led/diode are only used when the pads are literally A/K — SymbolKind.Pins()
// hands those kinds pins named A and K, which would not match numeric pads.
func inferKind(hint string, pads []core.LibraryPad) string {
	a, b, two := twoPadNumbers(pads)
	if !two {
		return "generic_ic"
	}
	h := strings.ToLower(hint)
	numeric := (a == "1" && b == "2") || (a == "2" && b == "1")
	ak := (strings.EqualFold(a, "A") && strings.EqualFold(b, "K")) ||
		(strings.EqualFold(a, "K") && strings.EqualFold(b, "A"))
	switch {
	case strings.Contains(h, "led"):
		if ak {
			return "led"
		}
	case strings.Contains(h, "diode"), strings.Contains(h, "schottky"), strings.Contains(h, "zener"):
		if ak {
			return "diode"
		}
	case strings.Contains(h, "resistor"):
		if numeric {
			return "resistor"
		}
	case strings.Contains(h, "capacitor"), strings.Contains(h, "mlcc"):
		if numeric {
			return "capacitor"
		}
	case strings.Contains(h, "inductor"), strings.Contains(h, "ferrite"), strings.Contains(h, "bead"):
		if numeric {
			return "inductor"
		}
	}
	return "generic_ic"
}

// genericPins builds a generic_ic pin list from pads alone (no names known),
// alternating sides so the symbol stays readable.
func genericPins(pads []core.LibraryPad) []core.SchPin {
	pins := make([]core.SchPin, 0, len(pads))
	for i, p := range pads {
		side := core.PinLeft
		if i >= (len(pads)+1)/2 {
			side = core.PinRight
		}
		pins = append(pins, core.SchPin{
			Number: p.Number,
			Name:   p.Name,
			Side:   side,
			Role:   core.PinPassive,
		})
	}
	return pins
}
