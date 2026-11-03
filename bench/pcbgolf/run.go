// Command pcbgolf measures one placement and two route passes of the
// comma.ai PCBGolf netlist.
//
//	go run ./bench/pcbgolf [-netlist path] [-outline 140x110] [-iters 8000] [-route 60]
//
// The netlist is not in this repo: https://github.com/commaai/PCBGolf has
// no license, so a generated board is not redistributable. See README.md
// next to this file for how to rebuild netlist.fs.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mentasystems/fragua/internal/core"
	"github.com/mentasystems/fragua/internal/drc"
	"github.com/mentasystems/fragua/internal/script"
)

func main() {
	netPath := flag.String("netlist", "", "path to netlist.fs (default: bench/pcbgolf/netlist.fs or PCBGOLF_NETLIST)")
	outline := flag.String("outline", "140x110", "WxH millimetres")
	iters := flag.Int("iters", 8000, "auto-place iterations")
	routeSec := flag.Float64("route", 60, "seconds for each route pass (0 skips routing)")
	seed := flag.Uint64("seed", 42, "auto-place seed")
	flag.Parse()

	path := *netPath
	if path == "" {
		path = os.Getenv("PCBGOLF_NETLIST")
	}
	if path == "" {
		path = "bench/pcbgolf/netlist.fs"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pcbgolf: cannot read %s: %v\n\n%s\n", path, err, regenHelp)
		os.Exit(2)
	}

	w, h, err := parseOutline(*outline)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	p := core.NewProject("pcbgolf")
	head := fmt.Sprintf("outline %g %g\nfab-rules jlcpcb\n", w, h)
	t0 := time.Now()
	if err := runAll(p, head+string(raw)); err != nil {
		fmt.Fprintf(os.Stderr, "pcbgolf: netlist: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("load %s\n", time.Since(t0).Round(time.Millisecond))

	place := fmt.Sprintf("auto-place seed=%d iters=%d", *seed, *iters)
	t1 := time.Now()
	rs := script.RunScript(p, place)
	placeWall := time.Since(t1)
	if len(rs) == 0 || !rs[len(rs)-1].OK {
		msg := "no result"
		if len(rs) > 0 {
			msg = rs[len(rs)-1].Result
		}
		fmt.Printf("place FAIL %s (%s)\n", msg, placeWall.Round(time.Millisecond))
	} else {
		fmt.Printf("place %s (%s)\n", rs[len(rs)-1].Result, placeWall.Round(time.Millisecond))
	}
	printBoard("after-place", p)

	if *routeSec <= 0 {
		return
	}
	for pass := 1; pass <= 2; pass++ {
		line := fmt.Sprintf("route max_seconds=%g", *routeSec)
		t := time.Now()
		rs = script.RunScript(p, line)
		wall := time.Since(t)
		if len(rs) == 0 || !rs[len(rs)-1].OK {
			msg := "no result"
			if len(rs) > 0 {
				msg = rs[len(rs)-1].Result
			}
			fmt.Printf("route-pass%d FAIL %s (%s)\n", pass, msg, wall.Round(time.Millisecond))
		} else {
			fmt.Printf("route-pass%d %s (wall %s)\n", pass, rs[len(rs)-1].Result, wall.Round(time.Millisecond))
		}
		printBoard(fmt.Sprintf("after-route-%d", pass), p)
	}
}

func runAll(p *core.Project, src string) error {
	rs := script.RunScript(p, src)
	for _, r := range rs {
		if !r.OK {
			return fmt.Errorf("line %d %s: %s", r.Line, r.Tool, r.Result)
		}
	}
	return nil
}

func printBoard(tag string, p *core.Project) {
	p.RLock()
	defer p.RUnlock()
	b := p.Board()
	rep := drc.Check(b, p.Schematic(), drc.DefaultOptions())
	// fab-rules are on the board; the script `drc` verb grows to honour
	// them, but this row stays on DefaultOptions so a before/after run
	// that predates that change still counts the same violations.
	// OptionsForBoard is applied in the drc_fab line below when it exists.
	overlap, off, unconn := 0, 0, 0
	kinds := map[string]int{}
	for _, v := range rep.Violations {
		if v.Kind == drc.KindUnconnectedPad {
			unconn++
		}
		if v.Severity != drc.SeverityError {
			continue
		}
		kinds[string(v.Kind)]++
		switch v.Kind {
		case drc.KindCourtyardOverlap:
			overlap++
		case drc.KindBodyOffBoard:
			off++
		}
	}
	pads := 0
	for _, fp := range b.Footprints {
		if fp == nil {
			continue
		}
		for i := range fp.Pads {
			if fp.Pads[i].Net != nil && *fp.Pads[i].Net != "" {
				pads++
			}
		}
	}
	fmt.Printf("%s outline=%.0fx%.0f parts=%d vias=%d drc_err=%d courtyard=%d off_board=%d unconnected_pads=%d/%d\n",
		tag, b.Outline.Width().ToMM(), b.Outline.Height().ToMM(),
		len(b.Footprints), len(b.Vias), rep.Errors, overlap, off, unconn, pads)
	for k, n := range kinds {
		fmt.Printf("  drc %s %d\n", k, n)
	}
}

func parseOutline(s string) (float64, float64, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.NewReplacer("×", "x", "*", "x", " ", "x").Replace(s)
	var w, h float64
	if _, err := fmt.Sscanf(s, "%gx%g", &w, &h); err != nil || w <= 0 || h <= 0 {
		return 0, 0, fmt.Errorf("outline %q: want WxH, e.g. 140x110", s)
	}
	return w, h, nil
}

const regenHelp = `comma.ai/PCBGolf publishes no license, so this repo does not carry the board.
Rebuild the netlist locally:

  git clone --depth 1 https://github.com/commaai/PCBGolf.git /tmp/PCBGolf
  python3 bench/pcbgolf/kicad2fragua.py /tmp/PCBGolf/pcbgolf.kicad_pcb > /tmp/netlist.fs
  go run ./bench/pcbgolf -netlist /tmp/netlist.fs -outline 140x110 -iters 8000 -route 60`
