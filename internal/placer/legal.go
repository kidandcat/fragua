package placer

import (
	"fmt"
	"math"
	"strings"

	"github.com/mentasystems/fragua/internal/core"
)

const (
	bottomOff  = 0
	bottomOn   = 1
	bottomAuto = 2
)

// finishPlacement is the last word on whether the parts actually fit.
// Decouple may have stacked the passives it could not seat; this puts
// those back on a legal site, moves SMD passives to the bottom when that
// is what makes them fit, and names the ones that still overlap.
func finishPlacement(board *core.Board, fps []*core.Footprint, opts Options) (bottom int, unfitted []string) {
	if board == nil || board.Outline == nil {
		return 0, nil
	}
	if opts.BottomPassives == bottomOn {
		bottom = flipPassives(board, fps, nil)
	}
	unfitted = sweepLegal(board, fps, opts)
	if len(unfitted) == 0 || opts.BottomPassives == bottomOff {
		return bottom, unfitted
	}
	if board.StackupOrDefault().CopperCount() < 2 {
		return bottom, unfitted
	}
	flipped := flipPassives(board, fps, unfitted)
	if flipped == 0 {
		return bottom, unfitted
	}
	bottom += flipped
	return bottom, sweepLegal(board, fps, opts)
}

// badlyPlaced is an overlap or a pad that has actually left the outline.
// The annealer's edge margin is a preference; treating it as illegal here
// dragged a finished pair off a short net and onto the first free cell.
func badlyPlaced(board *core.Board, fp *core.Footprint) bool {
	if fp == nil || board == nil || board.Outline == nil {
		return true
	}
	if !padsInside(fp, board.Outline, 0) || hitsNoPlace(board, fp) || componentClash(board, fp) {
		return true
	}
	return false
}

// poseClear is badlyPlaced plus the caller's edge margin. The grid search
// uses it so a repaired part does not sit on the routed edge.
func poseClear(board *core.Board, fp *core.Footprint, opts Options) bool {
	if badlyPlaced(board, fp) {
		return false
	}
	return padsInside(fp, board.Outline, opts.EdgeClearanceMM)
}

func sweepLegal(board *core.Board, fps []*core.Footprint, opts Options) []string {
	var bad []*core.Footprint
	for _, fp := range fps {
		if fp == nil || fp.Pinned || fp.EdgeMounted {
			continue
		}
		if badlyPlaced(board, fp) {
			bad = append(bad, fp)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	for pass := 0; pass < 48; pass++ {
		moved := false
		for _, fp := range bad {
			if !badlyPlaced(board, fp) {
				continue
			}
			if pushApart(board, fp, opts) {
				moved = true
			}
		}
		if !moved {
			break
		}
	}
	var left []string
	for _, fp := range bad {
		if !badlyPlaced(board, fp) {
			continue
		}
		// A handful of stragglers get a grid search. A crowd that still
		// overlaps after the push is the outline being too small; searching
		// every cell of a 140 mm board for each of them does not find a
		// seat the push could not.
		if len(bad) <= 8 {
			old := pos{fp.Position.X, fp.Position.Y, fp.Rotation}
			if seatOnGrid(board, fp, board.Outline, opts, []float64{2, 1}) {
				continue
			}
			fp.Position = core.NewPoint(old.x, old.y)
			fp.Rotation = old.rot
		}
		left = append(left, fp.Reference)
	}
	return left
}

// pushApart slides fp out of the parts it currently clashes with, along
// the line of centres, and keeps it inside the outline.
func pushApart(board *core.Board, fp *core.Footprint, opts Options) bool {
	rules := board.ComponentRules()
	moved := false
	for _, other := range footprintsAll(board) {
		if other == nil || other == fp || other.ID == fp.ID {
			continue
		}
		hits := core.ComponentHits(fp, other, rules)
		if len(hits) == 0 {
			continue
		}
		ob, ok := footprintBounds(other)
		if !ok {
			continue
		}
		dx := fp.Position.X.ToMM() - (ob.Min.X.ToMM()+ob.Max.X.ToMM())/2
		dy := fp.Position.Y.ToMM() - (ob.Min.Y.ToMM()+ob.Max.Y.ToMM())/2
		if dx == 0 && dy == 0 {
			dx, dy = 0.4, 0.2
		}
		d := math.Hypot(dx, dy)
		need := 0.2
		for _, h := range hits {
			sep := 0.2 - h.Gap
			if h.Need > 0 {
				sep = h.Need - h.Gap + 0.05
			}
			if sep > need {
				need = sep
			}
		}
		nx := fp.Position.X.ToMM() + dx/d*need
		ny := fp.Position.Y.ToMM() + dy/d*need
		nx, ny = clampToOutline(nx, ny, board.Outline, math.Max(opts.EdgeClearanceMM, 0.4))
		fp.Position = core.NewPoint(core.FromMM(nx), core.FromMM(ny))
		moved = true
	}
	if !padsInside(fp, board.Outline, opts.EdgeClearanceMM) {
		// The push walked a pad off the copper. Put the origin back on
		// the outline; the next pass tries again from there.
		nx, ny := clampToOutline(fp.Position.X.ToMM(), fp.Position.Y.ToMM(), board.Outline, math.Max(opts.EdgeClearanceMM, 1))
		fp.Position = core.NewPoint(core.FromMM(nx), core.FromMM(ny))
	}
	return moved
}

func seatOnGrid(board *core.Board, fp *core.Footprint, o *core.Rect, opts Options, steps []float64) bool {
	margin := math.Max(opts.EdgeClearanceMM, 0.4)
	minX := o.Min.X.ToMM() + margin
	minY := o.Min.Y.ToMM() + margin
	maxX := o.Max.X.ToMM() - margin
	maxY := o.Max.Y.ToMM() - margin
	if maxX < minX || maxY < minY {
		return false
	}
	baseRot := fp.Rotation
	for _, step := range steps {
		if step <= 0 {
			continue
		}
		for q := 0; q < 4; q++ {
			fp.Rotation = math.Mod(baseRot+90*float64(q), 360)
			for y := minY; y <= maxY+1e-6; y += step {
				for x := minX; x <= maxX+1e-6; x += step {
					fp.Position = core.NewPoint(core.FromMM(x), core.FromMM(y))
					if poseClear(board, fp, opts) {
						return true
					}
				}
			}
		}
	}
	return false
}

// flipPassives moves eligible SMD passives onto the bottom copper.
// onlyRefs nil means every eligible part in fps; otherwise only those refs.
func flipPassives(board *core.Board, fps []*core.Footprint, onlyRefs []string) int {
	if board == nil || board.StackupOrDefault().CopperCount() < 2 {
		return 0
	}
	bottom := board.StackupOrDefault().BottomLayer()
	if bottom.Index == 0 {
		return 0
	}
	want := map[string]bool{}
	for _, r := range onlyRefs {
		want[r] = true
	}
	n := 0
	for _, fp := range fps {
		if fp == nil {
			continue
		}
		if len(want) > 0 && !want[fp.Reference] {
			continue
		}
		if !passiveSMD(fp) || fp.Layer == bottom {
			continue
		}
		fp.Layer = bottom
		for i := range fp.Pads {
			if fp.Pads[i].Drill != nil && *fp.Pads[i].Drill > 0 {
				continue
			}
			fp.Pads[i].Layer = bottom
		}
		for i := range fp.Silk {
			fp.Silk[i].Layer = core.SilkBottom
		}
		n++
	}
	return n
}

func passiveSMD(fp *core.Footprint) bool {
	if fp == nil || fp.Pinned || fp.EdgeMounted || fp.IsModule() {
		return false
	}
	if len(fp.Pads) < 2 || len(fp.Pads) > 4 || hasThroughHole(fp) {
		return false
	}
	return true
}

func fitError(board *core.Board, refs []string, bottom int) error {
	w, h := 0.0, 0.0
	if board != nil && board.Outline != nil {
		w = board.Outline.Width().ToMM()
		h = board.Outline.Height().ToMM()
	}
	demand := 0.0
	for _, fp := range footprintsAll(board) {
		if c, _, ok := core.PackageOutlines(fp, board.ComponentRules().CourtyardMarginMM); ok {
			demand += c.Width().ToMM() * c.Height().ToMM()
		}
	}
	shown := refs
	extra := ""
	if len(shown) > 24 {
		extra = fmt.Sprintf(" +%d more", len(shown)-24)
		shown = shown[:24]
	}
	face := ""
	if bottom > 0 {
		face = fmt.Sprintf(" even with %d SMD passive(s) on the bottom", bottom)
	}
	return fmt.Errorf("place: %d part(s) do not fit in %.2fx%.2f mm%s (courtyard demand %.0f mm², outline %.0f mm²): %s%s — enlarge the outline",
		len(refs), w, h, face, demand, w*h, strings.Join(shown, " "), extra)
}
