package erc

import (
	"testing"

	"github.com/mentasystems/fragua/internal/core"
)

func TestEmptyNet(t *testing.T) {
	sch := core.NewSchematic()
	sch.Nets["ORPHAN"] = &core.Net{Name: "ORPHAN", Connections: nil}
	rep := Check(sch, nil, DefaultOptions())
	found := false
	for _, v := range rep.Violations {
		if v.Kind == KindEmptyNet {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected empty net, got %+v", rep)
	}
}

func TestFloatingPin(t *testing.T) {
	sch := core.NewSchematic()
	id := core.NewID()
	sch.Symbols[id.String()] = &core.Symbol{
		ID: id, Reference: "R1", Kind: core.SymbolKind{Kind: "resistor"},
		Position: core.Origin,
	}
	sch.SymbolOrder = []string{id.String()}
	// pin 1 connected, pin 2 floating
	sch.Nets["N1"] = &core.Net{
		Name: "N1",
		Connections: []core.NetConnection{
			{SymbolID: id, PinNumber: "1"},
		},
	}
	// need second symbol for net not floating
	id2 := core.NewID()
	sch.Symbols[id2.String()] = &core.Symbol{
		ID: id2, Reference: "R2", Kind: core.SymbolKind{Kind: "resistor"},
		Position: core.Origin,
	}
	sch.Nets["N1"].Connections = append(sch.Nets["N1"].Connections, core.NetConnection{SymbolID: id2, PinNumber: "1"})

	rep := Check(sch, nil, Options{Heuristics: false})
	found := false
	for _, v := range rep.Violations {
		if v.Kind == KindFloatingPin {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected floating pin, got %+v", rep)
	}
}
