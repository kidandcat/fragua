// Package router implements multi-layer grid autorouting (A* multi-source tree).
package router

import (
	"container/heap"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/mentasystems/fragua/internal/core"
)

// Options configures a route run.
type Options struct {
	CellMM        float64
	ClearanceMM   float64
	TraceWidthMM  float64
	ViaDrillMM    float64
	ViaDiameterMM float64
	ViaCost       float64
	MaxSeconds    float64
	Negotiate     bool
}

// DefaultOptions returns sensible 2-layer defaults.
// Cell pitch 0.25 mm matches the Rust router sweet spot; via cost prefers
// single-layer detours on layer 0 before punching vias.
func DefaultOptions() Options {
	return Options{
		CellMM:        0.25,
		ClearanceMM:   0.20,
		TraceWidthMM:  0.15,
		ViaDrillMM:    0.30,
		ViaDiameterMM: 0.60,
		ViaCost:       8.0,
		MaxSeconds:    60,
	}
}

// ParseOptions overlays key=value tokens from a script args string.
func ParseOptions(o Options, args string) Options {
	for _, f := range strings.Fields(args) {
		kv := strings.SplitN(f, "=", 2)
		if len(kv) != 2 {
			continue
		}
		k, v := kv[0], kv[1]
		var x float64
		fmt.Sscanf(v, "%f", &x)
		switch k {
		case "max_seconds":
			o.MaxSeconds = x
		case "clearance":
			o.ClearanceMM = x
		case "width", "trace_width":
			o.TraceWidthMM = x
		case "cell":
			o.CellMM = x
		case "via_cost":
			o.ViaCost = x
		case "negotiate":
			o.Negotiate = v == "true" || v == "1"
		}
	}
	return o
}

// Outcome for one net.
type Outcome struct {
	Status        string  `json:"status"` // ok | failed | skipped
	TraceSegments int     `json:"trace_segments,omitempty"`
	LengthMM      float64 `json:"length_mm,omitempty"`
	LowerBoundMM  float64 `json:"lower_bound_mm,omitempty"`
	Reason        string  `json:"reason,omitempty"`
}

// Report summarises a route call.
type Report struct {
	PerNet          []NetResult `json:"per_net"`
	TraceCount      int         `json:"trace_count"`
	ViaCount        int         `json:"via_count"`
	Iterations      int         `json:"iterations"`
	TotalLengthMM   float64     `json:"total_length_mm"`
	TotalLowerBound float64     `json:"total_lower_bound_mm"`
	Failed          int         `json:"failed"`
	ElapsedMS       int64       `json:"elapsed_ms"`
}

// NetResult pairs a net name with its outcome.
type NetResult struct {
	Net     string  `json:"net"`
	Outcome Outcome `json:"outcome"`
}

// Summary is agent-friendly text.
func (r Report) Summary() string {
	ok := 0
	for _, n := range r.PerNet {
		if n.Outcome.Status == "ok" {
			ok++
		}
	}
	return fmt.Sprintf("route: %d/%d nets ok, %d traces, %d vias, %.1f mm copper, %d ms",
		ok, len(r.PerNet), r.TraceCount, r.ViaCount, r.TotalLengthMM, r.ElapsedMS)
}

type padLoc struct {
	ref   string
	p     core.Point
	layer uint8
}

// Route autoroutes all multi-pad nets on the board. Mutates board traces/vias
// (appends to existing copper unless caller cleared first).
func Route(board *core.Board, opts Options) Report {
	start := time.Now()
	maxSec := opts.MaxSeconds
	if maxSec <= 0 {
		maxSec = 60
	}
	deadline := start.Add(time.Duration(maxSec * float64(time.Second)))

	rep := Report{Iterations: 1}

	nets := map[string][]padLoc{}
	for _, fp := range board.Footprints {
		if fp == nil {
			continue
		}
		for i := range fp.Pads {
			pad := &fp.Pads[i]
			if pad.Net == nil || *pad.Net == "" {
				continue
			}
			layer := pad.Layer.Index
			nets[*pad.Net] = append(nets[*pad.Net], padLoc{
				ref:   fp.Reference,
				p:     core.PadWorldCenter(fp, pad),
				layer: layer,
			})
		}
	}

	names := make([]string, 0, len(nets))
	for n := range nets {
		names = append(names, n)
	}
	// Fewest pads first (easier nets claim less board early).
	sort.Slice(names, func(i, j int) bool {
		if len(nets[names[i]]) != len(nets[names[j]]) {
			return len(nets[names[i]]) < len(nets[names[j]])
		}
		return names[i] < names[j]
	})

	g := newGrid(board, opts)

	var failedNets []string
	for _, name := range names {
		pads := nets[name]
		if len(pads) < 2 {
			rep.PerNet = append(rep.PerNet, NetResult{Net: name, Outcome: Outcome{Status: "skipped", Reason: "single pad"}})
			continue
		}
		if time.Now().After(deadline) {
			rep.PerNet = append(rep.PerNet, NetResult{Net: name, Outcome: Outcome{Status: "failed", Reason: "budget"}})
			rep.Failed++
			failedNets = append(failedNets, name)
			continue
		}

		out := routeNet(board, g, name, pads, opts, deadline)
		rep.PerNet = append(rep.PerNet, NetResult{Net: name, Outcome: out})
		if out.Status == "ok" {
			rep.TraceCount += out.TraceSegments
			rep.TotalLengthMM += out.LengthMM
			rep.TotalLowerBound += out.LowerBoundMM
			// Count vias just laid for this net (board already mutated).
		} else {
			rep.Failed++
			failedNets = append(failedNets, name)
		}
	}

	// Count vias on board that belong to routed nets (laid this run + prior).
	// Prefer delta from board after route: recount vias with nets we marked ok.
	okNets := map[string]bool{}
	for _, n := range rep.PerNet {
		if n.Outcome.Status == "ok" {
			okNets[n.Net] = true
		}
	}
	viaCount := 0
	for _, v := range board.Vias {
		if okNets[v.Net] {
			viaCount++
		}
	}
	rep.ViaCount = viaCount

	// Light second pass: retry failed nets once copper from successes is fixed.
	// Partial copper of failed nets is never committed, so corridors may open.
	if len(failedNets) > 0 && time.Now().Before(deadline) {
		rep.Iterations = 2
		retry := make([]string, 0, len(failedNets))
		for _, name := range failedNets {
			if time.Now().After(deadline) {
				break
			}
			pads := nets[name]
			out := routeNet(board, g, name, pads, opts, deadline)
			// Replace prior outcome for this net.
			for i := range rep.PerNet {
				if rep.PerNet[i].Net == name {
					if out.Status == "ok" {
						if rep.PerNet[i].Outcome.Status != "ok" {
							rep.Failed--
						}
						rep.TraceCount += out.TraceSegments
						rep.TotalLengthMM += out.LengthMM
						rep.TotalLowerBound += out.LowerBoundMM
					}
					rep.PerNet[i].Outcome = out
					break
				}
			}
			if out.Status != "ok" {
				retry = append(retry, name)
			}
		}
		_ = retry
		// Refresh via count after retry.
		viaCount = 0
		okNets = map[string]bool{}
		for _, n := range rep.PerNet {
			if n.Outcome.Status == "ok" {
				okNets[n.Net] = true
			}
		}
		for _, v := range board.Vias {
			if okNets[v.Net] {
				viaCount++
			}
		}
		rep.ViaCount = viaCount
	}

	rep.ElapsedMS = time.Since(start).Milliseconds()
	return rep
}

func routeNet(board *core.Board, g *grid, name string, pads []padLoc, opts Options, deadline time.Time) Outcome {
	// MST-style lower bound: sum of nearest-neighbour edges in pad order
	// (pad 0 as seed, then greedy closest remaining).
	lb := lowerBoundMM(pads)

	// Prim growth: connected set starts with pad 0; multi-source A* from
	// all same-net tree cells to the nearest unconnected pad.
	connected := map[int]bool{0: true}
	// Grid sources for multi-source search (pad + path cells).
	sources := []cellKey{}
	if sx, sy, ok := g.worldToCell(pads[0].p.X, pads[0].p.Y); ok {
		sources = append(sources, cellKey{sx, sy, pads[0].layer})
	} else {
		sx, sy := clampCell(pads[0].p, g)
		sources = append(sources, cellKey{sx, sy, pads[0].layer})
	}

	// Buffer board copper until the whole net succeeds.
	type pendingTrace struct {
		tr core.Trace
	}
	type pendingVia struct {
		v core.Via
	}
	var pendT []pendingTrace
	var pendV []pendingVia
	// Grid paints to apply on success (and already applied for search
	// continuity within this net — rolled back on failure).
	type paintRec struct {
		x, y int
		l    uint8
		prev string
	}
	var paints []paintRec
	recordPaint := func(cx, cy int, layer uint8, net string, radius int) {
		for dy := -radius; dy <= radius; dy++ {
			for dx := -radius; dx <= radius; dx++ {
				x, y := cx+dx, cy+dy
				if x < 0 || y < 0 || x >= g.w || y >= g.h {
					continue
				}
				idx := y*g.w + x
				cur := g.blocked[layer][idx]
				// Own-net and free cells may be claimed; never overwrite foreign or hard obstacles.
				if cur == "" || cur == net {
					paints = append(paints, paintRec{x, y, layer, cur})
					g.blocked[layer][idx] = net
				}
			}
		}
	}
	rollback := func() {
		// Restore in reverse so intermediate overwrites unwind correctly.
		for i := len(paints) - 1; i >= 0; i-- {
			p := paints[i]
			g.blocked[p.l][p.y*g.w+p.x] = p.prev
		}
		paints = nil
	}

	copperR := g.copperRadius()
	segs := 0
	length := 0.0
	failed := false
	reason := "unreachable"

	for len(connected) < len(pads) {
		if time.Now().After(deadline) {
			failed = true
			reason = "budget"
			break
		}
		// Pick unconnected pad closest (Manhattan) to any connected pad.
		// Multi-source A* still branches off the whole tree, not just that pad.
		bestJ := -1
		bestD := math.MaxFloat64
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
			failed = true
			break
		}

		goalLayer := pads[bestJ].layer
		path, ok := g.aStarMulti(sources, pads[bestJ].p, goalLayer, name, deadline)
		if !ok {
			failed = true
			break
		}
		// Already on the tree (goal cell is a multi-source seed) — no copper to lay.
		if len(path) < 2 {
			if gx, gy, okc := g.worldToCell(pads[bestJ].p.X, pads[bestJ].p.Y); okc {
				sources = append(sources, cellKey{gx, gy, goalLayer})
			}
			connected[bestJ] = true
			continue
		}

		w := core.FromMM(opts.TraceWidthMM)
		for k := 0; k < len(path)-1; k++ {
			a, b := path[k], path[k+1]
			if a.layer != b.layer {
				pendV = append(pendV, pendingVia{v: core.Via{
					ID:       core.NewID(),
					Position: core.NewPoint(a.x, a.y),
					Drill:    core.FromMM(opts.ViaDrillMM),
					Diameter: core.FromMM(opts.ViaDiameterMM),
					Net:      name,
				}})
				// Stamp via barrel as own-net copper on all layers (not hard obstacle).
				cx, cy, okc := g.worldToCell(a.x, a.y)
				if okc {
					for L := uint8(0); L < uint8(g.layers); L++ {
						recordPaint(cx, cy, L, name, copperR)
						sources = append(sources, cellKey{cx, cy, L})
					}
				}
				continue
			}
			pendT = append(pendT, pendingTrace{tr: core.Trace{
				ID:    core.NewID(),
				Layer: core.Layer{Index: a.layer},
				Start: core.NewPoint(a.x, a.y),
				End:   core.NewPoint(b.x, b.y),
				Width: w,
				Net:   name,
			}})
			// Stamp segment cells as own-net and add as future sources.
			c0x, c0y, ok0 := g.worldToCell(a.x, a.y)
			c1x, c1y, ok1 := g.worldToCell(b.x, b.y)
			if ok0 || ok1 {
				if !ok0 {
					c0x, c0y = c1x, c1y
				}
				if !ok1 {
					c1x, c1y = c0x, c0y
				}
				for _, c := range bresenham(c0x, c0y, c1x, c1y) {
					recordPaint(c[0], c[1], a.layer, name, copperR)
					sources = append(sources, cellKey{c[0], c[1], a.layer})
				}
			}
			segs++
			length += hypotMM(core.NewPoint(a.x, a.y), core.NewPoint(b.x, b.y))
		}
		// Goal pad joins the tree.
		if gx, gy, ok := g.worldToCell(pads[bestJ].p.X, pads[bestJ].p.Y); ok {
			sources = append(sources, cellKey{gx, gy, goalLayer})
		}
		connected[bestJ] = true
	}

	if failed {
		rollback()
		return Outcome{Status: "failed", Reason: reason, LowerBoundMM: lb}
	}

	// Commit buffered copper to the board (grid already painted).
	for _, p := range pendT {
		board.Traces = append(board.Traces, p.tr)
	}
	for _, p := range pendV {
		board.Vias = append(board.Vias, p.v)
	}
	return Outcome{
		Status: "ok", TraceSegments: segs, LengthMM: length, LowerBoundMM: lb,
	}
}

func lowerBoundMM(pads []padLoc) float64 {
	if len(pads) < 2 {
		return 0
	}
	// Greedy MST-ish: Prim on complete graph with Manhattan distances.
	in := make([]bool, len(pads))
	in[0] = true
	total := 0.0
	for n := 1; n < len(pads); n++ {
		best := math.MaxFloat64
		bestJ := -1
		for j := range pads {
			if in[j] {
				continue
			}
			for i := range pads {
				if !in[i] {
					continue
				}
				d := manhattanMM(pads[i].p, pads[j].p)
				if d < best {
					best, bestJ = d, j
				}
			}
		}
		if bestJ < 0 {
			break
		}
		in[bestJ] = true
		total += best
	}
	return total
}

func manhattanMM(a, b core.Point) float64 {
	dx := math.Abs(float64(a.X-b.X)) / 1e6
	dy := math.Abs(float64(a.Y-b.Y)) / 1e6
	return dx + dy
}

func hypotMM(a, b core.Point) float64 {
	dx := float64(a.X-b.X) / 1e6
	dy := float64(a.Y-b.Y) / 1e6
	return math.Hypot(dx, dy)
}

// --- grid A* ---

type gpos struct {
	x, y  core.Length
	layer uint8
}

type cellKey struct {
	x, y int
	l    uint8
}

type grid struct {
	originX, originY core.Length
	cell             core.Length
	cellMM           float64
	w, h             int
	layers           int
	// blocked[layer][y*w+x] = net name occupying, "" free, "*" hard obstacle.
	// Own-net pads/traces store the net name so the same net may re-enter
	// (critical for multi-pad tree growth and vias).
	blocked [][]string
	opts    Options
	board   *core.Board
}

// maxGridDim caps cells per axis; larger boards coarsen the cell pitch.
const maxGridDim = 320

func newGrid(board *core.Board, opts Options) *grid {
	cellMM := opts.CellMM
	if cellMM < 0.05 {
		cellMM = 0.05
	}
	var o core.Rect
	if board.Outline != nil {
		o = *board.Outline
	} else {
		o = core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(100), core.FromMM(80)))
	}
	edge := core.FromMM(opts.ClearanceMM)
	minX := o.Min.X + edge
	minY := o.Min.Y + edge
	maxX := o.Max.X - edge
	maxY := o.Max.Y - edge
	if maxX <= minX || maxY <= minY {
		minX, minY = o.Min.X, o.Min.Y
		maxX, maxY = o.Max.X, o.Max.Y
	}
	widthMM := float64(maxX-minX) / 1e6
	heightMM := float64(maxY-minY) / 1e6
	// Adaptive coarsening so large boards stay searchable.
	if widthMM/cellMM > maxGridDim {
		cellMM = widthMM / maxGridDim
	}
	if heightMM/cellMM > maxGridDim {
		c2 := heightMM / maxGridDim
		if c2 > cellMM {
			cellMM = c2
		}
	}
	cell := core.FromMM(cellMM)
	opts.CellMM = cellMM

	gw := int((maxX-minX)/cell) + 1
	gh := int((maxY-minY)/cell) + 1
	if gw < 2 {
		gw = 2
	}
	if gh < 2 {
		gh = 2
	}
	layers := board.StackupOrDefault().CopperCount()
	if layers < 2 {
		layers = 2
	}
	g := &grid{
		originX: minX, originY: minY, cell: cell, cellMM: cellMM,
		w: gw, h: gh, layers: layers,
		blocked: make([][]string, layers),
		opts:    opts, board: board,
	}
	for L := 0; L < layers; L++ {
		g.blocked[L] = make([]string, gw*gh)
	}

	// Stamp copper keepouts as hard obstacles on all layers.
	for _, k := range board.Keepouts {
		if k.Rect == nil {
			continue
		}
		// Place-only keepouts do not block copper.
		if k.NoPlace && !k.NoCopper {
			continue
		}
		g.stampRectObstacle(*k.Rect)
	}

	// Stamp pads: own-net cells are labelled with the net (walkable for that
	// net only). No-net pads are hard obstacles ("*").
	clearanceCells := int(math.Ceil(opts.ClearanceMM / cellMM))
	if clearanceCells < 1 {
		clearanceCells = 1
	}
	for _, fp := range board.Footprints {
		if fp == nil {
			continue
		}
		for i := range fp.Pads {
			pad := &fp.Pads[i]
			net := ""
			if pad.Net != nil {
				net = *pad.Net
			}
			c := core.PadWorldCenter(fp, pad)
			cx, cy, ok := g.worldToCell(c.X, c.Y)
			if !ok {
				continue
			}
			pw := int(pad.Size[0]/cell)/2 + clearanceCells
			ph := int(pad.Size[1]/cell)/2 + clearanceCells
			// Through-hole (drill set): copper on all layers.
			// SMD: pad layer only (default top).
			layersToStamp := []uint8{pad.Layer.Index}
			if pad.Drill != nil && *pad.Drill > 0 {
				layersToStamp = make([]uint8, layers)
				for L := 0; L < layers; L++ {
					layersToStamp[L] = uint8(L)
				}
			}
			label := net
			if label == "" {
				label = "*"
			}
			for _, layer := range layersToStamp {
				if int(layer) >= layers {
					layer = 0
				}
				for dy := -ph; dy <= ph; dy++ {
					for dx := -pw; dx <= pw; dx++ {
						x, y := cx+dx, cy+dy
						if x < 0 || y < 0 || x >= g.w || y >= g.h {
							continue
						}
						idx := y*g.w + x
						cur := g.blocked[layer][idx]
						// Hard obstacles stick; otherwise first copper wins.
						if cur == "*" {
							continue
						}
						if cur == "" {
							g.blocked[layer][idx] = label
						} else if cur != label && label == "*" {
							g.blocked[layer][idx] = "*"
						}
						// Foreign nets: leave the earlier claim (pad vs pad).
					}
				}
			}
		}
	}
	// Existing board copper (prior routes).
	for _, tr := range board.Traces {
		g.blockSeg(tr.Start.X, tr.Start.Y, tr.End.X, tr.End.Y, tr.Layer.Index, tr.Net)
	}
	for _, v := range board.Vias {
		// Via copper belongs to its net on every layer — not a hard wall.
		g.blockViaNet(v.Position.X, v.Position.Y, v.Net)
	}
	return g
}

func (g *grid) copperRadius() int {
	// Bare copper half-width in cells (clearance enforced via pad halos +
	// neighbour walkability; keep stamp tight so own-net tree cells stay dense).
	r := int(math.Ceil((g.opts.TraceWidthMM / 2) / g.cellMM))
	if r < 0 {
		r = 0
	}
	return r
}

func (g *grid) stampRectObstacle(r core.Rect) {
	c0x, c0y, ok0 := g.worldToCell(r.Min.X, r.Min.Y)
	c1x, c1y, ok1 := g.worldToCell(r.Max.X, r.Max.Y)
	if !ok0 && !ok1 {
		// Clamp corners into grid.
		c0x, c0y = clampCell(core.NewPoint(r.Min.X, r.Min.Y), g)
		c1x, c1y = clampCell(core.NewPoint(r.Max.X, r.Max.Y), g)
	} else {
		if !ok0 {
			c0x, c0y = clampCell(core.NewPoint(r.Min.X, r.Min.Y), g)
		}
		if !ok1 {
			c1x, c1y = clampCell(core.NewPoint(r.Max.X, r.Max.Y), g)
		}
	}
	if c0x > c1x {
		c0x, c1x = c1x, c0x
	}
	if c0y > c1y {
		c0y, c1y = c1y, c0y
	}
	for L := uint8(0); L < uint8(g.layers); L++ {
		for y := c0y; y <= c1y; y++ {
			for x := c0x; x <= c1x; x++ {
				if x < 0 || y < 0 || x >= g.w || y >= g.h {
					continue
				}
				g.blocked[L][y*g.w+x] = "*"
			}
		}
	}
}

func (g *grid) worldToCell(x, y core.Length) (int, int, bool) {
	cx := int((x - g.originX) / g.cell)
	cy := int((y - g.originY) / g.cell)
	if cx < 0 || cy < 0 || cx >= g.w || cy >= g.h {
		return 0, 0, false
	}
	return cx, cy, true
}

func (g *grid) cellToWorld(cx, cy int) (core.Length, core.Length) {
	return g.originX + core.Length(cx)*g.cell + g.cell/2,
		g.originY + core.Length(cy)*g.cell + g.cell/2
}

func (g *grid) blockSeg(x0, y0, x1, y1 core.Length, layer uint8, net string) {
	if int(layer) >= g.layers {
		layer = 0
	}
	c0x, c0y, ok0 := g.worldToCell(x0, y0)
	c1x, c1y, ok1 := g.worldToCell(x1, y1)
	if !ok0 && !ok1 {
		return
	}
	if !ok0 {
		c0x, c0y = c1x, c1y
	}
	if !ok1 {
		c1x, c1y = c0x, c0y
	}
	r := g.copperRadius()
	for _, c := range bresenham(c0x, c0y, c1x, c1y) {
		g.paint(c[0], c[1], layer, net, r)
	}
}

func (g *grid) blockViaNet(x, y core.Length, net string) {
	if net == "" {
		net = "*"
	}
	cx, cy, ok := g.worldToCell(x, y)
	if !ok {
		return
	}
	r := g.copperRadius()
	if r < 1 {
		r = 1
	}
	for L := uint8(0); L < uint8(g.layers); L++ {
		g.paint(cx, cy, L, net, r)
	}
}

func (g *grid) paint(cx, cy int, layer uint8, net string, radius int) {
	for dy := -radius; dy <= radius; dy++ {
		for dx := -radius; dx <= radius; dx++ {
			x, y := cx+dx, cy+dy
			if x < 0 || y < 0 || x >= g.w || y >= g.h {
				continue
			}
			idx := y*g.w + x
			cur := g.blocked[layer][idx]
			if cur == "*" {
				continue
			}
			if cur == "" || cur == net {
				g.blocked[layer][idx] = net
			} else if net == "*" {
				g.blocked[layer][idx] = "*"
			}
			// Foreign net copper left in place.
		}
	}
}

func (g *grid) passable(cx, cy int, layer uint8, net string) bool {
	if cx < 0 || cy < 0 || cx >= g.w || cy >= g.h {
		return false
	}
	if int(layer) >= g.layers {
		return false
	}
	cur := g.blocked[layer][cy*g.w+cx]
	// Free or same-net copper (pads, traces, vias) — never hard obstacles.
	return cur == "" || cur == net
}

// aStarMulti is multi-source A*: every source cell starts at g=0 so later
// spokes branch off the nearest point of the existing same-net tree.
// Prefers layer 0 (extra cost for non-zero layers); vias cost ViaCost.
func (g *grid) aStarMulti(sources []cellKey, to core.Point, goalLayer uint8, net string, deadline time.Time) ([]gpos, bool) {
	tx, ty, ok2 := g.worldToCell(to.X, to.Y)
	if !ok2 {
		tx, ty = clampCell(to, g)
	}
	if int(goalLayer) >= g.layers {
		goalLayer = 0
	}
	goal := cellKey{tx, ty, goalLayer}

	open := &astHeap{}
	heap.Init(open)
	came := map[cellKey]cellKey{}
	gScore := map[cellKey]float64{}
	closed := map[cellKey]bool{}

	if len(sources) == 0 {
		sources = []cellKey{{tx, ty, 0}} // degenerate
	}
	// Dedup sources.
	seenSrc := map[cellKey]bool{}
	for _, s := range sources {
		if int(s.l) >= g.layers {
			s.l = 0
		}
		if s.x < 0 || s.y < 0 || s.x >= g.w || s.y >= g.h {
			continue
		}
		if !g.passable(s.x, s.y, s.l, net) {
			// Force-allow source even if something odd; own pad should pass.
			// If truly blocked by foreign, skip.
			cur := g.blocked[s.l][s.y*g.w+s.x]
			if cur != "" && cur != net {
				continue
			}
		}
		if seenSrc[s] {
			continue
		}
		seenSrc[s] = true
		gScore[s] = 0
		heap.Push(open, &astNode{k: s, g: 0, f: heuristic(s, goal)})
	}
	if open.Len() == 0 {
		return nil, false
	}
	// Goal already in the tree — trivial path (single cell).
	if _, ok := gScore[goal]; ok {
		wx, wy := g.cellToWorld(goal.x, goal.y)
		return []gpos{{x: wx, y: wy, layer: goal.l}}, true
	}

	// 8-connected on a layer (orthogonal + diagonal).
	dirs := [][2]int{
		{1, 0}, {-1, 0}, {0, 1}, {0, -1},
		{1, 1}, {1, -1}, {-1, 1}, {-1, -1},
	}

	maxExpand := g.w * g.h * g.layers * 8
	if maxExpand < 10000 {
		maxExpand = 10000
	}
	expanded := 0
	// Check deadline every N expansions.
	const deadlineEvery = 256

	for open.Len() > 0 && expanded < maxExpand {
		expanded++
		if expanded%deadlineEvery == 0 && time.Now().After(deadline) {
			return nil, false
		}
		cur := heap.Pop(open).(*astNode)
		// Goal must match layer: SMD pads only exist on their copper layer.
		// (Through-hole pads are stamped on every layer, so goalLayer still works.)
		if cur.k.x == goal.x && cur.k.y == goal.y && cur.k.l == goal.l {
			return g.reconstruct(came, cur.k), true
		}
		if closed[cur.k] {
			continue
		}
		closed[cur.k] = true

		for _, d := range dirs {
			nx, ny := cur.k.x+d[0], cur.k.y+d[1]
			if !g.passable(nx, ny, cur.k.l, net) {
				continue
			}
			// Diagonal: both orthogonal flanks must be free (no corner cut).
			if d[0] != 0 && d[1] != 0 {
				if !g.passable(cur.k.x+d[0], cur.k.y, cur.k.l, net) ||
					!g.passable(cur.k.x, cur.k.y+d[1], cur.k.l, net) {
					continue
				}
			}
			nk := cellKey{nx, ny, cur.k.l}
			step := 1.0
			if d[0] != 0 && d[1] != 0 {
				step = 1.414
			}
			// Prefer layer 0: small surcharge for travelling on other layers.
			if cur.k.l != 0 {
				step += 0.15
			}
			ng := gScore[cur.k] + step
			if old, ok := gScore[nk]; ok && ng >= old {
				continue
			}
			gScore[nk] = ng
			came[nk] = cur.k
			heap.Push(open, &astNode{k: nk, g: ng, f: ng + heuristic(nk, goal)})
		}
		// Via flips: high cost so layer-0 detours win when free.
		for L := uint8(0); L < uint8(g.layers); L++ {
			if L == cur.k.l {
				continue
			}
			if !g.passable(cur.k.x, cur.k.y, L, net) {
				continue
			}
			nk := cellKey{cur.k.x, cur.k.y, L}
			ng := gScore[cur.k] + g.opts.ViaCost
			// Prefer landing on layer 0 slightly.
			if L != 0 {
				ng += 0.5
			}
			if old, ok := gScore[nk]; ok && ng >= old {
				continue
			}
			gScore[nk] = ng
			came[nk] = cur.k
			heap.Push(open, &astNode{k: nk, g: ng, f: ng + heuristic(nk, goal)})
		}
	}
	return nil, false
}

func (g *grid) reconstruct(came map[cellKey]cellKey, end cellKey) []gpos {
	path := []gpos{}
	k := end
	for {
		wx, wy := g.cellToWorld(k.x, k.y)
		path = append(path, gpos{x: wx, y: wy, layer: k.l})
		prev, ok := came[k]
		if !ok {
			break
		}
		k = prev
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return path
}

func bresenham(x0, y0, x1, y1 int) [][2]int {
	var out [][2]int
	dx := abs(x1 - x0)
	dy := abs(y1 - y0)
	sx, sy := 1, 1
	if x0 > x1 {
		sx = -1
	}
	if y0 > y1 {
		sy = -1
	}
	err := dx - dy
	x, y := x0, y0
	for {
		out = append(out, [2]int{x, y})
		if x == x1 && y == y1 {
			break
		}
		e2 := 2 * err
		if e2 > -dy {
			err -= dy
			x += sx
		}
		if e2 < dx {
			err += dx
			y += sy
		}
	}
	return out
}

func clampCell(p core.Point, g *grid) (int, int) {
	cx := int((p.X - g.originX) / g.cell)
	cy := int((p.Y - g.originY) / g.cell)
	if cx < 0 {
		cx = 0
	}
	if cy < 0 {
		cy = 0
	}
	if cx >= g.w {
		cx = g.w - 1
	}
	if cy >= g.h {
		cy = g.h - 1
	}
	return cx, cy
}

func heuristic(a, b cellKey) float64 {
	dx := float64(a.x - b.x)
	dy := float64(a.y - b.y)
	// Euclidean in-plane + cheap layer term.
	h := math.Hypot(dx, dy)
	if a.l != b.l {
		h += 0.5
	}
	return h
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

type astNode struct {
	k    cellKey
	g, f float64
	idx  int
}

type astHeap []*astNode

func (h astHeap) Len() int           { return len(h) }
func (h astHeap) Less(i, j int) bool { return h[i].f < h[j].f }
func (h astHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i]; h[i].idx = i; h[j].idx = j }
func (h *astHeap) Push(x any) {
	n := x.(*astNode)
	n.idx = len(*h)
	*h = append(*h, n)
}
func (h *astHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}
