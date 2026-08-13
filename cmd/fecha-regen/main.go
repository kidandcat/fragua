// One-shot: regenerate fecha-gateway with the Go engine and write
// compare artifacts. Never overwrites the shipped JSON / fab pack.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/mentasystems/fragua/internal/core"
	"github.com/mentasystems/fragua/internal/drc"
	"github.com/mentasystems/fragua/internal/erc"
	"github.com/mentasystems/fragua/internal/fab"
	"github.com/mentasystems/fragua/internal/placer"
	"github.com/mentasystems/fragua/internal/render"
	"github.com/mentasystems/fragua/internal/router"
)

func main() {
	src := "/Users/jairo/fecha/firmware/sf7/yellow/fecha-gateway-v2/fecha-gateway-v2.json"
	outDir := "/Users/jairo/fecha/firmware/sf7/yellow/fecha-gateway-v2/go-regen"
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fatal(err)
	}

	p, err := core.LoadFromPath(src)
	if err != nil {
		fatal(err)
	}

	// Snapshot shipped metrics (do not mutate the loaded board yet).
	writeReport(filepath.Join(outDir, "00-shipped-metrics.json"), metrics(p, "rust-shipped"))
	os.WriteFile(filepath.Join(outDir, "00-shipped.svg"), []byte(render.BoardSVG(p.Board())), 0o644)

	// A: keep product placement, Go route only.
	runKeepPlace(p, outDir)
	if os.Getenv("FECHA_ONLY_A") != "" {
		return
	}

	// Reload for a clean B: pin modules, auto-place passives, route.
	p2, err := core.LoadFromPath(src)
	if err != nil {
		fatal(err)
	}
	runRePlace(p2, outDir)
}

func runKeepPlace(p *core.Project, outDir string) {
	fmt.Println("== A: keep place, Go route ==")
	p.MutateBoard(func(b *core.Board) { b.ClearRoute() })
	opts := router.DefaultOptions()
	opts.MaxSeconds = 180
	t0 := time.Now()
	var rep router.Report
	p.MutateBoard(func(b *core.Board) { rep = router.Route(b, opts) })
	fmt.Printf("route %s in %s\n", rep.Summary(), time.Since(t0).Round(time.Millisecond))

	saveVariant(p, outDir, "A-keep-place", &rep)
	if os.Getenv("FECHA_ONLY_A") != "" {
		return
	}
}

func runRePlace(p *core.Project, outDir string) {
	fmt.Println("== B: pin modules, auto-place passives, Go route ==")
	// Mechanical / RF / connectors stay put. Passives + discretes move.
	movable := []string{"C1", "C2", "C3", "C4", "R1", "R2", "R3", "R4", "Q1", "Q2"}
	p.MutateBoard(func(b *core.Board) {
		b.ClearRoute()
		opts := placer.DefaultOptions()
		opts.Seed = 42
		opts.Iterations = 8000
		rep, err := placer.Place(b, movable, opts)
		if err != nil {
			fmt.Println("place:", err)
			return
		}
		fmt.Println(rep.Summary())
	})
	opts := router.DefaultOptions()
	opts.MaxSeconds = 90
	t0 := time.Now()
	var rep router.Report
	p.MutateBoard(func(b *core.Board) { rep = router.Route(b, opts) })
	fmt.Printf("route %s in %s\n", rep.Summary(), time.Since(t0).Round(time.Millisecond))
	saveVariant(p, outDir, "B-replace-passives", &rep)
}

func saveVariant(p *core.Project, outDir, name string, route *router.Report) {
	base := filepath.Join(outDir, name)
	_ = os.MkdirAll(base, 0o755)
	if err := p.SaveToPath(filepath.Join(base, name+".json")); err != nil {
		fmt.Println("save:", err)
	}
	os.WriteFile(filepath.Join(base, name+".svg"), []byte(render.BoardSVG(p.Board())), 0o644)
	m := metrics(p, name)
	if route != nil {
		okN := 0
		for _, n := range route.PerNet {
			if n.Outcome.Status == "ok" {
				okN++
			}
		}
		m["route_failed"] = route.Failed
		m["route_ok"] = okN
		m["route_length_mm"] = route.TotalLengthMM
		m["route_summary"] = route.Summary()
		per := map[string]string{}
		for _, n := range route.PerNet {
			per[n.Net] = n.Outcome.Status
		}
		m["route_per_net"] = per
	}
	writeReport(filepath.Join(base, "metrics.json"), m)
	pack, err := fab.Pack(p, "jlcpcb", filepath.Join(base, "fab"))
	if err != nil {
		fmt.Println("pack:", err)
	} else {
		fmt.Println("pack", pack.ZipPath, "drc_err", pack.DRCErrors, "erc_err", pack.ERCErrors)
	}
}

func metrics(p *core.Project, label string) map[string]any {
	p.RLock()
	defer p.RUnlock()
	b := p.Board()
	sch := p.Schematic()
	d := drc.Check(b, sch, drc.DefaultOptions())
	e := erc.Check(sch, b, erc.DefaultOptions())
	byD := map[string]int{}
	for _, v := range d.Violations {
		byD[string(v.Kind)]++
	}
	byE := map[string]int{}
	for _, v := range e.Violations {
		byE[string(v.Kind)]++
	}
	pos := map[string][3]float64{}
	for _, fp := range b.Footprints {
		if fp == nil {
			continue
		}
		pos[fp.Reference] = [3]float64{fp.Position.X.ToMM(), fp.Position.Y.ToMM(), fp.Rotation}
	}
	ow, oh := 0.0, 0.0
	if b.Outline != nil {
		ow, oh = b.Outline.Width().ToMM(), b.Outline.Height().ToMM()
	}
	return map[string]any{
		"label":        label,
		"outline_mm":   [2]float64{ow, oh},
		"footprints":   len(b.Footprints),
		"traces":       len(b.Traces),
		"vias":         len(b.Vias),
		"pours":        len(b.Pours),
		"drc_errors":   d.Errors,
		"drc_warnings": d.Warnings,
		"drc_by_kind":  byD,
		"erc_errors":   e.Errors,
		"erc_warnings": e.Warnings,
		"erc_by_kind":  byE,
		"positions":    pos,
		"drc_summary":  d.Summary(),
		"erc_summary":  e.Summary(),
	}
}

func writeReport(path string, m map[string]any) {
	// stable JSON
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	raw, _ := json.MarshalIndent(m, "", "  ")
	_ = os.WriteFile(path, raw, 0o644)
	fmt.Println("wrote", path)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
