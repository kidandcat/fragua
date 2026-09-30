package core

// ComponentRules are the component-to-component floors DRC and the placer share.
// Courtyards must not overlap. Bodies must keep MinBodyGapMM, or MinModuleGapMM
// when either part is a module.
type ComponentRules struct {
	CourtyardMarginMM float64
	MinBodyGapMM      float64
	MinModuleGapMM    float64
}

// DefaultComponentRules is the IPC-7351 nominal courtyard (0.25 mm), a 0.5 mm
// body gap, and a 1.0 mm gap when a module is involved.
func DefaultComponentRules() ComponentRules {
	return ComponentRules{
		CourtyardMarginMM: CourtyardMarginMM,
		MinBodyGapMM:      DefaultBodyGapMM,
		MinModuleGapMM:    DefaultModuleGapMM,
	}
}

// ComponentRules reads the board's fab-rules overrides, falling back to
// DefaultComponentRules for anything the board does not set.
func (b *Board) ComponentRules() ComponentRules {
	r := DefaultComponentRules()
	if b == nil || b.FabRules == nil {
		return r
	}
	f := b.FabRules
	if f.CourtyardMarginMM != nil {
		r.CourtyardMarginMM = *f.CourtyardMarginMM
	}
	if f.MinBodyGapMM != nil {
		r.MinBodyGapMM = *f.MinBodyGapMM
	}
	if f.MinModuleGapMM != nil {
		r.MinModuleGapMM = *f.MinModuleGapMM
	}
	return r
}

// GapBetween is the body-to-body minimum for this pair.
func (r ComponentRules) GapBetween(a, b *Footprint) float64 {
	if a.IsModule() || b.IsModule() {
		return r.MinModuleGapMM
	}
	return r.MinBodyGapMM
}

// Hit kinds. DRC reports these strings as violation kinds.
const (
	HitCourtyardOverlap = "courtyard_overlap"
	HitBodyClearance    = "body_clearance"
)

// ComponentHit is one component-clearance failure between two footprints.
type ComponentHit struct {
	Kind string
	A, B string
	// Gap is the signed clearance in mm (negative means the outlines overlap).
	Gap float64
	// Need is the required gap. Courtyard overlap needs a non-negative gap.
	Need float64
	X, Y float64
}

// PackageOutlines is the pair of rectangles the component-clearance rule uses.
//
// The courtyard is the declared body_rect when that rectangle is grown past
// the lands the way an IPC / KiCad courtyard is (it already contains the
// margin). A tight body_rect — a module outline that merely covers its pads —
// is the package body, and the courtyard is that body plus courtyardMarginMM,
// which is what "no courtyard" means.
//
// The body is the package. When body_rect is a courtyard it is inset by the
// margin so a pair of courtyards that merely touch already satisfies the
// 0.5 mm body gap; the margin is not counted twice.
func PackageOutlines(fp *Footprint, courtyardMarginMM float64) (courtyard, body Rect, ok bool) {
	if fp == nil {
		return Rect{}, Rect{}, false
	}
	if courtyardMarginMM < 0 {
		courtyardMarginMM = 0
	}
	pads, hasPads := padUnionWorld(fp)
	declared, hasDecl := bodyRectWorld(fp)
	// 0.45× leaves room for an IPC least courtyard (0.12 mm on a 0.25 mm
	// nominal) without treating a pad that merely kisses the outline as one.
	if hasDecl && hasPads && minSideExcessMM(declared, pads) >= courtyardMarginMM*0.45 && courtyardMarginMM > 0 {
		courtyard = declared
		body = insetMM(declared, courtyardMarginMM)
		return courtyard, body, true
	}
	if hasDecl {
		body = declared
		if hasPads {
			body = body.Union(pads)
		}
		return body.Expand(FromMM(courtyardMarginMM)), body, true
	}
	if !hasPads {
		return Rect{}, Rect{}, false
	}
	c, ok := CourtyardWorldMargin(fp, courtyardMarginMM)
	if !ok {
		c = pads.Expand(FromMM(courtyardMarginMM))
	}
	return c, pads, true
}

// minSideExcessMM is how far outer extends past inner on the tightest side.
// Negative means inner sticks out of outer.
func minSideExcessMM(outer, inner Rect) float64 {
	sides := []float64{
		(inner.Min.X - outer.Min.X).ToMM(),
		(outer.Max.X - inner.Max.X).ToMM(),
		(inner.Min.Y - outer.Min.Y).ToMM(),
		(outer.Max.Y - inner.Max.Y).ToMM(),
	}
	m := sides[0]
	for _, s := range sides[1:] {
		if s < m {
			m = s
		}
	}
	return m
}

func insetMM(r Rect, margin float64) Rect {
	m := FromMM(margin)
	out := Rect{
		Min: Point{X: r.Min.X + m, Y: r.Min.Y + m},
		Max: Point{X: r.Max.X - m, Y: r.Max.Y - m},
	}
	if out.Min.X >= out.Max.X || out.Min.Y >= out.Max.Y {
		return r
	}
	return out
}

// ComponentHits returns every component-clearance failure between a and b.
// Parts on opposite faces do not clash unless one is through-hole. An
// elevated part may overlap a part that is not. The same footprint compared
// with itself returns nothing.
func ComponentHits(a, b *Footprint, rules ComponentRules) []ComponentHit {
	if a == nil || b == nil || a == b {
		return nil
	}
	if !a.ID.IsZero() && a.ID == b.ID {
		return nil
	}
	if !sameAssemblyFace(a, b) {
		return nil
	}
	ca, ba, oka := PackageOutlines(a, rules.CourtyardMarginMM)
	cb, bb, okb := PackageOutlines(b, rules.CourtyardMarginMM)
	if !oka || !okb {
		return nil
	}
	var out []ComponentHit
	if gap := RectGapMM(ca, cb); gap < -1e-6 {
		cx, cy := rectMid(ca, cb)
		out = append(out, ComponentHit{
			Kind: HitCourtyardOverlap,
			A:    a.Reference, B: b.Reference,
			Gap: gap, Need: 0,
			X: cx, Y: cy,
		})
	}
	gap := RectGapMM(ba, bb)
	need := rules.GapBetween(a, b)
	if gap+0.001 < need {
		cx, cy := rectMid(ba, bb)
		out = append(out, ComponentHit{
			Kind: HitBodyClearance,
			A:    a.Reference, B: b.Reference,
			Gap: gap, Need: need,
			X: cx, Y: cy,
		})
	}
	return out
}

// sameAssemblyFace reports whether two parts share an assembly envelope.
// Opposite copper faces do not, unless a drilled pad occupies both. An
// elevated body may sit over a part that is not elevated.
func sameAssemblyFace(a, b *Footprint) bool {
	if a.Elevated != b.Elevated {
		return false
	}
	if a.Layer == b.Layer {
		return true
	}
	return a.HasThroughHole() || b.HasThroughHole()
}

func rectMid(a, b Rect) (float64, float64) {
	return (a.Min.X.ToMM() + a.Max.X.ToMM() + b.Min.X.ToMM() + b.Max.X.ToMM()) / 4,
		(a.Min.Y.ToMM() + a.Max.Y.ToMM() + b.Min.Y.ToMM() + b.Max.Y.ToMM()) / 4
}
