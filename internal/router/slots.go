package router

import (
	"math"
	"sort"

	"github.com/mentasystems/fragua/internal/core"
)

// Escape-slot assignment for 0.4 mm QFN/BGA. Replaces first-come dogbones:
// a lattice of barrel sites (including *between* pins) is matched to pads
// as min-cost max-cardinality, then legalised cheapest-first so two vias
// closer than one routable lane cannot both land.

const (
	slotRanksOut     = 5
	slotRanksIn      = 3
	slotAssignRounds = 3
	slotRepairRounds = 3
	slotWLen         = 10.0
	slotWDir         = 1.2
	slotWBlocked     = 3.0
	slotWCrowd       = 2.0
	slotWTight       = 8.0
	slotExitProbeMM  = 3.0
	slotCorridorMM   = 0.45
)

var slotLateral = [...]float64{
	0, 0.25, -0.25, 0.5, -0.5, 0.75, -0.75, 1.0, -1.0, 0.125, -0.125, 0.375, -0.375,
}

type slotTarget struct {
	idx    int
	pad    *core.Pad
	net    string
	cx, cy float64
	hw, hh float64
	tx, ty float64 // partner centroid (travel direction)
	ux, uy float64 // side normal (outward)
	side   int
	layer  uint8
}

type escSlot struct{ x, y float64 }

type slotEdge struct {
	slot     int
	baseCost float64
	dx, dy   float64
}

func assignEscapeSlots(board *core.Board, fp *core.Footprint, opts Options, viaDia, viaDrill, stubW float64, placed *[]fanoutXY) int {
	// Only skip pads that already sit in a same-layer pour. A +1V1
	// plane on B.Cu still needs QFN dogbones on F.Cu.
	samePour := map[string]map[uint8]bool{}
	for _, p := range board.Pours {
		if samePour[p.Net] == nil {
			samePour[p.Net] = map[uint8]bool{}
		}
		samePour[p.Net][p.Layer.Index] = true
	}
	bb, bbOK := fpPadBounds(fp)
	cx, cy := fp.Position.X.ToMM(), fp.Position.Y.ToMM()
	minX, minY, maxX, maxY := cx, cy, cx, cy
	if bbOK {
		minX, minY = bb.Min.X.ToMM(), bb.Min.Y.ToMM()
		maxX, maxY = bb.Max.X.ToMM(), bb.Max.Y.ToMM()
		cx = (minX + maxX) / 2
		cy = (minY + maxY) / 2
	}

	// Partner centroid per net (pads of this net not on this footprint).
	partner := netPartnerCentroid(board, fp)

	var targets []slotTarget
	for i := range fp.Pads {
		pad := &fp.Pads[i]
		if pad.Net == nil || *pad.Net == "" {
			continue
		}
		if layers, ok := samePour[*pad.Net]; ok && layers[pad.Layer.Index] {
			continue
		}
		if pad.Drill != nil && *pad.Drill > 0 {
			continue
		}
		pc := core.PadWorldCenter(fp, pad)
		px, py := pc.X.ToMM(), pc.Y.ToMM()
		if math.Hypot(px-cx, py-cy) < 0.15 {
			continue
		}
		w, h := core.PadWorldSize(fp, pad)
		padMin := math.Min(w.ToMM(), h.ToMM())
		// GND on a fat pad waits for the pour. A 0.24 mm WLP GND land
		// cannot leave the pin field unaided, so it still gets a dogbone.
		if netOrderKey(*pad.Net) == 2 && padMin >= 0.35 {
			continue
		}
		// Two-pin signals (SEL/EN pull) stay on one layer. A dogbone
		// aimed at the resistor lands in the inductor corridor and
		// splits the net: the via sits nearer the discrete than the
		// IC pad, so A* marks both ends reached without joining them.
		if netOrderKey(*pad.Net) >= 3 && netPadCount(board, *pad.Net) == 2 &&
			closeSameLayerPartner(board, fp, *pad.Net, pad.Layer.Index, px, py, shortNetSpanMM) {
			continue
		}
		// A pad that already has a same-layer partner within a hop (LX→L)
		// does not need a via. Tiny lands only skip a true pad-to-pad hop;
		// 2.8 mm would swallow SEL/GND on a WLP and leave them stranded.
		hop := 2.8
		if padMin < 0.30 {
			hop = 1.6
		}
		if closeSameLayerPartner(board, fp, *pad.Net, pad.Layer.Index, px, py, hop) {
			continue
		}
		side, ux, uy := sideNormal(px, py, minX, minY, maxX, maxY)
		tx, ty := px+ux, py+uy
		if p, ok := partner[*pad.Net]; ok {
			tx, ty = p[0], p[1]
		}
		targets = append(targets, slotTarget{
			idx: i, pad: pad, net: *pad.Net,
			cx: px, cy: py, hw: w.ToMM() / 2, hh: h.ToMM() / 2,
			tx: tx, ty: ty, ux: ux, uy: uy, side: side, layer: pad.Layer.Index,
		})
	}
	if len(targets) == 0 {
		return 0
	}

	viaR := viaDia / 2
	clear := opts.ClearanceMM
	if clear <= 0 {
		clear = core.ActiveFabRules(board).MinClearanceMM
	}
	if clear <= 0 {
		clear = 0.127
	}
	keepR := viaR + clear
	minViaPitch := viaDia + clear
	lane := 2*(viaR+clear+stubW/2) + opts.CellMM
	if lane < minViaPitch {
		lane = minViaPitch
	}

	added := 0
	// Explicit via-in-pad exceptions first — they occupy the pin itself.
	added += placeViaInPadEscapes(board, fp, targets, false, viaDia, viaDrill, placed)
	// Reserve far-ring barrels for power pins first so +3V3/+1V1 can
	// via onto inner planes without taking the first-rank GPIO lanes.
	added += placeEscapes(board, fp, targets, true, []float64{1.35, 1.70, 2.05, 2.40}, []float64{0, 0.40, -0.40, 0.80, -0.80}, viaDia, viaDrill, stubW, viaR, keepR, minViaPitch, placed)
	for s := 0; s < 4; s++ {
		var group []int
		for i, t := range targets {
			if t.side != s || netOrderKey(t.net) <= 1 {
				continue
			}
			group = append(group, i)
		}
		if len(group) == 0 {
			continue
		}
		sort.Slice(group, func(a, b int) bool {
			ta, tb := targets[group[a]], targets[group[b]]
			if s <= 1 {
				return ta.cy < tb.cy
			}
			return ta.cx < tb.cx
		})
		added += assignSide(board, fp, targets, group, opts, viaDia, viaDrill, stubW, viaR, keepR, minViaPitch, lane, placed)
	}
	// Stranded signals take an *outer* ring first. On a 0.4 mm QFN the
	// near ring is already packed (via pitch 0.54 > pin pitch), so a
	// leftover at 0.70 collides with its neighbours; 1.35+ clears them.
	// Keep full fab keep-out — do not sneak under the ceiling.
	added += placeEscapes(board, fp, targets, false, []float64{1.35, 1.70, 2.05, 2.40, 1.00, 0.70}, []float64{0, 0.40, -0.40, 0.80, -0.80, 1.20, -1.20}, viaDia, viaDrill, stubW, viaR, keepR, minViaPitch, placed)
	// Remaining power pins on leftover lattice.
	added += placeEscapes(board, fp, targets, true, []float64{1.35, 1.70, 2.05, 2.40, 1.00, 0.70}, []float64{0, 0.40, -0.40, 0.80, -0.80}, viaDia, viaDrill, stubW, viaR, keepR, minViaPitch, placed)
	if board.AutoViaInPadStranded {
		added += placeViaInPadEscapes(board, fp, targets, true, viaDia, viaDrill, placed)
	}
	return added
}

func placeViaInPadEscapes(board *core.Board, fp *core.Footprint, targets []slotTarget, strandedOnly bool, viaDia, viaDrill float64, placed *[]fanoutXY) int {
	added := 0
	for _, t := range targets {
		if strandedOnly {
			if netOrderKey(t.net) <= 1 {
				continue
			}
			has := false
			for _, v := range board.Vias {
				if v.Net == t.net && math.Hypot(v.Position.X.ToMM()-t.cx, v.Position.Y.ToMM()-t.cy) < 1.4 {
					has = true
					break
				}
			}
			if has {
				continue
			}
		} else if !core.HasEscapeException(board, fp.Reference, t.pad.Number, core.EscapeViaInPad) {
			continue
		} else {
			has := false
			for _, v := range board.Vias {
				if v.Net == t.net && math.Hypot(v.Position.X.ToMM()-t.cx, v.Position.Y.ToMM()-t.cy) < 0.4 {
					has = true
					break
				}
			}
			if has {
				continue
			}
		}
		if !viaFitsPad(t, viaDia, viaDrill) {
			continue
		}
		if !holeSiteOK(board, t.cx, t.cy, viaDrill) {
			continue
		}
		if fanoutHitsInductor(board, t.cx, t.cy, viaDia/2) {
			continue
		}
		board.Vias = append(board.Vias, core.Via{
			ID: core.NewID(), Net: t.net,
			Position: core.NewPoint(core.FromMM(t.cx), core.FromMM(t.cy)),
			Drill:    core.FromMM(viaDrill), Diameter: core.FromMM(viaDia),
		})
		*placed = append(*placed, fanoutXY{t.cx, t.cy})
		added++
	}
	return added
}

func placeEscapes(board *core.Board, fp *core.Footprint, targets []slotTarget, power bool, reaches, laterals []float64, viaDia, viaDrill, stubW, viaR, keepR, minViaPitch float64, placed *[]fanoutXY) int {
	added := 0
	for _, t := range targets {
		if power != (netOrderKey(t.net) <= 1) {
			continue
		}
		already := false
		for _, v := range board.Vias {
			if v.Net != t.net {
				continue
			}
			if math.Hypot(v.Position.X.ToMM()-t.cx, v.Position.Y.ToMM()-t.cy) < 1.4 {
				already = true
				break
			}
		}
		if already {
			continue
		}
		ok := false
		var vx, vy float64
		dirs := [][2]float64{{t.ux, t.uy}}
		if pdx, pdy := t.tx-t.cx, t.ty-t.cy; math.Hypot(pdx, pdy) > 1e-6 {
			n := math.Hypot(pdx, pdy)
			dirs = [][2]float64{{pdx / n, pdy / n}, {t.ux, t.uy}}
		}
	dirLoop:
		for _, dir := range dirs {
			px, py := -dir[1], dir[0]
			for _, r := range reaches {
				for _, lat := range laterals {
					x := t.cx + dir[0]*r + px*lat
					y := t.cy + dir[1]*r + py*lat
					if !escapeSiteOK(board, fp, t, x, y, viaDrill, stubW, keepR, minViaPitch, placed) {
						continue
					}
					vx, vy, ok = x, y, true
					break dirLoop
				}
			}
		}
		if !ok {
			continue
		}
		board.Vias = append(board.Vias, core.Via{
			ID: core.NewID(), Net: t.net,
			Position: core.NewPoint(core.FromMM(vx), core.FromMM(vy)),
			Drill:    core.FromMM(viaDrill), Diameter: core.FromMM(viaDia),
		})
		board.Traces = append(board.Traces, core.Trace{
			ID: core.NewID(), Layer: core.Layer{Index: t.layer}, Net: t.net,
			Width: core.FromMM(stubW),
			Start: core.NewPoint(core.FromMM(t.cx), core.FromMM(t.cy)),
			End:   core.NewPoint(core.FromMM(vx), core.FromMM(vy)),
		})
		*placed = append(*placed, fanoutXY{vx, vy})
		added++
	}
	return added
}

func assignSide(board *core.Board, fp *core.Footprint, targets []slotTarget, group []int, opts Options, viaDia, viaDrill, stubW, viaR, keepR, minViaPitch, lane float64, placed *[]fanoutXY) int {
	slots := generateSlots(targets, group, viaR, board, fp, keepR)
	if len(slots) == 0 {
		return 0
	}
	pending := append([]int(nil), group...)
	added := 0
	for round := 0; round <= slotRepairRounds && len(pending) > 0; round++ {
		taken := make([]bool, len(slots))
		for i, s := range slots {
			if fanoutHitsSite(*placed, s.x, s.y, minViaPitch) ||
				fanoutHitsPad(board, s.x, s.y, keepR, fp, -1) ||
				fanoutHitsInductor(board, s.x, s.y, keepR) ||
				!outlineContains(board.Outline, s.x, s.y, 0.55) {
				taken[i] = true
			}
		}
		edges := buildSlotEdges(targets, pending, slots, taken, board, fp, keepR, stubW, viaR)
		standing := make([][3]float64, 0, len(board.Vias))
		for _, v := range board.Vias {
			standing = append(standing, [3]float64{v.Position.X.ToMM(), v.Position.Y.ToMM(), 0})
		}
		chosen := solveSlotRounds(edges, slots, standing, targets, pending, lane)
		got, left := commitSlots(board, fp, targets, pending, chosen, edges, slots, viaDia, viaDrill, stubW, viaR, keepR, minViaPitch, placed)
		added += got
		if got == 0 || round == slotRepairRounds {
			break
		}
		pending = left
	}
	return added
}

func generateSlots(targets []slotTarget, group []int, viaR float64, board *core.Board, fp *core.Footprint, keepR float64) []escSlot {
	if len(group) == 0 {
		return nil
	}
	ux, uy := targets[group[0]].ux, targets[group[0]].uy
	px, py := -uy, ux // along-side
	pitch := sidePitch(targets, group, px, py)
	clear := keepR - viaR
	if clear < 0.05 {
		clear = 0.09
	}
	step := 2*viaR + clear
	base := viaR + 0.05

	seen := map[[2]int64]escSlot{}
	for _, gi := range group {
		t := targets[gi]
		ext := math.Abs(ux)*t.hw + math.Abs(uy)*t.hh
		for _, off := range slotLateral {
			lat := off * pitch
			for k := 0; k < slotRanksOut; k++ {
				reach := ext + base + float64(k)*step
				x := t.cx + t.ux*reach + px*lat
				y := t.cy + t.uy*reach + py*lat
				if !outlineContains(board.Outline, x, y, 0.55) || fanoutHitsPad(board, x, y, keepR, fp, t.idx) ||
					fanoutHitsInductor(board, x, y, keepR) {
					continue
				}
				key := [2]int64{int64(math.Round(x * 1e6)), int64(math.Round(y * 1e6))}
				seen[key] = escSlot{x, y}
			}
			for k := 0; k < slotRanksIn; k++ {
				reach := ext + base + float64(k)*step
				x := t.cx - t.ux*reach + px*lat
				y := t.cy - t.uy*reach + py*lat
				if !outlineContains(board.Outline, x, y, 0.55) || fanoutHitsPad(board, x, y, keepR, fp, t.idx) ||
					fanoutHitsInductor(board, x, y, keepR) {
					continue
				}
				key := [2]int64{int64(math.Round(x * 1e6)), int64(math.Round(y * 1e6))}
				seen[key] = escSlot{x, y}
			}
		}
	}
	out := make([]escSlot, 0, len(seen))
	for _, s := range seen {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].x != out[j].x {
			return out[i].x < out[j].x
		}
		return out[i].y < out[j].y
	})
	return out
}

func sidePitch(targets []slotTarget, group []int, px, py float64) float64 {
	var gaps []float64
	for i := 1; i < len(group); i++ {
		a := targets[group[i-1]].cx*px + targets[group[i-1]].cy*py
		b := targets[group[i]].cx*px + targets[group[i]].cy*py
		g := math.Abs(b - a)
		if g > 1e-6 {
			gaps = append(gaps, g)
		}
	}
	if len(gaps) == 0 {
		return 0.4
	}
	sort.Float64s(gaps)
	return gaps[len(gaps)/2]
}

func buildSlotEdges(targets []slotTarget, pending []int, slots []escSlot, taken []bool, board *core.Board, fp *core.Footprint, keepR, stubW, viaR float64) [][]slotEdge {
	out := make([][]slotEdge, len(pending))
	for pi, gi := range pending {
		t := targets[gi]
		dx, dy := t.tx-t.cx, t.ty-t.cy
		dlen := math.Hypot(dx, dy)
		if dlen > 1e-9 {
			dx /= dlen
			dy /= dlen
		} else {
			dx, dy = t.ux, t.uy
			dlen = 1
		}
		maxReach := math.Max(t.hw, t.hh) + viaR + 0.1 + float64(slotRanksOut)*(2*viaR+0.22)
		var edges []slotEdge
		for si, s := range slots {
			if taken[si] {
				continue
			}
			ex, ey := s.x-t.cx, s.y-t.cy
			length := math.Hypot(ex, ey)
			if length > maxReach+1e-9 || length < 0.15 {
				continue
			}
			if !outlineContains(board.Outline, s.x, s.y, 0.55) {
				continue
			}
			if fanoutHitsPad(board, s.x, s.y, keepR, fp, t.idx) || fanoutHitsInductor(board, s.x, s.y, keepR) {
				continue
			}
			if stubHitsForeignPad(board, t.cx, t.cy, s.x, s.y, t.net, stubW/2+core.ActiveFabRules(board).ClampClearance(0), fp, t.idx) {
				continue
			}
			cos := (ex*dx + ey*dy) / length
			edges = append(edges, slotEdge{
				slot: si, baseCost: slotWLen*length + slotWDir*(1-cos),
				dx: dx, dy: dy,
			})
		}
		out[pi] = edges
	}
	return out
}

func solveSlotRounds(edges [][]slotEdge, slots []escSlot, standing [][3]float64, targets []slotTarget, pending []int, lane float64) []int {
	nLeft, nRight := len(edges), len(slots)
	penalty := map[int]float64{}
	best := make([]int, nLeft)
	for i := range best {
		best[i] = -1
	}
	bestUnmatched, bestCost := nLeft+1, math.Inf(1)
	for round := 0; round < slotAssignRounds; round++ {
		adj := make([][][2]int64, nLeft)
		for i, row := range edges {
			for _, e := range row {
				c := e.baseCost + penalty[e.slot]
				adj[i] = append(adj[i], [2]int64{int64(e.slot), int64(math.Round(c * 10000))})
			}
			sort.Slice(adj[i], func(a, b int) bool {
				if adj[i][a][1] != adj[i][b][1] {
					return adj[i][a][1] < adj[i][b][1]
				}
				return adj[i][a][0] < adj[i][b][0]
			})
		}
		assign := minCostMatching(nLeft, nRight, adj)
		var placed [][2]int
		unmatched := 0
		cost := 0.0
		for p, s := range assign {
			if s < 0 {
				unmatched++
				continue
			}
			placed = append(placed, [2]int{p, s})
			for _, e := range edges[p] {
				if e.slot == s {
					cost += e.baseCost
					break
				}
			}
		}
		blocked, crowd, tight := exitPressure(placed, edges, slots, standing, lane)
		total := cost
		for _, v := range blocked {
			total += v * slotWBlocked
		}
		for _, v := range crowd {
			total += v * slotWCrowd
		}
		for _, v := range tight {
			total += v * slotWTight
		}
		if unmatched < bestUnmatched || (unmatched == bestUnmatched && total < bestCost) {
			bestUnmatched, bestCost = unmatched, total
			best = append([]int(nil), assign...)
		}
		for s, n := range blocked {
			penalty[s] += n * math.Max(slotWBlocked, slotWCrowd) * 0.5
		}
		for s, n := range crowd {
			penalty[s] += n * math.Max(slotWBlocked, slotWCrowd) * 0.5
		}
		for s, n := range tight {
			penalty[s] += n * slotWTight
		}
	}
	return best
}

func exitPressure(placed [][2]int, edges [][]slotEdge, slots []escSlot, standing [][3]float64, lane float64) (blocked, crowd, tight map[int]float64) {
	blocked, crowd, tight = map[int]float64{}, map[int]float64{}, map[int]float64{}
	for i, a := range placed {
		sa := slots[a[1]]
		for _, b := range placed[i+1:] {
			d := math.Hypot(sa.x-slots[b[1]].x, sa.y-slots[b[1]].y)
			if d < lane {
				p := (lane - d) / lane
				tight[a[1]] += p
				tight[b[1]] += p
			}
		}
		for _, st := range standing {
			d := math.Hypot(sa.x-st[0], sa.y-st[1])
			if d < lane {
				tight[a[1]] += (lane - d) / lane
			}
		}
	}
	for _, a := range placed {
		var dir slotEdge
		found := false
		for _, e := range edges[a[0]] {
			if e.slot == a[1] {
				dir, found = e, true
				break
			}
		}
		if !found || (dir.dx == 0 && dir.dy == 0) {
			continue
		}
		sx, sy := slots[a[1]].x, slots[a[1]].y
		ex, ey := sx+dir.dx*slotExitProbeMM, sy+dir.dy*slotExitProbeMM
		for _, b := range placed {
			if b[0] == a[0] {
				continue
			}
			o := slots[b[1]]
			if distPointSeg(o.x, o.y, sx, sy, ex, ey) < slotCorridorMM {
				blocked[a[1]]++
				crowd[b[1]]++
			}
		}
		for _, st := range standing {
			if distPointSeg(st[0], st[1], sx, sy, ex, ey) < slotCorridorMM {
				blocked[a[1]]++
			}
		}
	}
	return
}

func commitSlots(board *core.Board, fp *core.Footprint, targets []slotTarget, pending []int, chosen []int, edges [][]slotEdge, slots []escSlot, viaDia, viaDrill, stubW, viaR, keepR, minViaPitch float64, placed *[]fanoutXY) (int, []int) {
	type hit struct {
		cost   float64
		gi, si int
	}
	var order []hit
	var left []int
	for li, gi := range pending {
		if li >= len(chosen) || chosen[li] < 0 {
			left = append(left, gi)
			continue
		}
		c := math.Inf(1)
		for _, e := range edges[li] {
			if e.slot == chosen[li] && e.baseCost < c {
				c = e.baseCost
			}
		}
		order = append(order, hit{c, gi, chosen[li]})
	}
	sort.Slice(order, func(i, j int) bool { return order[i].cost < order[j].cost })
	committed := 0
	for _, h := range order {
		t := targets[h.gi]
		s := slots[h.si]
		if fanoutHitsSite(*placed, s.x, s.y, minViaPitch) ||
			!holeSiteOK(board, s.x, s.y, viaDrill) ||
			fanoutHitsPad(board, s.x, s.y, keepR, fp, t.idx) ||
			fanoutHitsInductor(board, s.x, s.y, keepR) ||
			stubHitsForeignPad(board, t.cx, t.cy, s.x, s.y, t.net, stubW/2+core.ActiveFabRules(board).ClampClearance(0), fp, t.idx) {
			left = append(left, h.gi)
			continue
		}
		// Lane tightness against already committed barrels this side.
		tight := false
		for _, p := range *placed {
			if math.Hypot(p.x-s.x, p.y-s.y) < minViaPitch {
				tight = true
				break
			}
		}
		if tight {
			left = append(left, h.gi)
			continue
		}
		board.Vias = append(board.Vias, core.Via{
			ID: core.NewID(), Net: t.net,
			Position: core.NewPoint(core.FromMM(s.x), core.FromMM(s.y)),
			Drill:    core.FromMM(viaDrill), Diameter: core.FromMM(viaDia),
		})
		board.Traces = append(board.Traces, core.Trace{
			ID: core.NewID(), Layer: core.Layer{Index: t.layer}, Net: t.net,
			Width: core.FromMM(stubW),
			Start: core.NewPoint(core.FromMM(t.cx), core.FromMM(t.cy)),
			End:   core.NewPoint(core.FromMM(s.x), core.FromMM(s.y)),
		})
		*placed = append(*placed, fanoutXY{s.x, s.y})
		committed++
	}
	return committed, left
}

func minCostMatching(nLeft, nRight int, adj [][][2]int64) []int {
	out := make([]int, nLeft)
	for i := range out {
		out[i] = -1
	}
	if nLeft == 0 || nRight == 0 {
		return out
	}
	src, sink := nLeft+nRight, nLeft+nRight+1
	nNodes := sink + 1
	var to []int
	var cap []int
	var cost []int64
	head := make([][]int, nNodes)
	add := func(u, v, c int, w int64) {
		head[u] = append(head[u], len(to))
		to = append(to, v)
		cap = append(cap, c)
		cost = append(cost, w)
		head[v] = append(head[v], len(to))
		to = append(to, u)
		cap = append(cap, 0)
		cost = append(cost, -w)
	}
	for i := 0; i < nLeft; i++ {
		add(src, i, 1, 0)
		for _, e := range adj[i] {
			add(i, nLeft+int(e[0]), 1, e[1])
		}
	}
	for j := 0; j < nRight; j++ {
		add(nLeft+j, sink, 1, 0)
	}
	const inf int64 = 1 << 60
	for {
		dist := make([]int64, nNodes)
		inq := make([]bool, nNodes)
		prev := make([]int, nNodes)
		for i := range dist {
			dist[i] = inf
			prev[i] = -1
		}
		dist[src] = 0
		q := []int{src}
		inq[src] = true
		for len(q) > 0 {
			u := q[0]
			q = q[1:]
			inq[u] = false
			for _, e := range head[u] {
				if cap[e] <= 0 {
					continue
				}
				v := to[e]
				nd := dist[u] + cost[e]
				if nd < dist[v] {
					dist[v] = nd
					prev[v] = e
					if !inq[v] {
						inq[v] = true
						q = append(q, v)
					}
				}
			}
		}
		if dist[sink] >= inf {
			break
		}
		v := sink
		for v != src {
			e := prev[v]
			cap[e]--
			cap[e^1]++
			v = to[e^1]
		}
	}
	for i := 0; i < nLeft; i++ {
		for _, e := range head[i] {
			if e%2 == 0 && cap[e] == 0 {
				v := to[e]
				if v >= nLeft && v < nLeft+nRight {
					out[i] = v - nLeft
					break
				}
			}
		}
	}
	return out
}

func sideNormal(px, py, minX, minY, maxX, maxY float64) (side int, ux, uy float64) {
	dl, dr := px-minX, maxX-px
	db, dt := py-minY, maxY-py
	side, ux, uy = 0, 1, 0
	best := dr
	if dl <= best {
		side, ux, uy, best = 1, -1, 0, dl
	}
	if dt < best {
		side, ux, uy, best = 2, 0, 1, dt
	}
	if db < best {
		side, ux, uy = 3, 0, -1
	}
	return
}

func netPartnerCentroid(board *core.Board, skip *core.Footprint) map[string][2]float64 {
	sum := map[string][2]float64{}
	n := map[string]int{}
	for _, fp := range footprintsStable(board) {
		if fp == skip {
			continue
		}
		for i := range fp.Pads {
			pad := &fp.Pads[i]
			if pad.Net == nil || *pad.Net == "" {
				continue
			}
			c := core.PadWorldCenter(fp, pad)
			s := sum[*pad.Net]
			sum[*pad.Net] = [2]float64{s[0] + c.X.ToMM(), s[1] + c.Y.ToMM()}
			n[*pad.Net]++
		}
	}
	out := map[string][2]float64{}
	for k, s := range sum {
		if n[k] == 0 {
			continue
		}
		out[k] = [2]float64{s[0] / float64(n[k]), s[1] / float64(n[k])}
	}
	return out
}

func viaFitsPad(t slotTarget, viaDia, viaDrill float64) bool {
	minDim := 2 * math.Min(t.hw, t.hh)
	if minDim <= 0 {
		return false
	}
	return viaDia <= minDim+1e-9 && viaDrill <= minDim+1e-9
}

func netPadCount(board *core.Board, net string) int {
	if board == nil || net == "" {
		return 0
	}
	n := 0
	for _, fp := range footprintsStable(board) {
		for i := range fp.Pads {
			if fp.Pads[i].Net != nil && *fp.Pads[i].Net == net {
				n++
			}
		}
	}
	return n
}

func closeSameLayerPartner(board *core.Board, skip *core.Footprint, net string, layer uint8, x, y, maxMM float64) bool {
	for _, fp := range footprintsStable(board) {
		if fp == skip {
			continue
		}
		for i := range fp.Pads {
			pad := &fp.Pads[i]
			if pad.Net == nil || *pad.Net != net {
				continue
			}
			if pad.Layer.Index != layer && (pad.Drill == nil || *pad.Drill == 0) {
				continue
			}
			c := core.PadWorldCenter(fp, pad)
			if math.Hypot(c.X.ToMM()-x, c.Y.ToMM()-y) <= maxMM {
				return true
			}
		}
	}
	return false
}

func fanoutHitsInductor(board *core.Board, x, y, r float64) bool {
	if board == nil {
		return false
	}
	pt := core.NewPoint(core.FromMM(x), core.FromMM(y))
	disk := core.RectFromCenter(pt, core.FromMM(r*2), core.FromMM(r*2))
	for _, fp := range footprintsStable(board) {
		if !inductorFootprint(fp) {
			continue
		}
		body, ok := core.CourtyardWorld(fp)
		if ok && disk.Intersects(body) {
			return true
		}
	}
	return false
}

func escapeSiteOK(board *core.Board, fp *core.Footprint, t slotTarget, x, y, viaDrill, stubW, keepR, minViaPitch float64, placed *[]fanoutXY) bool {
	if !outlineContains(board.Outline, x, y, 0.55) {
		return false
	}
	if fanoutHitsPad(board, x, y, keepR, fp, t.idx) || fanoutHitsSite(*placed, x, y, minViaPitch) {
		return false
	}
	if fanoutHitsInductor(board, x, y, keepR) {
		return false
	}
	if !holeSiteOK(board, x, y, viaDrill) {
		return false
	}
	if stubHitsForeignPad(board, t.cx, t.cy, x, y, t.net, stubW/2+core.ActiveFabRules(board).ClampClearance(0), fp, t.idx) {
		return false
	}
	return true
}

func stubHitsForeignPad(board *core.Board, x0, y0, x1, y1 float64, net string, need float64, skip *core.Footprint, skipPad int) bool {
	n := int(math.Hypot(x1-x0, y1-y0) / 0.08)
	if n < 2 {
		n = 2
	}
	for i := 0; i <= n; i++ {
		t := float64(i) / float64(n)
		x := x0 + t*(x1-x0)
		y := y0 + t*(y1-y0)
		for _, fp := range footprintsStable(board) {
			for j := range fp.Pads {
				if fp == skip && j == skipPad {
					continue
				}
				pad := &fp.Pads[j]
				if pad.Net != nil && *pad.Net == net {
					continue
				}
				aa := core.PadWorldAABB(fp, pad)
				if pointRectDist(x, y, aa.Min.X.ToMM(), aa.Min.Y.ToMM(), aa.Max.X.ToMM(), aa.Max.Y.ToMM()) < need {
					return true
				}
			}
		}
	}
	return false
}
