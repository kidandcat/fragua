package parts

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/mentasystems/fragua/internal/core"
)

// IPC-7351B land-pattern generators — offline, no library needed.
//
// Every family reduces to the same three IPC equations, taken over the
// component's own tolerances (RSS-combined with the fabrication and placement
// allowances F and P):
//
//	Zmax = Lmin + 2·JT + √(CL² + F² + P²)     outer span of the land pair
//	Gmin = Smax − 2·JH − √(CS² + F² + P²)     inner gap between the lands
//	Xmax = Wmin + 2·JS + √(CW² + F² + P²)     land width across the lead
//
// with L the lead-tip-to-lead-tip span, S = L − 2·(lead foot) the inner span,
// W the lead width, and JT/JH/JS the toe/heel/side fillets, which is where the
// density level enters. Land length is then Y = (Z−G)/2 and the land centre
// sits at C = (Z+G)/2 from the opposite land, i.e. ±(Z+G)/4 about the origin.
//
// No-lead families (QFN/DFN) have no gullwing foot, so they build the land
// directly from the terminal: Y = leadLen + JT + JH, outer edge at body/2 + JT.
//
// Through-hole families use IPC-2222 level B instead: drill = lead + 0.20 mm,
// land = drill + 0.60 mm.
const (
	ipcFabAllowance   = 0.05  // F — etch/registration tolerance
	ipcPlaceAccuracy  = 0.025 // P — pick-and-place accuracy
	ipcSilkWidth      = 0.12
	ipcSilkClearance  = 0.20 // silk-to-pad air
	ipcTHLeadToDrill  = 0.20 // IPC-2222 level B drill oversize
	ipcTHDrillToLand  = 0.60 // IPC-2222 level B annular ring (both sides)
	ipcPin1MarkerSize = 0.5
)

// Density is the IPC land-protrusion level.
type Density int

// Density levels: Nominal (level B) is the zero value and the default; Most
// (level A) suits wave/hand soldering, Least (level C) high-density reflow.
const (
	DensityNominal Density = iota
	DensityMost
	DensityLeast
)

// ParseDensity accepts M/N/L (or most/nominal/least, A/B/C).
func ParseDensity(s string) (Density, error) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "", "N", "NOMINAL", "B":
		return DensityNominal, nil
	case "M", "MOST", "MAX", "A":
		return DensityMost, nil
	case "L", "LEAST", "MIN", "C":
		return DensityLeast, nil
	}
	return DensityNominal, fmt.Errorf("density: expected M, N or L, got %q", s)
}

func (d Density) String() string {
	switch d {
	case DensityMost:
		return "M"
	case DensityLeast:
		return "L"
	default:
		return "N"
	}
}

// courtyardExcess is the IPC courtyard air around the land/body hull.
func (d Density) courtyardExcess() float64 {
	switch d {
	case DensityMost:
		return 0.50
	case DensityLeast:
		return 0.12
	default:
		return 0.25
	}
}

// fillets is a toe/heel/side fillet triple in mm.
type fillets struct{ toe, heel, side float64 }

func pick(d Density, most, nominal, least fillets) fillets {
	switch d {
	case DensityMost:
		return most
	case DensityLeast:
		return least
	default:
		return nominal
	}
}

// tol is a nominal ± tolerance pair.
type tol struct{ nom, tol float64 }

func (t tol) min() float64  { return t.nom - t.tol }
func (t tol) max() float64  { return t.nom + t.tol }
func (t tol) span() float64 { return 2 * t.tol }
func rss(vals ...float64) float64 {
	s := 0.0
	for _, v := range vals {
		s += v * v
	}
	return math.Sqrt(s)
}

// land is the computed pair geometry: span Z, gap G, width X.
type land struct{ z, g, x float64 }

func (l land) padLen() float64    { return (l.z - l.g) / 2 }
func (l land) padCentre() float64 { return (l.z + l.g) / 4 }

// solveLand applies the three IPC equations.
func solveLand(l, w, foot tol, f fillets) land {
	sMax := l.max() - 2*foot.min()
	cs := rss(l.span(), foot.span(), foot.span())
	return land{
		z: l.min() + 2*f.toe + rss(l.span(), ipcFabAllowance, ipcPlaceAccuracy),
		g: sMax - 2*f.heel - rss(cs, ipcFabAllowance, ipcPlaceAccuracy),
		x: w.min() + 2*f.side + rss(w.span(), ipcFabAllowance, ipcPlaceAccuracy),
	}
}

// GenSpec is a parsed `lib-gen` request.
type GenSpec struct {
	Key     string
	Family  string
	Size    string  // chip code: 0402, 0603, …
	Pins    int     // total pin count
	Pitch   float64 // mm
	Body    float64 // body width (SOIC/TSSOP) or square body (QFN/QFP)
	BodyLen float64 // body length for rectangular no-lead bodies
	EPX     float64 // exposed-pad size (QFN/DFN), 0 = none
	EPY     float64
	Rows    int     // header rows
	Spacing float64 // DIP row spacing
	Density Density
	// Kind forces the schematic symbol kind; empty infers it from Key/family.
	Kind string
}

// Families lists every generator name lib-gen understands.
func Families() []string {
	return []string{
		"chip", "sot23", "sot23-5", "sot23-6", "sot223", "sot89",
		"soic", "tssop", "ssop", "msop", "qfn", "dfn", "qfp", "lqfp",
		"dip", "header",
	}
}

// Generate builds a land pattern from spec.
func Generate(spec GenSpec) (*Part, error) {
	family := strings.ToLower(strings.TrimSpace(spec.Family))
	if spec.Key == "" {
		return nil, fmt.Errorf("lib-gen: a NAME is required")
	}
	var (
		part *Part
		err  error
	)
	switch family {
	case "chip":
		part, err = genChip(spec)
	case "sot23", "sot23-3", "sot-23":
		part, err = genGullwingDual(spec, sot23Dims(3))
	case "sot23-5", "sot-23-5":
		part, err = genGullwingDual(spec, sot23Dims(5))
	case "sot23-6", "sot-23-6":
		part, err = genGullwingDual(spec, sot23Dims(6))
	case "sot223", "sot-223":
		part, err = genGullwingDual(spec, sot223Dims())
	case "sot89", "sot-89":
		part, err = genGullwingDual(spec, sot89Dims())
	case "soic":
		part, err = genSOIC(spec)
	case "tssop", "ssop", "msop":
		part, err = genTSSOP(spec, family)
	case "qfn", "dfn":
		part, err = genNoLead(spec, family)
	case "qfp", "lqfp":
		part, err = genQFP(spec)
	case "dip":
		part, err = genDIP(spec)
	case "header", "pinheader":
		part, err = genHeader(spec)
	default:
		return nil, fmt.Errorf("lib-gen: unknown family %q (try %s)", spec.Family, strings.Join(Families(), ", "))
	}
	if err != nil {
		return nil, err
	}
	part.Entry.Key = spec.Key
	// A two-pad generator says nothing about what is soldered on it, so the
	// discrete kind comes from an explicit kind= or from the NAME the agent
	// chose (R0603 → resistor, C0402 → capacitor, L… → inductor).
	if spec.Kind != "" {
		part.Kind = spec.Kind
	} else if part.Kind == "" || part.Kind == "generic_ic" {
		part.Kind = inferKind(chipKindHint(spec.Key), part.Entry.Pads)
	}
	if len(part.Pins) == 0 {
		part.Pins = genericPins(part.Entry.Pads)
	}
	part.finish(SourceIPC)
	return part, nil
}

// chipKindHint turns a library key into an inferKind hint from its first letter.
func chipKindHint(key string) string {
	if key == "" {
		return ""
	}
	switch key[0] {
	case 'R', 'r':
		return "resistor"
	case 'C', 'c':
		return "capacitor"
	case 'L', 'l':
		return "inductor"
	case 'D', 'd':
		return "diode"
	}
	return ""
}

// ─── chip ────────────────────────────────────────────────────────────

// chipDims holds the IPC body dimensions of a two-terminal chip:
// L = length, W = width, T = termination band.
type chipDims struct{ l, w, t tol }

var chipTable = map[string]chipDims{
	"0201": {l: tol{0.60, 0.03}, w: tol{0.30, 0.03}, t: tol{0.15, 0.05}},
	"0402": {l: tol{1.00, 0.10}, w: tol{0.50, 0.10}, t: tol{0.25, 0.10}},
	"0603": {l: tol{1.60, 0.15}, w: tol{0.80, 0.15}, t: tol{0.35, 0.15}},
	"0805": {l: tol{2.00, 0.20}, w: tol{1.25, 0.20}, t: tol{0.40, 0.20}},
	"1206": {l: tol{3.20, 0.20}, w: tol{1.60, 0.20}, t: tol{0.50, 0.20}},
	"1210": {l: tol{3.20, 0.20}, w: tol{2.50, 0.20}, t: tol{0.50, 0.20}},
	"2512": {l: tol{6.35, 0.25}, w: tol{3.20, 0.20}, t: tol{0.60, 0.25}},
}

// ChipSizes lists the supported chip codes, smallest first.
func ChipSizes() []string {
	out := make([]string, 0, len(chipTable))
	for k := range chipTable {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func genChip(spec GenSpec) (*Part, error) {
	code := strings.TrimSpace(spec.Size)
	d, ok := chipTable[code]
	if !ok {
		return nil, fmt.Errorf("lib-gen chip: size=%s unknown (have %s)", spec.Size, strings.Join(ChipSizes(), " "))
	}
	// 0201 is too small for the standard chip fillets, so it gets its own set.
	f := pick(spec.Density,
		fillets{0.55, 0.05, 0.05}, fillets{0.35, 0.00, 0.03}, fillets{0.15, -0.05, 0.01})
	if code == "0201" {
		f = pick(spec.Density,
			fillets{0.30, 0.05, 0.05}, fillets{0.20, 0.00, 0.00}, fillets{0.10, -0.02, -0.02})
	}
	l := solveLand(d.l, d.w, d.t, f)
	c := l.padCentre()
	pads := []core.LibraryPad{
		{Number: "1", XMM: -c, WMM: l.padLen(), HMM: l.x},
		{Number: "2", XMM: c, WMM: l.padLen(), HMM: l.x},
	}
	body := core.BodyRect{
		MinXMM: -d.l.nom / 2, MinYMM: -d.w.nom / 2,
		MaxXMM: d.l.nom / 2, MaxYMM: d.w.nom / 2,
	}
	part := &Part{Entry: core.LibraryEntry{
		Description: fmt.Sprintf("chip %s land, IPC-7351B density %s", code, spec.Density),
		Pads:        pads,
	}}
	// Silk runs above and below the body, clear of both lands.
	half := d.w.nom/2 + ipcSilkClearance
	xEnd := c - l.padLen()/2 - ipcSilkClearance
	if xEnd > 0.05 {
		part.Entry.Silk = append(part.Entry.Silk,
			silkLine(-xEnd, half, xEnd, half), silkLine(-xEnd, -half, xEnd, -half))
	}
	part.Entry.Silk = append(part.Entry.Silk, pin1Marker(-c-l.padLen()/2-ipcSilkClearance, 0)...)
	part.Entry.BodyRect = courtyard(spec.Density, pads, body)
	return part, nil
}

// ─── gullwing (SOT / SOIC / TSSOP / QFP) ─────────────────────────────

// gullwing describes a lead row: span L across the leads, foot length, lead
// width W, pitch, plus the plastic body.
type gullwing struct {
	span, foot, width tol
	pitch             float64
	pins              int // pins per side (dual) or per side (quad)
	bodyX, bodyY      float64
	label             string
	// numbering runs down one side then back up the other (SOT/SOIC), unless
	// pin3Only marks a SOT-23-style row with a single lead on the far side.
	farSide  []string // explicit numbers on the +X side, nil = automatic
	nearSide []string
}

func sot23Dims(pins int) gullwing {
	g := gullwing{
		span:  tol{2.40, 0.20},
		foot:  tol{0.45, 0.10},
		width: tol{0.40, 0.10},
		pitch: 0.95, bodyX: 1.30, bodyY: 2.90, label: "SOT-23",
	}
	switch pins {
	case 5:
		g.label, g.pitch = "SOT-23-5", 0.95
		g.nearSide, g.farSide = []string{"1", "2", "3"}, []string{"4", "5"}
	case 6:
		g.label = "SOT-23-6"
		g.nearSide, g.farSide = []string{"1", "2", "3"}, []string{"6", "5", "4"}
	default:
		g.nearSide, g.farSide = []string{"1", "2"}, []string{"3"}
	}
	return g
}

func sot223Dims() gullwing {
	return gullwing{
		span:  tol{7.00, 0.20},
		foot:  tol{0.95, 0.15},
		width: tol{0.75, 0.10},
		pitch: 2.30, bodyX: 3.50, bodyY: 6.50, label: "SOT-223",
		nearSide: []string{"1", "2", "3"}, farSide: []string{"4"},
	}
}

func sot89Dims() gullwing {
	return gullwing{
		span:  tol{4.25, 0.20},
		foot:  tol{0.90, 0.15},
		width: tol{0.48, 0.10},
		pitch: 1.50, bodyX: 2.50, bodyY: 4.50, label: "SOT-89",
		nearSide: []string{"1", "2", "3"}, farSide: []string{"4"},
	}
}

func gullwingFillets(d Density) fillets {
	return pick(d, fillets{0.55, 0.45, 0.05}, fillets{0.35, 0.35, 0.03}, fillets{0.15, 0.25, 0.01})
}

// genGullwingDual lays two lead rows out along ±X, leads running in X.
func genGullwingDual(spec GenSpec, g gullwing) (*Part, error) {
	l := solveLand(g.span, g.width, g.foot, gullwingFillets(spec.Density))
	c := l.padCentre()
	padW, padH := l.padLen(), l.x

	place := func(nums []string, x float64) []core.LibraryPad {
		out := make([]core.LibraryPad, 0, len(nums))
		start := -g.pitch * float64(len(nums)-1) / 2
		for i, n := range nums {
			out = append(out, core.LibraryPad{
				Number: n, XMM: x, YMM: -(start + float64(i)*g.pitch),
				WMM: padW, HMM: padH,
			})
		}
		return out
	}
	pads := append(place(g.nearSide, -c), place(g.farSide, c)...)
	body := core.BodyRect{MinXMM: -g.bodyX / 2, MinYMM: -g.bodyY / 2, MaxXMM: g.bodyX / 2, MaxYMM: g.bodyY / 2}

	part := &Part{Entry: core.LibraryEntry{
		Description: fmt.Sprintf("%s land, IPC-7351B density %s", g.label, spec.Density),
		Pads:        pads,
	}}
	part.Entry.Silk = bodySilk(body, -c, padW)
	part.Entry.BodyRect = courtyard(spec.Density, pads, body)
	return part, nil
}

// genSOIC builds an N-pin SOIC. Narrow bodies (3.9 mm) have a 6.0 mm lead span,
// wide bodies (7.5 mm) a 10.3 mm one; both at the JEDEC 1.27 mm pitch.
func genSOIC(spec GenSpec) (*Part, error) {
	if spec.Pins < 4 || spec.Pins%2 != 0 {
		return nil, fmt.Errorf("lib-gen soic: pins=N must be an even number ≥ 4")
	}
	pitch := spec.Pitch
	if pitch <= 0 {
		pitch = 1.27
	}
	bodyX := spec.Body
	if bodyX <= 0 {
		bodyX = 3.9
	}
	span := tol{6.00, 0.20}
	if bodyX > 5.5 {
		span = tol{10.30, 0.30}
	}
	perSide := spec.Pins / 2
	g := gullwing{
		span: span, foot: tol{0.83, 0.20}, width: tol{0.42, 0.09},
		pitch: pitch, bodyX: bodyX,
		bodyY: pitch*float64(perSide) + 0.6,
		label: fmt.Sprintf("SOIC-%d", spec.Pins),
	}
	g.nearSide, g.farSide = dualNumbering(spec.Pins)
	return genGullwingDual(spec, g)
}

// genTSSOP builds a fine-pitch shrink package (TSSOP/SSOP/MSOP).
func genTSSOP(spec GenSpec, family string) (*Part, error) {
	if spec.Pins < 4 || spec.Pins%2 != 0 {
		return nil, fmt.Errorf("lib-gen %s: pins=N must be an even number ≥ 4", family)
	}
	pitch := spec.Pitch
	if pitch <= 0 {
		pitch = map[string]float64{"tssop": 0.65, "ssop": 0.65, "msop": 0.65}[family]
	}
	bodyX := spec.Body
	if bodyX <= 0 {
		bodyX = map[string]float64{"tssop": 4.40, "ssop": 5.30, "msop": 3.00}[family]
	}
	perSide := spec.Pins / 2
	g := gullwing{
		span: tol{bodyX + 1.90, 0.20}, foot: tol{0.60, 0.15}, width: tol{pitch * 0.42, 0.07},
		pitch: pitch, bodyX: bodyX,
		bodyY: pitch*float64(perSide) + 0.6,
		label: fmt.Sprintf("%s-%d", strings.ToUpper(family), spec.Pins),
	}
	g.nearSide, g.farSide = dualNumbering(spec.Pins)
	return genGullwingDual(spec, g)
}

// dualNumbering splits N pins into the counter-clockwise dual-row order:
// 1..N/2 down the left, N/2+1..N back up the right.
func dualNumbering(pins int) (near, far []string) {
	half := pins / 2
	for i := 1; i <= half; i++ {
		near = append(near, strconv.Itoa(i))
	}
	for i := pins; i > half; i-- {
		far = append(far, strconv.Itoa(i))
	}
	return near, far
}

// genQFP builds a quad flat pack: four gullwing rows, pin 1 top-left, numbering
// counter-clockwise (left side down, bottom right, right side up, top left).
func genQFP(spec GenSpec) (*Part, error) {
	if spec.Pins < 8 || spec.Pins%4 != 0 {
		return nil, fmt.Errorf("lib-gen qfp: pins=N must be a multiple of 4 and ≥ 8")
	}
	pitch := spec.Pitch
	if pitch <= 0 {
		pitch = 0.5
	}
	body := spec.Body
	if body <= 0 {
		body = float64(spec.Pins/4)*pitch + 1.6
	}
	perSide := spec.Pins / 4
	span := tol{body + 1.90, 0.20}
	foot := tol{0.60, 0.15}
	width := tol{pitch * 0.42, 0.07}
	l := solveLand(span, width, foot, gullwingFillets(spec.Density))
	c := l.padCentre()

	pads := make([]core.LibraryPad, 0, spec.Pins)
	n := 1
	start := -pitch * float64(perSide-1) / 2
	for i := 0; i < perSide; i++ { // left, top → bottom
		pads = append(pads, core.LibraryPad{Number: strconv.Itoa(n), XMM: -c, YMM: -(start + float64(i)*pitch), WMM: l.padLen(), HMM: l.x})
		n++
	}
	for i := 0; i < perSide; i++ { // bottom, left → right
		pads = append(pads, core.LibraryPad{Number: strconv.Itoa(n), XMM: start + float64(i)*pitch, YMM: -c, WMM: l.x, HMM: l.padLen()})
		n++
	}
	for i := perSide - 1; i >= 0; i-- { // right, bottom → top
		pads = append(pads, core.LibraryPad{Number: strconv.Itoa(n), XMM: c, YMM: -(start + float64(i)*pitch), WMM: l.padLen(), HMM: l.x})
		n++
	}
	for i := perSide - 1; i >= 0; i-- { // top, right → left
		pads = append(pads, core.LibraryPad{Number: strconv.Itoa(n), XMM: start + float64(i)*pitch, YMM: c, WMM: l.x, HMM: l.padLen()})
		n++
	}
	rect := core.BodyRect{MinXMM: -body / 2, MinYMM: -body / 2, MaxXMM: body / 2, MaxYMM: body / 2}
	part := &Part{Entry: core.LibraryEntry{
		Description: fmt.Sprintf("%s-%d P%.2f land, IPC-7351B density %s",
			strings.ToUpper(spec.Family), spec.Pins, pitch, spec.Density),
		Pads: pads,
	}}
	part.Entry.Silk = quadSilk(rect, c+l.padLen()/2+ipcSilkClearance)
	part.Entry.BodyRect = courtyard(spec.Density, pads, rect)
	return part, nil
}

// ─── no-lead (QFN / DFN) ─────────────────────────────────────────────

// genNoLead builds a QFN (four rows) or DFN (two rows). The terminal has no
// gullwing foot, so the land is built straight off it: outer edge at body/2 +
// toe, inner edge at body/2 − leadLen − heel.
func genNoLead(spec GenSpec, family string) (*Part, error) {
	sides := 4
	if family == "dfn" {
		sides = 2
	}
	if spec.Pins < sides || spec.Pins%sides != 0 {
		return nil, fmt.Errorf("lib-gen %s: pins=N must be a multiple of %d", family, sides)
	}
	pitch := spec.Pitch
	if pitch <= 0 {
		pitch = 0.5
	}
	perSide := spec.Pins / sides
	body := spec.Body
	if body <= 0 {
		body = float64(perSide)*pitch + 1.0
	}
	bodyY := spec.BodyLen
	if bodyY <= 0 {
		bodyY = body
		if sides == 2 {
			bodyY = float64(perSide)*pitch + 1.0
		}
	}
	f := pick(spec.Density,
		fillets{0.30, 0.10, 0.05}, fillets{0.20, 0.05, 0.00}, fillets{0.10, 0.00, -0.02})
	leadLen := 0.40
	leadW := math.Min(pitch*0.5, 0.35)
	padLen := leadLen + f.toe + f.heel
	padW := leadW + 2*f.side

	pads := make([]core.LibraryPad, 0, spec.Pins+1)
	n := 1
	push := func(x, y, w, h float64) {
		pads = append(pads, core.LibraryPad{Number: strconv.Itoa(n), XMM: x, YMM: y, WMM: w, HMM: h})
		n++
	}
	// Land centre: outer edge at half + toe, so centre = half + toe − len/2.
	cX := body/2 + f.toe - padLen/2
	cY := bodyY/2 + f.toe - padLen/2
	startY := -pitch * float64(perSide-1) / 2
	for i := 0; i < perSide; i++ { // left row, top → bottom
		push(-cX, -(startY + float64(i)*pitch), padLen, padW)
	}
	if sides == 4 {
		startX := -pitch * float64(perSide-1) / 2
		for i := 0; i < perSide; i++ { // bottom row, left → right
			push(startX+float64(i)*pitch, -cY, padW, padLen)
		}
	}
	for i := perSide - 1; i >= 0; i-- { // right row, bottom → top
		push(cX, -(startY + float64(i)*pitch), padLen, padW)
	}
	if sides == 4 {
		startX := -pitch * float64(perSide-1) / 2
		for i := perSide - 1; i >= 0; i-- { // top row, right → left
			push(startX+float64(i)*pitch, cY, padW, padLen)
		}
	}
	if spec.EPX > 0 {
		epy := spec.EPY
		if epy <= 0 {
			epy = spec.EPX
		}
		pads = append(pads, core.LibraryPad{Number: strconv.Itoa(n), WMM: spec.EPX, HMM: epy})
	}
	rect := core.BodyRect{MinXMM: -body / 2, MinYMM: -bodyY / 2, MaxXMM: body / 2, MaxYMM: bodyY / 2}
	part := &Part{Entry: core.LibraryEntry{
		Description: fmt.Sprintf("%s-%d P%.2f %.1fx%.1fmm land, IPC-7351B density %s",
			strings.ToUpper(family), spec.Pins, pitch, body, bodyY, spec.Density),
		Pads: pads,
	}}
	if sides == 4 {
		part.Entry.Silk = quadSilk(rect, math.Max(cX, cY)+padLen/2+ipcSilkClearance)
	} else {
		part.Entry.Silk = bodySilk(rect, -cX, padLen)
	}
	part.Entry.BodyRect = courtyard(spec.Density, pads, rect)
	return part, nil
}

// ─── through-hole (DIP / headers) ────────────────────────────────────

// thLand returns the IPC-2222 level-B drill and land for a lead of leadDia.
// pitch (0 = unconstrained) caps the land so neighbouring holes keep air.
func thLand(leadDia, pitch float64, d Density) (drill, pad float64) {
	drill = leadDia + ipcTHLeadToDrill
	pad = drill + ipcTHDrillToLand
	switch d {
	case DensityMost:
		pad += 0.20
	case DensityLeast:
		pad -= 0.15
	}
	if pitch > 0 {
		pad = math.Min(pad, pitch-ipcSilkClearance/2)
	}
	return drill, pad
}

func genDIP(spec GenSpec) (*Part, error) {
	if spec.Pins < 4 || spec.Pins%2 != 0 {
		return nil, fmt.Errorf("lib-gen dip: pins=N must be an even number ≥ 4")
	}
	pitch := spec.Pitch
	if pitch <= 0 {
		pitch = 2.54
	}
	rowSpacing := spec.Spacing
	if rowSpacing <= 0 {
		rowSpacing = 7.62
	}
	drill, padDia := thLand(0.55, pitch, spec.Density)
	perSide := spec.Pins / 2
	startY := -pitch * float64(perSide-1) / 2
	pads := make([]core.LibraryPad, 0, spec.Pins)
	add := func(num int, x, y float64) {
		d := drill
		pads = append(pads, core.LibraryPad{
			Number: strconv.Itoa(num), XMM: x, YMM: y,
			WMM: padDia, HMM: padDia, DrillMM: &d,
		})
	}
	for i := 0; i < perSide; i++ { // left column, top → bottom
		add(i+1, -rowSpacing/2, -(startY + float64(i)*pitch))
	}
	for i := perSide - 1; i >= 0; i-- { // right column, bottom → top
		add(spec.Pins-i, rowSpacing/2, -(startY + float64(i)*pitch))
	}
	bodyY := pitch*float64(perSide) + 0.5
	rect := core.BodyRect{MinXMM: -rowSpacing/2 + 0.5, MinYMM: -bodyY / 2, MaxXMM: rowSpacing/2 - 0.5, MaxYMM: bodyY / 2}
	part := &Part{Entry: core.LibraryEntry{
		Description: fmt.Sprintf("DIP-%d P%.2f row %.2fmm, IPC-2222 level B", spec.Pins, pitch, rowSpacing),
		Pads:        pads,
	}}
	part.Entry.Silk = bodySilk(rect, -rowSpacing/2, padDia)
	part.Entry.BodyRect = courtyard(spec.Density, pads, rect)
	return part, nil
}

// genHeader builds a 1- or 2-row through-hole pin header. Numbering matches
// the usual 2×N convention: odd on row 1, even on row 2, counting along.
func genHeader(spec GenSpec) (*Part, error) {
	rows := spec.Rows
	if rows == 0 {
		rows = 1
	}
	if rows != 1 && rows != 2 {
		return nil, fmt.Errorf("lib-gen header: rows= must be 1 or 2")
	}
	if spec.Pins < 1 {
		return nil, fmt.Errorf("lib-gen header: pins=N required (positions per row)")
	}
	pitch := spec.Pitch
	if pitch <= 0 {
		pitch = 2.54
	}
	// Square post side scales with pitch: 0.64 at 2.54, 0.5 at 2.0, 0.4 at 1.27.
	lead := 0.64
	switch {
	case pitch <= 1.30:
		lead = 0.40
	case pitch <= 2.10:
		lead = 0.50
	}
	drill, padDia := thLand(lead, pitch, spec.Density)
	startY := -pitch * float64(spec.Pins-1) / 2
	rowX := 0.0
	if rows == 2 {
		rowX = pitch / 2
	}
	pads := make([]core.LibraryPad, 0, spec.Pins*rows)
	for i := 0; i < spec.Pins; i++ {
		for r := 0; r < rows; r++ {
			num := i*rows + r + 1
			d := drill
			x := -rowX + float64(r)*pitch
			pads = append(pads, core.LibraryPad{
				Number: strconv.Itoa(num), XMM: x, YMM: -(startY + float64(i)*pitch),
				WMM: padDia, HMM: padDia, DrillMM: &d,
			})
		}
	}
	bodyX := pitch*float64(rows-1) + pitch
	bodyY := pitch * float64(spec.Pins)
	rect := core.BodyRect{MinXMM: -bodyX / 2, MinYMM: -bodyY / 2, MaxXMM: bodyX / 2, MaxYMM: bodyY / 2}
	part := &Part{Entry: core.LibraryEntry{
		Description: fmt.Sprintf("pin header %dx%02d P%.2fmm THT", rows, spec.Pins, pitch),
		Pads:        pads,
	}}
	part.Entry.Silk = append(rectSilk(rect), pin1Marker(rect.MinXMM-ipcSilkClearance, rect.MaxYMM)...)
	part.Entry.BodyRect = courtyard(spec.Density, pads, rect)
	part.RefPrefix = "J"
	return part, nil
}

// ─── silk / courtyard helpers ────────────────────────────────────────

func silkLine(x1, y1, x2, y2 float64) core.LibrarySilk {
	return core.LibrarySilk{
		Kind: "line", Layer: core.SilkTop,
		X1MM: x1, Y1MM: y1, X2MM: x2, Y2MM: y2, WidthMM: ipcSilkWidth,
	}
}

func rectSilk(r core.BodyRect) []core.LibrarySilk {
	return []core.LibrarySilk{
		silkLine(r.MinXMM, r.MinYMM, r.MaxXMM, r.MinYMM),
		silkLine(r.MaxXMM, r.MinYMM, r.MaxXMM, r.MaxYMM),
		silkLine(r.MaxXMM, r.MaxYMM, r.MinXMM, r.MaxYMM),
		silkLine(r.MinXMM, r.MaxYMM, r.MinXMM, r.MinYMM),
	}
}

// pin1Marker is a small open triangle pointing at pin 1.
func pin1Marker(x, y float64) []core.LibrarySilk {
	s := ipcPin1MarkerSize
	return []core.LibrarySilk{
		silkLine(x, y, x-s, y+s/2),
		silkLine(x-s, y+s/2, x-s, y-s/2),
		silkLine(x-s, y-s/2, x, y),
	}
}

// bodySilk draws the two body edges that run parallel to a dual lead row (the
// other two would cross the lands), plus a pin-1 marker outside the first pad.
func bodySilk(r core.BodyRect, pad1X, padLen float64) []core.LibrarySilk {
	out := []core.LibrarySilk{
		silkLine(r.MinXMM, r.MaxYMM, r.MaxXMM, r.MaxYMM),
		silkLine(r.MinXMM, r.MinYMM, r.MaxXMM, r.MinYMM),
	}
	return append(out, pin1Marker(pad1X-padLen/2-ipcSilkClearance, r.MaxYMM)...)
}

// quadSilk draws four short corner brackets outside the lands, plus a pin-1
// marker at the top-left corner.
func quadSilk(r core.BodyRect, reach float64) []core.LibrarySilk {
	arm := math.Max(0.4, math.Min(r.MaxXMM, r.MaxYMM)*0.25)
	corners := [][2]float64{
		{r.MinXMM, r.MaxYMM}, {r.MaxXMM, r.MaxYMM},
		{r.MaxXMM, r.MinYMM}, {r.MinXMM, r.MinYMM},
	}
	var out []core.LibrarySilk
	for _, c := range corners {
		sx := math.Copysign(1, c[0])
		sy := math.Copysign(1, c[1])
		out = append(out,
			silkLine(c[0], c[1], c[0]-sx*arm, c[1]),
			silkLine(c[0], c[1], c[0], c[1]-sy*arm))
	}
	return append(out, pin1Marker(r.MinXMM-ipcSilkClearance, r.MaxYMM+ipcSilkClearance)...)
}

// courtyard inflates the union of the lands and the body by the density's
// courtyard excess. Fragua has no courtyard layer; BodyRect *is* the placement
// keep-out, so the courtyard lives there.
func courtyard(d Density, pads []core.LibraryPad, body core.BodyRect) *core.BodyRect {
	b := newBoundsAcc()
	b.addPoint(body.MinXMM, body.MinYMM)
	b.addPoint(body.MaxXMM, body.MaxYMM)
	for _, p := range pads {
		b.addPoint(p.XMM-p.WMM/2, p.YMM-p.HMM/2)
		b.addPoint(p.XMM+p.WMM/2, p.YMM+p.HMM/2)
	}
	e := d.courtyardExcess()
	return &core.BodyRect{
		MinXMM: b.minX - e, MinYMM: b.minY - e,
		MaxXMM: b.maxX + e, MaxYMM: b.maxY + e,
	}
}
