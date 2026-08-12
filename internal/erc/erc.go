// Package erc implements electrical rule checking on the schematic.
package erc

import (
	"fmt"
	"strings"

	"github.com/mentasystems/fragua/internal/core"
)

// Severity of a finding.
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Kind classifies an ERC violation.
type Kind string

const (
	KindFloatingPin      Kind = "floating_pin"
	KindFloatingNet      Kind = "floating_net"
	KindDuplicatePin     Kind = "duplicate_pin"
	KindEmptyNet         Kind = "empty_net"
	KindPhantomNet       Kind = "phantom_net"
	KindOrphanSymbol     Kind = "orphan_symbol"
	KindMultipleDrivers  Kind = "multiple_drivers"
	KindUnpoweredPower   Kind = "unpowered_power_net"
	KindUnconnectedInput Kind = "unconnected_input"
	KindUndrivenInput    Kind = "undriven_input"
	KindMissingDecoupling Kind = "missing_decoupling_cap"
	KindMissingI2CPullup Kind = "missing_i2c_pullup"
)

// Violation is one ERC finding.
type Violation struct {
	Kind     Kind     `json:"kind"`
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
	Net      string   `json:"net,omitempty"`
	Symbol   string   `json:"symbol,omitempty"`
}

// Options configures ERC.
type Options struct {
	Heuristics           bool
	DecouplingMaxDistMM  float64
}

// DefaultOptions enables heuristics.
func DefaultOptions() Options {
	return Options{Heuristics: true, DecouplingMaxDistMM: 5.0}
}

// Report is the full ERC result.
type Report struct {
	Violations []Violation `json:"violations"`
	Errors     int         `json:"errors"`
	Warnings   int         `json:"warnings"`
}

// Summary is agent-friendly.
func (r Report) Summary() string {
	return fmt.Sprintf("erc: %d errors, %d warnings (%d findings)", r.Errors, r.Warnings, len(r.Violations))
}

func (r *Report) add(v Violation) {
	r.Violations = append(r.Violations, v)
	if v.Severity == SeverityError {
		r.Errors++
	} else {
		r.Warnings++
	}
}

// Check runs ERC over schematic (+ board for phantom nets / heuristics).
func Check(sch *core.Schematic, board *core.Board, opts Options) Report {
	var rep Report
	if sch == nil {
		return rep
	}

	// Build pin → nets index
	type pinKey struct{ sym, pin string }
	pinNets := map[pinKey][]string{}
	symWired := map[string]bool{}

	for name, net := range sch.Nets {
		if net == nil {
			continue
		}
		if len(net.Connections) == 0 {
			rep.add(Violation{Kind: KindEmptyNet, Severity: SeverityWarning, Message: "empty net " + name, Net: name})
			continue
		}
		if len(net.Connections) < 2 {
			rep.add(Violation{Kind: KindFloatingNet, Severity: SeverityWarning, Message: "net " + name + " has <2 connections", Net: name})
		}
		for _, c := range net.Connections {
			// resolve symbol ref
			ref := symbolRef(sch, c.SymbolID)
			k := pinKey{ref, c.PinNumber}
			pinNets[k] = append(pinNets[k], name)
			symWired[ref] = true
		}
	}

	// Duplicate pins
	for k, nets := range pinNets {
		if len(nets) > 1 {
			rep.add(Violation{
				Kind: KindDuplicatePin, Severity: SeverityError,
				Message: fmt.Sprintf("%s.%s in multiple nets: %s", k.sym, k.pin, strings.Join(nets, ",")),
				Symbol:  k.sym,
			})
		}
	}

	// Floating pins / orphan symbols
	for _, sym := range sch.Symbols {
		if sym == nil {
			continue
		}
		pins := sym.Kind.Pins()
		if len(pins) == 0 {
			continue
		}
		any := false
		for _, pin := range pins {
			k := pinKey{sym.Reference, pin.Number}
			if _, ok := pinNets[k]; !ok {
				rep.add(Violation{
					Kind: KindFloatingPin, Severity: SeverityWarning,
					Message: fmt.Sprintf("%s.%s floating", sym.Reference, pin.Number),
					Symbol:  sym.Reference,
				})
			} else {
				any = true
			}
		}
		if !any {
			rep.add(Violation{
				Kind: KindOrphanSymbol, Severity: SeverityWarning,
				Message: "orphan symbol " + sym.Reference,
				Symbol:  sym.Reference,
			})
		}
	}

	// Role-based checks
	for name, net := range sch.Nets {
		if net == nil || len(net.Connections) == 0 {
			continue
		}
		var roles []core.PinRole
		outputs := 0
		powerOut, powerIn := 0, 0
		inputs := 0
		for _, c := range net.Connections {
			role := pinRole(sch, c.SymbolID, c.PinNumber)
			roles = append(roles, role)
			switch role {
			case core.PinOutput:
				outputs++
			case core.PinPowerOut:
				powerOut++
			case core.PinPowerIn:
				powerIn++
			case core.PinInput:
				inputs++
			}
		}
		if outputs > 1 {
			rep.add(Violation{
				Kind: KindMultipleDrivers, Severity: SeverityError,
				Message: fmt.Sprintf("net %s has %d outputs", name, outputs),
				Net:     name,
			})
		}
		if powerIn > 0 && powerOut == 0 && !core.IsPowerNamedNet(name) && !hasPour(board, name) {
			rep.add(Violation{
				Kind: KindUnpoweredPower, Severity: SeverityError,
				Message: "power net " + name + " has PowerIn but no PowerOut",
				Net:     name,
			})
		}
		if inputs > 0 && outputs == 0 && powerOut == 0 {
			hasDriver := false
			for _, r := range roles {
				if r == core.PinBidir || r == core.PinPowerOut || r == core.PinOutput {
					hasDriver = true
				}
			}
			if !hasDriver && !core.IsPowerNamedNet(name) {
				rep.add(Violation{
					Kind: KindUndrivenInput, Severity: SeverityWarning,
					Message: "undriven input net " + name,
					Net:     name,
				})
			}
		}
	}

	// Phantom nets on board pads
	if board != nil {
		known := map[string]bool{}
		for n := range sch.Nets {
			known[n] = true
		}
		seen := map[string]bool{}
		for _, fp := range board.Footprints {
			if fp == nil {
				continue
			}
			for _, pad := range fp.Pads {
				if pad.Net == nil || *pad.Net == "" {
					continue
				}
				n := *pad.Net
				if !known[n] && !seen[n] {
					seen[n] = true
					rep.add(Violation{
						Kind: KindPhantomNet, Severity: SeverityWarning,
						Message: fmt.Sprintf("pad net %s not in schematic", n),
						Net:     n,
					})
				}
			}
		}
	}

	if opts.Heuristics {
		checkI2CPullups(sch, &rep)
	}

	return rep
}

func symbolRef(sch *core.Schematic, id core.ID) string {
	key := id.String()
	if s := sch.Symbols[key]; s != nil {
		return s.Reference
	}
	for _, s := range sch.Symbols {
		if s != nil && s.ID == id {
			return s.Reference
		}
	}
	return id.String()
}

func pinRole(sch *core.Schematic, id core.ID, pin string) core.PinRole {
	var sym *core.Symbol
	key := id.String()
	if s := sch.Symbols[key]; s != nil {
		sym = s
	} else {
		for _, s := range sch.Symbols {
			if s != nil && s.ID == id {
				sym = s
				break
			}
		}
	}
	if sym == nil {
		return core.PinPassive
	}
	for _, p := range sym.Kind.Pins() {
		if p.Number == pin {
			if p.Role == "" {
				return core.PinPassive
			}
			return p.Role
		}
	}
	return core.PinPassive
}

func hasPour(board *core.Board, net string) bool {
	if board == nil {
		return false
	}
	for _, p := range board.Pours {
		if p.Net == net {
			return true
		}
	}
	return false
}

func checkI2CPullups(sch *core.Schematic, rep *Report) {
	for name := range sch.Nets {
		u := strings.ToUpper(name)
		if !strings.Contains(u, "SDA") && !strings.Contains(u, "SCL") && !strings.Contains(u, "I2C") {
			continue
		}
		// look for resistor on net
		hasR := false
		net := sch.Nets[name]
		if net == nil {
			continue
		}
		for _, c := range net.Connections {
			ref := symbolRef(sch, c.SymbolID)
			if strings.HasPrefix(ref, "R") {
				hasR = true
				break
			}
			// also check symbol kind
			for _, s := range sch.Symbols {
				if s != nil && s.ID == c.SymbolID && strings.EqualFold(s.Kind.Kind, "resistor") {
					hasR = true
				}
			}
		}
		if !hasR {
			rep.add(Violation{
				Kind: KindMissingI2CPullup, Severity: SeverityWarning,
				Message: "I2C-like net " + name + " may need pull-up",
				Net:     name,
			})
		}
	}
}
