package script

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/mentasystems/fragua/internal/core"
	"github.com/mentasystems/fragua/internal/drc"
	"github.com/mentasystems/fragua/internal/placer"
	"github.com/mentasystems/fragua/internal/router"
)

// compact searches a smaller outline that still places + routes + DRC-cleans
// (Rust compact process: binary scale then greedy per-axis shrink).
func cmdCompact(p *core.Project, args string) (string, error) {
	step := 1.0
	seed := uint64(42)
	placeIters := 3000
	allowFailed := 0
	routeSec := 20.0
	// Whole-search budget. compact runs up to 24 place+route+DRC probes, so
	// without a wall clock of its own a generous route_seconds turns a verb
	// into an afternoon. The search is anytime: it keeps the best size it
	// has proven when the budget runs out.
	maxSec := router.MaxBudgetSeconds
	aspectFree := false
	var minW, minH float64
	for _, t := range strings.Fields(args) {
		kv := strings.SplitN(t, "=", 2)
		if len(kv) != 2 {
			continue
		}
		v, _ := strconv.ParseFloat(kv[1], 64)
		switch kv[0] {
		case "step":
			step = v
		case "seed":
			seed = uint64(v)
		case "iters":
			placeIters = int(v)
		case "allow_failed":
			allowFailed = int(v)
		case "route_seconds":
			routeSec = router.ClampBudget(v)
		case "max_seconds":
			maxSec = router.ClampBudget(v)
		case "min_w":
			minW = v
		case "min_h":
			minH = v
		case "aspect":
			aspectFree = kv[1] == "free"
		}
	}

	p.RLock()
	src := p.Board()
	// The probe must route at the same per-net widths the final route
	// uses, or it sizes the board against thinner copper than it gets.
	sch := p.Schematic()
	if src.Outline == nil {
		p.RUnlock()
		return "", fmt.Errorf("compact: board has no outline")
	}
	w0 := src.Outline.Width().ToMM()
	h0 := src.Outline.Height().ToMM()
	p.RUnlock()

	if minW <= 0 {
		minW = w0 * 0.45
	}
	if minH <= 0 {
		minH = h0 * 0.45
	}

	deadline := time.Now().Add(time.Duration(maxSec * float64(time.Second)))
	spent := func() bool { return time.Now().After(deadline) }

	type cand struct{ w, h float64 }
	best := cand{w0, h0}
	bestBoard := src.Clone()
	// Prove the current size first (baseline).
	if !compactFeasible(bestBoard, sch, w0, h0, seed, placeIters, routeSec, allowFailed) {
		// Still try to shrink from a freshly cloned current board.
		bestBoard = src.Clone()
	}

	lo, hi := 0.50, 1.0
	for i := 0; i < 7 && !spent(); i++ {
		mid := (lo + hi) / 2
		w, h := w0*mid, h0*mid
		if w < minW {
			w = minW
		}
		if h < minH {
			h = minH
		}
		try := src.Clone()
		if compactFeasible(try, sch, w, h, seed+uint64(i), placeIters, routeSec, allowFailed) {
			hi = mid
			best = cand{w, h}
			bestBoard = try
		} else {
			lo = mid
		}
	}
	if aspectFree {
		for dim := 0; dim < 2 && !spent(); dim++ {
			for k := 0; k < 8 && !spent(); k++ {
				w, h := best.w, best.h
				if dim == 0 {
					w -= step
					if w < minW {
						break
					}
				} else {
					h -= step
					if h < minH {
						break
					}
				}
				try := src.Clone()
				if compactFeasible(try, sch, w, h, seed+30+uint64(dim*10+k), placeIters, routeSec, allowFailed) {
					best = cand{w, h}
					bestBoard = try
				} else {
					break
				}
			}
		}
	}

	p.MutateBoard(func(b *core.Board) {
		*b = *bestBoard
	})
	area0 := w0 * h0
	area1 := best.w * best.h
	pct := 0.0
	if area0 > 0 {
		pct = (1 - area1/area0) * 100
	}
	return fmt.Sprintf("compact %.1fx%.1f → %.1fx%.1f mm (%.0f%% area)", w0, h0, best.w, best.h, pct), nil
}

func compactFeasible(b *core.Board, sch *core.Schematic, w, h float64, seed uint64, iters int, routeSec float64, allowFailed int) bool {
	if b.Outline == nil {
		return false
	}
	ow, oh := b.Outline.Width().ToMM(), b.Outline.Height().ToMM()
	if ow < 1e-6 || oh < 1e-6 {
		return false
	}
	sx, sy := w/ow, h/oh
	r := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(w), core.FromMM(h)))
	b.Outline = &r
	for _, fp := range b.Footprints {
		if fp == nil {
			continue
		}
		nx := math.Max(1.2, math.Min(w-1.2, fp.Position.X.ToMM()*sx))
		ny := math.Max(1.2, math.Min(h-1.2, fp.Position.Y.ToMM()*sy))
		fp.Position = core.NewPoint(core.FromMM(nx), core.FromMM(ny))
	}
	opts := placer.DefaultOptions()
	opts.Seed = seed
	opts.Iterations = iters
	if _, err := placer.Place(b, nil, opts); err != nil {
		return false
	}
	b.ClearRoute()
	ropts := router.DefaultOptions()
	ropts.MaxSeconds = routeSec
	ropts.Schematic = sch
	rep := router.Route(b, ropts)
	if rep.Failed > allowFailed {
		return false
	}
	d := drc.Check(b, nil, drc.DefaultOptions())
	return d.Errors == 0
}
