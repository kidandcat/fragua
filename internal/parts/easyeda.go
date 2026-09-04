package parts

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mentasystems/fragua/internal/core"
)

// EasyEDA geometry is in "10 mil" units: 1 unit = 10 mil = 0.254 mm, with the
// document origin at dataStr.head.x/y and Y growing downwards (canvas pixels).
// Fragua footprints are mm, Y-up, origin at the footprint centre.
const easyedaUnitMM = 0.254

// EasyEDAEndpoint is the public component API. version= is the editor build the
// response is rendered for; 6.4.19.5 is what easyeda2kicad pins and what the
// documented `~`-separated shape strings above were captured from.
const EasyEDAEndpoint = "https://easyeda.com/api/products/%s/components?version=6.4.19.5"

// easyedaUA — the endpoint sits behind CloudFront, which 403s Go's default
// User-Agent. A browser UA is required for a 200.
const easyedaUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

// Fetcher returns the raw EasyEDA JSON for an LCSC id. Injected so tests run
// entirely off testdata fixtures.
type Fetcher interface {
	Fetch(ctx context.Context, lcsc string) ([]byte, error)
}

// HTTPFetcher is the live net/http fetcher (no third-party deps, per VISION).
type HTTPFetcher struct {
	Client  *http.Client
	Timeout time.Duration
	// URLFormat overrides EasyEDAEndpoint (one %s for the LCSC id).
	URLFormat string
}

// Fetch GETs the component JSON for lcsc.
func (f HTTPFetcher) Fetch(ctx context.Context, lcsc string) ([]byte, error) {
	format := f.URLFormat
	if format == "" {
		format = EasyEDAEndpoint
	}
	client := f.Client
	if client == nil {
		to := f.Timeout
		if to <= 0 {
			to = 30 * time.Second
		}
		client = &http.Client{Timeout: to}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf(format, lcsc), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", easyedaUA)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Referer", "https://easyeda.com/")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("easyeda: fetch %s: %w", lcsc, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("easyeda: read %s: %w", lcsc, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("easyeda: %s returned HTTP %d", lcsc, resp.StatusCode)
	}
	return body, nil
}

// lcscRe matches a bare LCSC id (C followed by digits).
var lcscRe = regexp.MustCompile(`^[Cc][0-9]+$`)

// NormaliseLCSC accepts "C2040", "c2040" or "LCSC:C2040" and returns "C2040".
func NormaliseLCSC(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, ":"); i >= 0 {
		if !strings.EqualFold(strings.TrimSpace(s[:i]), "lcsc") {
			return "", false
		}
		s = strings.TrimSpace(s[i+1:])
	}
	if !lcscRe.MatchString(s) {
		return "", false
	}
	return "C" + s[1:], true
}

// ─── response shape ──────────────────────────────────────────────────

type easyedaResponse struct {
	Success bool `json:"success"`
	Result  *struct {
		Title       string   `json:"title"`
		Description string   `json:"description"`
		Tags        []string `json:"tags"`
		SZLCSC      struct {
			Number string `json:"number"`
			URL    string `json:"url"`
		} `json:"szlcsc"`
		DataStr       easyedaDoc `json:"dataStr"`
		PackageDetail *struct {
			Title   string     `json:"title"`
			DataStr easyedaDoc `json:"dataStr"`
		} `json:"packageDetail"`
	} `json:"result"`
}

type easyedaDoc struct {
	Head struct {
		X     float64           `json:"x"`
		Y     float64           `json:"y"`
		CPara map[string]string `json:"c_para"`
	} `json:"head"`
	Shape []string `json:"shape"`
}

// ─── entry point ─────────────────────────────────────────────────────

// ParseEasyEDA turns a raw component response into a Part. key is the library
// key to store it under (normally the LCSC id).
func ParseEasyEDA(raw []byte, key string) (*Part, error) {
	var resp easyedaResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("easyeda: bad JSON: %w", err)
	}
	if resp.Result == nil {
		return nil, fmt.Errorf("easyeda: no result (unknown part?)")
	}
	r := resp.Result
	if r.PackageDetail == nil || len(r.PackageDetail.DataStr.Shape) == 0 {
		return nil, fmt.Errorf("easyeda: part has no footprint")
	}

	pads, silk, body, err := parseEasyEDAFootprint(r.PackageDetail.DataStr)
	if err != nil {
		return nil, err
	}
	if len(pads) == 0 {
		return nil, fmt.Errorf("easyeda: footprint has no pads")
	}

	symPara := r.DataStr.Head.CPara
	pkgPara := r.PackageDetail.DataStr.Head.CPara
	pins := parseEasyEDASymbol(r.DataStr.Shape)
	if len(pins) == 0 {
		pins = genericPins(pads)
	} else {
		pins = alignPinsToPads(pins, pads)
	}

	hint := strings.Join(append(append([]string{}, r.Tags...), symPara["package"], r.Title), " ")
	part := &Part{
		Pins:      pins,
		Kind:      inferKind(hint, pads),
		RefPrefix: RefPrefixFromEasyEDA(symPara["pre"]),
	}
	pkgName := r.PackageDetail.Title
	if pkgName == "" {
		pkgName = pkgPara["package"]
	}
	desc := strings.TrimSpace(r.Title + " " + pkgName)
	if d := strings.TrimSpace(r.Description); d != "" {
		desc = strings.TrimSpace(r.Title+" "+d) + " " + pkgName
	}
	entry := core.LibraryEntry{
		Key:          key,
		Description:  strings.TrimSpace(desc),
		DefaultValue: firstNonEmpty(symPara["Value"], r.Title),
		Pads:         pads,
		Silk:         silk,
		BodyRect:     body,
	}
	if id := firstNonEmpty(r.SZLCSC.Number, symPara["Supplier Part"]); id != "" {
		entry.LcscID = &id
	}
	if mpn := firstNonEmpty(symPara["Manufacturer Part"], r.Title); mpn != "" {
		entry.MPN = &mpn
	}
	if mfr := symPara["Manufacturer"]; mfr != "" {
		entry.Manufacturer = &mfr
	}
	if ds := firstNonEmpty(pkgPara["link"], r.SZLCSC.URL); ds != "" {
		entry.Datasheet = &ds
	}
	if cls := symPara["JLCPCB Part Class"]; cls != "" {
		entry.JLCClass = &cls
	}
	part.Entry = entry
	part.finish(SourceLCSC)
	return part, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// ─── footprint ───────────────────────────────────────────────────────

// EasyEDA footprint layer ids (packageDetail shapes).
const (
	eeLayerTopCu     = 1
	eeLayerBottomCu  = 2
	eeLayerTopSilk   = 3
	eeLayerBotSilk   = 4
	eeLayerMultiCu   = 11
	eeLayerBodyShape = 99 // ComponentShapeLayer — courtyard/body outline
	eeLayerAltSilk   = 101
)

// parseEasyEDAFootprint converts packageDetail shapes to Fragua geometry.
// Pads come from PAD, silk from TRACK/ARC/CIRCLE on the silk layers, and the
// body rect from the layer-99 shape region (falling back to silk + pads).
func parseEasyEDAFootprint(doc easyedaDoc) ([]core.LibraryPad, []core.LibrarySilk, *core.BodyRect, error) {
	ox, oy := doc.Head.X, doc.Head.Y
	toMM := func(x, y float64) (float64, float64) {
		return (x - ox) * easyedaUnitMM, -(y - oy) * easyedaUnitMM
	}
	lenMM := func(v float64) float64 { return v * easyedaUnitMM }

	var pads []core.LibraryPad
	var silk []core.LibrarySilk
	body := newBoundsAcc()
	seen := map[string]int{}

	for _, raw := range doc.Shape {
		f := strings.Split(raw, "~")
		switch f[0] {
		case "PAD":
			pad, ok := easyedaPad(f, toMM, lenMM)
			if !ok {
				continue
			}
			// Duplicate numbers happen on multi-terminal parts (USB-C shells).
			// Keep the first and suffix the rest so `net REF.PIN` stays unique.
			if n, dup := seen[pad.Number]; dup {
				seen[pad.Number] = n + 1
				pad.Number = fmt.Sprintf("%s_%d", pad.Number, n+1)
			} else {
				seen[pad.Number] = 1
			}
			pads = append(pads, pad)
		case "TRACK":
			if len(f) < 5 {
				continue
			}
			layer, _ := strconv.Atoi(f[2])
			w := lenMM(parseFloat(f[1]))
			pts := parseCoordList(f[4])
			if sl, ok := easyedaSilkLayer(layer); ok {
				silk = append(silk, polylineSilk(pts, w, sl, toMM)...)
			}
			if layer == eeLayerBodyShape {
				for _, p := range pts {
					body.addPoint(toMM(p[0], p[1]))
				}
			}
		case "ARC":
			if len(f) < 5 {
				continue
			}
			layer, _ := strconv.Atoi(f[2])
			sl, ok := easyedaSilkLayer(layer)
			if !ok {
				continue
			}
			w := lenMM(parseFloat(f[1]))
			pts := flattenSVGArc(f[4])
			silk = append(silk, polylineSilk(pts, w, sl, toMM)...)
		case "CIRCLE":
			if len(f) < 6 {
				continue
			}
			layer, _ := strconv.Atoi(f[5])
			sl, ok := easyedaSilkLayer(layer)
			if !ok {
				continue
			}
			cx, cy, r := parseFloat(f[1]), parseFloat(f[2]), parseFloat(f[3])
			w := lenMM(parseFloat(f[4]))
			silk = append(silk, polylineSilk(circlePoints(cx, cy, r, 16), w, sl, toMM)...)
		case "SOLIDREGION":
			if len(f) < 4 {
				continue
			}
			layer, _ := strconv.Atoi(f[1])
			if layer != eeLayerBodyShape {
				continue
			}
			for _, p := range parseSVGPathPoints(f[3]) {
				body.addPoint(toMM(p[0], p[1]))
			}
		}
	}

	// Fall back to pads + silk when the part carries no body region.
	for _, p := range pads {
		body.addPoint(p.XMM-p.WMM/2, p.YMM-p.HMM/2)
		body.addPoint(p.XMM+p.WMM/2, p.YMM+p.HMM/2)
	}
	for _, s := range silk {
		body.addPoint(s.X1MM, s.Y1MM)
		body.addPoint(s.X2MM, s.Y2MM)
	}
	return pads, silk, body.rect(), nil
}

func easyedaSilkLayer(id int) (core.SilkLayer, bool) {
	switch id {
	case eeLayerTopSilk, eeLayerAltSilk:
		return core.SilkTop, true
	case eeLayerBotSilk:
		return core.SilkBottom, true
	}
	return "", false
}

// padNumRe strips EasyEDA's "NAME(NUMBER)" pad labels down to NUMBER.
var padNumRe = regexp.MustCompile(`\(([^()]+)\)\s*$`)

// easyedaPad converts one PAD shape string:
//
//	PAD~shape~x~y~w~h~layer~net~number~holeR~points~rot~id~holeLen~slotPts~plated~…
//
// w/h are the *unrotated* pad size; the rotation field turns them, so 90/270
// swap and any other angle falls back to the rotated AABB. POLYGON pads bake
// their rotation into the point list, so the bbox of the points wins there.
func easyedaPad(f []string, toMM func(float64, float64) (float64, float64), lenMM func(float64) float64) (core.LibraryPad, bool) {
	if len(f) < 12 {
		return core.LibraryPad{}, false
	}
	shape := strings.ToUpper(f[1])
	x, y := toMM(parseFloat(f[2]), parseFloat(f[3]))
	w, h := lenMM(parseFloat(f[4])), lenMM(parseFloat(f[5]))
	number := strings.TrimSpace(f[8])
	if m := padNumRe.FindStringSubmatch(number); m != nil {
		number = strings.TrimSpace(m[1])
	}
	if number == "" {
		return core.LibraryPad{}, false
	}
	rot := parseFloat(f[11])

	if shape == "POLYGON" {
		if pts := parseCoordList(f[10]); len(pts) >= 3 {
			b := newBoundsAcc()
			for _, p := range pts {
				b.addPoint(toMM(p[0], p[1]))
			}
			x, y = b.centre()
			w, h = b.size()
		}
	} else {
		w, h = rotatedExtent(w, h, rot)
	}

	pad := core.LibraryPad{Number: number, XMM: x, YMM: y, WMM: w, HMM: h}
	// holeR>0 → through-hole. f[13] is the slot length for oval drills, but
	// core carries a single round drill, so keep the bit diameter (2·holeR):
	// the milled extension runs along the pad's long axis and widening the
	// drill to it would fake a negative annular ring on the short axis.
	if r := parseFloat(f[9]); r > 0 {
		d := lenMM(2 * r)
		if lim := math.Min(pad.WMM, pad.HMM); lim > 0 && d > lim {
			d = lim
		}
		pad.DrillMM = &d
	}
	if pad.WMM <= 0 || pad.HMM <= 0 {
		return core.LibraryPad{}, false
	}
	return pad, true
}

// rotatedExtent returns the AABB of a w×h rectangle turned by rot degrees.
func rotatedExtent(w, h, rot float64) (float64, float64) {
	switch mod360(rot) {
	case 0, 180:
		return w, h
	case 90, 270:
		return h, w
	}
	t := rot * math.Pi / 180
	s, c := math.Abs(math.Sin(t)), math.Abs(math.Cos(t))
	return w*c + h*s, w*s + h*c
}

func mod360(v float64) float64 {
	v = math.Mod(v, 360)
	if v < 0 {
		v += 360
	}
	return v
}

// ─── symbol ──────────────────────────────────────────────────────────

// parseEasyEDASymbol pulls pins out of the symbol document's shape list.
//
// A pin is one "P~…" string of 7 `^^`-separated segments:
//
//	seg0  P~display~elecType~spiceNum~x~y~rotation~id~locked
//	seg1  dotX~dotY
//	seg2  path~colour
//	seg3  shown~x~y~rot~NAME~anchor~font~size~colour
//	seg4  shown~x~y~rot~NUMBER~anchor~font~size~colour
//	seg5  shown~dotX~dotY          (inversion bubble)
//	seg6  shown~path               (clock marker)
//
// The authoritative pin number is seg4[4] — seg0's spice number is only a
// fallback. Side comes from seg0's rotation, which points *into* the body:
// 0 → the dot sits right of the body, 90 → above it, and so on.
func parseEasyEDASymbol(shapes []string) []core.SchPin {
	var pins []core.SchPin
	for _, raw := range shapes {
		if !strings.HasPrefix(raw, "P~") {
			continue
		}
		segs := strings.Split(raw, "^^")
		s0 := strings.Split(segs[0], "~")
		if len(s0) < 7 {
			continue
		}
		pin := core.SchPin{
			Role: easyedaPinRole(s0[2]),
			Side: easyedaPinSide(parseFloat(s0[6])),
		}
		if len(segs) > 4 {
			if s4 := strings.Split(segs[4], "~"); len(s4) > 4 {
				pin.Number = strings.TrimSpace(s4[4])
			}
		}
		if pin.Number == "" {
			pin.Number = strings.TrimSpace(s0[3])
		}
		if len(segs) > 3 {
			if s3 := strings.Split(segs[3], "~"); len(s3) > 4 {
				name := strings.TrimSpace(s3[4])
				if name != pin.Number && name != "~" {
					pin.Name = name
				}
			}
		}
		if pin.Number == "" {
			continue
		}
		if pin.Role == core.PinNC {
			pin.NC = true
		}
		pins = append(pins, pin)
	}
	return pins
}

// easyedaPinRole maps the electrical-type integer (seg0[1] after "P").
func easyedaPinRole(s string) core.PinRole {
	switch strings.TrimSpace(s) {
	case "1":
		return core.PinInput
	case "2":
		return core.PinOutput
	case "3":
		return core.PinBidir
	case "4":
		return core.PinPowerIn
	default:
		return core.PinPassive
	}
}

// easyedaPinSide maps the pin rotation. EasyEDA Y grows downwards, so a pin
// drawn "upwards" (rot 90) hangs off the top of the body.
func easyedaPinSide(rot float64) core.PinSide {
	switch mod360(rot) {
	case 90:
		return core.PinTop
	case 180:
		return core.PinLeft
	case 270:
		return core.PinBottom
	default:
		return core.PinRight
	}
}

// alignPinsToPads drops symbol pins that have no pad and copies pin names onto
// the matching pads, so `net U1.GND` resolves as readily as `net U1.3`.
func alignPinsToPads(pins []core.SchPin, pads []core.LibraryPad) []core.SchPin {
	byNum := map[string]int{}
	for i := range pads {
		byNum[pads[i].Number] = i
	}
	out := make([]core.SchPin, 0, len(pins))
	for _, p := range pins {
		i, ok := byNum[p.Number]
		if !ok {
			continue
		}
		if p.Name != "" {
			pads[i].Name = p.Name
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return genericPins(pads)
	}
	return out
}

// ─── small geometry helpers ──────────────────────────────────────────

type boundsAcc struct {
	minX, minY, maxX, maxY float64
	any                    bool
}

func newBoundsAcc() *boundsAcc {
	return &boundsAcc{minX: math.Inf(1), minY: math.Inf(1), maxX: math.Inf(-1), maxY: math.Inf(-1)}
}

func (b *boundsAcc) addPoint(x, y float64) {
	b.any = true
	b.minX, b.minY = math.Min(b.minX, x), math.Min(b.minY, y)
	b.maxX, b.maxY = math.Max(b.maxX, x), math.Max(b.maxY, y)
}

func (b *boundsAcc) centre() (float64, float64) {
	return (b.minX + b.maxX) / 2, (b.minY + b.maxY) / 2
}

func (b *boundsAcc) size() (float64, float64) { return b.maxX - b.minX, b.maxY - b.minY }

func (b *boundsAcc) rect() *core.BodyRect {
	if !b.any {
		return nil
	}
	return &core.BodyRect{MinXMM: b.minX, MinYMM: b.minY, MaxXMM: b.maxX, MaxYMM: b.maxY}
}

func parseFloat(s string) float64 {
	v, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return v
}

// parseCoordList reads "x1 y1 x2 y2 …" into point pairs.
func parseCoordList(s string) [][2]float64 {
	f := strings.Fields(s)
	out := make([][2]float64, 0, len(f)/2)
	for i := 0; i+1 < len(f); i += 2 {
		out = append(out, [2]float64{parseFloat(f[i]), parseFloat(f[i+1])})
	}
	return out
}

// parseSVGPathPoints collects every absolute vertex of an SVG path, enough for
// a bounding box (M/L/H/V/A endpoints; curve controls are ignored).
func parseSVGPathPoints(path string) [][2]float64 {
	toks := strings.Fields(strings.NewReplacer(",", " ", "M", " M ", "L", " L ", "H", " H ",
		"V", " V ", "A", " A ", "Z", " Z ", "C", " C ", "Q", " Q ").Replace(path))
	var out [][2]float64
	var cx, cy float64
	cmd := ""
	nums := []float64{}
	flush := func() {
		switch cmd {
		case "M", "L":
			for i := 0; i+1 < len(nums); i += 2 {
				cx, cy = nums[i], nums[i+1]
				out = append(out, [2]float64{cx, cy})
			}
		case "H":
			for _, v := range nums {
				cx = v
				out = append(out, [2]float64{cx, cy})
			}
		case "V":
			for _, v := range nums {
				cy = v
				out = append(out, [2]float64{cx, cy})
			}
		case "A":
			// rx ry rot large sweep x y — only the endpoint is a vertex.
			for i := 0; i+6 < len(nums); i += 7 {
				cx, cy = nums[i+5], nums[i+6]
				out = append(out, [2]float64{cx, cy})
			}
		case "C":
			for i := 0; i+5 < len(nums); i += 6 {
				cx, cy = nums[i+4], nums[i+5]
				out = append(out, [2]float64{cx, cy})
			}
		case "Q":
			for i := 0; i+3 < len(nums); i += 4 {
				cx, cy = nums[i+2], nums[i+3]
				out = append(out, [2]float64{cx, cy})
			}
		}
		nums = nums[:0]
	}
	for _, t := range toks {
		switch t {
		case "M", "L", "H", "V", "A", "Z", "C", "Q":
			flush()
			cmd = t
		default:
			if v, err := strconv.ParseFloat(t, 64); err == nil {
				nums = append(nums, v)
			}
		}
	}
	flush()
	return out
}

// flattenSVGArc turns "M x y A rx ry rot large sweep x y" into a polyline.
// Only circular arcs appear in EasyEDA footprints (rx == ry).
func flattenSVGArc(path string) [][2]float64 {
	toks := strings.Fields(strings.NewReplacer(",", " ", "M", " M ", "A", " A ").Replace(path))
	var nums []float64
	for _, t := range toks {
		if v, err := strconv.ParseFloat(t, 64); err == nil {
			nums = append(nums, v)
		}
	}
	if len(nums) < 9 {
		return parseSVGPathPoints(path)
	}
	x0, y0 := nums[0], nums[1]
	rx, ry := math.Abs(nums[2]), math.Abs(nums[3])
	large, sweep := nums[5] != 0, nums[6] != 0
	x1, y1 := nums[7], nums[8]
	r := math.Max(rx, ry)
	if r <= 0 {
		return [][2]float64{{x0, y0}, {x1, y1}}
	}
	// Centre of a circular arc through both endpoints.
	dx, dy := (x1-x0)/2, (y1-y0)/2
	mx, my := (x0+x1)/2, (y0+y1)/2
	d2 := dx*dx + dy*dy
	hh := r*r - d2
	if hh < 0 {
		hh = 0
	}
	h := math.Sqrt(hh / math.Max(d2, 1e-12))
	sign := 1.0
	if large != sweep {
		sign = -1
	}
	cx, cy := mx+sign*h*dy, my-sign*h*dx
	a0 := math.Atan2(y0-cy, x0-cx)
	a1 := math.Atan2(y1-cy, x1-cx)
	if sweep && a1 < a0 {
		a1 += 2 * math.Pi
	}
	if !sweep && a1 > a0 {
		a1 -= 2 * math.Pi
	}
	const steps = 8
	out := make([][2]float64, 0, steps+1)
	for i := 0; i <= steps; i++ {
		a := a0 + (a1-a0)*float64(i)/steps
		out = append(out, [2]float64{cx + r*math.Cos(a), cy + r*math.Sin(a)})
	}
	return out
}

func circlePoints(cx, cy, r float64, n int) [][2]float64 {
	out := make([][2]float64, 0, n+1)
	for i := 0; i <= n; i++ {
		a := 2 * math.Pi * float64(i) / float64(n)
		out = append(out, [2]float64{cx + r*math.Cos(a), cy + r*math.Sin(a)})
	}
	return out
}

// polylineSilk turns a point list into silk line segments in mm.
func polylineSilk(pts [][2]float64, widthMM float64, layer core.SilkLayer, toMM func(float64, float64) (float64, float64)) []core.LibrarySilk {
	if len(pts) < 2 {
		return nil
	}
	if widthMM <= 0 {
		widthMM = 0.12
	}
	out := make([]core.LibrarySilk, 0, len(pts)-1)
	x1, y1 := toMM(pts[0][0], pts[0][1])
	for _, p := range pts[1:] {
		x2, y2 := toMM(p[0], p[1])
		out = append(out, core.LibrarySilk{
			Kind: "line", Layer: layer,
			X1MM: x1, Y1MM: y1, X2MM: x2, Y2MM: y2, WidthMM: widthMM,
		})
		x1, y1 = x2, y2
	}
	return out
}
