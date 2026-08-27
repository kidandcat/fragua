package router

import (
	"github.com/mentasystems/fragua/internal/core"
	"github.com/mentasystems/fragua/internal/impedance"
)

// netWidths resolves the nominal trace width of a net, per copper layer:
//
//  1. the net class impedance target, solved against the stackup for that
//     layer (microstrip on the outers, stripline inside — the two need
//     different widths for the same Z0)
//  2. the net class width
//  3. the caller default (router option, already at the fab floor)
//
// A stackup too incomplete for the closed-form solver falls through to the
// class width instead of failing the route.
type netWidths struct {
	sch   *core.Schematic
	stack core.LayerStackup
	minMM float64 // fab floor; a class never routes below it
	cache map[widthKey]float64
}

type widthKey struct {
	class string
	layer uint8
}

// newNetWidths returns nil when the schematic declares no class — every net
// then keeps Options.TraceWidthMM.
func newNetWidths(board *core.Board, opts Options) *netWidths {
	if opts.Schematic == nil || len(opts.Schematic.NetClasses) == 0 {
		return nil
	}
	stack := core.Default2Layer()
	if board != nil {
		stack = board.StackupOrDefault()
	}
	return &netWidths{
		sch:   opts.Schematic,
		stack: stack,
		minMM: core.ActiveFabRules(board).MinTraceWidthMM,
		cache: map[widthKey]float64{},
	}
}

// widthMM is the class width (mm) for net on copper layer, or 0 when the net
// has no class rule and the caller default applies.
func (n *netWidths) widthMM(net string, layer uint8) float64 {
	if n == nil {
		return 0
	}
	name, cls := n.classOf(net)
	if cls == nil {
		return 0
	}
	k := widthKey{class: name, layer: layer}
	if w, ok := n.cache[k]; ok {
		return w
	}
	w := n.solve(cls, layer)
	n.cache[k] = w
	return w
}

// maxMM is the widest width net can take on any copper layer. Planning and
// clearance use it so a per-layer narrower trace is never under-cleared.
func (n *netWidths) maxMM(net string) float64 {
	if n == nil {
		return 0
	}
	best := 0.0
	for L := 0; L < n.stack.CopperCount() && L < 256; L++ {
		if w := n.widthMM(net, uint8(L)); w > best {
			best = w
		}
	}
	return best
}

func (n *netWidths) solve(cls *core.NetClass, layer uint8) float64 {
	w := 0.0
	if cls.ImpedanceOhms > 0 {
		if p, err := impedance.LineParams(n.stack, int(layer)); err == nil {
			if zw, err := impedance.WidthForZ(p, cls.ImpedanceOhms); err == nil {
				w = zw
			}
		}
	}
	if w <= 0 {
		w = cls.TraceWidthMM
	}
	if w <= 0 {
		return 0
	}
	if n.minMM > 0 && w < n.minMM {
		w = n.minMM
	}
	return w
}

// classOf maps a net to its class the same way the schematic does: the net's
// own Class field first, then the NetToClass table.
func (n *netWidths) classOf(net string) (string, *core.NetClass) {
	if n.sch == nil || net == "" {
		return "", nil
	}
	name := ""
	if v := n.sch.Nets[net]; v != nil && v.Class != "" {
		name = v.Class
	}
	if name == "" && n.sch.NetToClass != nil {
		name = n.sch.NetToClass[net]
	}
	if name == "" {
		return "", nil
	}
	return name, n.sch.NetClasses[name]
}

// widthFor is the nominal trace width (mm) for net on copper layer: the net
// class (impedance target, then class width) wins over the router default.
func (o Options) widthFor(net string, layer uint8) float64 {
	if o.overrideMM > 0 {
		return o.overrideMM
	}
	if w := o.widths.widthMM(net, layer); w > 0 {
		return w
	}
	return o.TraceWidthMM
}

// netWidthMax is widthFor's widest value over the stackup — what planning and
// clearance use before the layer of a segment is known.
func (o Options) netWidthMax(net string) float64 {
	if o.overrideMM > 0 {
		return o.overrideMM
	}
	if w := o.widths.maxMM(net); w > 0 {
		return w
	}
	return o.TraceWidthMM
}
