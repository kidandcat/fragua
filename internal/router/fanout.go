package router

import (
	"math"

	"github.com/mentasystems/fragua/internal/core"
)

// planFanout lays short dogbone stubs + vias off dense SMD packages so
// the coarse grid can reach fine-pitch pads (commercial QFN/module escape).
// Conservative: ≥8 pads, skip pour nets, skip PTH, stay inside the outline.
func planFanout(board *core.Board, opts Options) int {
	if board.Outline == nil {
		return 0
	}
	pour := map[string]bool{}
	for _, p := range board.Pours {
		pour[p.Net] = true
	}
	added := 0
	for _, fp := range footprintsStable(board) {
		if len(fp.Pads) < 8 {
			continue
		}
		cx, cy := fp.Position.X.ToMM(), fp.Position.Y.ToMM()
		if bb, ok := fpPadBounds(fp); ok {
			cx = (bb.Min.X.ToMM() + bb.Max.X.ToMM()) / 2
			cy = (bb.Min.Y.ToMM() + bb.Max.Y.ToMM()) / 2
		}
		for i := range fp.Pads {
			pad := &fp.Pads[i]
			if pad.Net == nil || *pad.Net == "" || pour[*pad.Net] {
				continue
			}
			if pad.Drill != nil {
				continue
			}
			pc := core.PadWorldCenter(fp, pad)
			dx := pc.X.ToMM() - cx
			dy := pc.Y.ToMM() - cy
			d := math.Hypot(dx, dy)
			if d < 0.05 {
				continue
			}
			ux, uy := dx/d, dy/d
			w, h := core.PadWorldSize(fp, pad)
			reach := math.Max(w.ToMM(), h.ToMM())/2 + 0.45
			vx := pc.X.ToMM() + ux*reach
			vy := pc.Y.ToMM() + uy*reach
			if !outlineContains(board.Outline, vx, vy, 0.6) {
				continue
			}
			if fanoutHitsPad(board, vx, vy, 0.35, fp, i) {
				continue
			}
			net := *pad.Net
			board.Vias = append(board.Vias, core.Via{
				ID:       core.NewID(),
				Net:      net,
				Position: core.NewPoint(core.FromMM(vx), core.FromMM(vy)),
				Drill:    core.FromMM(opts.ViaDrillMM),
				Diameter: core.FromMM(opts.ViaDiameterMM),
			})
			board.Traces = append(board.Traces, core.Trace{
				ID:    core.NewID(),
				Layer: pad.Layer,
				Net:   net,
				Width: core.FromMM(opts.TraceWidthMM),
				Start: pc,
				End:   core.NewPoint(core.FromMM(vx), core.FromMM(vy)),
			})
			added++
		}
	}
	return added
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
