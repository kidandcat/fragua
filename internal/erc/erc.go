// Package erc implements electrical rule checking on the schematic.
// Process order and kinds match crates/pcb-erc (Rust oracle).
package erc

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/mentasystems/fragua/internal/core"
)

// Severity of a finding.
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Kind classifies an ERC violation (snake_case; parity dump maps to PascalCase).
type Kind string

const (
	KindFloatingPin       Kind = "floating_pin"
	KindFloatingNet       Kind = "floating_net"
	KindDuplicatePin      Kind = "duplicate_pin"
	KindEmptyNet          Kind = "empty_net"
	KindPhantomNet        Kind = "phantom_net"
	KindOrphanSymbol      Kind = "orphan_symbol"
	KindMultipleDrivers   Kind = "multiple_drivers"
	KindUnpoweredPowerNet Kind = "unpowered_power_net"
	KindUnconnectedInput  Kind = "unconnected_input"
	KindUndrivenInput     Kind = "undriven_input"
	KindMissingDecoupling Kind = "missing_decoupling_cap"
	KindMissingPullup     Kind = "missing_pullup"
)

// Violation is one ERC finding.
type Violation struct {
	Kind     Kind     `json:"kind"`
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
	Net      string   `json:"net,omitempty"`
	Symbol   string   `json:"symbol,omitempty"`
	Involved []string `json:"involved,omitempty"`
}

// Options configures ERC.
type Options struct {
	Heuristics          bool
	DecouplingMaxDistMM float64
}

// DefaultOptions enables heuristics (Rust default).
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
// Order matches pcb_erc::run.
func Check(sch *core.Schematic, board *core.Board, opts Options) Report {
	var rep Report
	if sch == nil {
		return rep
	}
	checkDuplicatePins(sch, &rep)
	checkFloatingPins(sch, &rep)
	checkFloatingAndEmptyNets(sch, &rep)
	checkOrphanSymbols(sch, &rep)
	checkPhantomNets(board, sch, &rep)
	checkRoleBasedRules(board, sch, &rep)
	if opts.Heuristics {
		checkDecoupling(board, sch, opts.DecouplingMaxDistMM, &rep)
		checkI2CPullups(sch, &rep)
	}
	return rep
}

func checkDuplicatePins(sch *core.Schematic, rep *Report) {
	type key struct {
		id  core.ID
		pin string
	}
	homes := map[key][]string{}
	for netName, net := range sch.Nets {
		if net == nil {
			continue
		}
		for _, c := range net.Connections {
			k := key{c.SymbolID, c.PinNumber}
			homes[k] = append(homes[k], netName)
		}
	}
	for k, nets := range homes {
		sort.Strings(nets)
		// dedup
		uniq := nets[:0]
		var prev string
		for i, n := range nets {
			if i == 0 || n != prev {
				uniq = append(uniq, n)
				prev = n
			}
		}
		if len(uniq) < 2 {
			continue
		}
		ref := symbolRef(sch, k.id)
		rep.add(Violation{
			Kind: KindDuplicatePin, Severity: SeverityError,
			Message:  fmt.Sprintf("%s.%s in multiple nets: %s", ref, k.pin, strings.Join(uniq, ",")),
			Symbol:   ref,
			Involved: append([]string{ref + "." + k.pin}, uniq...),
		})
	}
}

func checkFloatingPins(sch *core.Schematic, rep *Report) {
	wired := map[string]map[string]bool{} // ref → pin → true
	for _, net := range sch.Nets {
		if net == nil {
			continue
		}
		for _, c := range net.Connections {
			ref := symbolRef(sch, c.SymbolID)
			if wired[ref] == nil {
				wired[ref] = map[string]bool{}
			}
			wired[ref][c.PinNumber] = true
		}
	}
	for _, sym := range symbolsInOrder(sch) {
		pins := sym.Kind.Pins()
		for _, pin := range pins {
			if pin.IsNC() {
				continue
			}
			if wired[sym.Reference][pin.Number] {
				continue
			}
			if pin.Role == core.PinInput {
				rep.add(Violation{
					Kind: KindUnconnectedInput, Severity: SeverityError,
					Message:  fmt.Sprintf("%s.%s required input is open", sym.Reference, pin.Number),
					Symbol:   sym.Reference,
					Involved: []string{sym.Reference + "." + pin.Number},
				})
				continue
			}
			rep.add(Violation{
				Kind: KindFloatingPin, Severity: SeverityWarning,
				Message:  fmt.Sprintf("%s.%s floating", sym.Reference, pin.Number),
				Symbol:   sym.Reference,
				Involved: []string{sym.Reference + "." + pin.Number},
			})
		}
	}
}

func checkFloatingAndEmptyNets(sch *core.Schematic, rep *Report) {
	// stable order
	names := make([]string, 0, len(sch.Nets))
	for n := range sch.Nets {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		net := sch.Nets[name]
		if net == nil {
			continue
		}
		if len(net.Connections) == 0 {
			rep.add(Violation{
				Kind: KindEmptyNet, Severity: SeverityWarning,
				Message: "empty net " + name, Net: name, Involved: []string{name},
			})
			continue
		}
		if len(net.Connections) < 2 {
			rep.add(Violation{
				Kind: KindFloatingNet, Severity: SeverityWarning,
				Message: "net " + name + " has <2 connections", Net: name, Involved: []string{name},
			})
		}
	}
}

func checkOrphanSymbols(sch *core.Schematic, rep *Report) {
	wired := map[string]bool{}
	for _, net := range sch.Nets {
		if net == nil {
			continue
		}
		for _, c := range net.Connections {
			wired[symbolRef(sch, c.SymbolID)] = true
		}
	}
	for _, sym := range symbolsInOrder(sch) {
		if len(sym.Kind.Pins()) == 0 {
			continue
		}
		if !wired[sym.Reference] {
			rep.add(Violation{
				Kind: KindOrphanSymbol, Severity: SeverityWarning,
				Message: "orphan symbol " + sym.Reference, Symbol: sym.Reference,
				Involved: []string{sym.Reference},
			})
		}
	}
}

func checkPhantomNets(board *core.Board, sch *core.Schematic, rep *Report) {
	if board == nil {
		return
	}
	known := map[string]bool{}
	for n := range sch.Nets {
		known[n] = true
	}
	seen := map[string]bool{}
	var names []string
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
				names = append(names, n)
			}
		}
	}
	sort.Strings(names)
	for _, n := range names {
		rep.add(Violation{
			Kind: KindPhantomNet, Severity: SeverityWarning,
			Message: fmt.Sprintf("pad net %s not in schematic", n), Net: n, Involved: []string{n},
		})
	}
}

func checkRoleBasedRules(board *core.Board, sch *core.Schematic, rep *Report) {
	poured := map[string]bool{}
	if board != nil {
		for _, p := range board.Pours {
			poured[p.Net] = true
		}
	}

	type pinRole struct {
		label string
		role  core.PinRole
	}
	roles := map[string][]pinRole{}
	for netName, net := range sch.Nets {
		if net == nil {
			continue
		}
		for _, c := range net.Connections {
			sym := symbolByID(sch, c.SymbolID)
			if sym == nil {
				continue
			}
			if pinIsNC(sym, c.PinNumber) {
				continue
			}
			role := pinRoleOf(sym, c.PinNumber)
			roles[netName] = append(roles[netName], pinRole{
				label: sym.Reference + "." + c.PinNumber,
				role:  role,
			})
		}
	}

	names := make([]string, 0, len(roles))
	for n := range roles {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, netName := range names {
		pins := roles[netName]
		var outputs []string
		hasPowerOut := poured[netName]
		hasPowerIn := false
		hasDriver := poured[netName]
		for _, p := range pins {
			switch p.role {
			case core.PinOutput:
				outputs = append(outputs, p.label)
				hasDriver = true
			case core.PinPowerOut:
				hasPowerOut = true
				hasDriver = true
			case core.PinPowerIn:
				hasPowerIn = true
			case core.PinBidir:
				hasDriver = true
			}
		}
		if len(outputs) >= 2 {
			rep.add(Violation{
				Kind: KindMultipleDrivers, Severity: SeverityError,
				Message:  fmt.Sprintf("net %s has %d Output drivers", netName, len(outputs)),
				Net:      netName,
				Involved: append([]string{netName}, outputs...),
			})
		}
		// Rust: Warning (not Error); pour counts as PowerOut.
		if hasPowerIn && !hasPowerOut {
			rep.add(Violation{
				Kind: KindUnpoweredPowerNet, Severity: SeverityWarning,
				Message:  fmt.Sprintf("net %s has PowerIn pin(s) but no PowerOut source", netName),
				Net:      netName,
				Involved: []string{netName},
			})
		}
		for _, p := range pins {
			if p.role == core.PinInput && !hasDriver {
				rep.add(Violation{
					Kind: KindUnconnectedInput, Severity: SeverityError,
					Message:  fmt.Sprintf("input pin %s on net %s has no driver", p.label, netName),
					Net:      netName,
					Symbol:   strings.Split(p.label, ".")[0],
					Involved: []string{netName, p.label},
				})
			}
		}
		// UndrivenInput: every pin is Input and no driver.
		inputCount := 0
		for _, p := range pins {
			if p.role == core.PinInput {
				inputCount++
			}
		}
		if len(pins) > 0 && inputCount == len(pins) && !hasDriver {
			inv := []string{netName}
			for _, p := range pins {
				inv = append(inv, p.label)
			}
			rep.add(Violation{
				Kind: KindUndrivenInput, Severity: SeverityError,
				Message:  fmt.Sprintf("net %s has only Input pins (%d) and no driver", netName, len(pins)),
				Net:      netName,
				Involved: inv,
			})
		}
	}
}

func checkDecoupling(board *core.Board, sch *core.Schematic, maxDistMM float64, rep *Report) {
	if board == nil {
		return
	}
	// symbol id → footprint position mm
	symPos := map[core.ID][2]float64{}
	for _, sym := range symbolsInOrder(sch) {
		for _, fp := range board.Footprints {
			if fp != nil && fp.Reference == sym.Reference {
				symPos[sym.ID] = [2]float64{fp.Position.X.ToMM(), fp.Position.Y.ToMM()}
				break
			}
		}
	}

	// caps by net name
	capsByNet := map[string][]struct {
		ref  string
		x, y float64
	}{}
	for _, sym := range symbolsInOrder(sch) {
		if !strings.EqualFold(sym.Kind.Kind, "capacitor") {
			continue
		}
		pos, ok := symPos[sym.ID]
		if !ok {
			continue
		}
		for _, net := range sch.Nets {
			if net == nil {
				continue
			}
			for _, c := range net.Connections {
				if c.SymbolID == sym.ID {
					capsByNet[net.Name] = append(capsByNet[net.Name], struct {
						ref  string
						x, y float64
					}{sym.Reference, pos[0], pos[1]})
					break
				}
			}
		}
	}

	for _, sym := range symbolsInOrder(sch) {
		if !isGenericIC(sym) {
			continue
		}
		sxsy, ok := symPos[sym.ID]
		if !ok {
			continue
		}
		sx, sy := sxsy[0], sxsy[1]
		for _, pin := range sym.Kind.Pins() {
			if pin.Role != core.PinPowerIn {
				continue
			}
			netName := netForPin(sch, sym.ID, pin.Number)
			if netName == "" {
				continue
			}
			// pours = decoupling-equivalent
			if board != nil {
				hasPour := false
				for _, p := range board.Pours {
					if p.Net == netName {
						hasPour = true
						break
					}
				}
				if hasPour {
					continue
				}
			}
			closeCap := false
			for _, cap := range capsByNet[netName] {
				dx, dy := cap.x-sx, cap.y-sy
				if math.Hypot(dx, dy) <= maxDistMM {
					closeCap = true
					break
				}
			}
			if !closeCap {
				label := sym.Reference + "." + pin.Number
				rep.add(Violation{
					Kind: KindMissingDecoupling, Severity: SeverityWarning,
					Message: fmt.Sprintf("no decoupling cap within %.1f mm of %s (net %s)", maxDistMM, label, netName),
					Net:     netName, Symbol: sym.Reference,
					Involved: []string{label, netName},
				})
			}
		}
	}
}

func checkI2CPullups(sch *core.Schematic, rep *Report) {
	resistorIDs := map[core.ID]bool{}
	for _, sym := range symbolsInOrder(sch) {
		if strings.EqualFold(sym.Kind.Kind, "resistor") {
			resistorIDs[sym.ID] = true
		}
	}
	names := make([]string, 0, len(sch.Nets))
	for n := range sch.Nets {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		net := sch.Nets[name]
		if net == nil || !isI2CNet(name) {
			continue
		}
		hasR := false
		for _, c := range net.Connections {
			if resistorIDs[c.SymbolID] {
				hasR = true
				break
			}
		}
		if !hasR {
			rep.add(Violation{
				Kind: KindMissingPullup, Severity: SeverityWarning,
				Message:  fmt.Sprintf("I²C net %s has no pull-up resistor", name),
				Net:      name,
				Involved: []string{name},
			})
		}
	}
}

// isI2CNet matches Rust check_i2c_pullups::is_i2c.
func isI2CNet(name string) bool {
	n := strings.ToUpper(strings.TrimSpace(name))
	n = strings.TrimPrefix(n, "+")
	n = strings.TrimPrefix(n, "-")
	n = strings.TrimPrefix(n, "I2C_")
	n = strings.TrimPrefix(n, "I2C")
	if n == "SDA" || n == "SCL" {
		return true
	}
	if strings.HasSuffix(n, "_SDA") || strings.HasSuffix(n, "_SCL") {
		return true
	}
	if strings.HasPrefix(n, "SDA") || strings.HasPrefix(n, "SCL") {
		return true
	}
	return false
}

func isGenericIC(sym *core.Symbol) bool {
	k := strings.ToLower(sym.Kind.Kind)
	return k == "generic_ic" || k == "genericic" || len(sym.Kind.ICPins) > 0 && k != "resistor" && k != "capacitor"
}

func symbolsInOrder(sch *core.Schematic) []*core.Symbol {
	var out []*core.Symbol
	seen := map[string]bool{}
	for _, id := range sch.SymbolOrder {
		if s := sch.Symbols[id]; s != nil {
			out = append(out, s)
			seen[id] = true
		}
	}
	var rest []string
	for id, s := range sch.Symbols {
		if s != nil && !seen[id] {
			rest = append(rest, id)
		}
	}
	sort.Strings(rest)
	for _, id := range rest {
		out = append(out, sch.Symbols[id])
	}
	return out
}

func symbolByID(sch *core.Schematic, id core.ID) *core.Symbol {
	if s := sch.Symbols[id.String()]; s != nil {
		return s
	}
	for _, s := range sch.Symbols {
		if s != nil && s.ID == id {
			return s
		}
	}
	return nil
}

func symbolRef(sch *core.Schematic, id core.ID) string {
	if s := symbolByID(sch, id); s != nil {
		return s.Reference
	}
	return id.String()
}

func pinIsNC(sym *core.Symbol, pin string) bool {
	if sym == nil {
		return false
	}
	for _, p := range sym.Kind.Pins() {
		if p.Number == pin {
			return p.IsNC()
		}
	}
	return false
}

func pinRoleOf(sym *core.Symbol, pin string) core.PinRole {
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

func netForPin(sch *core.Schematic, id core.ID, pin string) string {
	for name, net := range sch.Nets {
		if net == nil {
			continue
		}
		for _, c := range net.Connections {
			if c.SymbolID == id && c.PinNumber == pin {
				if net.Name != "" {
					return net.Name
				}
				return name
			}
		}
	}
	return ""
}
