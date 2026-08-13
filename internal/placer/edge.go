package placer

import (
	"math"
	"strings"

	"github.com/mentasystems/fragua/internal/core"
)

// SnapToNearestEdge translates an edge-mounted footprint so its pad
// bbox touches the closest outline side (Rust snap_delta_to_nearest_edge).
func SnapToNearestEdge(fp *core.Footprint, outline core.Rect) {
	b, ok := footprintBounds(fp)
	if !ok {
		return
	}
	dLeft := math.Abs((b.Min.X - outline.Min.X).ToMM())
	dRight := math.Abs((outline.Max.X - b.Max.X).ToMM())
	dBottom := math.Abs((b.Min.Y - outline.Min.Y).ToMM())
	dTop := math.Abs((outline.Max.Y - b.Max.Y).ToMM())
	nearest := math.Min(math.Min(dLeft, dRight), math.Min(dBottom, dTop))
	dx, dy := core.Length(0), core.Length(0)
	switch nearest {
	case dLeft:
		dx = outline.Min.X - b.Min.X
	case dRight:
		dx = outline.Max.X - b.Max.X
	case dBottom:
		dy = outline.Min.Y - b.Min.Y
	default:
		dy = outline.Max.Y - b.Max.Y
	}
	fp.Position = core.NewPoint(fp.Position.X+dx, fp.Position.Y+dy)
}

// EdgePlace snaps fp onto the named outline side. alongMM is the
// coordinate along that edge (Y for left/right, X for top/bottom).
// Rotation is set so the part's long axis runs along the edge.
func EdgePlace(fp *core.Footprint, outline core.Rect, side string, alongMM *float64) {
	bb, ok := footprintBounds(fp)
	if !ok {
		return
	}
	hw := bb.Width().ToMM() / 2
	hh := bb.Height().ToMM() / 2
	cx := (bb.Min.X.ToMM() + bb.Max.X.ToMM()) / 2
	cy := (bb.Min.Y.ToMM() + bb.Max.Y.ToMM()) / 2
	offX := fp.Position.X.ToMM() - cx
	offY := fp.Position.Y.ToMM() - cy

	minX, maxX := outline.Min.X.ToMM(), outline.Max.X.ToMM()
	minY, maxY := outline.Min.Y.ToMM(), outline.Max.Y.ToMM()
	midX := (minX + maxX) / 2
	midY := (minY + maxY) / 2

	var nx, ny, rot float64
	switch strings.ToLower(side) {
	case "left":
		rot = 90
		ny = midY
		if alongMM != nil {
			ny = *alongMM
		}
		nx = minX + hh
		if fp.EdgeSide != nil && *fp.EdgeSide == core.EdgeTop {
			rot = 90
		}
	case "right":
		rot = 270
		ny = midY
		if alongMM != nil {
			ny = *alongMM
		}
		nx = maxX - hh
	case "top":
		rot = 180
		nx = midX
		if alongMM != nil {
			nx = *alongMM
		}
		ny = maxY - hh
	default: // bottom
		rot = 0
		nx = midX
		if alongMM != nil {
			nx = *alongMM
		}
		ny = minY + hh
	}
	_ = hw
	fp.Rotation = rot
	fp.Position = core.NewPoint(core.FromMM(nx+offX), core.FromMM(ny+offY))
	// Recompute after rotation so the pad bbox actually kisses the edge.
	SnapToNearestEdge(fp, outline)
}

// PlanEdges assigns each named edge-mounted footprint to the outline
// side that minimises the HPWL of its nets (wiring pull).
func PlanEdges(board *core.Board, refs []string) int {
	if board.Outline == nil {
		return 0
	}
	o := *board.Outline
	moved := 0
	sides := []string{"left", "right", "top", "bottom"}
	for _, ref := range refs {
		fp := board.FootprintByRef(ref)
		if fp == nil {
			continue
		}
		fp.EdgeMounted = true
		bestSide := "bottom"
		best := math.Inf(1)
		old := pos{fp.Position.X, fp.Position.Y, fp.Rotation}
		for _, s := range sides {
			EdgePlace(fp, o, s, nil)
			h := rawHPWL(board)
			if h < best {
				best = h
				bestSide = s
			}
		}
		EdgePlace(fp, o, bestSide, nil)
		if fp.Position.X != old.x || fp.Position.Y != old.y || fp.Rotation != old.rot {
			moved++
		}
	}
	return moved
}
