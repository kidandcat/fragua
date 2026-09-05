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

type cand struct {
	fp   *core.Footprint
	nets []string
}

// PullPassivesToAnchors seats 2–4 pad passives on a ring outside a
// specific same-net IC/connector pad (Rust decouple.rs process).
func PullPassivesToAnchors(board *core.Board, movable []*core.Footprint, opts Options) {
	if board.Outline == nil || len(movable) == 0 {
		return
	}
	// Power-island seating packs to courtyard clearance (~0.35 mm), not the
	// SA assembly gap (DefaultOptions uses MinFootprintGapMM = 2 mm). With a
	// 2 mm floor the boost inductor never cleared U3's body_rect and stayed
	// parked on the outline — LX/IN then failed to route.
	hard := math.Max(opts.MinClearanceMM, 0.35)
	if opts.SolderGapMM > 0 && opts.SolderGapMM < hard {
		hard = opts.SolderGapMM
	}
	movableSet := map[string]bool{}
	for _, fp := range movable {
		movableSet[fp.ID.String()] = true
	}

	var cands []cand
	for _, fp := range movable {
		if fp == nil || fp.Pinned || fp.EdgeMounted || len(fp.Pads) < 2 || len(fp.Pads) > 4 {
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
	// SA leaves passives scattered on the island. Park them on the outline
	// edge so a high-priority inductor can claim LX/IN without colliding
	// with a Cin that still sits where the annealer dumped it.
	parkPassives(board, cands)

	// Inductors and output caps claim their IC pads first so Cin does not
	// steal LX / OUT. HF (smaller) output caps sit closer than bulk.
	sort.Slice(cands, func(i, j int) bool {
		pi, pj := passivePriority(cands[i]), passivePriority(cands[j])
		if pi != pj {
			return pi < pj
		}
		if pi == priHFCap {
			si, sj := passiveAreaMM2(cands[i].fp), passiveAreaMM2(cands[j].fp)
			if si != sj {
				return si < sj
			}
		}
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
			kind  int
			gnd   bool
			count int
			name  string
		}
		var ranks []rank
		for _, n := range c.nets {
			ranks = append(ranks, rank{passiveNetKind(c.fp, n), isGround(n), netCount[n], n})
		}
		sort.Slice(ranks, func(i, j int) bool {
			if ranks[i].kind != ranks[j].kind {
				return ranks[i].kind < ranks[j].kind
			}
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
			if as[i].ok {
				continue
			}
			if seatInductorBridge(board, as[i].fp, pins, hard, opts) {
				as[i].ok = true
				continue
			}
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

const (
	priInductor = 0
	priHFCap    = 1
	priOther    = 2
)

func passivePriority(c cand) int {
	if isInductor(c.fp) {
		return priInductor
	}
	if hasOutAndGND(c.nets) {
		return priHFCap
	}
	return priOther
}

func passiveAreaMM2(fp *core.Footprint) float64 {
	b, ok := footprintBounds(fp)
	if !ok {
		return 0
	}
	return b.Width().ToMM() * b.Height().ToMM()
}

func hasOutAndGND(nets []string) bool {
	out, gnd := false, false
	for _, n := range nets {
		if isGround(n) {
			gnd = true
		}
		if isPowerOutNet(n) {
			out = true
		}
	}
	return out && gnd
}

func passiveNetKind(fp *core.Footprint, net string) int {
	if isInductor(fp) {
		if isSwitchNet(net) {
			return 0
		}
		if isPowerInNet(net) {
			return 1
		}
		if isGround(net) {
			return 4
		}
		return 2
	}
	if isPowerOutNet(net) {
		return 0
	}
	if isPowerInNet(net) {
		return 1
	}
	if isGround(net) {
		return 4
	}
	return 2
}

func isInductor(fp *core.Footprint) bool {
	if fp == nil {
		return false
	}
	ref := strings.ToUpper(strings.TrimSpace(fp.Reference))
	if len(ref) > 0 && ref[0] == 'L' && (len(ref) == 1 || !isAlphaByte(ref[1])) {
		return true
	}
	blob := strings.ToLower(fp.Key + " " + fp.Library + " " + fp.Description + " " + fp.Value)
	if strings.Contains(blob, "inductor") {
		return true
	}
	return false
}

func isAlphaByte(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

func isSwitchNet(net string) bool {
	u := strings.ToUpper(strings.TrimPrefix(net, "+"))
	switch u {
	case "LX", "SW", "SWITCH", "VSW", "SWX", "SWITCHING":
		return true
	}
	return strings.HasPrefix(u, "LX") || strings.HasPrefix(u, "SW")
}

func isPowerInNet(net string) bool {
	u := strings.ToUpper(strings.TrimPrefix(net, "+"))
	switch u {
	case "IN", "VIN", "VBAT", "VSYS", "PVIN", "PVCC":
		return true
	}
	return u == "VIN" || strings.HasSuffix(u, "_IN") || strings.HasPrefix(u, "IN")
}

func isPowerOutNet(net string) bool {
	u := strings.ToUpper(strings.TrimPrefix(net, "+"))
	switch u {
	case "OUT", "VOUT", "VCC", "VDD", "3V3", "5V", "1V8", "1V2":
		return true
	}
	return strings.Contains(u, "OUT") || strings.HasPrefix(u, "3V") || strings.HasPrefix(u, "5V")
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
	if fp == nil || fp.Pinned || pin.fp == nil || board.Outline == nil {
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
			if firstOverlapperGap(board, fp, hard) || hitsNoPlace(board, fp) {
				continue
			}
			return true
		}
	}
	fp.Position, fp.Rotation = oldPos, oldRot
	return false
}

func pullToCentroid(board *core.Board, fp *core.Footprint, nets []string, movableSet map[string]bool, hard float64, opts Options) {
	if fp == nil || fp.Pinned || board.Outline == nil {
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
			firstOverlapperGap(board, fp, hard) || hitsNoPlace(board, fp) {
			continue
		}
		return
	}
	fp.Position = core.NewPoint(old.x, old.y)
	fp.Rotation = old.rot
}


func parkPassives(board *core.Board, cands []cand) {
	if board == nil || board.Outline == nil {
		return
	}
	o := board.Outline
	x := o.Max.X.ToMM() - 2.0
	y0 := o.Min.Y.ToMM() + 2.0
	for i, c := range cands {
		if c.fp == nil || c.fp.Pinned || c.fp.EdgeMounted {
			continue
		}
		y := y0 + float64(i)*3.5
		if y > o.Max.Y.ToMM()-2.0 {
			y = o.Max.Y.ToMM() - 2.0 - float64(i%3)*0.2
			x = o.Min.X.ToMM() + 2.0
		}
		c.fp.Position = core.NewPoint(core.FromMM(x), core.FromMM(y))
	}
}

// seatInductorBridge places an LX/IN (or SW/VIN) inductor so its pads sit
// next to the matching IC pads on the power island — pad-to-pad, not on a
// ring around the whole IC body. That is what makes a boost LX hop short
// enough for a same-layer route with no via.
func seatInductorBridge(board *core.Board, fp *core.Footprint, pins map[string][]anchorPin, hard float64, opts Options) bool {
	if fp == nil || fp.Pinned || board == nil || board.Outline == nil || !isInductor(fp) {
		return false
	}
	var swNet, inNet string
	swOwn, inOwn := -1, -1
	for i := range fp.Pads {
		if fp.Pads[i].Net == nil || *fp.Pads[i].Net == "" {
			continue
		}
		n := *fp.Pads[i].Net
		if isSwitchNet(n) && swOwn < 0 {
			swNet, swOwn = n, i
		}
		if isPowerInNet(n) && inOwn < 0 {
			inNet, inOwn = n, i
		}
	}
	if swNet == "" || inNet == "" || swOwn < 0 || inOwn < 0 {
		return false
	}
	swList, inList := pins[swNet], pins[inNet]
	if len(swList) == 0 || len(inList) == 0 {
		return false
	}
	swPin, inPin := &swList[0], &inList[0]
	for i := range swList {
		for j := range inList {
			if swList[i].fp.ID == inList[j].fp.ID {
				swPin, inPin = &swList[i], &inList[j]
			}
		}
	}
	sx, sy := swPin.centre[0], swPin.centre[1]
	ix, iy := inPin.centre[0], inPin.centre[1]
	mx, my := (sx+ix)/2, (sy+iy)/2
	ax := (swPin.fp.Position.X.ToMM() + inPin.fp.Position.X.ToMM()) / 2
	ay := (swPin.fp.Position.Y.ToMM() + inPin.fp.Position.Y.ToMM()) / 2
	alongX, alongY := ix-sx, iy-sy
	alongLen := math.Hypot(alongX, alongY)
	if alongLen < 1e-6 {
		return false
	}
	alongX, alongY = alongX/alongLen, alongY/alongLen
	outX, outY := -alongY, alongX
	if (mx-ax)*outX+(my-ay)*outY < 0 {
		outX, outY = -outX, -outY
	}

	oldPos, oldRot := fp.Position, fp.Rotation
	bestScore := math.Inf(1)
	bestPos := oldPos
	bestRot := oldRot
	found := false
	clearances := []float64{1.2, 1.4, 1.6, 1.8, 2.0, 2.4, 2.8, 3.2}
	laterals := []float64{0, 0.4, -0.4, 0.8, -0.8, 1.2, -1.2}
	for q := 0; q < 4; q++ {
		rot := math.Mod(90*float64(q), 360)
		fp.Rotation = rot
		fp.Position = core.Origin
		os := core.PadWorldCenter(fp, &fp.Pads[swOwn])
		oi := core.PadWorldCenter(fp, &fp.Pads[inOwn])
		ownAlongX := oi.X.ToMM() - os.X.ToMM()
		ownAlongY := oi.Y.ToMM() - os.Y.ToMM()
		// Prefer rotations whose pad axis matches the IC LX→IN axis.
		align := math.Abs(ownAlongX*alongX + ownAlongY*alongY)
		ownLen := math.Hypot(ownAlongX, ownAlongY)
		if ownLen < 1e-6 {
			continue
		}
		align /= ownLen
		offSX, offSY := os.X.ToMM(), os.Y.ToMM()
		for _, d := range clearances {
			for _, lat := range laterals {
				tx := sx + outX*d + alongX*lat
				ty := sy + outY*d + alongY*lat
				fp.Position = core.NewPoint(core.FromMM(tx-offSX), core.FromMM(ty-offSY))
				fp.Rotation = rot
				if !padsInside(fp, board.Outline, opts.EdgeClearanceMM) {
					continue
				}
				if minGapAgainstOthers(board, fp) < hard+seatSlackMM {
					continue
				}
				if firstOverlapperGap(board, fp, hard) || hitsNoPlace(board, fp) {
					continue
				}
				ws := core.PadWorldCenter(fp, &fp.Pads[swOwn])
				wi := core.PadWorldCenter(fp, &fp.Pads[inOwn])
				dSW := math.Hypot(ws.X.ToMM()-sx, ws.Y.ToMM()-sy)
				dIN := math.Hypot(wi.X.ToMM()-ix, wi.Y.ToMM()-iy)
				// Reward pad-pair closeness and axis alignment.
				score := dSW*dSW + dIN*dIN - 0.5*align
				if score < bestScore {
					bestScore = score
					bestPos = fp.Position
					bestRot = rot
					found = true
				}
			}
		}
	}
	if !found {
		fp.Position, fp.Rotation = oldPos, oldRot
		return false
	}
	fp.Position, fp.Rotation = bestPos, bestRot
	return true
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
