// Package si implements signal-integrity audits: impedance deviation,
// return-path (reference plane) continuity, diff-pair skew and via budget.
// Impedance numbers come from internal/impedance (closed form) — there is
// no field solver in this tree, so treat the results as a design smell
// detector, not as a simulation.
package si

import (
	"fmt"
	"math"
	"sort"

	"github.com/mentasystems/fragua/internal/core"
	"github.com/mentasystems/fragua/internal/impedance"
)

// Severity of a violation.
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Kind classifies an SI finding.
type Kind string

const (
	KindImpedanceDeviation Kind = "impedance_deviation"
	KindStackupIncomplete  Kind = "stackup_incomplete"
	KindNoReferencePlane   Kind = "no_reference_plane"
	KindReferencePlaneGap  Kind = "reference_plane_gap"
	KindDiffPairSkew       Kind = "diff_pair_skew"
	KindViaBudget          Kind = "via_budget"
	KindUnknownNet         Kind = "unknown_net"
)

// pourEdgeInsetMM matches drc / gerber: an outline pour is inset from the
// board edge, so copper does not reach the rim.
const pourEdgeInsetMM = 0.3

// maxGapsPerNet caps the per-segment return-path findings for one net; the
// rest collapse into a single "… and N more gaps" line.
const maxGapsPerNet = 10

// Violation is one SI finding.
type Violation struct {
	Kind     Kind     `json:"kind"`
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
	Net      string   `json:"net,omitempty"`
	XMM      float64  `json:"x_mm,omitempty"`
	YMM      float64  `json:"y_mm,omitempty"`
}

// Options configures an SI run.
type Options struct {
	// Nets restricts the run to these nets. Empty ⇒ every net with an
	// impedance target or a diff-pair peer.
	Nets []string
	// Tolerance is the relative impedance tolerance (0.10 = ±10%).
	// Deviation beyond 2× this is an error, not a warning.
	Tolerance float64
	// MaxVias flags checked nets with more vias than this. 0 disables it.
	MaxVias int
	// SampleStepMM is the return-path sampling pitch along a segment.
	SampleStepMM float64
	// SkewToleranceMM is used when the net class sets no LengthToleranceMM.
	SkewToleranceMM float64
}

// DefaultOptions: ±10% impedance, 0.5 mm plane sampling, 0.5 mm skew,
// no via budget.
func DefaultOptions() Options {
	return Options{
		Tolerance:       0.10,
		SampleStepMM:    0.5,
		SkewToleranceMM: 0.5,
	}
}

// Report is the full SI result.
type Report struct {
	Violations []Violation `json:"violations"`
	Errors     int         `json:"errors"`
	Warnings   int         `json:"warnings"`
}

// Summary is a one-line agent-friendly summary.
func (r Report) Summary() string {
	return fmt.Sprintf("si: %d errors, %d warnings (%d findings)", r.Errors, r.Warnings, len(r.Violations))
}

func (r *Report) add(v Violation) {
	r.Violations = append(r.Violations, v)
	if v.Severity == SeverityError {
		r.Errors++
	} else {
		r.Warnings++
	}
}

// Check runs the SI audit on board (schematic optional for net classes).
// Read-only: it never mutates the board.
func Check(b *core.Board, sch *core.Schematic, opts Options) Report {
	var rep Report
	if b == nil {
		return rep
	}
	if opts.Tolerance <= 0 {
		opts.Tolerance = DefaultOptions().Tolerance
	}
	if opts.SampleStepMM <= 0 {
		opts.SampleStepMM = DefaultOptions().SampleStepMM
	}
	if opts.SkewToleranceMM <= 0 {
		opts.SkewToleranceMM = DefaultOptions().SkewToleranceMM
	}

	nets := selectNets(b, sch, opts)
	if len(nets) == 0 {
		return rep
	}
	if len(opts.Nets) > 0 {
		checkUnknownNets(b, sch, nets, &rep)
	}
	checkImpedance(b, sch, nets, opts, &rep)
	checkReturnPath(b, nets, opts, &rep)
	checkDiffPairSkew(b, sch, nets, opts, &rep)
	checkViaBudget(b, nets, opts, &rep)
	return rep
}

// selectNets is the v1 scope: explicit args win; otherwise every net with an
// impedance target or a diff-pair peer. Always sorted, so output is stable.
func selectNets(b *core.Board, sch *core.Schematic, opts Options) []string {
	seen := map[string]bool{}
	var out []string
	add := func(net string) {
		if net == "" || seen[net] {
			return
		}
		seen[net] = true
		out = append(out, net)
	}
	if len(opts.Nets) > 0 {
		for _, n := range opts.Nets {
			add(n)
		}
		sort.Strings(out)
		return out
	}
	if sch == nil {
		return nil
	}
	consider := func(net string) {
		if net == "" || seen[net] {
			return
		}
		if n := sch.Nets[net]; n != nil && n.DiffPair != "" {
			add(net)
			return
		}
		cls := classOf(sch, net)
		if cls == nil {
			return
		}
		if cls.ImpedanceOhms > 0 || cls.DiffPair != "" {
			add(net)
		}
	}
	for name := range sch.Nets {
		consider(name)
	}
	for name := range sch.NetToClass {
		consider(name)
	}
	if b != nil {
		for i := range b.Traces {
			consider(b.Traces[i].Net)
		}
	}
	sort.Strings(out)
	return out
}

// checkUnknownNets flags an explicitly requested net the design knows nothing
// about — a typo in `si-check NET` would otherwise pass silently.
func checkUnknownNets(b *core.Board, sch *core.Schematic, nets []string, rep *Report) {
	for _, net := range nets {
		if sch != nil && sch.Nets[net] != nil {
			continue
		}
		if netHasCopper(b, net) {
			continue
		}
		rep.add(Violation{
			Kind: KindUnknownNet, Severity: SeverityWarning,
			Message: fmt.Sprintf("net %s is not in the schematic and has no copper", net),
			Net:     net,
		})
	}
}

// ---------------------------------------------------------------------------
// 1. Impedance audit
// ---------------------------------------------------------------------------

type lineParams struct {
	p   impedance.Params
	err error
}

// devKey groups segments that carry the identical impedance verdict. Width is
// the nm fixed-point value, so grouping is exact — no float bucketing.
type devKey struct {
	layer uint8
	width core.Length
}

// devGroup is one coalesced impedance finding: a routed net keeps the same
// width for hundreds of segments, and 310 copies of one sentence is noise,
// not 310 facts.
type devGroup struct {
	key       devKey
	sev       Severity
	z         float64
	layerName string
	widthMM   float64
	count     int
	xMM, yMM  float64 // first offending segment midpoint
}

func checkImpedance(b *core.Board, sch *core.Schematic, nets []string, opts Options, rep *Report) {
	if sch == nil {
		return
	}
	stack := b.StackupOrDefault()
	cache := map[uint8]lineParams{}
	for _, net := range nets {
		cls := classOf(sch, net)
		if cls == nil || cls.ImpedanceOhms <= 0 {
			continue
		}
		// One warning per net when the stackup cannot feed the closed form —
		// missing dielectric data must not sink the whole run.
		warned := false
		warn := func(err error) {
			if warned {
				return
			}
			warned = true
			rep.add(Violation{
				Kind: KindStackupIncomplete, Severity: SeverityWarning,
				Message: fmt.Sprintf("net %s: impedance not checked (%v)", net, err),
				Net:     net,
			})
		}
		// Coalesce by (net, layer, width): every segment in a group has the
		// same Z0, so it is one fact, reported once with a segment count.
		groups := map[devKey]*devGroup{}
		var order []devKey
		for i := range b.Traces {
			tr := &b.Traces[i]
			if tr.Net != net {
				continue
			}
			lp, ok := cache[tr.Layer.Index]
			if !ok {
				p, err := impedance.LineParams(stack, int(tr.Layer.Index))
				lp = lineParams{p: p, err: err}
				cache[tr.Layer.Index] = lp
			}
			if lp.err != nil {
				warn(lp.err)
				continue
			}
			z, err := impedance.Z0(lp.p, tr.Width.ToMM())
			if err != nil {
				warn(err)
				continue
			}
			dev := math.Abs(z-cls.ImpedanceOhms) / cls.ImpedanceOhms
			if dev <= opts.Tolerance {
				continue
			}
			sev := SeverityWarning
			if dev > 2*opts.Tolerance {
				sev = SeverityError
			}
			key := devKey{layer: tr.Layer.Index, width: tr.Width}
			g := groups[key]
			if g == nil {
				mx, my := segMidMM(tr)
				g = &devGroup{
					key: key, sev: sev, z: z, layerName: lp.p.LayerName,
					widthMM: tr.Width.ToMM(), xMM: mx, yMM: my,
				}
				groups[key] = g
				order = append(order, key)
			}
			g.count++
			if sev == SeverityError {
				g.sev = SeverityError // worst segment wins the group
			}
		}
		for _, key := range order {
			g := groups[key]
			msg := fmt.Sprintf("net %s on %s: Z0 %.1f Ω vs target %.1f Ω (%+.1f%%, tol ±%.0f%%) at width %.3f mm",
				net, g.layerName, g.z, cls.ImpedanceOhms,
				100*(g.z-cls.ImpedanceOhms)/cls.ImpedanceOhms, 100*opts.Tolerance, g.widthMM)
			if g.count > 1 {
				msg += fmt.Sprintf(" (%d segments)", g.count)
			}
			rep.add(Violation{
				Kind: KindImpedanceDeviation, Severity: g.sev,
				Message: msg, Net: net, XMM: g.xMM, YMM: g.yMM,
			})
		}
	}
}

// ---------------------------------------------------------------------------
// 2. Return path / reference plane
// ---------------------------------------------------------------------------

func checkReturnPath(b *core.Board, nets []string, opts Options, rep *Report) {
	stack := b.StackupOrDefault()
	anyPlane := false
	for i := 0; i < stack.CopperCount(); i++ {
		if stack.IsPlane(i) {
			anyPlane = true
			break
		}
	}
	for _, net := range nets {
		if !anyPlane {
			if netHasTraces(b, net) {
				rep.add(Violation{
					Kind: KindNoReferencePlane, Severity: SeverityWarning,
					Message: fmt.Sprintf("net %s: no reference plane in stackup", net),
					Net:     net,
				})
			}
			continue
		}
		reportedNoPour := map[int]bool{}
		// Gap locations matter, so they stay per-segment — but a net routed
		// entirely off the plane would bury the report, hence the cap.
		gaps, suppressed := 0, 0
		for i := range b.Traces {
			tr := &b.Traces[i]
			if tr.Net != net {
				continue
			}
			sig := int(tr.Layer.Index)
			if sig >= stack.CopperCount() || stack.IsPlane(sig) {
				continue // plane copper references itself
			}
			plane, ok := referencePlane(stack, sig)
			if !ok {
				continue
			}
			pours := poursOnLayer(b, plane)
			if len(pours) == 0 {
				if !reportedNoPour[plane] {
					reportedNoPour[plane] = true
					mx, my := segMidMM(tr)
					rep.add(Violation{
						Kind: KindReferencePlaneGap, Severity: SeverityError,
						Message: fmt.Sprintf("net %s references %s, which has no pour (no return path)",
							net, layerName(stack, plane)),
						Net: net, XMM: mx, YMM: my,
					})
				}
				continue
			}
			if x, y, gap := firstUncovered(b, pours, tr, opts.SampleStepMM); gap {
				if gaps >= maxGapsPerNet {
					suppressed++
					continue
				}
				gaps++
				rep.add(Violation{
					Kind: KindReferencePlaneGap, Severity: SeverityError,
					Message: fmt.Sprintf("reference plane gap under %s at (%.2f, %.2f) mm on %s",
						net, x, y, layerName(stack, plane)),
					Net: net, XMM: x, YMM: y,
				})
			}
		}
		if suppressed > 0 {
			rep.add(Violation{
				Kind: KindReferencePlaneGap, Severity: SeverityError,
				Message: fmt.Sprintf("… and %d more gaps on %s", suppressed, net),
				Net:     net,
			})
		}
	}
}

// referencePlane is the nearest plane copper layer to sig. Ties (a plane one
// step above and one step below) go to the layer above — the shorter loop on
// a top-referenced microstrip.
func referencePlane(s core.LayerStackup, sig int) (int, bool) {
	n := s.CopperCount()
	for d := 1; d < n; d++ {
		if up := sig - d; up >= 0 && s.IsPlane(up) {
			return up, true
		}
		if dn := sig + d; dn < n && s.IsPlane(dn) {
			return dn, true
		}
	}
	return 0, false
}

func layerName(s core.LayerStackup, i int) string {
	if i >= 0 && i < len(s.Layers) && s.Layers[i].Name != "" {
		return s.Layers[i].Name
	}
	return fmt.Sprintf("L%d", i+1)
}

func poursOnLayer(b *core.Board, layer int) []*core.Pour {
	var out []*core.Pour
	for i := range b.Pours {
		if int(b.Pours[i].Layer.Index) == layer {
			out = append(out, &b.Pours[i])
		}
	}
	return out
}

// firstUncovered samples the segment (endpoints plus every step mm) and
// returns the first sample no pour on the reference layer covers.
func firstUncovered(b *core.Board, pours []*core.Pour, tr *core.Trace, stepMM float64) (float64, float64, bool) {
	x0, y0 := tr.Start.X.ToMM(), tr.Start.Y.ToMM()
	x1, y1 := tr.End.X.ToMM(), tr.End.Y.ToMM()
	n := 1
	if l := math.Hypot(x1-x0, y1-y0); l > stepMM {
		n = int(math.Ceil(l / stepMM))
	}
	cutouts := cutoutPolys(b)
	for i := 0; i <= n; i++ {
		t := float64(i) / float64(n)
		x, y := x0+t*(x1-x0), y0+t*(y1-y0)
		covered := false
		for _, p := range pours {
			if pourCovers(b, p, cutouts, x, y) {
				covered = true
				break
			}
		}
		if !covered {
			return x, y, true
		}
	}
	return 0, 0, false
}

// pourCovers reports filled copper at (x, y). An explicit polygon is the fill
// region; an empty polygon means the pour follows the board outline (inset the
// same 0.3 mm as the gerber writer). Cutouts remove copper in both cases.
// A pour with no polygon on a board with no shape cannot be bounded, so it
// counts as covering everything rather than flagging phantom gaps.
func pourCovers(b *core.Board, p *core.Pour, cutouts [][]core.Point, x, y float64) bool {
	pt := core.NewPoint(core.FromMM(x), core.FromMM(y))
	if len(p.Polygon) >= 3 {
		if !core.PointInPolygon(pt, p.Polygon) {
			return false
		}
		return !inAnyCutout(pt, cutouts)
	}
	if b.Outline == nil && len(b.OutlinePoly) < 3 {
		return true
	}
	if len(b.OutlinePoly) >= 3 {
		if !core.PointInBoardShape(pt, nil, b.OutlinePoly, cutouts) {
			return false
		}
		return true
	}
	r := *b.Outline
	inset := pourEdgeInsetMM
	if x < r.Min.X.ToMM()+inset || x > r.Max.X.ToMM()-inset ||
		y < r.Min.Y.ToMM()+inset || y > r.Max.Y.ToMM()-inset {
		return false
	}
	return !inAnyCutout(pt, cutouts)
}

func inAnyCutout(pt core.Point, cutouts [][]core.Point) bool {
	for _, c := range cutouts {
		if core.PointInPolygon(pt, c) {
			return true
		}
	}
	return false
}

func cutoutPolys(b *core.Board) [][]core.Point {
	var out [][]core.Point
	for i := range b.Cutouts {
		if len(b.Cutouts[i].Polygon) >= 3 {
			out = append(out, b.Cutouts[i].Polygon)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 3. Diff-pair skew
// ---------------------------------------------------------------------------

func checkDiffPairSkew(b *core.Board, sch *core.Schematic, nets []string, opts Options, rep *Report) {
	if sch == nil {
		return
	}
	done := map[string]bool{}
	for _, net := range nets {
		peer := diffPeer(sch, net)
		if peer == "" || peer == net {
			continue
		}
		key := net + "\x00" + peer
		if peer < net {
			key = peer + "\x00" + net
		}
		if done[key] {
			continue
		}
		done[key] = true
		a, bLen := netLengthMM(b, net), netLengthMM(b, peer)
		if a <= 0 && bLen <= 0 {
			continue // nothing routed yet
		}
		tol := skewToleranceMM(sch, net, peer, opts)
		skew := math.Abs(a - bLen)
		if skew <= tol {
			continue
		}
		sev := SeverityWarning
		if skew > 2*tol {
			sev = SeverityError
		}
		lo, hi := net, peer
		if peer < net {
			lo, hi = peer, net
		}
		rep.add(Violation{
			Kind: KindDiffPairSkew, Severity: sev,
			Message: fmt.Sprintf("diff pair %s/%s skew %.3f mm > %.3f mm (%.3f / %.3f mm)",
				lo, hi, skew, tol, a, bLen),
			Net: net,
		})
	}
}

// diffPeer is the net's diff-pair partner, from the net or from its class.
func diffPeer(sch *core.Schematic, net string) string {
	if n := sch.Nets[net]; n != nil && n.DiffPair != "" {
		return n.DiffPair
	}
	if cls := classOf(sch, net); cls != nil {
		return cls.DiffPair
	}
	return ""
}

// skewToleranceMM prefers the net's own class, then the peer's, then the
// option default — a pair usually declares the tolerance on one side only.
func skewToleranceMM(sch *core.Schematic, net, peer string, opts Options) float64 {
	if cls := classOf(sch, net); cls != nil && cls.LengthToleranceMM > 0 {
		return cls.LengthToleranceMM
	}
	if cls := classOf(sch, peer); cls != nil && cls.LengthToleranceMM > 0 {
		return cls.LengthToleranceMM
	}
	return opts.SkewToleranceMM
}

// ---------------------------------------------------------------------------
// 4. Via budget
// ---------------------------------------------------------------------------

func checkViaBudget(b *core.Board, nets []string, opts Options, rep *Report) {
	if opts.MaxVias <= 0 {
		return
	}
	for _, net := range nets {
		n := 0
		for i := range b.Vias {
			if b.Vias[i].Net == net {
				n++
			}
		}
		if n <= opts.MaxVias {
			continue
		}
		rep.add(Violation{
			Kind: KindViaBudget, Severity: SeverityWarning,
			Message: fmt.Sprintf("net %s has %d vias > budget %d", net, n, opts.MaxVias),
			Net:     net,
		})
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func classOf(sch *core.Schematic, net string) *core.NetClass {
	if sch == nil {
		return nil
	}
	name := ""
	if n := sch.Nets[net]; n != nil && n.Class != "" {
		name = n.Class
	}
	if name == "" && sch.NetToClass != nil {
		name = sch.NetToClass[net]
	}
	if name == "" {
		return nil
	}
	return sch.NetClasses[name]
}

func segMidMM(tr *core.Trace) (float64, float64) {
	return (tr.Start.X.ToMM() + tr.End.X.ToMM()) / 2, (tr.Start.Y.ToMM() + tr.End.Y.ToMM()) / 2
}

func netLengthMM(b *core.Board, net string) float64 {
	total := 0.0
	for i := range b.Traces {
		tr := &b.Traces[i]
		if tr.Net != net {
			continue
		}
		total += math.Hypot(tr.End.X.ToMM()-tr.Start.X.ToMM(), tr.End.Y.ToMM()-tr.Start.Y.ToMM())
	}
	return total
}

func netHasTraces(b *core.Board, net string) bool {
	for i := range b.Traces {
		if b.Traces[i].Net == net {
			return true
		}
	}
	return false
}

func netHasCopper(b *core.Board, net string) bool {
	if b == nil {
		return false
	}
	if netHasTraces(b, net) {
		return true
	}
	for i := range b.Vias {
		if b.Vias[i].Net == net {
			return true
		}
	}
	for i := range b.Pours {
		if b.Pours[i].Net == net {
			return true
		}
	}
	return false
}
