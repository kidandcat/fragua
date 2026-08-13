package router

import (
	"math"

	"github.com/mentasystems/fragua/internal/core"
)

// StitchIsolatedPads is the script-facing pour stitch.
func StitchIsolatedPads(board *core.Board, opts Options) int {
	return stitchIsolatedPads(board, opts)
}

func stitchIsolatedPads(board *core.Board, opts Options) int {
	// net → layers that already have a pour. A pad sitting in a same-layer
	// pour is already connected; we only via-stitch to a pour on another layer.
	pourLayers := map[string][]uint8{}
	for _, p := range board.Pours {
		if p.Net != "" {
			pourLayers[p.Net] = append(pourLayers[p.Net], p.Layer.Index)
		}
	}
	if len(pourLayers) == 0 {
		return 0
	}
	added := 0
	for _, fp := range footprintsStable(board) {
		for i := range fp.Pads {
			pad := &fp.Pads[i]
			if pad.Net == nil {
				continue
			}
			layers, ok := pourLayers[*pad.Net]
			if !ok {
				continue
			}
			sameLayer := false
			for _, L := range layers {
				if L == pad.Layer.Index {
					sameLayer = true
					break
				}
			}
			if sameLayer {
				continue
			}
			c := core.PadWorldCenter(fp, pad)
			if hasNearbyCopper(board, *pad.Net, c, 0.45) {
				continue
			}
			vx, vy := c.X.ToMM(), c.Y.ToMM()
			if pad.Drill == nil {
				// Off-pad toward board centre so the via is not under the body.
				bx, by := 0.0, 0.0
				if board.Outline != nil {
					bx = (board.Outline.Min.X.ToMM() + board.Outline.Max.X.ToMM()) / 2
					by = (board.Outline.Min.Y.ToMM() + board.Outline.Max.Y.ToMM()) / 2
				}
				dx, dy := bx-vx, by-vy
				d := math.Hypot(dx, dy)
				if d < 1e-6 {
					dx, dy, d = 1, 0, 1
				}
				w, h := core.PadWorldSize(fp, pad)
				reach := math.Max(w.ToMM(), h.ToMM())/2 + 0.40
				vx += (dx / d) * reach
				vy += (dy / d) * reach
			}
			if !outlineContains(board.Outline, vx, vy, 0.4) {
				continue
			}
			net := *pad.Net
			vp := core.NewPoint(core.FromMM(vx), core.FromMM(vy))
			board.Vias = append(board.Vias, core.Via{
				ID: core.NewID(), Net: net, Position: vp,
				Drill: core.FromMM(opts.ViaDrillMM), Diameter: core.FromMM(opts.ViaDiameterMM),
			})
			if pad.Drill == nil {
				board.Traces = append(board.Traces, core.Trace{
					ID: core.NewID(), Layer: pad.Layer, Net: net,
					Width: core.FromMM(opts.TraceWidthMM), Start: c, End: vp,
				})
			}
			added++
		}
	}
	return added
}

func hasNearbyCopper(board *core.Board, net string, p core.Point, rMM float64) bool {
	r2 := rMM * rMM
	near := func(q core.Point) bool {
		dx := q.X.ToMM() - p.X.ToMM()
		dy := q.Y.ToMM() - p.Y.ToMM()
		return dx*dx+dy*dy <= r2
	}
	for _, v := range board.Vias {
		if v.Net == net && near(v.Position) {
			return true
		}
	}
	for _, t := range board.Traces {
		if t.Net == net && (near(t.Start) || near(t.End)) {
			return true
		}
	}
	return false
}
