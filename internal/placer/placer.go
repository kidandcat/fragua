// Package placer implements global electrostatic placement + SA legalisation.
package placer

import (
	"fmt"
	"math"
	"math/rand"
	"strings"

	"github.com/mentasystems/fragua/internal/core"
)

// Options configures a place run.
type Options struct {
	Seed         uint64
	SolderGapMM  float64
	Iterations   int
	MoveStdMM    float64
}

// DefaultOptions returns SA defaults.
func DefaultOptions() Options {
	return Options{
		Seed:        42,
		SolderGapMM: core.MinFootprintGapMM,
		Iterations:  4000,
		MoveStdMM:   2.0,
	}
}

// ParseOptions overlays script args.
func ParseOptions(o Options, args string) Options {
	for _, f := range strings.Fields(args) {
		kv := strings.SplitN(f, "=", 2)
		if len(kv) != 2 {
			continue
		}
		var x float64
		fmt.Sscanf(kv[1], "%f", &x)
		switch kv[0] {
		case "seed":
			o.Seed = uint64(x)
		case "iterations":
			o.Iterations = int(x)
		case "solder_gap":
			o.SolderGapMM = x
		}
	}
	return o
}

// Report summarises placement.
type Report struct {
	InitialHPWLMM float64  `json:"initial_hpwl_mm"`
	FinalHPWLMM   float64  `json:"final_hpwl_mm"`
	Moved         []string `json:"moved"`
	Iterations    int      `json:"iterations"`
}

// Summary is agent-friendly.
func (r Report) Summary() string {
	return fmt.Sprintf("place: HPWL %.2f → %.2f mm, moved %d parts, %d iters",
		r.InitialHPWLMM, r.FinalHPWLMM, len(r.Moved), r.Iterations)
}

// Place runs ePlace-style gradient steps then SA legalisation on refs
// (nil refs = all non-edge-mounted footprints).
func Place(board *core.Board, refs []string, opts Options) (Report, error) {
	if board.Outline == nil {
		return Report{}, fmt.Errorf("place: board has no outline")
	}
	rng := rand.New(rand.NewSource(int64(opts.Seed))) //nolint:gosec

	// Select movable footprints
	var fps []*core.Footprint
	if len(refs) == 0 {
		for _, id := range board.FootprintOrder {
			fp := board.Footprints[id]
			if fp != nil && !fp.EdgeMounted {
				fps = append(fps, fp)
			}
		}
		if len(fps) == 0 {
			for _, fp := range board.Footprints {
				if fp != nil && !fp.EdgeMounted {
					fps = append(fps, fp)
				}
			}
		}
	} else {
		for _, r := range refs {
			if fp := board.FootprintByRef(r); fp != nil {
				fps = append(fps, fp)
			}
		}
	}
	if len(fps) == 0 {
		return Report{}, fmt.Errorf("place: no movable footprints")
	}

	initHPWL := hpwl(board)
	bestHPWL := initHPWL
	// snapshot best positions
	type pos struct{ x, y core.Length; rot float64 }
	best := map[string]pos{}
	for _, fp := range fps {
		best[fp.Reference] = pos{fp.Position.X, fp.Position.Y, fp.Rotation}
	}

	// Global stage: pull same-net pads together (simple force)
	for iter := 0; iter < 80; iter++ {
		forces := map[string][2]float64{}
		// net centers
		netPads := map[string][][3]float64{} // ref, x,y
		for _, fp := range board.Footprints {
			if fp == nil {
				continue
			}
			for i := range fp.Pads {
				pad := &fp.Pads[i]
				if pad.Net == nil || *pad.Net == "" {
					continue
				}
				c := core.PadWorldCenter(fp, pad)
				netPads[*pad.Net] = append(netPads[*pad.Net], [3]float64{
					0, c.X.ToMM(), c.Y.ToMM(),
				})
				_ = fp.Reference
			}
		}
		// rebuild with ref
		netPads = map[string][][3]float64{}
		refOf := map[*core.Footprint]string{}
		for _, fp := range fps {
			refOf[fp] = fp.Reference
		}
		for _, fp := range board.Footprints {
			if fp == nil {
				continue
			}
			for i := range fp.Pads {
				pad := &fp.Pads[i]
				if pad.Net == nil || *pad.Net == "" {
					continue
				}
				c := core.PadWorldCenter(fp, pad)
				netPads[*pad.Net] = append(netPads[*pad.Net], [3]float64{
					hashRef(fp.Reference), c.X.ToMM(), c.Y.ToMM(),
				})
			}
		}
		// For each movable fp, attract toward netmates
		for _, fp := range fps {
			fx, fy := 0.0, 0.0
			n := 0
			for i := range fp.Pads {
				pad := &fp.Pads[i]
				if pad.Net == nil {
					continue
				}
				mates := netPads[*pad.Net]
				if len(mates) < 2 {
					continue
				}
				cx, cy := 0.0, 0.0
				for _, m := range mates {
					cx += m[1]
					cy += m[2]
				}
				cx /= float64(len(mates))
				cy /= float64(len(mates))
				pc := core.PadWorldCenter(fp, pad)
				fx += cx - pc.X.ToMM()
				fy += cy - pc.Y.ToMM()
				n++
			}
			if n > 0 {
				forces[fp.Reference] = [2]float64{fx / float64(n), fy / float64(n)}
			}
		}
		step := 0.15
		for _, fp := range fps {
			f := forces[fp.Reference]
			nx := fp.Position.X.ToMM() + f[0]*step
			ny := fp.Position.Y.ToMM() + f[1]*step
			nx, ny = clampToOutline(nx, ny, board.Outline, 1.0)
			fp.Position = core.NewPoint(core.FromMM(nx), core.FromMM(ny))
		}
		// weak repulsion
		for i := 0; i < len(fps); i++ {
			for j := i + 1; j < len(fps); j++ {
				a, b := fps[i], fps[j]
				dx := b.Position.X.ToMM() - a.Position.X.ToMM()
				dy := b.Position.Y.ToMM() - a.Position.Y.ToMM()
				d := math.Hypot(dx, dy)
				if d < 0.01 {
					d = 0.01
				}
				if d < opts.SolderGapMM*2 {
					push := (opts.SolderGapMM*2 - d) * 0.1
					ux, uy := dx/d, dy/d
					a.Position = core.NewPoint(
						core.FromMM(a.Position.X.ToMM()-ux*push),
						core.FromMM(a.Position.Y.ToMM()-uy*push),
					)
					b.Position = core.NewPoint(
						core.FromMM(b.Position.X.ToMM()+ux*push),
						core.FromMM(b.Position.Y.ToMM()+uy*push),
					)
				}
			}
		}
		h := hpwl(board)
		if h < bestHPWL {
			bestHPWL = h
			for _, fp := range fps {
				best[fp.Reference] = pos{fp.Position.X, fp.Position.Y, fp.Rotation}
			}
		}
	}

	// SA legalisation
	T := 5.0
	curHPWL := hpwl(board)
	for iter := 0; iter < opts.Iterations; iter++ {
		fp := fps[rng.Intn(len(fps))]
		old := pos{fp.Position.X, fp.Position.Y, fp.Rotation}
		// propose move or 90° rotate
		if rng.Float64() < 0.15 {
			fp.Rotation = math.Mod(fp.Rotation+90, 360)
		} else {
			nx := old.x.ToMM() + rng.NormFloat64()*opts.MoveStdMM
			ny := old.y.ToMM() + rng.NormFloat64()*opts.MoveStdMM
			nx, ny = clampToOutline(nx, ny, board.Outline, 1.0)
			fp.Position = core.NewPoint(core.FromMM(nx), core.FromMM(ny))
		}
		// hard gap constraint
		if !legalGaps(board, opts.SolderGapMM) {
			fp.Position = core.NewPoint(old.x, old.y)
			fp.Rotation = old.rot
			continue
		}
		newHPWL := hpwl(board)
		dE := newHPWL - curHPWL
		if dE < 0 || rng.Float64() < math.Exp(-dE/T) {
			curHPWL = newHPWL
			if curHPWL < bestHPWL {
				bestHPWL = curHPWL
				for _, f := range fps {
					best[f.Reference] = pos{f.Position.X, f.Position.Y, f.Rotation}
				}
			}
		} else {
			fp.Position = core.NewPoint(old.x, old.y)
			fp.Rotation = old.rot
		}
		T *= 0.999
		if T < 0.01 {
			T = 0.01
		}
	}

	// restore best-seen
	var moved []string
	for _, fp := range fps {
		b := best[fp.Reference]
		if fp.Position.X != b.x || fp.Position.Y != b.y || fp.Rotation != b.rot {
			moved = append(moved, fp.Reference)
		}
		fp.Position = core.NewPoint(b.x, b.y)
		fp.Rotation = b.rot
	}

	return Report{
		InitialHPWLMM: initHPWL,
		FinalHPWLMM:   hpwl(board),
		Moved:         moved,
		Iterations:    opts.Iterations,
	}, nil
}

func hashRef(s string) float64 {
	h := 0.0
	for _, c := range s {
		h = h*33 + float64(c)
	}
	return h
}

func clampToOutline(x, y float64, o *core.Rect, marginMM float64) (float64, float64) {
	if o == nil {
		return x, y
	}
	minX := o.Min.X.ToMM() + marginMM
	minY := o.Min.Y.ToMM() + marginMM
	maxX := o.Max.X.ToMM() - marginMM
	maxY := o.Max.Y.ToMM() - marginMM
	if x < minX {
		x = minX
	}
	if y < minY {
		y = minY
	}
	if x > maxX {
		x = maxX
	}
	if y > maxY {
		y = maxY
	}
	return x, y
}

func hpwl(board *core.Board) float64 {
	// weighted HPWL: half-perimeter of pad bbox per net, ×4 for 2-pin nets (Rust-ish)
	nets := map[string][]core.Point{}
	for _, fp := range board.Footprints {
		if fp == nil {
			continue
		}
		for i := range fp.Pads {
			pad := &fp.Pads[i]
			if pad.Net == nil || *pad.Net == "" {
				continue
			}
			nets[*pad.Net] = append(nets[*pad.Net], core.PadWorldCenter(fp, pad))
		}
	}
	total := 0.0
	for _, pts := range nets {
		if len(pts) < 2 {
			continue
		}
		minX, maxX := pts[0].X, pts[0].X
		minY, maxY := pts[0].Y, pts[0].Y
		for _, p := range pts[1:] {
			if p.X < minX {
				minX = p.X
			}
			if p.X > maxX {
				maxX = p.X
			}
			if p.Y < minY {
				minY = p.Y
			}
			if p.Y > maxY {
				maxY = p.Y
			}
		}
		hp := (maxX-minX).ToMM() + (maxY-minY).ToMM()
		w := 1.0
		if len(pts) == 2 {
			w = 4.0
		}
		total += w * hp
	}
	return total
}

func legalGaps(board *core.Board, gapMM float64) bool {
	var fps []*core.Footprint
	for _, fp := range board.Footprints {
		if fp != nil {
			fps = append(fps, fp)
		}
	}
	gap := core.FromMM(gapMM)
	for i := 0; i < len(fps); i++ {
		for j := i + 1; j < len(fps); j++ {
			a, b := fps[i], fps[j]
			// pad AABB expansion
			for pi := range a.Pads {
				aa := core.PadWorldAABB(a, &a.Pads[pi]).Expand(gap / 2)
				for pj := range b.Pads {
					bb := core.PadWorldAABB(b, &b.Pads[pj]).Expand(gap / 2)
					if aa.Intersects(bb) {
						// same net pads may be closer? keep hard floor for all
						return false
					}
				}
			}
		}
	}
	return true
}
