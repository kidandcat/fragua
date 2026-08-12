package core

// FabRules are manufacturing floor limits adopted for the board.
type FabRules struct {
	Preset              string  `json:"preset,omitempty"`
	MinTraceWidthMM     float64 `json:"min_trace_width_mm,omitempty"`
	MinClearanceMM      float64 `json:"min_clearance_mm,omitempty"`
	MinDrillMM          float64 `json:"min_drill_mm,omitempty"`
	MinAnnularRingMM    float64 `json:"min_annular_ring_mm,omitempty"`
	MinViaDiameterMM    float64 `json:"min_via_diameter_mm,omitempty"`
	MinEdgeClearanceMM  float64 `json:"min_edge_clearance_mm,omitempty"`
	MaxBoardWidthMM     float64 `json:"max_board_width_mm,omitempty"`
	MaxBoardHeightMM    float64 `json:"max_board_height_mm,omitempty"`
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

// ClearanceAt returns the required clearance at p for the given net.
func (r *RuleResolver) ClearanceAt(p Point, net string) Length {
	cl := r.Defaults.Clearance
	// Net class
	if r.NetToClass != nil && r.NetClass != nil {
		if cn, ok := r.NetToClass[net]; ok {
			if c, ok := r.NetClass[cn]; ok && c.ClearanceMM > 0 {
				cl = FromMM(c.ClearanceMM)
			}
		}
	}
	// Areas: higher priority wins when point inside
	bestPrio := -1
	for _, a := range r.Areas {
		if a.ClearanceMM == nil {
			continue
		}
		if !a.Rect.ContainsPoint(p) {
			continue
		}
		if a.Priority >= bestPrio {
			bestPrio = a.Priority
			cl = FromMM(*a.ClearanceMM)
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
