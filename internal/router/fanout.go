package router

import (
	"math"

	"github.com/mentasystems/fragua/internal/core"
)

// planFanout lays radial escape stubs + vias off dense SMD packages so
// the grid can leave a 0.4 mm QFN. Vias are staggered on two rings when
// the pad pitch cannot fit a via+clearance on a single ring (otherwise
// the vias overlap and stamp the whole package as a wall).
func planFanout(board *core.Board, opts Options) int {
	if board.Outline == nil {
		return 0
	}
	pour := map[string]bool{}
	for _, p := range board.Pours {
		pour[p.Net] = true
	}

	viaDia := opts.ViaDiameterMM
	if viaDia < 0.40 {
		viaDia = 0.45
	}
	viaDrill := opts.ViaDrillMM
	if viaDrill < 0.18 {
		viaDrill = 0.20
	}
	stubW := opts.TraceWidthMM
	if stubW > 0.15 {
		stubW = 0.15
	}
	var placed []fanoutXY
	added := 0
	for _, fp := range footprintsStable(board) {
		pitch := minPadPitchMM(fp)
		// Fine-pitch only. A 0.7 mm USB-C row is not a grid — radial vias
		// walk along the pin row and short the connector. WLP-6 (6 pads,
		// 0.4 mm) needs the same dogbone treatment as a 0.4 mm QFN: via
		// drill 0.3 does not fit a 0.25 mm land, so escape is outward.
		if pitch >= 0.55 {
			continue
		}
		if len(fp.Pads) < 6 {
			continue
		}
		if len(fp.Pads) < 16 && pitch >= 0.45 {
			continue
		}
		added += assignEscapeSlots(board, fp, opts, viaDia, viaDrill, stubW, &placed)
	}
	return added
}

type fanoutXY struct{ x, y float64 }

func minPadPitchMM(fp *core.Footprint) float64 {
	if len(fp.Pads) < 2 {
		return 99
	}
	best := 99.0
	cs := make([]core.Point, len(fp.Pads))
	for i := range fp.Pads {
		cs[i] = core.PadWorldCenter(fp, &fp.Pads[i])
	}
	for i := 0; i < len(cs); i++ {
		for j := i + 1; j < len(cs); j++ {
			d := math.Hypot(cs[i].X.ToMM()-cs[j].X.ToMM(), cs[i].Y.ToMM()-cs[j].Y.ToMM())
			if d > 0.05 && d < best {
				best = d
			}
		}
	}
	return best
}

func fpPadBounds(fp *core.Footprint) (core.Rect, bool) {
	if fp == nil || len(fp.Pads) == 0 {
		return core.Rect{}, false
	}
	r := core.PadWorldAABB(fp, &fp.Pads[0])
	for i := 1; i < len(fp.Pads); i++ {
		r = r.Union(core.PadWorldAABB(fp, &fp.Pads[i]))
	}
	return r, true
}

func outlineContains(o *core.Rect, x, y, margin float64) bool {
	if o == nil {
		return true
	}
	return x >= o.Min.X.ToMM()+margin && y >= o.Min.Y.ToMM()+margin &&
		x <= o.Max.X.ToMM()-margin && y <= o.Max.Y.ToMM()-margin
}

func fanoutHitsPad(board *core.Board, x, y, r float64, skip *core.Footprint, skipPad int) bool {
	pt := core.NewPoint(core.FromMM(x), core.FromMM(y))
	disk := core.RectFromCenter(pt, core.FromMM(r*2), core.FromMM(r*2))
	for _, fp := range footprintsStable(board) {
		for i := range fp.Pads {
			if fp == skip && i == skipPad {
				continue
			}
			if disk.Intersects(core.PadWorldAABB(fp, &fp.Pads[i])) {
				return true
			}
		}
	}
	return false
}

// stripStrandedFanout removes via+stub islands that never joined a net's
// tree. Those are DRC NetSplit errors, not connections.
// dropDuplicateVias removes barrels that sit on exactly the same point as an
// earlier barrel of the same net. Two passes can each decide the same net
// needs to change layer at the same cell — the tree that lays it and the
// stitch or jumper that reaches it again — and the second drill is not a
// second connection, it is the same hole ordered twice. DRC saw it as it
// should (hole-to-hole -0.300 mm against itself), which left every board
// carrying a fab-blocking error that had to be deleted by hand.
//
// Only exact coincidence goes: two barrels a hair apart may well be a real
// pair, and judging that is viaSiteOK's job, not this one.
func dropDuplicateVias(board *core.Board) {
	if board == nil || len(board.Vias) < 2 {
		return
	}
	type key struct {
		x, y core.Length
		net  string
	}
	seen := make(map[key]bool, len(board.Vias))
	kept := board.Vias[:0]
	for _, v := range board.Vias {
		k := key{v.Position.X, v.Position.Y, v.Net}
		if seen[k] {
			continue
		}
		seen[k] = true
		kept = append(kept, v)
	}
	board.Vias = kept
}

func stripStrandedFanout(board *core.Board) {
	touch := func(a, b core.Point) bool {
		return hypotMM(a, b) < 0.35
	}
	viaHits := make([]int, len(board.Vias))
	traceHits := make([]int, len(board.Traces))
	for i, v := range board.Vias {
		for j, t := range board.Traces {
			if t.Net != v.Net {
				continue
			}
			if touch(t.Start, v.Position) || touch(t.End, v.Position) {
				viaHits[i]++
				traceHits[j]++
			}
		}
	}
	dropVia := make([]bool, len(board.Vias))
	dropTr := make([]bool, len(board.Traces))
	for i, v := range board.Vias {
		if viaHits[i] != 1 {
			continue
		}
		for j, t := range board.Traces {
			if t.Net != v.Net || dropTr[j] {
				continue
			}
			if !(touch(t.Start, v.Position) || touch(t.End, v.Position)) {
				continue
			}
			if isFanoutStub(board, t) && traceHits[j] == 1 {
				dropVia[i] = true
				dropTr[j] = true
			}
		}
	}
	keptV := board.Vias[:0]
	for i, v := range board.Vias {
		if !dropVia[i] {
			keptV = append(keptV, v)
		}
	}
	board.Vias = keptV
	keptT := board.Traces[:0]
	for i, t := range board.Traces {
		if !dropTr[i] {
			keptT = append(keptT, t)
		}
	}
	board.Traces = keptT
}

func fanoutHitsSite(sites []fanoutXY, x, y, minD float64) bool {
	min2 := minD * minD
	for _, s := range sites {
		dx, dy := s.x-x, s.y-y
		if dx*dx+dy*dy < min2 {
			return true
		}
	}
	return false
}
