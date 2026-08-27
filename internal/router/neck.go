package router

import (
	"math"

	"github.com/mentasystems/fragua/internal/core"
)

// Escape necking. A controlled-impedance width solved against the stackup is
// often wider than the package it leaves: 45 Ω on a 4-layer JLC stack is
// 0.414 mm, and nothing that wide walks out of a 0.4 mm-pitch QFN. Real layout
// necks the trace down for the first fraction of a millimetre off the pad and
// widens back to the controlled width in the open field — a neck that short is
// a negligible discontinuity, and it is the difference between a routed net
// and no net at all.
const (
	// escapeZoneRadiusMM bounds the necked stretch around one pad: a trace can
	// never be narrow more than this far from copper it must reach anyway, and
	// only where the nominal width does not fit. One escape per pad, so a
	// point-to-point net carries at most ~2.4 mm of narrow copper — far below
	// the length where a 45 Ω discontinuity starts to matter at USB 2.0 edge
	// rates (~λ/10 is tens of mm).
	escapeZoneRadiusMM = 1.2
	// escapeSampleMM is the resolution at which one straight run is split
	// into its necked and its nominal stretch.
	escapeSampleMM = 0.1
)

// neckZone is a disk around one pad of the net being routed, inside which the
// trace may narrow to the neck width.
type neckZone struct{ x, y, r float64 }

// neckPlan is the escape plan for the net currently being routed: nil on the
// first (nominal width everywhere) attempt, non-nil on the necked retry.
type neckPlan struct {
	zones                  []neckZone
	widthMM                float64
	net                    string
	g                      *grid   // asks whether the nominal width fits after all
	minX, minY, maxX, maxY float64 // bounding box, for a cheap reject
}

// newNeckPlan puts an escape zone on every pad of the net. widthMM is the
// narrowest the trace may go — the router default, already at the fab floor.
// nil when necking would not narrow anything.
func newNeckPlan(pads []padLoc, widthMM, nominalMM float64) *neckPlan {
	if widthMM <= 0 || widthMM >= nominalMM || len(pads) == 0 {
		return nil
	}
	p := &neckPlan{
		widthMM: widthMM,
		minX:    math.MaxFloat64, minY: math.MaxFloat64,
		maxX: -math.MaxFloat64, maxY: -math.MaxFloat64,
	}
	for _, pd := range pads {
		z := neckZone{x: pd.p.X.ToMM(), y: pd.p.Y.ToMM(), r: escapeZoneRadiusMM}
		p.zones = append(p.zones, z)
		p.minX = math.Min(p.minX, z.x-z.r)
		p.minY = math.Min(p.minY, z.y-z.r)
		p.maxX = math.Max(p.maxX, z.x+z.r)
		p.maxY = math.Max(p.maxY, z.y+z.r)
	}
	return p
}

// covers reports whether (x, y) in mm sits in an escape zone.
func (p *neckPlan) covers(x, y float64) bool {
	if p == nil {
		return false
	}
	if x < p.minX || x > p.maxX || y < p.minY || y > p.maxY {
		return false
	}
	for _, z := range p.zones {
		dx, dy := z.x-x, z.y-y
		if dx*dx+dy*dy <= z.r*z.r {
			return true
		}
	}
	return false
}

// neckAt decides the width of the copper actually laid at (x, y) on layer: the
// escape necks only where the nominal width does not fit. A zone is a licence
// to narrow, not an order to — the shortest neck that clears the pin field is
// the one with no impedance story to tell.
func (p *neckPlan) neckAt(x, y float64, layer uint8, nominalMM float64) bool {
	if !p.covers(x, y) {
		return false
	}
	if p.g != nil && p.g.widthFits(x, y, layer, p.net, nominalMM/2) {
		return false
	}
	return true
}

// halfWidthAt is half the planning width at (x, y): the neck inside an escape
// zone, the net's nominal width outside. Drives the search clearance disk, so
// the A* may thread a pin field the nominal width cannot enter.
func (o Options) halfWidthAt(x, y float64) float64 {
	if o.neck != nil && o.neck.covers(x, y) {
		return o.neck.widthMM / 2
	}
	return o.TraceWidthMM / 2
}

// needAt is the gap a trace of this net needs from foreign copper at (x, y).
func (o Options) needAt(x, y float64) float64 {
	return o.halfWidthAt(x, y) + o.padClear()
}

// escapeTraces is the copper for one straight run a→b of net on layer: the
// stretch inside an escape zone at the neck width, the rest at the nominal
// width for that layer, split where the run crosses the zone boundary. Without
// a plan it is one segment at the nominal width, exactly as before.
func (o Options) escapeTraces(net string, layer uint8, a, b core.Point) []core.Trace {
	nom := o.widthFor(net, layer)
	seg := func(w float64, s, e core.Point) core.Trace {
		return core.Trace{
			ID: core.NewID(), Layer: core.Layer{Index: layer}, Net: net,
			Width: core.FromMM(w), Start: s, End: e,
		}
	}
	if o.neck == nil || o.neck.widthMM >= nom {
		return []core.Trace{seg(nom, a, b)}
	}
	width := func(necked bool) float64 {
		if necked {
			return o.neck.widthMM
		}
		return nom
	}
	ax, ay := a.X.ToMM(), a.Y.ToMM()
	bx, by := b.X.ToMM(), b.Y.ToMM()
	length := math.Hypot(bx-ax, by-ay)
	n := int(math.Ceil(length / escapeSampleMM))
	if n < 1 {
		n = 1
	}
	var out []core.Trace
	start, t0 := a, 0.0
	prev := o.neck.neckAt(ax, ay, layer, nom)
	for i := 1; i <= n; i++ {
		t := float64(i) / float64(n)
		cur := o.neck.neckAt(ax+t*(bx-ax), ay+t*(by-ay), layer, nom)
		if cur == prev {
			continue
		}
		// Cut halfway between the two samples that disagree.
		tb := t - 0.5/float64(n)
		if (tb-t0)*length >= escapeSampleMM/2 {
			p := core.NewPoint(core.FromMM(ax+tb*(bx-ax)), core.FromMM(ay+tb*(by-ay)))
			out = append(out, seg(width(prev), start, p))
			start, t0 = p, tb
		}
		prev = cur
	}
	return append(out, seg(width(prev), start, b))
}

// fallbackMM is the next width tier below the impedance-solved nominal: the
// class's own TraceWidthMM when it declares one (never below the fab floor).
func (n *netWidths) fallbackMM(net string) float64 {
	if n == nil {
		return 0
	}
	_, cls := n.classOf(net)
	if cls == nil || cls.TraceWidthMM <= 0 {
		return 0
	}
	w := cls.TraceWidthMM
	if n.minMM > 0 && w < n.minMM {
		w = n.minMM
	}
	return w
}

// fallbackWidth is what a net retries at once its nominal width has been shown
// not to fit: the class width, else the caller default.
func (o Options) fallbackWidth(net string) float64 {
	if w := o.widths.fallbackMM(net); w > 0 {
		return w
	}
	return o.TraceWidthMM
}
