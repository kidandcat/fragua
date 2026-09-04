package erc

import (
	"strings"
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

func TestNCPinNotFloating(t *testing.T) {
	sch := core.NewSchematic()
	id := core.NewID()
	sch.Symbols[id.String()] = &core.Symbol{
		ID: id, Reference: "U1",
		Kind: core.SymbolKind{Kind: "generic_ic", ICPins: []core.SchPin{
			{Number: "1", Side: core.PinLeft, Role: core.PinInput},
			{Number: "2", Side: core.PinRight, Role: core.PinNC, NC: true},
		}},
	}
	sch.SymbolOrder = []string{id.String()}
	rep := Check(sch, nil, Options{Heuristics: false})
	for _, v := range rep.Violations {
		if strings.Contains(v.Message, "U1.2") {
			t.Fatalf("NC pin should be silent: %+v", rep.Violations)
		}
	}
	if countKind(rep, KindUnconnectedInput) == 0 {
		t.Fatalf("open required input U1.1 must be an error, got %+v", rep.Violations)
	}
	if rep.Errors == 0 {
		t.Fatal("open input must count as ERC error")
	}
}

func countKind(rep Report, k Kind) int {
	n := 0
	for _, v := range rep.Violations {
		if v.Kind == k {
			n++
		}
	}
	return n
}

// The script API returns whatever Summary/Detail produce; an agent that only
// gets counts cannot fix its netlist.
func TestReportDetailNamesTheViolations(t *testing.T) {
	r := Report{}
	r.add(Violation{Kind: KindFloatingPin, Severity: SeverityError,
		Message: "pin not connected", Symbol: "U1", Net: ""})
	r.add(Violation{Kind: KindFloatingNet, Severity: SeverityWarning,
		Message: "net has one connection", Net: "SDA"})
	got := r.Detail()
	for _, want := range []string{"1 errors", "1 warnings", "pin not connected",
		"sym=U1", "net=SDA"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Detail() missing %q:\n%s", want, got)
		}
	}
}
