// Package impedance is a closed-form microstrip / symmetric-stripline
// calculator. There is no 2D FEM in this tree; do not treat the numbers
// as a field solution.
package impedance

import (
	"fmt"
	"math"

	"github.com/mentasystems/fragua/internal/core"
)

// Kind of transmission-line model.
type Kind int

const (
	KindMicrostrip Kind = iota
	KindStripline
)

func (k Kind) String() string {
	if k == KindStripline {
		return "stripline"
	}
	return "microstrip"
}

// Params are the stackup numbers a closed-form model needs.
type Params struct {
	Kind      Kind
	HMM       float64 // dielectric height (mm): substrate, or signal-to-plane
	Er        float64
	TMM       float64 // copper thickness (mm)
	LayerName string
}

// Result is one computed (or solved) impedance.
type Result struct {
	Params      Params
	WidthMM     float64
	Z0          float64
	TargetOhms  float64
	RequiredWMM float64
	Kind        Kind
	Note        string
}

// LineParams extracts H, Er, T for copper layer index i.
// Missing dielectric thickness or Er is an error — never a silent FR-4 guess.
func LineParams(s core.LayerStackup, copperIndex int) (Params, error) {
	n := s.CopperCount()
	if copperIndex < 0 || copperIndex >= n {
		return Params{}, fmt.Errorf("impedance: copper layer %d out of range (stack has %d)", copperIndex, n)
	}
	if len(s.Dielectrics) == 0 {
		return Params{}, fmt.Errorf("impedance: stackup has no dielectric (need thickness and Er)")
	}
	tMM := s.CopperOz(copperIndex) * 0.035
	name := ""
	if copperIndex < len(s.Layers) {
		name = s.Layers[copperIndex].Name
	}
	if name == "" {
		name = fmt.Sprintf("L%d", copperIndex+1)
	}

	// Outer layers: microstrip against the adjacent dielectric.
	if copperIndex == 0 {
		return microstripDielectric(s, 0, tMM, name)
	}
	if copperIndex == n-1 {
		return microstripDielectric(s, len(s.Dielectrics)-1, tMM, name)
	}

	// Inner: symmetric stripline only when both adjacent dielectrics
	// exist, have Er/thickness, and match within 20%.
	above := copperIndex - 1
	below := copperIndex
	if above < 0 || below >= len(s.Dielectrics) {
		return Params{}, fmt.Errorf("impedance: inner layer %s missing adjacent dielectric", name)
	}
	d0, d1 := s.Dielectrics[above], s.Dielectrics[below]
	if d0.ThicknessMM <= 0 || d1.ThicknessMM <= 0 {
		return Params{}, fmt.Errorf("impedance: inner layer %s dielectric thickness unset", name)
	}
	if d0.Er <= 0 || d1.Er <= 0 {
		return Params{}, fmt.Errorf("impedance: inner layer %s Er unset (set dielectric Er; FR-4 is typically 4.6)", name)
	}
	hi := math.Max(d0.ThicknessMM, d1.ThicknessMM)
	if math.Abs(d0.ThicknessMM-d1.ThicknessMM)/hi > 0.20 {
		return Params{}, fmt.Errorf("impedance: inner layer %s is asymmetric stripline (H=%.3f / %.3f mm) — no closed-form here",
			name, d0.ThicknessMM, d1.ThicknessMM)
	}
	if math.Abs(d0.Er-d1.Er)/math.Max(d0.Er, d1.Er) > 0.10 {
		return Params{}, fmt.Errorf("impedance: inner layer %s Er mismatch (%.2f / %.2f)", name, d0.Er, d1.Er)
	}
	return Params{
		Kind:      KindStripline,
		HMM:       (d0.ThicknessMM + d1.ThicknessMM) / 2,
		Er:        (d0.Er + d1.Er) / 2,
		TMM:       tMM,
		LayerName: name,
	}, nil
}

func microstripDielectric(s core.LayerStackup, di int, tMM float64, name string) (Params, error) {
	if di < 0 || di >= len(s.Dielectrics) {
		return Params{}, fmt.Errorf("impedance: layer %s has no adjacent dielectric", name)
	}
	d := s.Dielectrics[di]
	if d.ThicknessMM <= 0 {
		return Params{}, fmt.Errorf("impedance: layer %s dielectric thickness unset", name)
	}
	if d.Er <= 0 {
		return Params{}, fmt.Errorf("impedance: layer %s Er unset (set dielectric Er; FR-4 is typically 4.6)", name)
	}
	return Params{
		Kind:      KindMicrostrip,
		HMM:       d.ThicknessMM,
		Er:        d.Er,
		TMM:       tMM,
		LayerName: name,
	}, nil
}

// Z0 is the single-ended characteristic impedance for width W (mm).
func Z0(p Params, widthMM float64) (float64, error) {
	if p.HMM <= 0 || p.Er <= 0 {
		return 0, fmt.Errorf("impedance: H and Er must be set")
	}
	if widthMM <= 0 {
		return 0, fmt.Errorf("impedance: width must be > 0")
	}
	switch p.Kind {
	case KindStripline:
		return striplineIPC2141(p.Er, p.HMM, widthMM, p.TMM), nil
	default:
		return microstripHammerstad(p.Er, p.HMM, widthMM, p.TMM), nil
	}
}

// WidthForZ solves for the width (mm) that yields targetOhms.
func WidthForZ(p Params, targetOhms float64) (float64, error) {
	if targetOhms <= 0 {
		return 0, fmt.Errorf("impedance: target Z must be > 0")
	}
	lo, hi := 0.02, 50.0
	zLo, err := Z0(p, lo)
	if err != nil {
		return 0, err
	}
	zHi, err := Z0(p, hi)
	if err != nil {
		return 0, err
	}
	// Z falls as W grows. If target is outside the bracket, say so.
	if targetOhms > zLo+1e-6 || targetOhms < zHi-1e-6 {
		return 0, fmt.Errorf("impedance: target %.1f Ω is outside closed-form range (W=%.2f→%.1f Ω … W=%.2f→%.1f Ω)",
			targetOhms, lo, zLo, hi, zHi)
	}
	for i := 0; i < 48; i++ {
		mid := (lo + hi) / 2
		z, err := Z0(p, mid)
		if err != nil {
			return 0, err
		}
		if z > targetOhms {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2, nil
}

// microstripHammerstad is Hammerstad/Jensen (IEEE Trans. MTT, 1980)
// with the Wheeler thickness correction from the same paper / Wadell
// Transmission Line Design Handbook §4.4.
//
//	ε_eff = (εr+1)/2 + (εr-1)/2 * (1 + 10/u)^(-a b)     (Jensen)
//	Z0    = 60/√ε_eff * ln(6/u + √(1+(2/u)²))           (Hammerstad)
//
// u = Weff/H after the thickness correction
//
//	ΔW/H = (1.25/π)(T/H)[1 + ln(4π W/T)]     W/H ≤ 1/(2π)
//	ΔW/H = (1.25/π)(T/H)[1 + ln(2H/T)]       W/H >  1/(2π)
func microstripHammerstad(er, h, w, t float64) float64 {
	if h <= 0 || w <= 0 {
		return 0
	}
	we := w
	if t > 0 {
		th := t / h
		var dwh float64
		if w/h <= 1.0/(2*math.Pi) {
			dwh = (1.25 / math.Pi) * th * (1 + math.Log(4*math.Pi*w/t))
		} else {
			dwh = (1.25 / math.Pi) * th * (1 + math.Log(2*h/t))
		}
		we = w + dwh*h
	}
	u := we / h
	// Hammerstad–Jensen ε_eff (1980).
	a := 1 + (1.0/49.0)*math.Log((math.Pow(u, 4)+math.Pow(u/52, 2))/(math.Pow(u, 4)+0.432)) +
		(1.0/18.7)*math.Log(1+math.Pow(u/18.1, 3))
	b := 0.564 * math.Pow((er-0.9)/(er+3), 0.053)
	eeff := (er+1)/2 + (er-1)/2*math.Pow(1+10/u, -a*b)
	// Characteristic impedance (Hammerstad).
	return (60 / math.Sqrt(eeff)) * math.Log(6/u+math.Sqrt(1+4/(u*u)))
}

// striplineIPC2141 is the IPC-2141 / Wadell symmetric-stripline formula:
//
//	Z0 = (60/√εr) * ln(1.9 (2H + T) / (0.8 W + T))
//
// H is the dielectric from the signal to each plane.
func striplineIPC2141(er, h, w, t float64) float64 {
	if er <= 0 || h <= 0 || w <= 0 {
		return 0
	}
	if t < 0 {
		t = 0
	}
	num := 1.9 * (2*h + t)
	den := 0.8*w + t
	if den <= 0 {
		return 0
	}
	return (60 / math.Sqrt(er)) * math.Log(num/den)
}

// TraceWidthMM is the typical routed width of net on the board (mm).
// 0 if the net has no traces.
func TraceWidthMM(b *core.Board, net string) (width float64, layer core.Layer, ok bool) {
	if b == nil {
		return 0, core.Layer{}, false
	}
	var sum float64
	var n int
	var lyr core.Layer
	for i := range b.Traces {
		t := &b.Traces[i]
		if t.Net != net {
			continue
		}
		sum += t.Width.ToMM()
		n++
		lyr = t.Layer
	}
	if n == 0 {
		return 0, core.Layer{}, false
	}
	return sum / float64(n), lyr, true
}

// AnalyzeNet reports actual Z (if routed) and required width for a class target.
// Diff-pair nets get single-ended Z only — we do not print a fake Zdiff.
func AnalyzeNet(b *core.Board, sch *core.Schematic, net string) (Result, error) {
	if net == "" {
		return Result{}, fmt.Errorf("impedance: net name required")
	}
	stack := core.Default2Layer()
	if b != nil {
		stack = b.StackupOrDefault()
	}
	layerIdx := 0
	width := 0.0
	if w, lyr, ok := TraceWidthMM(b, net); ok {
		width = w
		layerIdx = int(lyr.Index)
	}
	p, err := LineParams(stack, layerIdx)
	if err != nil {
		return Result{}, err
	}
	res := Result{Params: p, WidthMM: width, Kind: p.Kind}
	if sch != nil {
		if n := sch.Nets[net]; n != nil && n.DiffPair != "" {
			res.Note = fmt.Sprintf("diff pair %s/%s: single-ended Z only (no coupled-microstrip Zdiff)", net, n.DiffPair)
		}
		if cls := classOf(sch, net); cls != nil && cls.ImpedanceOhms > 0 {
			res.TargetOhms = cls.ImpedanceOhms
			rw, err := WidthForZ(p, cls.ImpedanceOhms)
			if err != nil {
				return res, err
			}
			res.RequiredWMM = rw
		}
	}
	if width > 0 {
		z, err := Z0(p, width)
		if err != nil {
			return res, err
		}
		res.Z0 = z
	}
	return res, nil
}

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

// Format is a one-line agent-friendly report.
func (r Result) Format(net string) string {
	var s string
	s = fmt.Sprintf("%s: %s %s H=%.3f mm Er=%.2f T=%.3f mm",
		net, r.Kind, r.Params.LayerName, r.Params.HMM, r.Params.Er, r.Params.TMM)
	if r.WidthMM > 0 && r.Z0 > 0 {
		s += fmt.Sprintf("  width=%.3f mm Z0=%.1f Ω", r.WidthMM, r.Z0)
	} else if r.WidthMM == 0 {
		s += "  (no traces)"
	}
	if r.TargetOhms > 0 {
		s += fmt.Sprintf("  target=%.0f Ω → width %.3f mm", r.TargetOhms, r.RequiredWMM)
		if r.WidthMM > 0 && r.RequiredWMM > 0 {
			pct := 100 * (r.WidthMM - r.RequiredWMM) / r.RequiredWMM
			s += fmt.Sprintf(" (actual %+.1f%%)", pct)
		}
	}
	if r.Note != "" {
		s += "  " + r.Note
	}
	return s
}
