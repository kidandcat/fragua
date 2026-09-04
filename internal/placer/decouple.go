package placer

import (
	"math"
	"sort"
	"strings"

	"github.com/mentasystems/fragua/internal/core"
)

const (
	ringStepMM  = 0.5
	ringRanks   = 12
	ringSlackMM = 0.02
	seatSlackMM = 0.05
)

var ringLateral = []float64{0, 1, -1, 2, -2}

type anchorPin struct {
	fp     *core.Footprint
	pad    int
	sortK  string
	centre [2]float64
}

// PullPassivesToAnchors seats 2–4 pad passives on a ring outside a
// specific same-net IC/connector pad (Rust decouple.rs process).
func PullPassivesToAnchors(board *core.Board, movable []*core.Footprint, opts Options) {
	if board.Outline == nil || len(movable) == 0 {
		return
	}
	hard := math.Max(opts.MinClearanceMM, opts.SolderGapMM)
	movableSet := map[string]bool{}
	for _, fp := range movable {
		movableSet[fp.ID.String()] = true
	}

	type cand struct {
		fp   *core.Footprint
		nets []string
	}
	var cands []cand
	for _, fp := range movable {
		if fp.EdgeMounted || len(fp.Pads) < 2 || len(fp.Pads) > 4 {
			continue
		}
		seen := map[string]bool{}
		var nets []string
		for i := range fp.Pads {
			if fp.Pads[i].Net == nil || *fp.Pads[i].Net == "" {
				continue
			}
			n := *fp.Pads[i].Net
			if !seen[n] {
				seen[n] = true
				nets = append(nets, n)
			}
		}
		if len(nets) == 0 {
			continue
		}
		sort.Strings(nets)
		cands = append(cands, cand{fp: fp, nets: nets})
	}
	if len(cands) == 0 {
		return
	}
	sort.Slice(cands, func(i, j int) bool {
		return cands[i].fp.Reference < cands[j].fp.Reference
	})

	pins := map[string][]anchorPin{}
	netCount := map[string]int{}
	for _, fp := range footprintsAll(board) {
		isPassive := movableSet[fp.ID.String()] && len(fp.Pads) <= 4
		for i := range fp.Pads {
			if fp.Pads[i].Net == nil || *fp.Pads[i].Net == "" {
				continue
			}
			net := *fp.Pads[i].Net
			netCount[net]++
			if isPassive {
				continue
			}
			c := core.PadWorldCenter(fp, &fp.Pads[i])
			pins[net] = append(pins[net], anchorPin{
				fp: fp, pad: i,
				sortK:  fp.Reference + "\x00" + fp.Pads[i].Number,
				centre: [2]float64{c.X.ToMM(), c.Y.ToMM()},
			})
		}
	}
	for n := range pins {
		sort.Slice(pins[n], func(i, j int) bool { return pins[n][i].sortK < pins[n][j].sortK })
	}

	type assign struct {
		fp   *core.Footprint
		nets []string
		pin  *anchorPin
		net  string
		ok   bool
	}
	used := map[string]bool{}
	var as []assign
	for _, c := range cands {
		here := [2]float64{c.fp.Position.X.ToMM(), c.fp.Position.Y.ToMM()}
		type rank struct {
			gnd   bool
			count int
			name  string
		}
		var ranks []rank
		for _, n := range c.nets {
			ranks = append(ranks, rank{isGround(n), netCount[n], n})
		}
		sort.Slice(ranks, func(i, j int) bool {
			if ranks[i].gnd != ranks[j].gnd {
				return !ranks[i].gnd && ranks[j].gnd
			}
			if ranks[i].count != ranks[j].count {
				return ranks[i].count < ranks[j].count
			}
			return ranks[i].name < ranks[j].name
		})
		var chosen *anchorPin
		var chosenNet string
		for _, r := range ranks {
			list := pins[r.name]
			if len(list) == 0 {
				continue
			}
			if p := nearestPin(list, here, used, true); p != nil {
				chosen, chosenNet = p, r.name
				break
			}
			if p := nearestPin(list, here, used, false); p != nil {
				chosen, chosenNet = p, r.name
				break
			}
		}
		if chosen != nil {
			used[chosen.fp.ID.String()+":"+chosen.fp.Pads[chosen.pad].Number] = true
		}
		as = append(as, assign{fp: c.fp, nets: c.nets, pin: chosen, net: chosenNet})
	}

	for pass := 0; pass < 2; pass++ {
		for i := range as {
			if as[i].pin == nil {
				continue
			}
			if ringPlace(board, as[i].fp, as[i].pin, as[i].net, hard, opts) {
				as[i].ok = true
			}
		}
	}
	for i := range as {
		if !as[i].ok {
			pullToCentroid(board, as[i].fp, as[i].nets, movableSet, hard, opts)
		}
	}
}

func isGround(net string) bool {
	u := strings.ToUpper(net)
	switch u {
	case "GND", "AGND", "DGND", "PGND", "VSS", "VSSA", "0V", "GROUND":
		return true
	}
	return strings.HasPrefix(u, "GND")
}

func nearestPin(list []anchorPin, here [2]float64, used map[string]bool, freeOnly bool) *anchorPin {
	var best *anchorPin
	bestD := math.Inf(1)
	for i := range list {
		p := &list[i]
		key := p.fp.ID.String() + ":" + p.fp.Pads[p.pad].Number
		if freeOnly && used[key] {
			continue
		}
		d := math.Hypot(p.centre[0]-here[0], p.centre[1]-here[1])
		if d < bestD {
			bestD = d
			best = p
		}
	}
	return best
}

func ringPlace(board *core.Board, fp *core.Footprint, pin *anchorPin, net string, hard float64, opts Options) bool {
	if pin.fp == nil || board.Outline == nil {
		return false
	}
	abb, ok := footprintBounds(pin.fp)
	if !ok {
		return false
	}
	anchorC := [2]float64{
		(abb.Min.X.ToMM() + abb.Max.X.ToMM()) / 2,
		(abb.Min.Y.ToMM() + abb.Max.Y.ToMM()) / 2,
	}
	ownPad := -1
	for i := range fp.Pads {
		if fp.Pads[i].Net != nil && *fp.Pads[i].Net == net {
			ownPad = i
			break
		}
	}
	if ownPad < 0 {
		return false
	}
	dx := pin.centre[0] - anchorC[0]
	dy := pin.centre[1] - anchorC[1]
	if math.Hypot(dx, dy) < 1e-6 {
		dx = fp.Position.X.ToMM() - anchorC[0]
		dy = fp.Position.Y.ToMM() - anchorC[1]
	}
	if math.Hypot(dx, dy) < 1e-6 {
		return false
	}
	ux, uy := 1.0, 0.0
	if math.Abs(dx) >= math.Abs(dy) {
		ux, uy = math.Copysign(1, dx), 0
	} else {
		ux, uy = 0, math.Copysign(1, dy)
	}
	vx, vy := -uy, ux

	oldPos, oldRot := fp.Position, fp.Rotation
	bestRot := fp.Rotation
	bestDot := math.Inf(1)
	for q := 0; q < 4; q++ {
		fp.Rotation = math.Mod(oldRot+90*float64(q), 360)
		fp.Position = core.Origin
		c := core.PadWorldCenter(fp, &fp.Pads[ownPad])
		dot := c.X.ToMM()*ux + c.Y.ToMM()*uy
		if dot < bestDot-1e-9 {
			bestDot = dot
			bestRot = fp.Rotation
		}
	}
	fp.Rotation = bestRot
	fp.Position = oldPos
	ob, ok := footprintBounds(fp)
	if !ok {
		fp.Position, fp.Rotation = oldPos, oldRot
		return false
	}
	px, py := fp.Position.X.ToMM(), fp.Position.Y.ToMM()
	half := [2]float64{ob.Width().ToMM() / 2, ob.Height().ToMM() / 2}
	bbOff := [2]float64{
		(ob.Min.X.ToMM()+ob.Max.X.ToMM())/2 - px,
		(ob.Min.Y.ToMM()+ob.Max.Y.ToMM())/2 - py,
	}
	reachU := half[0]*math.Abs(ux) + half[1]*math.Abs(uy)
	reachV := half[0]*math.Abs(vx) + half[1]*math.Abs(vy)
	anchorReach := (abb.Width().ToMM()/2)*math.Abs(ux) + (abb.Height().ToMM()/2)*math.Abs(uy)
	padAlong := (pin.centre[0]-anchorC[0])*ux + (pin.centre[1]-anchorC[1])*uy
	base := math.Max(0, anchorReach-padAlong) + reachU + hard + ringSlackMM
	latStep := 2*reachV + hard

	for rank := 0; rank < ringRanks; rank++ {
		d := base + ringStepMM*float64(rank)
		for _, l := range ringLateral {
			cx := pin.centre[0] + ux*d + vx*l*latStep
			cy := pin.centre[1] + uy*d + vy*l*latStep
			fp.Position = core.NewPoint(core.FromMM(cx-bbOff[0]), core.FromMM(cy-bbOff[1]))
			fp.Rotation = bestRot
			if !padsInside(fp, board.Outline, opts.EdgeClearanceMM) {
				continue
			}
			if minGapAgainstOthers(board, fp) < hard+seatSlackMM {
				continue
			}
			if firstOverlapperGap(board, fp, opts.SolderGapMM) || hitsNoPlace(board, fp) {
				continue
			}
			return true
		}
	}
	fp.Position, fp.Rotation = oldPos, oldRot
	return false
}

func pullToCentroid(board *core.Board, fp *core.Footprint, nets []string, movableSet map[string]bool, hard float64, opts Options) {
	if board.Outline == nil {
		return
	}
	ax, ay, n := 0.0, 0.0, 0.0
	for _, other := range footprintsAll(board) {
		if other == fp || other.ID == fp.ID {
			continue
		}
		if movableSet[other.ID.String()] && len(other.Pads) <= 4 {
			continue
		}
		for i := range other.Pads {
			if other.Pads[i].Net == nil {
				continue
			}
			ok := false
			for _, net := range nets {
				if *other.Pads[i].Net == net {
					ok = true
					break
				}
			}
			if !ok {
				continue
			}
			c := core.PadWorldCenter(other, &other.Pads[i])
			ax += c.X.ToMM()
			ay += c.Y.ToMM()
			n++
		}
	}
	if n < 1 {
		return
	}
	ax /= n
	ay /= n
	old := pos{fp.Position.X, fp.Position.Y, fp.Rotation}
	offsets := [][2]float64{{0, 0}, {1.2, 0}, {-1.2, 0}, {0, 1.2}, {0, -1.2}, {1.2, 1.2}, {-1.2, 1.2}}
	for _, o := range offsets {
		fp.Position = core.NewPoint(core.FromMM(ax+o[0]), core.FromMM(ay+o[1]))
		if !padsInside(fp, board.Outline, opts.EdgeClearanceMM) {
			continue
		}
		if minGapAgainstOthers(board, fp) < hard+seatSlackMM ||
			firstOverlapperGap(board, fp, opts.SolderGapMM) || hitsNoPlace(board, fp) {
			continue
		}
		return
	}
	fp.Position = core.NewPoint(old.x, old.y)
	fp.Rotation = old.rot
}

func footprintsAll(board *core.Board) []*core.Footprint {
	var out []*core.Footprint
	seen := map[string]bool{}
	for _, id := range board.FootprintOrder {
		if fp := board.Footprints[id]; fp != nil {
			out = append(out, fp)
			seen[id] = true
		}
	}
	for id, fp := range board.Footprints {
		if fp != nil && !seen[id] {
			out = append(out, fp)
		}
	}
	return out
}
