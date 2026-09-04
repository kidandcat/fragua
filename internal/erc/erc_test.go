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

// The decoupling rule says "within N mm of U1.1" and must measure to that pad,
// not to the part's origin. On a big part — the minus pilot board's 18 x 23.5 mm
// castellated ESP32-S3-Zero — the 5V land is 12 mm from the origin, so a bulk
// cap sitting 2 mm from the land still reported `missing_decoupling_cap` and no
// legal placement could have cleared it.
func TestDecouplingMeasuresToThePinNotTheOrigin(t *testing.T) {
	sch := core.NewSchematic()
	uid, cid := core.NewID(), core.NewID()
	sch.Symbols[uid.String()] = &core.Symbol{
		ID: uid, Reference: "U1",
		Kind: core.SymbolKind{Kind: "generic_ic", ICPins: []core.SchPin{
			{Number: "1", Name: "P5V", Side: core.PinLeft, Role: core.PinPowerIn},
		}},
	}
	sch.Symbols[cid.String()] = &core.Symbol{
		ID: cid, Reference: "C5V", Kind: core.SymbolKind{Kind: "capacitor"},
	}
	sch.SymbolOrder = []string{uid.String(), cid.String()}
	sch.Nets["+5V"] = &core.Net{Name: "+5V", Connections: []core.NetConnection{
		{SymbolID: uid, PinNumber: "1"}, {SymbolID: cid, PinNumber: "1"},
	}}

	net5 := "+5V"
	b := core.NewBoard()
	// U1's origin is at (16, 14.3); its 5V land is 8.42 mm west and 10.16 mm
	// north of that, at (7.58, 24.46) — 13.2 mm from the origin.
	b.AddFootprint(&core.Footprint{
		ID: core.NewID(), Reference: "U1", Library: "esp32_s3_zero_cast",
		Position: core.NewPoint(core.FromMM(16), core.FromMM(14.3)),
		Layer:    core.LayerTop,
		Pads: []core.Pad{{
			Number: "1", Offset: core.NewPoint(core.FromMM(-8.42), core.FromMM(10.16)),
			Size:  [2]core.Length{core.FromMM(2.4), core.FromMM(1.5)},
			Layer: core.LayerTop, Net: &net5,
		}},
	})
	// The cap sits 2.7 mm from that land and 12.4 mm from U1's origin.
	b.AddFootprint(&core.Footprint{
		ID: core.NewID(), Reference: "C5V", Library: "c_0805",
		Position: core.NewPoint(core.FromMM(10.3), core.FromMM(23.6)),
		Layer:    core.LayerBottom,
		Pads: []core.Pad{{
			Number: "1", Offset: core.Origin,
			Size:  [2]core.Length{core.FromMM(1.2), core.FromMM(1.3)},
			Layer: core.LayerBottom, Net: &net5,
		}},
	})

	rep := Check(sch, b, DefaultOptions())
	for _, v := range rep.Violations {
		if v.Kind == KindMissingDecoupling {
			t.Fatalf("cap is 2.7 mm from U1.1: %s", v.Message)
		}
	}

	// And it still fires when the cap really is far from the pin.
	b.Footprints[fpID(b, "C5V")].Position = core.NewPoint(core.FromMM(24), core.FromMM(4))
	rep = Check(sch, b, DefaultOptions())
	found := false
	for _, v := range rep.Violations {
		if v.Kind == KindMissingDecoupling {
			found = true
		}
	}
	if !found {
		t.Fatal("expected missing_decoupling_cap with the cap 25 mm from the pin")
	}
}

func fpID(b *core.Board, ref string) string {
	for id, fp := range b.Footprints {
		if fp != nil && fp.Reference == ref {
			return id
		}
	}
	return ""
}
