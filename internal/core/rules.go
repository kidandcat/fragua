package core

import "strings"

// FabRules are manufacturing floor limits adopted for the board.
// JSON tags match Rust pcb_core::FabRules (min_via_drill_mm etc.).
type FabRules struct {
	Preset             string      `json:"preset,omitempty"`
	MinTraceWidthMM    float64     `json:"min_trace_width_mm,omitempty"`
	MinClearanceMM     float64     `json:"min_clearance_mm,omitempty"`
	MinViaDrillMM      float64     `json:"min_via_drill_mm,omitempty"`
	MinAnnularRingMM   float64     `json:"min_annular_ring_mm,omitempty"`
	MinViaDiameterMM   float64     `json:"min_via_diameter_mm,omitempty"`
	MinEdgeClearanceMM float64     `json:"min_edge_clearance_mm,omitempty"`
	MinHoleToHoleMM    float64     `json:"min_hole_to_hole_mm,omitempty"`
	MinSliverMM        float64     `json:"min_sliver_mm,omitempty"`
	MaxBoardSizeMM     *[2]float64 `json:"max_board_size_mm,omitempty"`
	// Component-to-component assembly floors. Nil means "use the built-in
	// default" so a fab preset and an old saved board keep today's numbers.
	// A stored 0 is explicit: no extra courtyard air, or bodies may touch.
	CourtyardMarginMM *float64 `json:"courtyard_margin_mm,omitempty"`
	MinBodyGapMM      *float64 `json:"min_body_gap_mm,omitempty"`
	MinModuleGapMM    *float64 `json:"min_module_gap_mm,omitempty"`
}

// RuleArea is a rectangular region with rule overrides.
type RuleArea struct {
	ID             ID       `json:"id"`
	Name           string   `json:"name"`
	Rect           Rect     `json:"rect"`
	ClearanceMM    *float64 `json:"clearance_mm,omitempty"`
	TraceWidthMM   *float64 `json:"trace_width_mm,omitempty"`
	ViaDrillMM     *float64 `json:"via_drill_mm,omitempty"`
	ViaDiameterMM  *float64 `json:"via_diameter_mm,omitempty"`
	Priority       int      `json:"priority,omitempty"`
	AnchorRef      string   `json:"anchor_ref,omitempty"`
	AnchorMarginMM float64  `json:"anchor_margin_mm,omitempty"`
	// Layers empty means all.
	Layers []Layer `json:"layers,omitempty"`
}

// RuleDefaults are global design defaults.
type RuleDefaults struct {
	Clearance     Length
	TraceWidth    Length
	ViaDrill      Length
	ViaDiameter   Length
	EdgeClearance Length
}

// EscapeException is a pin-level override when fab geometry cannot
// produce a legal dogbone (typically via-in-pad on a stranded QFN pin).
type EscapeException struct {
	Ref  string `json:"ref"`           // footprint reference, e.g. U1
	Pad  string `json:"pad,omitempty"` // pad number; empty = every pad on Ref
	Mode string `json:"mode"`          // "via_in_pad"
}

const EscapeViaInPad = "via_in_pad"

// ActiveFabRules returns the board's fab profile, or JLCPCB 2-layer as
// the default ceiling (we never design tighter than a known fab).
func ActiveFabRules(b *Board) FabRules {
	if b != nil && b.FabRules != nil && b.FabRules.MinClearanceMM > 0 {
		return *b.FabRules
	}
	if p := FabRulesPreset("jlcpcb-2l"); p != nil {
		return *p
	}
	return FabRules{MinClearanceMM: 0.127, MinTraceWidthMM: 0.127, MinViaDrillMM: 0.30, MinViaDiameterMM: 0.60, MinAnnularRingMM: 0.15, MinEdgeClearanceMM: 0.30, MinHoleToHoleMM: 0.50, MinSliverMM: 0.15}
}

// ClampToFab raises any value that would be illegal at the fab.
func (f FabRules) ClampClearance(mm float64) float64 {
	if f.MinClearanceMM > 0 && mm < f.MinClearanceMM {
		return f.MinClearanceMM
	}
	if mm <= 0 {
		return f.MinClearanceMM
	}
	return mm
}

func HasEscapeException(b *Board, ref, pad, mode string) bool {
	if b == nil {
		return false
	}
	for _, e := range b.EscapeExceptions {
		if !strings.EqualFold(e.Mode, mode) && e.Mode != "" {
			continue
		}
		if !strings.EqualFold(e.Ref, ref) {
			continue
		}
		if e.Pad == "" || e.Pad == pad {
			return true
		}
	}
	return false
}

// DefaultRules matches the Rust router/DRC baseline (0.2 mm clearance, etc.).
func DefaultRules() RuleDefaults {
	return RuleDefaults{
		Clearance:     FromMM(0.2),
		TraceWidth:    FromMM(0.15),
		ViaDrill:      FromMM(0.3),
		ViaDiameter:   FromMM(0.6),
		EdgeClearance: FromMM(0.3),
	}
}

// RuleResolver picks clearance/width for a point and net.
type RuleResolver struct {
	Defaults   RuleDefaults
	Areas      []RuleArea
	NetClass   map[string]NetClass // name → class
	NetToClass map[string]string   // net → class name
	// FabFloorMM is the manufacturing clearance. A net class may loosen the
	// design default down to this floor, and no further: a class of 0.05 mm
	// must not hide a 0.10 mm short the fab will reject.
	FabFloorMM float64
}

// ClearanceAt returns the required clearance at p for the given net
// (area absolute override, else max of defaults and net-class clearances).
func (r *RuleResolver) ClearanceAt(p Point, net string) Length {
	return r.ClearanceBetween(p, net, "")
}

// ClearanceBetween implements Rust RuleResolver::clearance for two nets at site p.
// Area with clearance set containing p (highest priority) wins absolutely;
// otherwise max(default, class_a, class_b).
func (r *RuleResolver) ClearanceBetween(p Point, netA, netB string) Length {
	// Areas: highest priority wins; ties → smaller area (more specific).
	type hit struct {
		prio int
		area float64
		cl   Length
	}
	var best *hit
	for _, a := range r.Areas {
		if a.ClearanceMM == nil {
			continue
		}
		if !a.Rect.ContainsPoint(p) {
			continue
		}
		// empty layers = all layers
		if len(a.Layers) > 0 {
			// layer filter omitted for site-only calls (caller may filter later)
		}
		area := a.Rect.Width().ToMM() * a.Rect.Height().ToMM()
		cl := FromMM(*a.ClearanceMM)
		if best == nil || a.Priority > best.prio || (a.Priority == best.prio && area < best.area) {
			best = &hit{prio: a.Priority, area: area, cl: cl}
		}
	}
	if best != nil {
		return best.cl
	}
	cl := r.Defaults.Clearance
	// A class states the clearance for its own nets, so it can be looser
	// than the board default (a 0.15 mm signal class on a 0.20 mm board)
	// or tighter (a 0.40 mm power class). The pair takes the stricter of
	// the two. A net with no class keeps the default, so putting one side
	// in a loose class does not pull the other side down with it.
	a, aok := r.netClearance(netA)
	b, bok := r.netClearance(netB)
	if aok || bok {
		if !aok {
			a = cl
		}
		if !bok {
			b = cl
		}
		cl = a
		if b > cl {
			cl = b
		}
	}
	if r.FabFloorMM > 0 {
		floor := FromMM(r.FabFloorMM)
		if cl < floor {
			cl = floor
		}
	}
	return cl
}

// netClearance is the class clearance for net, when the net has one.
func (r *RuleResolver) netClearance(net string) (Length, bool) {
	if net == "" || r.NetClass == nil {
		return 0, false
	}
	cn := net
	if r.NetToClass != nil {
		if mapped, ok := r.NetToClass[net]; ok && mapped != "" {
			cn = mapped
		}
	}
	if c, ok := r.NetClass[cn]; ok && c.ClearanceMM > 0 {
		return FromMM(c.ClearanceMM), true
	}
	var best Length
	found := false
	for _, c := range r.NetClass {
		if c.ClearanceMM <= 0 || (c.Name != cn && c.Name != net) {
			continue
		}
		cc := FromMM(c.ClearanceMM)
		if !found || cc > best {
			best, found = cc, true
		}
	}
	return best, found
}

// TraceWidthAt returns the trace width for net at p.
func (r *RuleResolver) TraceWidthAt(p Point, net string) Length {
	w := r.Defaults.TraceWidth
	if r.NetToClass != nil && r.NetClass != nil {
		if cn, ok := r.NetToClass[net]; ok {
			if c, ok := r.NetClass[cn]; ok && c.TraceWidthMM > 0 {
				w = FromMM(c.TraceWidthMM)
			}
		}
	}
	bestPrio := -1
	for _, a := range r.Areas {
		if a.TraceWidthMM == nil {
			continue
		}
		if !a.Rect.ContainsPoint(p) {
			continue
		}
		if a.Priority >= bestPrio {
			bestPrio = a.Priority
			w = FromMM(*a.TraceWidthMM)
		}
	}
	return w
}
