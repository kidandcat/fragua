// Package router implements multi-layer grid autorouting (A* multi-source tree).
//
// Process approximates Rust pcb_router Grid engine defaults:
//  1. Collect multi-pad nets, stable sort (fewest pads first, then name)
//  2. Stamp foreign pads/traces/vias inflated by clearance (ceil quant guard)
//  3. Multi-source tree growth (Prim-style from same-net copper)
//  4. Light RR&R: retry failed nets after successes claim copper
//  5. Organic string-pull post-pass when Options.Organic (default true)
//  6. Per-net Ok/Failed with length_mm and lower_bound_mm
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
// Defaults match Rust pcb_router::RouteOptions::default.
type Options struct {
	CellMM          float64
	ClearanceMM     float64
	TraceWidthMM    float64
	ViaDrillMM      float64
	ViaDiameterMM   float64
	ViaCost         float64
	MaxSeconds      float64 // >0 wall budget; ≤0 = unlimited (Rust None / 0)
	Organic         bool
	OrganicFilletMM float64 // reserved; string-pull only in this pass
	FineEscape      bool    // opt-in; ignored (not implemented)
	Negotiate       bool    // opt-in; ignored (not implemented)
}

// DefaultOptions returns Rust-aligned 2-layer Grid defaults.
func DefaultOptions() Options {
	return Options{
		CellMM:          0.25,
		ClearanceMM:     0.40,
		TraceWidthMM:    0.25,
		ViaDrillMM:      0.30,
		ViaDiameterMM:   0.60,
		ViaCost:         8.0,
		MaxSeconds:      90,
		Organic:         true,
		OrganicFilletMM: 3.0,
		FineEscape:      false,
		Negotiate:       false,
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
		switch k {
		case "organic":
			o.Organic = v == "true" || v == "1"
			continue
		case "fine_escape":
			o.FineEscape = v == "true" || v == "1"
			continue
		case "negotiate":
			o.Negotiate = v == "true" || v == "1"
			continue
		}
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
		case "via_drill":
			o.ViaDrillMM = x
		case "via_diameter":
			o.ViaDiameterMM = x
		case "organic_fillet_mm":
			o.OrganicFilletMM = x
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
	// Rust: max_seconds None/0/non-finite → no deadline.
	var deadline time.Time
	if opts.MaxSeconds > 0 && !math.IsInf(opts.MaxSeconds, 0) && !math.IsNaN(opts.MaxSeconds) {
		deadline = start.Add(time.Duration(opts.MaxSeconds * float64(time.Second)))
	}
	hasDeadline := !deadline.IsZero()
	pastDeadline := func() bool {
		return hasDeadline && time.Now().After(deadline)
	}

	rep := Report{Iterations: 1}

	// 1. Collect multi-pad nets from footprints in stable order.
	nets := map[string][]padLoc{}
	for _, fp := range footprintsStable(board) {
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
	// Power rails first (they must span the board), then fewest pads, then name.
	sort.Slice(names, func(i, j int) bool {
		pi, pj := netOrderKey(names[i]), netOrderKey(names[j])
		if pi != pj {
			return pi < pj
		}
		if len(nets[names[i]]) != len(nets[names[j]]) {
			return len(nets[names[i]]) < len(nets[names[j]])
		}
		ti, tj := signalTieBreak(names[i]), signalTieBreak(names[j])
		if ti != tj {
			return ti < tj
		}
		return names[i] < names[j]
	})

	// 2. Fine-pitch dogbone fanout, then grid with all copper stamped.
	_ = planFanout(board, opts)
	g := newGrid(board, opts)

	// Pour-only nets: the pour is the connection (Rust route_one_net).
	pourNets := map[string]bool{}
	for _, p := range board.Pours {
		pourNets[p.Net] = true
	}

	var failedNets []string
	for _, name := range names {
		pads := nets[name]
		// Rust: 1-pad nets and pour nets are Outcome::Ok with 0 copper.
		if pourNets[name] || len(pads) < 2 {
			rep.PerNet = append(rep.PerNet, NetResult{Net: name, Outcome: Outcome{
				Status: "ok", Reason: "pour_or_single",
			}})
			continue
		}
		if pastDeadline() {
			rep.PerNet = append(rep.PerNet, NetResult{Net: name, Outcome: Outcome{Status: "failed", Reason: "budget"}})
			rep.Failed++
			failedNets = append(failedNets, name)
			continue
		}

		out := routeNet(board, g, name, pads, opts, deadline, hasDeadline)
		if out.Status != "ok" && len(pads) <= 3 && !pastDeadline() {
			cheap := opts
			cheap.ViaCost = 2
			out = routeNet(board, g, name, pads, cheap, deadline, hasDeadline)
		}
		rep.PerNet = append(rep.PerNet, NetResult{Net: name, Outcome: out})
		if out.Status == "ok" {
			rep.TraceCount += out.TraceSegments
			rep.TotalLengthMM += out.LengthMM
			rep.TotalLowerBound += out.LowerBoundMM
		} else {
			rep.Failed++
			failedNets = append(failedNets, name)
		}
	}

	// 4. RR&R: snapshot, rip one blocking signal net, retry the failed
	// net then the victim. Restore the snapshot if either side fails.
	if len(failedNets) > 0 && !pastDeadline() {
		rep.Iterations = 2
		still := []string{}
		for _, name := range failedNets {
			if pastDeadline() {
				still = append(still, name)
				continue
			}
			victim := pickRipVictim(board, nets[name], name)
			snapT := append([]core.Trace(nil), board.Traces...)
			snapV := append([]core.Via(nil), board.Vias...)
			if victim != "" {
				ripNetCopper(board, victim)
				g = newGrid(board, opts)
			}
			out := routeNet(board, g, name, nets[name], opts, deadline, hasDeadline)
			if out.Status != "ok" {
				board.Traces = snapT
				board.Vias = snapV
				g = newGrid(board, opts)
				still = append(still, name)
			} else {
				// Keep the recovered net even if the victim cannot
				// re-route — wiping both was leaving SCK/QG dead.
				rep.Failed--
				rep.TraceCount += out.TraceSegments
				rep.TotalLengthMM += out.LengthMM
				if victim != "" {
					vout := routeNet(board, g, victim, nets[victim], opts, deadline, hasDeadline)
					if vout.Status != "ok" {
						for i := range rep.PerNet {
							if rep.PerNet[i].Net == victim && rep.PerNet[i].Outcome.Status == "ok" {
								rep.PerNet[i].Outcome.Status = "failed"
								rep.Failed++
							}
						}
					}
				}
			}
			for i := range rep.PerNet {
				if rep.PerNet[i].Net == name {
					rep.PerNet[i].Outcome = out
				}
			}
		}
		failedNets = still
	}

	// 4b. Last chance: rebuild grid and try direct/jumper on leftovers.
	if len(failedNets) > 0 && !pastDeadline() {
		g = newGrid(board, opts)
		left := []string{}
		for _, name := range failedNets {
			out := routeNet(board, g, name, nets[name], opts, deadline, hasDeadline)
			if out.Status == "ok" {
				rep.Failed--
				rep.TraceCount += out.TraceSegments
				rep.TotalLengthMM += out.LengthMM
				for i := range rep.PerNet {
					if rep.PerNet[i].Net == name {
						rep.PerNet[i].Outcome = out
					}
				}
			} else {
				left = append(left, name)
			}
		}
		failedNets = left
	}

	// 5. Organic string-pull (default on). Roll back if it would make
	// copper illegal under DRC min-clearance (0.20 mm), not just the
	// router's 0.40 mm search clearance.
	if opts.Organic && !pastDeadline() {
		before := append([]core.Trace(nil), board.Traces...)
		organicPass(board, opts)
		if !copperClearanceLegal(board, 0.20) {
			board.Traces = before
		}
		refreshReportLengths(board, &rep)
	}

	// Pour stitching: tie floating plane pads to their pour.
	if !pastDeadline() {
		stitchIsolatedPads(board, opts)
	}

	// Via count for nets that finished ok.
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
	// TraceCount from board for accuracy after organic re-segmentation.
	traceCount := 0
	for _, tr := range board.Traces {
		if okNets[tr.Net] {
			traceCount++
		}
	}
	rep.TraceCount = traceCount

	rep.ElapsedMS = time.Since(start).Milliseconds()
	return rep
}

func refreshReportLengths(board *core.Board, rep *Report) {
	byNet := map[string]float64{}
	for _, tr := range board.Traces {
		byNet[tr.Net] += hypotMM(tr.Start, tr.End)
	}
	rep.TotalLengthMM = 0
	for i := range rep.PerNet {
		n := rep.PerNet[i].Net
		if rep.PerNet[i].Outcome.Status != "ok" {
			continue
		}
		if L, ok := byNet[n]; ok {
			rep.PerNet[i].Outcome.LengthMM = L
			rep.PerNet[i].Outcome.TraceSegments = 0
			for _, tr := range board.Traces {
				if tr.Net == n {
					rep.PerNet[i].Outcome.TraceSegments++
				}
			}
			rep.TotalLengthMM += L
		}
	}
}

// footprintsStable returns footprints in deterministic order (FootprintOrder, then ref).
func ripNetCopper(board *core.Board, net string) (tr []core.Trace, vias []core.Via) {
	keptT := board.Traces[:0]
	for _, t := range board.Traces {
		if t.Net == net {
			tr = append(tr, t)
			continue
		}
		keptT = append(keptT, t)
	}
	board.Traces = keptT
	keptV := board.Vias[:0]
	for _, v := range board.Vias {
		if v.Net == net {
			vias = append(vias, v)
			continue
		}
		keptV = append(keptV, v)
	}
	board.Vias = keptV
	return tr, vias
}

func pickRipVictim(board *core.Board, pads []padLoc, failed string) string {
	if len(pads) == 0 {
		return ""
	}
	bestNet, bestD := "", 1e9
	for _, t := range board.Traces {
		if t.Net == failed || netOrderKey(t.Net) < 3 {
			continue
		}
		for _, p := range pads {
			d := distPointSeg(p.p.X.ToMM(), p.p.Y.ToMM(), t.Start.X.ToMM(), t.Start.Y.ToMM(), t.End.X.ToMM(), t.End.Y.ToMM())
			if d < bestD {
				bestD, bestNet = d, t.Net
			}
		}
	}
	if bestD < 3.0 {
		return bestNet
	}
	return ""
}

func distPointSeg(px, py, x0, y0, x1, y1 float64) float64 {
	dx, dy := x1-x0, y1-y0
	l2 := dx*dx + dy*dy
	if l2 < 1e-12 {
		return math.Hypot(px-x0, py-y0)
	}
	t := ((px-x0)*dx + (py-y0)*dy) / l2
	if t < 0 {
		t = 0
	} else if t > 1 {
		t = 1
	}
	return math.Hypot(px-(x0+t*dx), py-(y0+t*dy))
}

func signalTieBreak(name string) int {
	switch strings.ToUpper(name) {
	case "SCK", "SCL", "SCLK", "CLK":
		return 0
	case "MOSI", "SDA", "SDO":
		return 1
	case "MISO", "SDI":
		return 2
	case "NSS", "CS", "NRESET", "BUSY", "DIO1":
		return 3
	default:
		return 4
	}
}

func netOrderKey(name string) int {
	u := strings.ToUpper(name)
	switch {
	case u == "V5" || u == "+5V" || u == "5V" || u == "VIN":
		return 0
	case u == "+3V3" || u == "3V3" || u == "VCC" || u == "VDD" || u == "VSW":
		return 1
	case u == "GND" || strings.HasPrefix(u, "GND") || u == "VSS" || u == "AGND":
		return 2
	default:
		return 3
	}
}

func footprintsStable(board *core.Board) []*core.Footprint {
	seen := map[string]bool{}
	var out []*core.Footprint
	for _, id := range board.FootprintOrder {
		fp := board.Footprints[id]
		if fp == nil || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, fp)
	}
	// Leftovers not in order (legacy loads).
	var extra []*core.Footprint
	for id, fp := range board.Footprints {
		if fp == nil || seen[id] {
			continue
		}
		extra = append(extra, fp)
	}
	sort.Slice(extra, func(i, j int) bool {
		if extra[i].Reference != extra[j].Reference {
			return extra[i].Reference < extra[j].Reference
		}
		return extra[i].ID.String() < extra[j].ID.String()
	})
	return append(out, extra...)
}

func nearestConnectedPad(pads []padLoc, connected map[int]bool, g gpos) int {
	best, bestD := -1, math.MaxFloat64
	for i := range pads {
		if !connected[i] {
			continue
		}
		dx := pads[i].p.X.ToMM() - g.x.ToMM()
		dy := pads[i].p.Y.ToMM() - g.y.ToMM()
		d := dx*dx + dy*dy
		if d < bestD {
			bestD, best = d, i
		}
	}
	if best >= 0 && bestD < 1.0 { // within 1 mm of a connected pad
		return best
	}
	return -1
}

func existingNetSources(board *core.Board, g *grid, net string) []cellKey {
	seen := map[cellKey]bool{}
	var out []cellKey
	add := func(x, y core.Length, layer uint8) {
		cx, cy, ok := g.worldToCell(x, y)
		if !ok {
			cx, cy = clampCell(core.NewPoint(x, y), g)
		}
		k := cellKey{cx, cy, layer}
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, k)
	}
	for _, v := range board.Vias {
		if v.Net != net {
			continue
		}
		for L := uint8(0); L < uint8(g.layers); L++ {
			add(v.Position.X, v.Position.Y, L)
		}
	}
	for _, t := range board.Traces {
		if t.Net != net {
			continue
		}
		add(t.Start.X, t.Start.Y, t.Layer.Index)
		add(t.End.X, t.End.Y, t.Layer.Index)
	}
	return out
}

func routeNet(board *core.Board, g *grid, name string, pads []padLoc, opts Options, deadline time.Time, hasDeadline bool) Outcome {
	if d := routeDirect(board, g, name, pads, opts); d.Status == "ok" {
		return d
	}
	out := routeNetFrom(board, g, name, pads, 0, opts, deadline, hasDeadline)
	if out.Status == "ok" {
		return out
	}
	if len(pads) >= 3 && len(pads) <= 4 {
		for seed := 1; seed < len(pads); seed++ {
			if hasDeadline && time.Now().After(deadline) {
				break
			}
			out = routeNetFrom(board, g, name, pads, seed, opts, deadline, hasDeadline)
			if out.Status == "ok" {
				return out
			}
		}
	}
	if len(pads) <= 3 && (!hasDeadline || time.Now().Before(deadline)) {
		if j := routeViaJumper(board, g, name, pads, opts); j.Status == "ok" {
			return j
		}
	}
	return out
}

// routeDirect lays straight / L-shape segments when every hop is
// short and geometrically clear of foreign pads. Closes tight nets
// (fecha QG on the SOT-23 cluster) that the grid search over-clears.
func routeDirect(board *core.Board, g *grid, name string, pads []padLoc, opts Options) Outcome {
	if len(pads) < 2 || len(pads) > 6 {
		return Outcome{Status: "failed", Reason: "direct"}
	}
	need := opts.TraceWidthMM/2 + searchPadClearMM
	span := 0.0
	for i := range pads {
		for j := i + 1; j < len(pads); j++ {
			d := manhattanMM(pads[i].p, pads[j].p)
			if d > span {
				span = d
			}
		}
	}
	if span > 25 {
		return Outcome{Status: "failed", Reason: "direct-span"}
	}
	w := core.FromMM(opts.TraceWidthMM)
	connected := map[int]bool{0: true}
	var segs []core.Trace
	length := 0.0
	for len(connected) < len(pads) {
		bestI, bestJ, bestD := -1, -1, math.MaxFloat64
		for j := range pads {
			if connected[j] {
				continue
			}
			for i := range connected {
				d := manhattanMM(pads[i].p, pads[j].p)
				if d < bestD {
					bestD, bestI, bestJ = d, i, j
				}
			}
		}
		if bestJ < 0 {
			return Outcome{Status: "failed", Reason: "direct"}
		}
		a, b := pads[bestI].p, pads[bestJ].p
		layer := pads[bestJ].layer
		ax, ay := a.X.ToMM(), a.Y.ToMM()
		bx, by := b.X.ToMM(), b.Y.ToMM()
		var path [][2]float64
		switch {
		case g.segmentPadClear(ax, ay, bx, by, layer, name, need):
			path = [][2]float64{{ax, ay}, {bx, by}}
		case g.segmentPadClear(ax, ay, bx, ay, layer, name, need) &&
			g.segmentPadClear(bx, ay, bx, by, layer, name, need):
			path = [][2]float64{{ax, ay}, {bx, ay}, {bx, by}}
		case g.segmentPadClear(ax, ay, ax, by, layer, name, need) &&
			g.segmentPadClear(ax, by, bx, by, layer, name, need):
			path = [][2]float64{{ax, ay}, {ax, by}, {bx, by}}
		default:
			return Outcome{Status: "failed", Reason: "direct-block"}
		}
		for k := 0; k+1 < len(path); k++ {
			s := core.NewPoint(core.FromMM(path[k][0]), core.FromMM(path[k][1]))
			e := core.NewPoint(core.FromMM(path[k+1][0]), core.FromMM(path[k+1][1]))
			segs = append(segs, core.Trace{
				ID: core.NewID(), Layer: core.Layer{Index: layer}, Net: name, Width: w, Start: s, End: e,
			})
			length += hypotMM(s, e)
		}
		connected[bestJ] = true
	}
	board.Traces = append(board.Traces, segs...)
	for _, t := range segs {
		g.blockSegObstacle(t.Start.X, t.Start.Y, t.End.X, t.End.Y, t.Layer.Index, name, t.Width.ToMM()/2)
	}
	return Outcome{Status: "ok", Reason: "direct", TraceSegments: len(segs), LengthMM: length, LowerBoundMM: lowerBoundMM(pads)}
}

func (g *grid) segmentPadClear(x0, y0, x1, y1 float64, layer uint8, net string, need float64) bool {
	dx, dy := x1-x0, y1-y0
	n := int(math.Hypot(dx, dy) / 0.08)
	if n < 2 {
		n = 2
	}
	for i := 0; i <= n; i++ {
		t := float64(i) / float64(n)
		if g.nearForeignPad(x0+t*dx, y0+t*dy, layer, net, need) {
			return false
		}
	}
	return true
}

// routeViaJumper drops a via beside each pad and routes the other
// layer — the usual header-to-header escape when the top is packed.
func routeViaJumper(board *core.Board, g *grid, name string, pads []padLoc, opts Options) Outcome {
	if len(pads) < 2 {
		return Outcome{Status: "failed", Reason: "jumper"}
	}
	need := opts.TraceWidthMM/2 + searchPadClearMM
	type site struct {
		pad padLoc
		via core.Point
	}
	var sites []site
	dirs := [][2]float64{
		{0, -0.55}, {0, 0.55}, {-0.55, 0}, {0.55, 0},
		{0.55, -0.55}, {-0.55, -0.55}, {0.55, 0.55}, {-0.55, 0.55},
		{0, -0.85}, {0, 0.85}, {-0.85, 0}, {0.85, 0},
		{0, -1.15}, {1.15, 0}, {-1.15, 0}, {0, 1.15},
	}
	for _, p := range pads {
		var chosen *core.Point
		px, py := p.p.X.ToMM(), p.p.Y.ToMM()
		for _, d := range dirs {
			x, y := px+d[0], py+d[1]
			if board.Outline != nil && !outlineContains(board.Outline, x, y, 0.35) {
				continue
			}
			if g.nearForeignPad(x, y, p.layer, name, need) {
				continue
			}
			other := uint8(1)
			if p.layer == 1 {
				other = 0
			}
			if g.nearForeignPad(x, y, other, name, need) {
				continue
			}
			pt := core.NewPoint(core.FromMM(x), core.FromMM(y))
			chosen = &pt
			break
		}
		if chosen == nil {
			return Outcome{Status: "failed", Reason: "jumper-site"}
		}
		sites = append(sites, site{pad: p, via: *chosen})
	}
	w := core.FromMM(opts.TraceWidthMM)
	// Stubs + vias first so A* can use them as sources.
	var stubs []core.Trace
	var vias []core.Via
	var sources []cellKey
	for _, s := range sites {
		vias = append(vias, core.Via{
			ID: core.NewID(), Net: name, Position: s.via,
			Drill: core.FromMM(opts.ViaDrillMM), Diameter: core.FromMM(opts.ViaDiameterMM),
		})
		stubs = append(stubs, core.Trace{
			ID: core.NewID(), Layer: core.Layer{Index: s.pad.layer}, Net: name, Width: w,
			Start: s.pad.p, End: s.via,
		})
		if cx, cy, ok := g.worldToCell(s.via.X, s.via.Y); ok {
			for L := uint8(0); L < uint8(g.layers); L++ {
				sources = append(sources, cellKey{cx, cy, L})
			}
		}
	}
	// Connect via sites on the opposite layer (Prim).
	connected := map[int]bool{0: true}
	var hops []core.Trace
	for len(connected) < len(sites) {
		bestJ, bestD := -1, math.MaxFloat64
		for j := range sites {
			if connected[j] {
				continue
			}
			for i := range sites {
				if !connected[i] {
					continue
				}
				d := manhattanMM(sites[i].via, sites[j].via)
				if d < bestD {
					bestD, bestJ = d, j
				}
			}
		}
		if bestJ < 0 {
			return Outcome{Status: "failed", Reason: "jumper"}
		}
		other := uint8(1)
		if sites[bestJ].pad.layer == 1 {
			other = 0
		}
		path, ok := g.aStarMulti(sources, sites[bestJ].via, other, name, time.Time{}, false)
		if !ok {
			// try the pad's own layer
			path, ok = g.aStarMulti(sources, sites[bestJ].via, sites[bestJ].pad.layer, name, time.Time{}, false)
		}
		if !ok || len(path) < 2 {
			return Outcome{Status: "failed", Reason: "jumper-path"}
		}
		for k := 0; k < len(path)-1; k++ {
			a, b := path[k], path[k+1]
			if a.layer != b.layer {
				continue
			}
			hops = append(hops, core.Trace{
				ID: core.NewID(), Layer: core.Layer{Index: a.layer}, Net: name, Width: w,
				Start: core.NewPoint(a.x, a.y), End: core.NewPoint(b.x, b.y),
			})
		}
		if cx, cy, ok := g.worldToCell(sites[bestJ].via.X, sites[bestJ].via.Y); ok {
			for L := uint8(0); L < uint8(g.layers); L++ {
				sources = append(sources, cellKey{cx, cy, L})
			}
		}
		connected[bestJ] = true
	}
	board.Traces = append(board.Traces, stubs...)
	board.Traces = append(board.Traces, hops...)
	board.Vias = append(board.Vias, vias...)
	for _, t := range stubs {
		g.blockSegObstacle(t.Start.X, t.Start.Y, t.End.X, t.End.Y, t.Layer.Index, name, t.Width.ToMM()/2)
	}
	for _, t := range hops {
		g.blockSegObstacle(t.Start.X, t.Start.Y, t.End.X, t.End.Y, t.Layer.Index, name, t.Width.ToMM()/2)
	}
	for _, v := range vias {
		g.blockViaObstacle(v.Position.X, v.Position.Y, name, v.Diameter.ToMM()/2)
	}
	length := 0.0
	for _, t := range stubs {
		length += hypotMM(t.Start, t.End)
	}
	for _, t := range hops {
		length += hypotMM(t.Start, t.End)
	}
	return Outcome{Status: "ok", Reason: "jumper", TraceSegments: len(stubs) + len(hops), LengthMM: length}
}

func routeNetFrom(board *core.Board, g *grid, name string, pads []padLoc, seed int, opts Options, deadline time.Time, hasDeadline bool) Outcome {
	lb := lowerBoundMM(pads)

	// Prim growth: connected set starts with pad 0; multi-source A* from
	// all same-net tree cells to the nearest unconnected pad.
	if seed < 0 || seed >= len(pads) {
		seed = 0
	}
	connected := map[int]bool{seed: true}
	sources := existingNetSources(board, g, name)
	if sx, sy, ok := g.worldToCell(pads[seed].p.X, pads[seed].p.Y); ok {
		sources = append(sources, cellKey{sx, sy, pads[seed].layer})
	} else {
		sx, sy := clampCell(pads[seed].p, g)
		sources = append(sources, cellKey{sx, sy, pads[seed].layer})
	}

	type pendingTrace struct {
		tr core.Trace
	}
	type pendingVia struct {
		v core.Via
	}
	var pendT []pendingTrace
	var pendV []pendingVia
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
		if hasDeadline && time.Now().After(deadline) {
			failed = true
			reason = "budget"
			break
		}
		// Nearest unconnected pad (Manhattan) to any connected pad.
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
		path, ok := g.aStarMulti(sources, pads[bestJ].p, goalLayer, name, deadline, hasDeadline)
		if !ok {
			failed = true
			break
		}
		if len(path) < 2 {
			if gx, gy, okc := g.worldToCell(pads[bestJ].p.X, pads[bestJ].p.Y); okc {
				sources = append(sources, cellKey{gx, gy, goalLayer})
			}
			connected[bestJ] = true
			continue
		}

		// Do NOT rewrite path ends to pad centres: a Theta* parent
		// 10 mm away would then chord through a neighbour pad
		// (fecha NRESET/NSS vs U2 header, 0.16 mm DRC).
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
		// Always land a same-layer stub on the pad centre. A last
		// step that is only a via (layer flip at the pad cell) used
		// to leave U1's pad as its own island.
		padP := pads[bestJ].p
		from := path[len(path)-1]
		if from.layer != goalLayer {
			pendV = append(pendV, pendingVia{v: core.Via{
				ID: core.NewID(), Position: core.NewPoint(from.x, from.y),
				Drill: core.FromMM(opts.ViaDrillMM), Diameter: core.FromMM(opts.ViaDiameterMM), Net: name,
			}})
			from.layer = goalLayer
		}
		if from.x != padP.X || from.y != padP.Y {
			need := opts.TraceWidthMM/2 + searchPadClearMM
			fx, fy := from.x.ToMM(), from.y.ToMM()
			px, py := padP.X.ToMM(), padP.Y.ToMM()
			if g.segmentPadClear(fx, fy, px, py, goalLayer, name, need) {
				pendT = append(pendT, pendingTrace{tr: core.Trace{
					ID: core.NewID(), Layer: core.Layer{Index: goalLayer},
					Start: core.NewPoint(from.x, from.y), End: padP, Width: w, Net: name,
				}})
				segs++
				length += hypotMM(core.NewPoint(from.x, from.y), padP)
			}
		}
		if gx, gy, ok := g.worldToCell(padP.X, padP.Y); ok {
			sources = append(sources, cellKey{gx, gy, goalLayer})
		}
		connected[bestJ] = true
	}

	if failed {
		rollback()
		return Outcome{Status: "failed", Reason: reason, LowerBoundMM: lb}
	}

	// Commit copper and expand grid halos to full clearance so later nets
	// treat this net as a foreign obstacle (search paint was bare half-width).
	for _, p := range pendT {
		board.Traces = append(board.Traces, p.tr)
		g.blockSegObstacle(p.tr.Start.X, p.tr.Start.Y, p.tr.End.X, p.tr.End.Y, p.tr.Layer.Index, p.tr.Net, p.tr.Width.ToMM()/2)
	}
	for _, p := range pendV {
		board.Vias = append(board.Vias, p.v)
		g.blockViaObstacle(p.v.Position.X, p.v.Position.Y, p.v.Net, p.v.Diameter.ToMM()/2)
	}
	return Outcome{
		Status: "ok", TraceSegments: segs, LengthMM: length, LowerBoundMM: lb,
	}
}

func lowerBoundMM(pads []padLoc) float64 {
	if len(pads) < 2 {
		return 0
	}
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

// copperClearanceLegal reports whether every pair of different-net traces
// on the same layer (and every trace vs foreign pad) keeps minClearanceMM.
func copperClearanceLegal(board *core.Board, minClearanceMM float64) bool {
	trs := board.Traces
	for i := 0; i < len(trs); i++ {
		for j := i + 1; j < len(trs); j++ {
			a, b := trs[i], trs[j]
			if a.Layer.Index != b.Layer.Index || a.Net == b.Net {
				continue
			}
			gap := segSegDistMM(
				[2]float64{a.Start.X.ToMM(), a.Start.Y.ToMM()},
				[2]float64{a.End.X.ToMM(), a.End.Y.ToMM()},
				[2]float64{b.Start.X.ToMM(), b.Start.Y.ToMM()},
				[2]float64{b.End.X.ToMM(), b.End.Y.ToMM()},
			) - a.Width.ToMM()/2 - b.Width.ToMM()/2
			if gap+1e-6 < minClearanceMM {
				return false
			}
		}
	}
	for _, tr := range trs {
		half := tr.Width.ToMM() / 2
		a := [2]float64{tr.Start.X.ToMM(), tr.Start.Y.ToMM()}
		b := [2]float64{tr.End.X.ToMM(), tr.End.Y.ToMM()}
		for _, fp := range footprintsStable(board) {
			for k := range fp.Pads {
				pad := &fp.Pads[k]
				if pad.Net != nil && *pad.Net == tr.Net {
					continue
				}
				if pad.Layer.Index != tr.Layer.Index && (pad.Drill == nil || *pad.Drill == 0) {
					continue
				}
				aabb := core.PadWorldAABB(fp, pad)
				d := segAABBDistMM(a, b, aabb)
				if d-half+1e-6 < minClearanceMM {
					return false
				}
			}
		}
	}
	return true
}

func segSegDistMM(a0, a1, b0, b1 [2]float64) float64 {
	if segsIntersect(p2(a0), p2(a1), p2(b0), p2(b1)) {
		return 0
	}
	return math.Min(
		math.Min(pointSegDist(p2(a0), p2(b0), p2(b1)), pointSegDist(p2(a1), p2(b0), p2(b1))),
		math.Min(pointSegDist(p2(b0), p2(a0), p2(a1)), pointSegDist(p2(b1), p2(a0), p2(a1))),
	)
}

func segAABBDistMM(a, b [2]float64, r core.Rect) float64 {
	rx0, ry0 := r.Min.X.ToMM(), r.Min.Y.ToMM()
	rx1, ry1 := r.Max.X.ToMM(), r.Max.Y.ToMM()
	inside := func(p [2]float64) bool {
		return p[0] >= rx0 && p[0] <= rx1 && p[1] >= ry0 && p[1] <= ry1
	}
	if inside(a) || inside(b) {
		return 0
	}
	corners := [4][2]float64{{rx0, ry0}, {rx1, ry0}, {rx1, ry1}, {rx0, ry1}}
	best := math.Inf(1)
	for i := 0; i < 4; i++ {
		d := segSegDistMM(a, b, corners[i], corners[(i+1)%4])
		if d < best {
			best = d
		}
	}
	return best
}

// --- organic string-pull ---

type p2 [2]float64

func ptMM(p core.Point) p2 {
	return p2{p.X.ToMM(), p.Y.ToMM()}
}

func dist2(a, b p2) float64 {
	return math.Hypot(a[0]-b[0], a[1]-b[1])
}

// organicPass short-cuts polylines when clearance still holds (Rust organic string-pull core).
func organicPass(board *core.Board, opts Options) {
	if board.Outline == nil {
		return
	}
	// Stable net list from copper insertion order.
	var nets []string
	seen := map[string]bool{}
	for _, tr := range board.Traces {
		if !seen[tr.Net] {
			seen[tr.Net] = true
			nets = append(nets, tr.Net)
		}
	}
	sort.Strings(nets)

	before := append([]core.Trace(nil), board.Traces...)
	clr := opts.ClearanceMM

	for _, net := range nets {
		// Layers that actually have copper for this net.
		layerSet := map[uint8]bool{}
		for _, tr := range board.Traces {
			if tr.Net == net {
				layerSet[tr.Layer.Index] = true
			}
		}
		var layers []uint8
		for L := range layerSet {
			layers = append(layers, L)
		}
		sort.Slice(layers, func(i, j int) bool { return layers[i] < layers[j] })

		for _, layer := range layers {
			obs := collectObstacles(board, net, layer, opts)
			chains := extractChains(board, net, layer)
			for _, ch := range chains {
				if len(ch.pts) < 3 {
					continue
				}
				hw := ch.width.ToMM() / 2
				pts := make([]p2, len(ch.pts))
				for i, p := range ch.pts {
					pts[i] = ptMM(p)
				}
				// Pin vertices that land on same-net pads so string-pull
				// cannot drop a tree spoke (e.g. L-junction at the middle pad).
				pinned := padPinnedVertices(board, net, pts)
				pulled := stringPullPinned(pts, pinned, obs, hw, clr)
				if len(pulled) >= 2 && polylineClear(pulled, obs, hw, clr) {
					replaceChain(board, net, layer, ch, pulled)
				}
			}
		}
	}

	// Final legality sweep: roll back whole pass if any chain collides.
	for _, net := range nets {
		layerSet := map[uint8]bool{}
		for _, tr := range board.Traces {
			if tr.Net == net {
				layerSet[tr.Layer.Index] = true
			}
		}
		for layer := range layerSet {
			obs := collectObstacles(board, net, layer, opts)
			for _, ch := range extractChains(board, net, layer) {
				pts := make([]p2, len(ch.pts))
				for i, p := range ch.pts {
					pts[i] = ptMM(p)
				}
				hw := ch.width.ToMM() / 2
				if !polylineClear(pts, obs, hw, clr) {
					board.Traces = before
					return
				}
			}
		}
	}
}

type obstacle struct {
	// kind: rect (pad), capsule (trace), circle (via)
	kind  int // 0 rect, 1 capsule, 2 circle
	min   p2
	max   p2
	a, b  p2
	halfW float64
	c     p2
	r     float64
	clrMM float64
}

type obstacleSet struct {
	items   []obstacle
	outline core.Rect
	edgeMM  float64
}

func collectObstacles(board *core.Board, net string, layer uint8, opts Options) *obstacleSet {
	os := &obstacleSet{
		outline: *board.Outline,
		edgeMM:  0.3,
	}
	clr := opts.ClearanceMM
	for _, fp := range footprintsStable(board) {
		for i := range fp.Pads {
			pad := &fp.Pads[i]
			if pad.Net != nil && *pad.Net == net {
				continue
			}
			// SMD only on its layer; through-hole on all.
			if pad.Drill == nil || *pad.Drill <= 0 {
				if pad.Layer.Index != layer {
					continue
				}
			}
			c := core.PadWorldCenter(fp, pad)
			cm := ptMM(c)
			hw := pad.Size[0].ToMM() / 2
			hh := pad.Size[1].ToMM() / 2
			padClr := clr
			os.items = append(os.items, obstacle{
				kind:  0,
				min:   p2{cm[0] - hw, cm[1] - hh},
				max:   p2{cm[0] + hw, cm[1] + hh},
				clrMM: padClr,
			})
		}
	}
	for _, tr := range board.Traces {
		if tr.Net == net || tr.Layer.Index != layer {
			continue
		}
		os.items = append(os.items, obstacle{
			kind:  1,
			a:     ptMM(tr.Start),
			b:     ptMM(tr.End),
			halfW: tr.Width.ToMM() / 2,
			clrMM: clr,
		})
	}
	for _, v := range board.Vias {
		if v.Net == net {
			continue
		}
		os.items = append(os.items, obstacle{
			kind:  2,
			c:     ptMM(v.Position),
			r:     v.Diameter.ToMM() / 2,
			clrMM: clr,
		})
	}
	for _, k := range board.Keepouts {
		if k.Rect == nil {
			continue
		}
		// Place-only keepouts do not block copper (match grid stamp).
		if k.NoPlace && !k.NoCopper {
			continue
		}
		os.items = append(os.items, obstacle{
			kind:  0,
			min:   ptMM(k.Rect.Min),
			max:   ptMM(k.Rect.Max),
			clrMM: 0,
		})
	}
	return os
}

func pointSegDist(p, a, b p2) float64 {
	ab0, ab1 := b[0]-a[0], b[1]-a[1]
	len2 := ab0*ab0 + ab1*ab1
	if len2 <= 1e-18 {
		return dist2(p, a)
	}
	t := ((p[0]-a[0])*ab0 + (p[1]-a[1])*ab1) / len2
	if t < 0 {
		t = 0
	} else if t > 1 {
		t = 1
	}
	return dist2(p, p2{a[0] + t*ab0, a[1] + t*ab1})
}

func segsIntersect(a, b, c, d p2) bool {
	orient := func(p, q, r p2) float64 {
		return (q[0]-p[0])*(r[1]-p[1]) - (q[1]-p[1])*(r[0]-p[0])
	}
	o1, o2 := orient(a, b, c), orient(a, b, d)
	o3, o4 := orient(c, d, a), orient(c, d, b)
	return ((o1 > 0) != (o2 > 0)) && ((o3 > 0) != (o4 > 0))
}

func segSegDist(a, b, c, d p2) float64 {
	if segsIntersect(a, b, c, d) {
		return 0
	}
	return math.Min(
		math.Min(pointSegDist(a, c, d), pointSegDist(b, c, d)),
		math.Min(pointSegDist(c, a, b), pointSegDist(d, a, b)),
	)
}

func segRectDist(a, b, min, max p2) float64 {
	inside := func(p p2) bool {
		return p[0] >= min[0] && p[0] <= max[0] && p[1] >= min[1] && p[1] <= max[1]
	}
	if inside(a) || inside(b) {
		return 0
	}
	corners := [4]p2{
		{min[0], min[1]}, {max[0], min[1]}, {max[0], max[1]}, {min[0], max[1]},
	}
	best := math.Inf(1)
	for i := 0; i < 4; i++ {
		d := segSegDist(a, b, corners[i], corners[(i+1)%4])
		if d < best {
			best = d
		}
		if best == 0 {
			return 0
		}
	}
	return best
}

func (os *obstacleSet) approachClear(a, b p2, hw, clr float64) bool {
	// Outline band: centreline must stay inside outline shrunk by hw+edge.
	edge := hw + os.edgeMM
	omin := p2{os.outline.Min.X.ToMM() + edge, os.outline.Min.Y.ToMM() + edge}
	omax := p2{os.outline.Max.X.ToMM() - edge, os.outline.Max.Y.ToMM() - edge}
	if omax[0] > omin[0] && omax[1] > omin[1] {
		// Both endpoints and a few samples must stay inside.
		for _, t := range []float64{0, 0.5, 1} {
			p := p2{a[0] + t*(b[0]-a[0]), a[1] + t*(b[1]-a[1])}
			if p[0] < omin[0] || p[0] > omax[0] || p[1] < omin[1] || p[1] > omax[1] {
				return false
			}
		}
	}
	const eps = 1e-4
	for _, ob := range os.items {
		need := hw + math.Max(clr, ob.clrMM)
		var d float64
		switch ob.kind {
		case 0: // rect pad / keepout
			d = segRectDist(a, b, ob.min, ob.max)
		case 1: // foreign trace capsule
			d = segSegDist(a, b, ob.a, ob.b) - ob.halfW
			if d < 0 {
				d = 0
			}
		case 2: // via
			d = pointSegDist(ob.c, a, b) - ob.r
			if d < 0 {
				d = 0
			}
		}
		if d+eps < need {
			return false
		}
	}
	return true
}

func polylineClear(pts []p2, obs *obstacleSet, hw, clr float64) bool {
	for i := 0; i+1 < len(pts); i++ {
		if !obs.approachClear(pts[i], pts[i+1], hw, clr) {
			return false
		}
	}
	return true
}

func stringPull(pts []p2, obs *obstacleSet, hw, clr float64) []p2 {
	return stringPullPinned(pts, nil, obs, hw, clr)
}

func padPinnedVertices(board *core.Board, net string, pts []p2) []bool {
	pin := make([]bool, len(pts))
	if len(pts) == 0 {
		return pin
	}
	const tol = 0.40 // mm — ~one coarse cell; pad centre vs grid vertex
	for _, fp := range footprintsStable(board) {
		for i := range fp.Pads {
			pad := &fp.Pads[i]
			if pad.Net == nil || *pad.Net != net {
				continue
			}
			c := ptMM(core.PadWorldCenter(fp, pad))
			best, bestD := 0, math.Inf(1)
			for j, p := range pts {
				d := dist2(c, p)
				if d < bestD {
					best, bestD = j, d
				}
			}
			if bestD <= tol {
				pin[best] = true
			}
		}
	}
	if len(pin) > 0 {
		pin[0] = true
		pin[len(pin)-1] = true
	}
	return pin
}

func stringPullPinned(pts []p2, pinned []bool, obs *obstacleSet, hw, clr float64) []p2 {
	if len(pts) <= 2 {
		return append([]p2(nil), pts...)
	}
	out := make([]p2, 0, len(pts))
	i := 0
	out = append(out, pts[0])
	for i+1 < len(pts) {
		j := i + 1
		// Never jump over a pinned pad vertex.
		limit := len(pts) - 1
		if len(pinned) == len(pts) {
			for k := i + 1; k < len(pts)-1; k++ {
				if pinned[k] {
					limit = k
					break
				}
			}
		}
		for cand := limit; cand >= i+2; cand-- {
			if obs.approachClear(pts[i], pts[cand], hw, clr) {
				j = cand
				break
			}
		}
		out = append(out, pts[j])
		i = j
	}
	return out
}

type chain struct {
	pts   []core.Point
	width core.Length
	// original segment indices into board.Traces (for replace)
	segIDs []core.ID
}

func ptKey(p core.Point) [2]int64 { return [2]int64{int64(p.X), int64(p.Y)} }

func extractChains(board *core.Board, net string, layer uint8) []chain {
	type segRef struct {
		idx int
		tr  core.Trace
	}
	var segs []segRef
	for i, tr := range board.Traces {
		if tr.Net == net && tr.Layer.Index == layer {
			segs = append(segs, segRef{i, tr})
		}
	}
	if len(segs) == 0 {
		return nil
	}
	adj := map[[2]int64][]int{}
	for i, s := range segs {
		adj[ptKey(s.tr.Start)] = append(adj[ptKey(s.tr.Start)], i)
		adj[ptKey(s.tr.End)] = append(adj[ptKey(s.tr.End)], i)
	}
	hard := map[[2]int64]bool{}
	for _, v := range board.Vias {
		if v.Net == net {
			hard[ptKey(v.Position)] = true
		}
	}
	for _, fp := range footprintsStable(board) {
		for i := range fp.Pads {
			pad := &fp.Pads[i]
			if pad.Net != nil && *pad.Net == net {
				hard[ptKey(core.PadWorldCenter(fp, pad))] = true
			}
		}
	}
	isAnchor := func(k [2]int64) bool {
		if hard[k] {
			return true
		}
		ss := adj[k]
		if len(ss) != 2 {
			return true
		}
		return segs[ss[0]].tr.Width != segs[ss[1]].tr.Width
	}

	used := make([]bool, len(segs))
	var chains []chain
	var anchors [][2]int64
	for k := range adj {
		if isAnchor(k) {
			anchors = append(anchors, k)
		}
	}
	sort.Slice(anchors, func(i, j int) bool {
		if anchors[i][0] != anchors[j][0] {
			return anchors[i][0] < anchors[j][0]
		}
		return anchors[i][1] < anchors[j][1]
	})

	for _, startKey := range anchors {
		for _, first := range adj[startKey] {
			if used[first] {
				continue
			}
			pts := []core.Point{}
			ids := []core.ID{}
			curKey := startKey
			curSeg := first
			// start point
			t0 := segs[curSeg].tr
			if ptKey(t0.Start) == startKey {
				pts = append(pts, t0.Start)
			} else {
				pts = append(pts, t0.End)
			}
			for {
				used[curSeg] = true
				t := segs[curSeg].tr
				ids = append(ids, t.ID)
				var nxt [2]int64
				var nxtPt core.Point
				if ptKey(t.Start) == curKey {
					nxt = ptKey(t.End)
					nxtPt = t.End
				} else {
					nxt = ptKey(t.Start)
					nxtPt = t.Start
				}
				pts = append(pts, nxtPt)
				if isAnchor(nxt) {
					break
				}
				cands := adj[nxt]
				nextSeg := -1
				for _, s := range cands {
					if s != curSeg && !used[s] {
						nextSeg = s
						break
					}
				}
				if nextSeg < 0 {
					break
				}
				curKey = nxt
				curSeg = nextSeg
			}
			if len(pts) >= 2 {
				chains = append(chains, chain{pts: pts, width: segs[first].tr.Width, segIDs: ids})
			}
		}
	}
	return chains
}

func replaceChain(board *core.Board, net string, layer uint8, ch chain, pts []p2) {
	drop := map[core.ID]bool{}
	for _, id := range ch.segIDs {
		drop[id] = true
	}
	var kept []core.Trace
	for _, tr := range board.Traces {
		if !drop[tr.ID] {
			kept = append(kept, tr)
		}
	}
	for i := 0; i+1 < len(pts); i++ {
		kept = append(kept, core.Trace{
			ID:    core.NewID(),
			Layer: core.Layer{Index: layer},
			Start: core.NewPoint(core.FromMM(pts[i][0]), core.FromMM(pts[i][1])),
			End:   core.NewPoint(core.FromMM(pts[i+1][0]), core.FromMM(pts[i+1][1])),
			Width: ch.width,
			Net:   net,
		})
	}
	board.Traces = kept
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

type padObs struct {
	net                    string
	layer                  uint8
	through                bool
	minX, minY, maxX, maxY float64
}

type grid struct {
	originX, originY core.Length
	cell             core.Length
	cellMM           float64
	w, h             int
	layers           int
	// blocked[layer][y*w+x] = net name occupying, "" free, "*" hard obstacle.
	// Own-net pads/traces store the net name so the same net may re-enter.
	blocked [][]string
	pads    []padObs
	opts    Options
	board   *core.Board
}

// maxGridDim caps cells per axis; larger boards coarsen the cell pitch.
const maxGridDim = 320

// searchPadClearMM is the pad-edge gap the search/commit enforce.
// A hair above DRC min (0.20) so a 0.193 mm skim cannot land.
const searchPadClearMM = 0.22

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
	// Full outline so edge-mounted pads (fecha ESP 5V at x=69.25 on
	// 70 mm) and the perimeter channel Rust uses stay on-grid.
	minX, minY := o.Min.X, o.Min.Y
	maxX, maxY := o.Max.X, o.Max.Y
	widthMM := float64(maxX-minX) / 1e6
	heightMM := float64(maxY-minY) / 1e6
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

	// Hard keepouts (no-copper).
	for _, k := range board.Keepouts {
		if k.Rect == nil {
			continue
		}
		if k.NoPlace && !k.NoCopper {
			continue
		}
		g.stampRectObstacle(*k.Rect)
	}

	// Pads: stamp BARE copper only. A 2.54 mm header with a 0.40 mm
	// clearance halo on each pad overwrites its neighbour's centre
	// (fecha U1 V5/GND) and A* "arrives" on a foreign cell. Foreign
	// separation is searchClearanceOK, not the stamp.
	for _, fp := range footprintsStable(board) {
		for i := range fp.Pads {
			pad := &fp.Pads[i]
			net := ""
			if pad.Net != nil {
				net = *pad.Net
			}
			c := core.PadWorldCenter(fp, pad)
			cx, cy, ok := g.worldToCell(c.X, c.Y)
			if !ok {
				cx, cy = clampCell(c, g)
			}
			ww, hh := core.PadWorldSize(fp, pad)
			pw := int(ww/cell) / 2
			ph := int(hh/cell) / 2
			if pw < 0 {
				pw = 0
			}
			if ph < 0 {
				ph = 0
			}
			layersToStamp := []uint8{pad.Layer.Index}
			if pad.Drill != nil && *pad.Drill > 0 {
				layersToStamp = make([]uint8, layers)
				for L := 0; L < layers; L++ {
					layersToStamp[L] = uint8(L)
				}
			}
			aa := core.PadWorldAABB(fp, pad)
			g.pads = append(g.pads, padObs{
				net: net, layer: pad.Layer.Index,
				through: pad.Drill != nil && *pad.Drill > 0,
				minX:    aa.Min.X.ToMM(), minY: aa.Min.Y.ToMM(),
				maxX: aa.Max.X.ToMM(), maxY: aa.Max.Y.ToMM(),
			})
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
						if cur == "*" {
							continue
						}
						if cur == "" {
							g.blocked[layer][idx] = label
						} else if cur != label && label == "*" {
							g.blocked[layer][idx] = "*"
						}
					}
				}
			}
		}
	}
	// Existing board copper: foreign obstacles use full clearance inflation.
	for _, tr := range board.Traces {
		g.blockSegObstacle(tr.Start.X, tr.Start.Y, tr.End.X, tr.End.Y, tr.Layer.Index, tr.Net, tr.Width.ToMM()/2)
	}
	for _, v := range board.Vias {
		g.blockViaObstacle(v.Position.X, v.Position.Y, v.Net, v.Diameter.ToMM()/2)
	}
	return g
}

func (g *grid) copperRadius() int {
	// Bare copper half-width in cells (own-net tree paint).
	r := int(math.Ceil((g.opts.TraceWidthMM / 2) / g.cellMM))
	if r < 0 {
		r = 0
	}
	return r
}

// obstacleRadius: half-width + clearance, rounded up (quantization guard).
func (g *grid) obstacleRadius(halfWidthMM float64) int {
	r := int(math.Ceil((halfWidthMM + g.opts.ClearanceMM) / g.cellMM))
	if r < 1 {
		r = 1
	}
	return r
}

func (g *grid) stampRectObstacle(r core.Rect) {
	c0x, c0y, ok0 := g.worldToCell(r.Min.X, r.Min.Y)
	c1x, c1y, ok1 := g.worldToCell(r.Max.X, r.Max.Y)
	if !ok0 && !ok1 {
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

func (g *grid) blockSegObstacle(x0, y0, x1, y1 core.Length, layer uint8, net string, halfWMM float64) {
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
	r := g.obstacleRadius(halfWMM)
	for _, c := range bresenham(c0x, c0y, c1x, c1y) {
		g.paint(c[0], c[1], layer, net, r)
	}
}

func (g *grid) blockViaObstacle(x, y core.Length, net string, halfWMM float64) {
	if net == "" {
		net = "*"
	}
	cx, cy, ok := g.worldToCell(x, y)
	if !ok {
		return
	}
	r := g.obstacleRadius(halfWMM)
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
		}
	}
}

// searchClearanceOK: no foreign copper within ceil((halfWidth+0.20)/cell)
// of (cx,cy), and no foreign pad AABB closer than 0.20+half-width
// (continuous — catches Theta* chords the discrete disk misses).
func (g *grid) searchClearanceOK(cx, cy int, layer uint8, net string) bool {
	half := g.opts.TraceWidthMM / 2
	need := half + searchPadClearMM
	r := int(math.Ceil(need / g.cellMM))
	if r < 1 {
		r = 1
	}
	r2 := r * r
	for dy := -r; dy <= r; dy++ {
		for dx := -r; dx <= r; dx++ {
			if dx*dx+dy*dy > r2 {
				continue
			}
			x, y := cx+dx, cy+dy
			if x < 0 || y < 0 || x >= g.w || y >= g.h {
				continue
			}
			cur := g.blocked[layer][y*g.w+x]
			if cur != "" && cur != net {
				return false
			}
		}
	}
	wx, wy := g.cellToWorld(cx, cy)
	return !g.nearForeignPad(wx.ToMM(), wy.ToMM(), layer, net, need)
}

func (g *grid) nearForeignPad(x, y float64, layer uint8, net string, need float64) bool {
	for i := range g.pads {
		p := &g.pads[i]
		if p.net == net || p.net == "" {
			continue
		}
		if !p.through && p.layer != layer {
			continue
		}
		if pointRectDist(x, y, p.minX, p.minY, p.maxX, p.maxY) < need {
			return true
		}
	}
	return false
}

func pointRectDist(x, y, minX, minY, maxX, maxY float64) float64 {
	dx := 0.0
	if x < minX {
		dx = minX - x
	} else if x > maxX {
		dx = x - maxX
	}
	dy := 0.0
	if y < minY {
		dy = minY - y
	} else if y > maxY {
		dy = y - maxY
	}
	if dx == 0 && dy == 0 {
		return 0
	}
	return math.Hypot(dx, dy)
}

func (g *grid) passable(cx, cy int, layer uint8, net string) bool {
	if cx < 0 || cy < 0 || cx >= g.w || cy >= g.h {
		return false
	}
	if int(layer) >= g.layers {
		return false
	}
	cur := g.blocked[layer][cy*g.w+cx]
	return cur == "" || cur == net
}

// aStarMulti: multi-source A*; prefers layer 0; vias cost ViaCost.
func (g *grid) aStarMulti(sources []cellKey, to core.Point, goalLayer uint8, net string, deadline time.Time, hasDeadline bool) ([]gpos, bool) {
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
		sources = []cellKey{{tx, ty, 0}}
	}
	seenSrc := map[cellKey]bool{}
	for _, s := range sources {
		if int(s.l) >= g.layers {
			s.l = 0
		}
		if s.x < 0 || s.y < 0 || s.x >= g.w || s.y >= g.h {
			continue
		}
		if !g.passable(s.x, s.y, s.l, net) {
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
	if _, ok := gScore[goal]; ok {
		wx, wy := g.cellToWorld(goal.x, goal.y)
		return []gpos{{x: wx, y: wy, layer: goal.l}}, true
	}

	dirs := [][2]int{
		{1, 0}, {-1, 0}, {0, 1}, {0, -1},
		{1, 1}, {1, -1}, {-1, 1}, {-1, -1},
	}

	maxExpand := g.w * g.h * g.layers * 8
	if maxExpand < 10000 {
		maxExpand = 10000
	}
	expanded := 0
	const deadlineEvery = 256

	for open.Len() > 0 && expanded < maxExpand {
		expanded++
		if hasDeadline && expanded%deadlineEvery == 0 && time.Now().After(deadline) {
			return nil, false
		}
		cur := heap.Pop(open).(*astNode)
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
			if d[0] != 0 && d[1] != 0 {
				if !g.passable(cur.k.x+d[0], cur.k.y, cur.k.l, net) ||
					!g.passable(cur.k.x, cur.k.y+d[1], cur.k.l, net) {
					continue
				}
			}
			// Search-time clearance disk. Own-net cells (the pad we
			// are landing on) skip it: header pads sit 2.54 mm apart
			// and the disk would otherwise refuse the pin itself.
			if g.blocked[cur.k.l][ny*g.w+nx] != net && !g.searchClearanceOK(nx, ny, cur.k.l, net) {
				continue
			}
			nk := cellKey{nx, ny, cur.k.l}
			step := 1.0
			if d[0] != 0 && d[1] != 0 {
				step = 1.414
			}
			if cur.k.l != 0 {
				step += 0.15
			}
			bestParent := cur.k
			ng := gScore[cur.k] + step
			// Lazy Theta*: if the parent has line-of-sight to nk on the
			// same layer, parent→nk is one any-angle segment.
			if parent, ok := came[cur.k]; ok && parent.l == nk.l {
				if g.lineOfSight(parent, nk, net) {
					eu := hypotCells(parent, nk)
					cand := gScore[parent] + eu
					if cand < ng {
						ng = cand
						bestParent = parent
					}
				}
			}
			if old, ok := gScore[nk]; ok && ng >= old {
				continue
			}
			gScore[nk] = ng
			came[nk] = bestParent
			heap.Push(open, &astNode{k: nk, g: ng, f: ng + heuristic(nk, goal)})
		}
		for L := uint8(0); L < uint8(g.layers); L++ {
			if L == cur.k.l {
				continue
			}
			if !g.passable(cur.k.x, cur.k.y, L, net) {
				continue
			}
			nk := cellKey{cur.k.x, cur.k.y, L}
			ng := gScore[cur.k] + g.opts.ViaCost
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

func hypotCells(a, b cellKey) float64 {
	dx := float64(a.x - b.x)
	dy := float64(a.y - b.y)
	return math.Hypot(dx, dy)
}

func heuristic(a, b cellKey) float64 {
	h := hypotCells(a, b)
	if a.l != b.l {
		h += 0.5
	}
	return h
}

func (g *grid) lineOfSight(a, b cellKey, net string) bool {
	if a.l != b.l {
		return false
	}
	for _, c := range bresenham(a.x, a.y, b.x, b.y) {
		if !g.passable(c[0], c[1], a.l, net) {
			return false
		}
		if g.blocked[a.l][c[1]*g.w+c[0]] != net && !g.searchClearanceOK(c[0], c[1], a.l, net) {
			return false
		}
	}
	// Continuous sample: a diagonal can skim a pad between cell centres.
	need := g.opts.TraceWidthMM/2 + searchPadClearMM
	ax, ay := g.cellToWorld(a.x, a.y)
	bx, by := g.cellToWorld(b.x, b.y)
	dx, dy := bx.ToMM()-ax.ToMM(), by.ToMM()-ay.ToMM()
	n := int(math.Hypot(dx, dy) / 0.25)
	if n < 2 {
		n = 2
	}
	for i := 0; i <= n; i++ {
		t := float64(i) / float64(n)
		if g.nearForeignPad(ax.ToMM()+t*dx, ay.ToMM()+t*dy, a.l, net, need) {
			return false
		}
	}
	return true
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
