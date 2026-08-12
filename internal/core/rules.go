package core

// FabRules are manufacturing floor limits adopted for the board.
// JSON tags match Rust pcb_core::FabRules (min_via_drill_mm etc.).
type FabRules struct {
	Preset             string     `json:"preset,omitempty"`
	MinTraceWidthMM    float64    `json:"min_trace_width_mm,omitempty"`
	MinClearanceMM     float64    `json:"min_clearance_mm,omitempty"`
	MinViaDrillMM      float64    `json:"min_via_drill_mm,omitempty"`
	MinAnnularRingMM   float64    `json:"min_annular_ring_mm,omitempty"`
	MinViaDiameterMM   float64    `json:"min_via_diameter_mm,omitempty"`
	MinEdgeClearanceMM float64    `json:"min_edge_clearance_mm,omitempty"`
	MaxBoardSizeMM     *[2]float64 `json:"max_board_size_mm,omitempty"`
}

// RuleArea is a rectangular region with rule overrides.
type RuleArea struct {
	ID            ID      `json:"id"`
	Name          string  `json:"name"`
	Rect          Rect    `json:"rect"`
	ClearanceMM   *float64 `json:"clearance_mm,omitempty"`
	TraceWidthMM  *float64 `json:"trace_width_mm,omitempty"`
	ViaDrillMM    *float64 `json:"via_drill_mm,omitempty"`
	ViaDiameterMM *float64 `json:"via_diameter_mm,omitempty"`
	Priority      int     `json:"priority,omitempty"`
	AnchorRef     string  `json:"anchor_ref,omitempty"`
	AnchorMarginMM float64 `json:"anchor_margin_mm,omitempty"`
	// Layers empty means all.
	Layers []Layer `json:"layers,omitempty"`
}

// RuleDefaults are global design defaults.
type RuleDefaults struct {
	Clearance    Length
	TraceWidth   Length
	ViaDrill     Length
	ViaDiameter  Length
	EdgeClearance Length
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
	Defaults  RuleDefaults
	Areas     []RuleArea
	NetClass  map[string]NetClass // name → class
	NetToClass map[string]string  // net → class name
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
	for _, net := range []string{netA, netB} {
		if net == "" || r.NetClass == nil {
			continue
		}
		cn := net
		if r.NetToClass != nil {
			if mapped, ok := r.NetToClass[net]; ok {
				cn = mapped
			}
		}
		// also: class name might equal net's class field — look up by class map key
		if c, ok := r.NetClass[cn]; ok && c.ClearanceMM > 0 {
			cc := FromMM(c.ClearanceMM)
			if cc > cl {
				cl = cc
			}
		}
		// try net class names from schematic: match class whose name is used
		for _, c := range r.NetClass {
			if c.ClearanceMM > 0 && (c.Name == cn || c.Name == net) {
				cc := FromMM(c.ClearanceMM)
				if cc > cl {
					cl = cc
				}
			}
		}
	}
	return cl
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
