// board-regen: keep placement, clear copper, Go-route, dump metrics + JLCPCB pack.
// Does not overwrite the source project.
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
	"github.com/mentasystems/fragua/internal/render"
	"github.com/mentasystems/fragua/internal/router"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: board-regen <project.fragua|json> <out-dir> [max_seconds] [4layer]")
		os.Exit(2)
	}
	src, outDir := os.Args[1], os.Args[2]
	maxSec := 180.0
	fourLayer := false
	for _, a := range os.Args[3:] {
		if a == "4" || a == "4l" || a == "4layer" {
			fourLayer = true
			continue
		}
		fmt.Sscanf(a, "%f", &maxSec)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fatal(err)
	}
	p, err := core.LoadFromPath(src)
	if err != nil {
		fatal(err)
	}
	writeJSON(filepath.Join(outDir, "00-load-metrics.json"), metrics(p, "load"))
	_ = os.WriteFile(filepath.Join(outDir, "00-load.svg"), []byte(render.BoardSVG(p.Board())), 0o644)

	dumpNets(p)
	if fourLayer {
		fmt.Println("== 4-layer stackup (F / GND / +3V3 / B) ==")
		p.MutateBoard(func(b *core.Board) {
			b.Apply4Layer()
			// Via-in-pad is an explicit exception, not the default:
			// a 0.45 mm via on a 0.2 mm QFN pad is ~14 µm under
			// JLCPCB 4L clearance. Far-ring leftovers cover GPIO8.
		})
	}
	fmt.Println("== keep place, Go route ==")
	p.MutateBoard(func(b *core.Board) { b.ClearRoute() })
	opts := router.DefaultOptions()
	opts.MaxSeconds = maxSec
	opts.Negotiate = true
	t0 := time.Now()
	var rep router.Report
	p.MutateBoard(func(b *core.Board) { rep = router.Route(b, opts) })
	fmt.Printf("%s in %s\n", rep.Summary(), time.Since(t0).Round(time.Millisecond))

	_ = p.SaveToPath(filepath.Join(outDir, "go-routed.json"))
	_ = os.WriteFile(filepath.Join(outDir, "go-routed.svg"), []byte(render.BoardSVG(p.Board())), 0o644)
	m := metrics(p, "go-routed")
	okN := 0
	per := map[string]string{}
	for _, n := range rep.PerNet {
		st := n.Outcome.Status
		if n.Outcome.Reason != "" {
			st = st + "/" + n.Outcome.Reason
		}
		per[n.Net] = st
		fmt.Printf("  %-12s %s segs=%d len=%.1f\n", n.Net, st, n.Outcome.TraceSegments, n.Outcome.LengthMM)
		if n.Outcome.Status == "ok" {
			okN++
		}
	}
	m["route_failed"] = rep.Failed
	m["route_ok"] = okN
	m["route_length_mm"] = rep.TotalLengthMM
	m["route_summary"] = rep.Summary()
	m["route_per_net"] = per
	writeJSON(filepath.Join(outDir, "metrics.json"), m)

	pack, err := fab.Pack(p, "jlcpcb", filepath.Join(outDir, "fab"))
	if err != nil {
		fmt.Println("pack:", err)
		os.Exit(1)
	}
	fmt.Println("pack", pack.ZipPath, "drc_err", pack.DRCErrors, "erc_err", pack.ERCErrors)
	if pack.DRCErrors > 0 || pack.ERCErrors > 0 || rep.Failed > 0 {
		os.Exit(1)
	}
}

func metrics(p *core.Project, label string) map[string]any {
	p.RLock()
	defer p.RUnlock()
	b, sch := p.Board(), p.Schematic()
	drcOpts := drc.DefaultOptions()
	if fab := core.ActiveFabRules(b); fab.MinClearanceMM > 0 {
		drcOpts.MinClearance = core.FromMM(fab.MinClearanceMM)
		if fab.MinTraceWidthMM > 0 {
			drcOpts.MinTraceWidth = core.FromMM(fab.MinTraceWidthMM)
		}
		if fab.MinViaDrillMM > 0 {
			drcOpts.MinDrill = core.FromMM(fab.MinViaDrillMM)
		}
		if fab.MinAnnularRingMM > 0 {
			drcOpts.MinAnnularRing = core.FromMM(fab.MinAnnularRingMM)
		}
		if fab.MinEdgeClearanceMM > 0 {
			drcOpts.EdgeClearance = core.FromMM(fab.MinEdgeClearanceMM)
		}
	}
	d := drc.Check(b, sch, drcOpts)
	e := erc.Check(sch, b, erc.DefaultOptions())
	byD, byE := map[string]int{}, map[string]int{}
	for _, v := range d.Violations {
		byD[string(v.Kind)]++
	}
	for _, v := range e.Violations {
		byE[string(v.Kind)]++
	}
	ow, oh := 0.0, 0.0
	if b.Outline != nil {
		ow, oh = b.Outline.Width().ToMM(), b.Outline.Height().ToMM()
	}
	return map[string]any{
		"label": label, "outline_mm": [2]float64{ow, oh},
		"footprints": len(b.Footprints), "traces": len(b.Traces), "vias": len(b.Vias), "pours": len(b.Pours),
		"drc_errors": d.Errors, "drc_warnings": d.Warnings, "drc_by_kind": byD, "drc_summary": d.Summary(),
		"erc_errors": e.Errors, "erc_warnings": e.Warnings, "erc_by_kind": byE, "erc_summary": e.Summary(),
	}
}

func dumpNets(p *core.Project) {
	p.RLock()
	defer p.RUnlock()
	b := p.Board()
	type pad struct {
		ref, num, net string
		x, y          float64
		layer         uint8
		pth           bool
	}
	by := map[string][]pad{}
	for _, fp := range b.Footprints {
		if fp == nil {
			continue
		}
		for i := range fp.Pads {
			pd := &fp.Pads[i]
			if pd.Net == nil || *pd.Net == "" {
				continue
			}
			c := core.PadWorldCenter(fp, pd)
			by[*pd.Net] = append(by[*pd.Net], pad{
				ref: fp.Reference, num: pd.Number, net: *pd.Net,
				x: c.X.ToMM(), y: c.Y.ToMM(), layer: pd.Layer.Index,
				pth: pd.Drill != nil && *pd.Drill > 0,
			})
		}
	}
	names := make([]string, 0, len(by))
	for n := range by {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Println("== nets ==")
	for _, n := range names {
		pads := by[n]
		fmt.Printf("  %s (%d)\n", n, len(pads))
		for _, pd := range pads {
			kind := "smd"
			if pd.pth {
				kind = "pth"
			}
			fmt.Printf("    %s.%s %s (%.2f,%.2f) L%d\n", pd.ref, pd.num, kind, pd.x, pd.y, pd.layer)
		}
	}
}

func writeJSON(path string, v any) {
	raw, _ := json.MarshalIndent(v, "", "  ")
	_ = os.WriteFile(path, raw, 0o644)
	fmt.Println("wrote", path)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
