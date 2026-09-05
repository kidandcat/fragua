package script

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mentasystems/fragua/internal/core"
	"github.com/mentasystems/fragua/internal/drc"
	"github.com/mentasystems/fragua/internal/router"
)

// Fair MAX17220 island: WLP-6, dual Cout, R15 EN pull-up, R16 SEL.
// Must finish 6/6 with DRC 0 and no hand traces.
func TestFairMAX17220WLPBoostRoutes(t *testing.T) {
	p := core.NewProject("boost-wlp-fair")
	script := `
outline 18 14 radius=1
fab-rules jlcpcb
class ground pour=both
class power width=0.35
class switch width=0.35
lib-gen max17220_wlp6 family=wlp pins=6 pitch=0.4 body=0.89 body_len=1.42 pad=0.24
lib-gen l_2016 family=chip size=0805 kind=l
lib-gen c_0603 family=chip size=0603 kind=c
lib-gen r_0603 family=chip size=0603 kind=r
sym U3 ic key=max17220_wlp6
  pin A1 L OUT role=power_out
  pin A2 R BATT role=power_in
  pin B1 L GND role=power_in
  pin B2 R LX role=output
  pin C1 L EN role=passive
  pin C2 R SEL role=passive
sym L2 inductor key=l_2016
sym C8 capacitor key=c_0603
sym C9 capacitor key=c_0603
sym C11 capacitor key=c_0603
sym R15 resistor key=r_0603
sym R16 resistor key=r_0603
net GND U3.GND C8.2 C9.2 C11.2 R16.2 class=ground
net VSTOR U3.BATT L2.2 C8.1 R15.1 class=power
net +3V0 U3.OUT C9.1 C11.1 class=power
net LX U3.LX L2.1 class=switch
net SEL U3.SEL R16.1
net EN U3.EN R15.2
palette U3 max17220_wlp6
palette L2 l_2016
palette C8 c_0603
palette C9 c_0603
palette C11 c_0603
palette R15 r_0603
palette R16 r_0603
place U3 9 7
auto-place seed=42
`
	rs := RunScript(p, script)
	for _, r := range rs {
		if !r.OK {
			t.Fatalf("line %d %s: %s", r.Line, r.Tool, r.Result)
		}
	}
	var rep router.Report
	p.MutateBoard(func(b *core.Board) {
		opts := router.DefaultOptions()
		opts.MaxSeconds = 90
		opts.Schematic = p.Schematic()
		rep = router.Route(b, opts)
	})
	if !strings.Contains(rep.Summary(), "6/6") {
		var pos []string
		p.RLock()
		b := p.Board()
		for _, fp := range b.Footprints {
			pos = append(pos, fmt.Sprintf("%s@%.2f,%.2f r=%.0f", fp.Reference, fp.Position.X.ToMM(), fp.Position.Y.ToMM(), fp.Rotation))
		}
		p.RUnlock()
		var nets []string
		for _, n := range rep.PerNet {
			nets = append(nets, fmt.Sprintf("%s=%s/%s", n.Net, n.Outcome.Status, n.Outcome.Reason))
		}
		var extra []string
		u3 := b.FootprintByRef("U3")
		r16 := b.FootprintByRef("R16")
		l2 := b.FootprintByRef("L2")
		if u3 != nil {
			for i := range u3.Pads {
				if u3.Pads[i].Name == "SEL" || u3.Pads[i].Number == "C2" {
					c := core.PadWorldCenter(u3, &u3.Pads[i])
					extra = append(extra, fmt.Sprintf("U3.SEL=%.2f,%.2f", c.X.ToMM(), c.Y.ToMM()))
				}
			}
		}
		if r16 != nil {
			c := core.PadWorldCenter(r16, &r16.Pads[0])
			extra = append(extra, fmt.Sprintf("R16.1=%.2f,%.2f", c.X.ToMM(), c.Y.ToMM()))
			if cy, ok := core.CourtyardWorld(r16); ok {
				extra = append(extra, fmt.Sprintf("R16.cy=%.2f..%.2f,%.2f..%.2f", cy.Min.X.ToMM(), cy.Max.X.ToMM(), cy.Min.Y.ToMM(), cy.Max.Y.ToMM()))
			}
		}
		if l2 != nil {
			if cy, ok := core.CourtyardWorld(l2); ok {
				extra = append(extra, fmt.Sprintf("L2.cy=%.2f..%.2f,%.2f..%.2f", cy.Min.X.ToMM(), cy.Max.X.ToMM(), cy.Min.Y.ToMM(), cy.Max.Y.ToMM()))
			}
		}
		for _, v := range b.Vias {
			if v.Net == "SEL" {
				extra = append(extra, fmt.Sprintf("viaSEL=%.2f,%.2f", v.Position.X.ToMM(), v.Position.Y.ToMM()))
			}
		}
		for _, tr := range b.Traces {
			if tr.Net == "SEL" {
				extra = append(extra, fmt.Sprintf("trSEL=%.2f,%.2f→%.2f,%.2f w=%.2f", tr.Start.X.ToMM(), tr.Start.Y.ToMM(), tr.End.X.ToMM(), tr.End.Y.ToMM(), tr.Width.ToMM()))
			}
		}
		t.Fatalf("route did not finish 6/6: %s\n%s\nplace %s\n%s", rep.Summary(), strings.Join(nets, " "), strings.Join(pos, " "), strings.Join(extra, " "))
	}
	rs = RunScript(p, "auto-pour\nstitch\n")
	for _, r := range rs {
		if !r.OK {
			t.Fatalf("line %d %s: %s", r.Line, r.Tool, r.Result)
		}
	}
	p.RLock()
	b := p.Board()
	l2 := b.FootprintByRef("L2")
	r16 := b.FootprintByRef("R16")
	if l2 == nil || r16 == nil {
		p.RUnlock()
		t.Fatal("missing L2 or R16")
	}
	if r16CY, ok := core.CourtyardWorld(r16); ok {
		if l2CY, ok := core.CourtyardWorld(l2); ok && r16CY.Intersects(l2CY) {
			p.RUnlock()
			t.Fatalf("R16 courtyard intersects L2")
		}
	}
	drep := drc.Check(b, p.Schematic(), drc.DefaultOptions())
	p.RUnlock()
	if drep.Errors != 0 {
		t.Fatalf("drc: %s", drep.Detail())
	}
}
