package host

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"

	"github.com/mentasystems/fragua/internal/core"
	"github.com/mentasystems/fragua/internal/drc"
	"github.com/mentasystems/fragua/internal/erc"
	"github.com/mentasystems/fragua/internal/render"
)

// Violation is one DRC/ERC finding as the UI consumes it: the id is what the
// canvas marker carries, so clicking a row can point at the right marker.
type Violation struct {
	ID       string   `json:"id"`
	Source   string   `json:"source"` // drc | erc
	Kind     string   `json:"kind"`
	Severity string   `json:"severity"`
	Message  string   `json:"message"`
	Net      string   `json:"net,omitempty"`
	Symbol   string   `json:"symbol,omitempty"`
	XMM      *float64 `json:"x_mm,omitempty"`
	YMM      *float64 `json:"y_mm,omitempty"`
}

// CheckReport is a DRC or ERC run, JSON-shaped for the UI panel.
type CheckReport struct {
	Source     string      `json:"source"`
	Errors     int         `json:"errors"`
	Warnings   int         `json:"warnings"`
	Summary    string      `json:"summary"`
	Violations []Violation `json:"violations"`
}

// Summary is the status bar: everything it shows in one cheap read. DRC
// counts are not here on purpose — they cost a full check, so the UI asks
// for them when the human does.
type Summary struct {
	Name       string   `json:"name"`
	Path       string   `json:"path"`
	Version    string   `json:"version"`
	WidthMM    float64  `json:"width_mm"`
	HeightMM   float64  `json:"height_mm"`
	Layers     []string `json:"layers"`
	Parts      int      `json:"parts"`
	Symbols    int      `json:"symbols"`
	Traces     int      `json:"traces"`
	Vias       int      `json:"vias"`
	Pours      int      `json:"pours"`
	Nets       int      `json:"nets"`
	NetsRouted int      `json:"nets_routed"`
	Unrouted   int      `json:"unrouted"`
	Op         *core.Op `json:"op,omitempty"`
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func runDRC(p *core.Project) CheckReport {
	p.RLock()
	rep := drc.Check(p.Board(), p.Schematic(), drc.DefaultOptions())
	p.RUnlock()
	out := CheckReport{Source: "drc", Errors: rep.Errors, Warnings: rep.Warnings, Summary: rep.Summary()}
	out.Violations = make([]Violation, 0, len(rep.Violations))
	for i, v := range rep.Violations {
		item := Violation{
			ID: idFor("drc", i), Source: "drc", Kind: string(v.Kind),
			Severity: string(v.Severity), Message: v.Message, Net: v.Net,
		}
		if v.XMM != 0 || v.YMM != 0 {
			x, y := v.XMM, v.YMM
			item.XMM, item.YMM = &x, &y
		}
		out.Violations = append(out.Violations, item)
	}
	sortViolations(out.Violations)
	return out
}

func runERC(p *core.Project) CheckReport {
	p.RLock()
	rep := erc.Check(p.Schematic(), p.Board(), erc.DefaultOptions())
	p.RUnlock()
	out := CheckReport{Source: "erc", Errors: rep.Errors, Warnings: rep.Warnings, Summary: rep.Summary()}
	out.Violations = make([]Violation, 0, len(rep.Violations))
	for i, v := range rep.Violations {
		out.Violations = append(out.Violations, Violation{
			ID: idFor("erc", i), Source: "erc", Kind: string(v.Kind),
			Severity: string(v.Severity), Message: v.Message, Net: v.Net, Symbol: v.Symbol,
		})
	}
	sortViolations(out.Violations)
	return out
}

func idFor(source string, i int) string {
	return source + "-" + strconv.Itoa(i)
}

// sortViolations puts errors first: the cap in any list must never hide the
// one error behind a page of warnings.
func sortViolations(vs []Violation) {
	sort.SliceStable(vs, func(i, j int) bool {
		return vs[i].Severity == "error" && vs[j].Severity != "error"
	})
}

func markersFor(rep CheckReport) []render.Marker {
	out := make([]render.Marker, 0, len(rep.Violations))
	for _, v := range rep.Violations {
		if v.XMM == nil || v.YMM == nil {
			continue
		}
		out = append(out, render.Marker{
			ID: v.ID, Severity: v.Severity, Kind: v.Kind,
			Message: v.Message, Net: v.Net, XMM: *v.XMM, YMM: *v.YMM,
		})
	}
	return out
}

func summarize(p *core.Project) Summary {
	// Name/SavePath take the read lock themselves; take theirs first so this
	// never recurses into RLock while a writer is queued.
	s := Summary{Name: p.Name(), Path: p.SavePath(), Version: core.Version}
	p.RLock()
	defer p.RUnlock()
	b := p.Board()
	if b.Outline != nil {
		s.WidthMM = b.Outline.Width().ToMM()
		s.HeightMM = b.Outline.Height().ToMM()
	}
	stack := b.StackupOrDefault()
	for i := 0; i < stack.CopperCount(); i++ {
		s.Layers = append(s.Layers, stack.LayerName(i))
	}
	s.Parts = len(b.Footprints)
	s.Traces = len(b.Traces)
	s.Vias = len(b.Vias)
	s.Pours = len(b.Pours)
	if sch := p.Schematic(); sch != nil {
		s.Symbols = len(sch.Symbols)
	}
	// A net is "routed" when it has copper or a pour to live in. The
	// ratsnest is exactly the complement, so the two always agree.
	routed := map[string]bool{}
	for _, t := range b.Traces {
		routed[t.Net] = true
	}
	for _, pr := range b.Pours {
		routed[pr.Net] = true
	}
	pads := map[string]int{}
	for _, fp := range b.Footprints {
		if fp == nil {
			continue
		}
		for i := range fp.Pads {
			if n := fp.Pads[i].Net; n != nil && *n != "" {
				pads[*n]++
			}
		}
	}
	for name, count := range pads {
		if count < 2 {
			continue
		}
		s.Nets++
		if routed[name] {
			s.NetsRouted++
		}
	}
	s.Unrouted = s.Nets - s.NetsRouted
	if op, ok := p.Ops().Current(); ok {
		s.Op = &op
	}
	return s
}
