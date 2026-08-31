package router

import (
	"math"
	"sort"
	"time"

	"github.com/mentasystems/fragua/internal/core"
)

const (
	negotiateRounds     = 4
	negotiateMaxVictims = 5
	fanoutStubMaxMM     = 1.55
)

func (g *grid) shareCost(cx, cy int, layer uint8, net string) float64 {
	if g.blocked == nil || int(layer) >= len(g.blocked) {
		return 0
	}
	idx := cy*g.w + cx
	if idx < 0 || idx >= len(g.blocked[layer]) {
		return 0
	}
	cur := g.blocked[layer][idx]
	if cur == "" || cur == net || cur == "*" {
		return 0
	}
	p := g.present
	if p <= 0 {
		p = 4
	}
	return p
}

// negotiateLeftovers is PathFinder-lite for nets the greedy pass left
// failed: probe a sharing path to learn who owns the corridor, rip those
// nets' long-haul traces (fanout vias stay), route the leftover, then
// put the victims back. Power traces that sit on a failed pad are
// evicted locally so a +3V3 maze cannot entomb QSPI/XIN/SWD.
func negotiateLeftovers(board *core.Board, gp **grid, nets map[string][]padLoc, failed []string, rep *Report, opts Options, deadline time.Time, hasDeadline bool) []string {
	if len(failed) == 0 {
		return failed
	}
	rounds := negotiateRounds
	if opts.Negotiate {
		rounds = 10
	}
	left := append([]string(nil), failed...)
	for round := 0; round < rounds; round++ {
		if hasDeadline && time.Now().After(deadline) {
			break
		}
		if len(left) == 0 {
			break
		}
		g := *gp
		progress := false
		next := []string{}
		for _, name := range left {
			if hasDeadline && time.Now().After(deadline) {
				next = append(next, name)
				continue
			}
			pads := nets[name]
			if len(pads) < 2 {
				continue
			}
			if tryNegotiateOne(board, gp, name, pads, nets, opts, deadline, hasDeadline, rep) {
				progress = true
				continue
			}
			next = append(next, name)
		}
		left = next
		if !progress {
			break
		}
		_ = g
		rep.Iterations++
	}
	return left
}

func tryNegotiateOne(board *core.Board, gp **grid, name string, pads []padLoc, nets map[string][]padLoc, opts Options, deadline time.Time, hasDeadline bool, rep *Report) bool {
	g := *gp
	cands := probeBlockingNets(board, g, name, pads, deadline, hasDeadline)
	cands = append(cands, pickRipVictims(board, pads, name, negotiateMaxVictims)...)
	cands = uniqueNets(cands, name)

	// Smallest rip sets first so the victim can actually come back.
	type attempt struct {
		victims   []string
		nickPower bool
		fullRip   bool
	}
	var sets []attempt
	for _, v := range cands {
		sets = append(sets, attempt{victims: []string{v}})
	}
	if len(cands) >= 2 {
		sets = append(sets, attempt{victims: cands[:min(2, len(cands))]})
	}
	bundle := netsNearPads(nets, pads, name, 4.0)
	if len(bundle) > 0 && len(bundle) <= 8 {
		sets = append(sets, attempt{victims: bundle})
	}
	sets = append(sets, attempt{victims: cands, nickPower: true})
	// Last resort: rip a single neighbour including its fanout via so
	// the leftover can take that escape slot.
	for _, v := range cands {
		if len(nets[v]) <= 4 {
			sets = append(sets, attempt{victims: []string{v}, fullRip: true})
		}
	}

	// One rip set must not spend the clock the rest of them need: this is
	// the pass that has to work through its whole list to find the eviction
	// that frees a walled-in pad, and it used to hit the wall on set one.
	for k, a := range sets {
		if hasDeadline && time.Now().After(deadline) {
			return false
		}
		tryDead := repairBudget(deadline, hasDeadline, len(sets)-k, len(pads))
		if stealAndRestore(board, gp, name, pads, a.victims, a.nickPower, a.fullRip, nets, opts, tryDead, deadline, hasDeadline, rep) {
			return true
		}
	}
	return false
}

// stealAndRestore rips victims, routes the leftover, then puts the victims
// back. tryDead bounds the leftover's own two searches so a hopeless rip set
// cannot eat the pass; deadline — the wall — bounds putting the victims back,
// because a victim that fails to return aborts the whole swap and starving it
// would throw away a rescue that worked.
func stealAndRestore(board *core.Board, gp **grid, name string, pads []padLoc, victims []string, nickPower, fullRip bool, nets map[string][]padLoc, opts Options, tryDead, deadline time.Time, hasDeadline bool, rep *Report) bool {
	failedBefore := 0
	for _, n := range rep.PerNet {
		if n.Outcome.Status != "ok" {
			failedBefore++
		}
	}
	snapT := append([]core.Trace(nil), board.Traces...)
	snapV := append([]core.Via(nil), board.Vias...)
	snapRep := append([]NetResult(nil), rep.PerNet...)
	snapFailed := rep.Failed
	abort := func() {
		board.Traces = snapT
		board.Vias = snapV
		rep.PerNet = snapRep
		rep.Failed = snapFailed
		*gp = newGrid(board, opts)
	}

	for _, v := range victims {
		if fullRip {
			ripNetCopper(board, v)
		} else {
			ripLongHaul(board, v)
		}
	}
	var nickedPower []string
	if nickPower {
		for _, pwr := range powerNetsOn(board) {
			if pwr == name {
				continue
			}
			before := countNetTraces(board, pwr)
			ripTracesNearPads(board, pwr, pads, 2.2)
			if countNetTraces(board, pwr) < before {
				nickedPower = append(nickedPower, pwr)
			}
		}
	}

	*gp = newGrid(board, opts)
	g := *gp
	out := routeNet(board, g, name, pads, opts, tryDead, hasDeadline)
	if out.Status != "ok" {
		cheap := opts
		cheap.ViaCost = 2
		out = routeNet(board, g, name, pads, cheap, tryDead, hasDeadline)
	}
	if out.Status != "ok" {
		abort()
		return false
	}

	for _, v := range victims {
		if len(nets[v]) < 2 {
			continue
		}
		vout := routeNet(board, g, v, nets[v], opts, deadline, hasDeadline)
		if vout.Status != "ok" {
			// Victim must come back — a swap is not an improvement.
			abort()
			return false
		}
		markNetOK(rep, v, vout)
	}
	for _, pwr := range nickedPower {
		if len(nets[pwr]) < 2 {
			continue
		}
		pout := routeNet(board, g, pwr, nets[pwr], opts, deadline, hasDeadline)
		if pout.Status != "ok" {
			abort()
			return false
		}
		markNetOK(rep, pwr, pout)
	}

	markNetOK(rep, name, out)
	failedAfter := 0
	for _, n := range rep.PerNet {
		if n.Outcome.Status != "ok" {
			failedAfter++
		}
	}
	if failedAfter >= failedBefore {
		abort()
		return false
	}
	rep.TraceCount += out.TraceSegments
	rep.TotalLengthMM += out.LengthMM
	*gp = newGrid(board, opts)
	return true
}

func probeBlockingNets(board *core.Board, g *grid, name string, pads []padLoc, deadline time.Time, hasDeadline bool) []string {
	if g == nil || len(pads) < 2 {
		return nil
	}
	oldShare, oldPresent := g.share, g.present
	g.share = true
	g.present = 6
	defer func() {
		g.share = oldShare
		g.present = oldPresent
	}()

	seed := pickSeed(board, name, pads)
	connected := map[int]bool{seed: true}
	sources := existingNetSources(board, g, name, pads, connected)
	sources = append(sources, g.padSources(pads[seed])...)
	seen := map[string]bool{}
	var out []string
	for len(connected) < len(pads) {
		bestJ, bestD := -1, math.MaxFloat64
		for j := range pads {
			if connected[j] {
				continue
			}
			for i := range pads {
				if !connected[i] {
					continue
				}
				d := manhattanMM(pads[i].p, pads[j].p)
				if d < bestD {
					bestD, bestJ = d, j
				}
			}
		}
		if bestJ < 0 {
			break
		}
		goalP := pads[bestJ].p
		goalL := pads[bestJ].layer
		anyLayer := pads[bestJ].through
		if v, ok := closestNetVia(board, name, goalP, pads, bestJ); ok {
			goalP, anyLayer = v, true
		}
		path, ok := g.aStarMultiAt(sources, goalP, goalL, anyLayer, name, deadline, hasDeadline)
		if !ok && !anyLayer {
			// signalOtherLayer, not 1-goalL: the latter underflows on a
			// 4-layer stackup and asks for layer 255.
			path, ok = g.aStarMulti(sources, goalP, signalOtherLayer(g, goalL), name, deadline, hasDeadline)
		}
		if !ok {
			break
		}
		for _, n := range g.netsAlongPath(path, name) {
			if !seen[n] && netOrderKey(n) >= 3 {
				seen[n] = true
				out = append(out, n)
			}
		}
		connected[bestJ] = true
		if gx, gy, okc := g.worldToCell(goalP.X, goalP.Y); okc {
			sources = append(sources, cellKey{gx, gy, goalL})
		}
	}
	return out
}

func (g *grid) netsAlongPath(path []gpos, self string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(n string) {
		if n == "" || n == self || n == "*" || seen[n] {
			return
		}
		seen[n] = true
		out = append(out, n)
	}
	r := int(math.Ceil((g.opts.TraceWidthMM/2 + g.opts.padClear()) / g.cellMM))
	if r < 1 {
		r = 1
	}
	for _, p := range path {
		cx, cy, ok := g.worldToCell(p.x, p.y)
		if !ok {
			cx, cy = clampCell(core.NewPoint(p.x, p.y), g)
		}
		if int(p.layer) >= g.layers {
			continue
		}
		for dy := -r; dy <= r; dy++ {
			for dx := -r; dx <= r; dx++ {
				x, y := cx+dx, cy+dy
				if x < 0 || y < 0 || x >= g.w || y >= g.h {
					continue
				}
				idx := y*g.w + x
				// Pad copper is not negotiable.
				if g.padCell != nil && g.padCell[p.layer][idx] != "" && g.padCell[p.layer][idx] != self {
					continue
				}
				add(g.blocked[p.layer][idx])
			}
		}
	}
	return out
}

func pickRipVictims(board *core.Board, pads []padLoc, failed string, max int) []string {
	type hit struct {
		net string
		d   float64
	}
	by := map[string]float64{}
	for _, t := range board.Traces {
		if t.Net == failed || netOrderKey(t.Net) <= 2 || isFanoutStub(board, t) {
			continue
		}
		for _, p := range pads {
			d := distPointSeg(p.p.X.ToMM(), p.p.Y.ToMM(), t.Start.X.ToMM(), t.Start.Y.ToMM(), t.End.X.ToMM(), t.End.Y.ToMM())
			if old, ok := by[t.Net]; !ok || d < old {
				by[t.Net] = d
			}
		}
	}
	var hits []hit
	for n, d := range by {
		if d < 3.5 {
			hits = append(hits, hit{n, d})
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].d < hits[j].d })
	var out []string
	for _, h := range hits {
		if len(out) >= max {
			break
		}
		out = append(out, h.net)
	}
	return out
}

func ripLongHaul(board *core.Board, net string) {
	kept := board.Traces[:0]
	for _, t := range board.Traces {
		if t.Net == net && !isFanoutStub(board, t) {
			continue
		}
		kept = append(kept, t)
	}
	board.Traces = kept
}

func ripTracesNearPads(board *core.Board, net string, pads []padLoc, radius float64) {
	kept := board.Traces[:0]
	for _, t := range board.Traces {
		if t.Net != net || isFanoutStub(board, t) {
			kept = append(kept, t)
			continue
		}
		near := false
		for _, p := range pads {
			if distPointSeg(p.p.X.ToMM(), p.p.Y.ToMM(), t.Start.X.ToMM(), t.Start.Y.ToMM(), t.End.X.ToMM(), t.End.Y.ToMM()) < radius {
				near = true
				break
			}
		}
		if !near {
			kept = append(kept, t)
		}
	}
	board.Traces = kept
}

func isFanoutStub(board *core.Board, t core.Trace) bool {
	if hypotMM(t.Start, t.End) > fanoutStubMaxMM {
		return false
	}
	for _, fp := range footprintsStable(board) {
		for i := range fp.Pads {
			pad := &fp.Pads[i]
			if pad.Net == nil || *pad.Net != t.Net {
				continue
			}
			c := core.PadWorldCenter(fp, pad)
			if hypotMM(c, t.Start) < fanoutStubMaxMM || hypotMM(c, t.End) < fanoutStubMaxMM {
				return true
			}
		}
	}
	return false
}

func netsNearPads(nets map[string][]padLoc, pads []padLoc, skip string, radius float64) []string {
	var out []string
	seen := map[string]bool{skip: true}
	for n, ps := range nets {
		if seen[n] || netOrderKey(n) <= 2 || len(ps) < 2 || len(ps) > 6 {
			continue
		}
		hit := false
		for _, a := range pads {
			for _, b := range ps {
				if math.Hypot(a.p.X.ToMM()-b.p.X.ToMM(), a.p.Y.ToMM()-b.p.Y.ToMM()) < radius {
					hit = true
					break
				}
			}
			if hit {
				break
			}
		}
		if hit {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func powerNetsOn(board *core.Board) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range board.Traces {
		if netOrderKey(t.Net) <= 1 && !seen[t.Net] {
			seen[t.Net] = true
			out = append(out, t.Net)
		}
	}
	return out
}

func countNetTraces(board *core.Board, net string) int {
	n := 0
	for _, t := range board.Traces {
		if t.Net == net {
			n++
		}
	}
	return n
}

func uniqueNets(in []string, skip string) []string {
	seen := map[string]bool{skip: true}
	var out []string
	for _, n := range in {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

func markNetOK(rep *Report, name string, out Outcome) {
	for i := range rep.PerNet {
		if rep.PerNet[i].Net != name {
			continue
		}
		was := rep.PerNet[i].Outcome.Status
		rep.PerNet[i].Outcome = out
		if was != "ok" && out.Status == "ok" && rep.Failed > 0 {
			rep.Failed--
		}
		if was == "ok" && out.Status != "ok" {
			rep.Failed++
		}
		return
	}
	rep.PerNet = append(rep.PerNet, NetResult{Net: name, Outcome: out})
}

func markNetFailed(rep *Report, name string) {
	for i := range rep.PerNet {
		if rep.PerNet[i].Net == name && rep.PerNet[i].Outcome.Status == "ok" {
			rep.PerNet[i].Outcome.Status = "failed"
			rep.PerNet[i].Outcome.Reason = "negotiate-victim"
			rep.Failed++
			return
		}
	}
}
