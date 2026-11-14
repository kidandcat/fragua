package dxf

import (
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/mentasystems/fragua/internal/core"
)

type entity struct {
	kind  string
	layer string
	codes map[string][]string
}

func (e entity) f(t *testing.T, code string) float64 {
	t.Helper()
	v, ok := e.codes[code]
	if !ok || len(v) == 0 {
		t.Fatalf("%s on %s has no group %s", e.kind, e.layer, code)
	}
	x, err := strconv.ParseFloat(v[0], 64)
	if err != nil {
		t.Fatalf("group %s=%q: %v", code, v[0], err)
	}
	return x
}

// parse is a strict reader for what Write emits: code/value line pairs,
// sections in order, every entity inside ENTITIES.
func parse(t *testing.T, text string) (header map[string]string, ents []entity, layerNames []string) {
	t.Helper()
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines)%2 != 0 {
		t.Fatalf("odd number of lines (%d): group codes and values must pair", len(lines))
	}
	header = map[string]string{}
	var sections []string
	section, lastVar := "", ""
	var cur *entity
	flush := func() {
		if cur != nil && section == "ENTITIES" {
			ents = append(ents, *cur)
		}
		cur = nil
	}
	for i := 0; i < len(lines); i += 2 {
		code, val := strings.TrimSpace(lines[i]), lines[i+1]
		if _, err := strconv.Atoi(code); err != nil {
			t.Fatalf("line %d: group code %q is not an integer", i+1, code)
		}
		switch {
		case code == "0":
			flush()
			switch val {
			case "SECTION", "ENDSEC", "EOF", "TABLE", "ENDTAB":
				if val == "ENDSEC" {
					section = ""
				}
				continue
			}
			if section == "ENTITIES" || section == "TABLES" {
				cur = &entity{kind: val, codes: map[string][]string{}}
			}
		case code == "2" && cur == nil && section == "":
			section = val
			sections = append(sections, val)
		case section == "HEADER" && code == "9":
			lastVar = val
		case section == "HEADER":
			header[lastVar] = val
		case cur != nil:
			if code == "8" {
				cur.layer = val
			}
			if section == "TABLES" && cur.kind == "LAYER" && code == "2" {
				layerNames = append(layerNames, val)
			}
			cur.codes[code] = append(cur.codes[code], val)
		}
	}
	flush()
	if got := strings.Join(sections, ","); got != "HEADER,TABLES,ENTITIES" {
		t.Fatalf("sections = %s", got)
	}
	if lines[len(lines)-1] != "EOF" {
		t.Fatalf("file does not end with EOF")
	}
	return header, ents, layerNames
}

func mm(v float64) core.Length { return core.FromMM(v) }

func pt(x, y float64) core.Point { return core.Point{X: mm(x), Y: mm(y)} }

func testBoard() *core.Board {
	b := core.NewBoard()
	b.Outline = &core.Rect{Min: pt(0, 0), Max: pt(40, 30)}
	b.OutlineCornerRadius = mm(2)
	b.Cutouts = []core.Cutout{{Polygon: []core.Point{pt(10, 10), pt(20, 10), pt(20, 18), pt(10, 18)}}}
	b.MountHoles = []core.MountHole{{Center: pt(3.5, 3.5), Diameter: mm(3.2), Label: "M3"}}
	drill := mm(1.0)
	b.AddFootprint(&core.Footprint{
		ID: core.NewID(), Reference: "J1", Position: pt(30, 20), Layer: core.LayerTop,
		Pads: []core.Pad{
			{Number: "1", Offset: pt(-1.27, 0), Size: [2]core.Length{mm(1.7), mm(1.7)}, Layer: core.LayerTop, Drill: &drill},
			{Number: "2", Offset: pt(1.27, 0), Size: [2]core.Length{mm(1.7), mm(1.7)}, Layer: core.LayerTop, Drill: &drill},
		},
	})
	b.AddFootprint(&core.Footprint{
		ID: core.NewID(), Reference: "U2", Position: pt(8, 24), Layer: core.LayerBottom,
		Pads: []core.Pad{
			{Number: "1", Offset: pt(-2, 0), Size: [2]core.Length{mm(1), mm(0.6)}, Layer: core.LayerBottom},
			{Number: "2", Offset: pt(2, 0), Size: [2]core.Length{mm(1), mm(0.6)}, Layer: core.LayerBottom},
		},
	})
	return b
}

func byLayer(ents []entity, layer, kind string) []entity {
	var out []entity
	for _, e := range ents {
		if e.layer == layer && e.kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestWriteMechanicalLayers(t *testing.T) {
	text, st := Write(testBoard())
	header, ents, names := parse(t, text)

	if header["$INSUNITS"] != "4" {
		t.Fatalf("$INSUNITS = %q, want 4 (mm)", header["$INSUNITS"])
	}
	if got := strings.Join(names, ","); got != "OUTLINE,CUTOUTS,HOLES_NPTH,HOLES_PTH,COMP_TOP,COMP_BOTTOM" {
		t.Fatalf("layer table = %s", got)
	}
	if st.OutlineMM != [2]float64{40, 30} || st.Cutouts != 1 || st.NPTH != 1 || st.PTH != 2 || st.CompTop != 1 || st.CompBottom != 1 {
		t.Fatalf("stats = %+v", st)
	}

	// Rounded outline: 4 straight edges inset by the radius + 4 quarter arcs.
	lines, arcs := byLayer(ents, LayerOutline, "LINE"), byLayer(ents, LayerOutline, "ARC")
	if len(lines) != 4 || len(arcs) != 4 {
		t.Fatalf("outline: %d lines, %d arcs; want 4 and 4", len(lines), len(arcs))
	}
	if l := lines[0]; !near(l.f(t, "10"), 2) || !near(l.f(t, "20"), 0) || !near(l.f(t, "11"), 38) {
		t.Fatalf("bottom edge = (%v,%v)-(%v)", l.f(t, "10"), l.f(t, "20"), l.f(t, "11"))
	}
	for _, a := range arcs {
		if !near(a.f(t, "40"), 2) || !near(math.Mod(a.f(t, "51")-a.f(t, "50")+360, 360), 90) {
			t.Fatalf("arc radius %v span %v..%v", a.f(t, "40"), a.f(t, "50"), a.f(t, "51"))
		}
	}
	// The outline must be closed: every endpoint shared by exactly two pieces.
	ends := map[[2]int64]int{}
	key := func(x, y float64) [2]int64 { return [2]int64{int64(math.Round(x * 1e4)), int64(math.Round(y * 1e4))} }
	for _, l := range lines {
		ends[key(l.f(t, "10"), l.f(t, "20"))]++
		ends[key(l.f(t, "11"), l.f(t, "21"))]++
	}
	for _, a := range arcs {
		cx, cy, r := a.f(t, "10"), a.f(t, "20"), a.f(t, "40")
		for _, deg := range []float64{a.f(t, "50"), a.f(t, "51")} {
			rad := deg * math.Pi / 180
			ends[key(cx+r*math.Cos(rad), cy+r*math.Sin(rad))]++
		}
	}
	for p, n := range ends {
		if n != 2 {
			t.Fatalf("outline endpoint %v used %d times: outline is not closed", p, n)
		}
	}

	if n := len(byLayer(ents, LayerCutouts, "LINE")); n != 4 {
		t.Fatalf("cutout lines = %d, want 4", n)
	}
	npth := byLayer(ents, LayerNPTH, "CIRCLE")
	if len(npth) != 1 || !near(npth[0].f(t, "10"), 3.5) || !near(npth[0].f(t, "40"), 1.6) {
		t.Fatalf("npth = %+v", npth)
	}
	pth := byLayer(ents, LayerPTH, "CIRCLE")
	if len(pth) != 2 || !near(pth[0].f(t, "10"), 28.73) || !near(pth[1].f(t, "10"), 31.27) || !near(pth[0].f(t, "40"), 0.5) {
		t.Fatalf("pth = %+v", pth)
	}

	top := byLayer(ents, LayerCompTop, "TEXT")
	if len(top) != 1 || top[0].codes["1"][0] != "J1" || len(byLayer(ents, LayerCompTop, "LINE")) != 4 {
		t.Fatalf("top parts: texts %+v", top)
	}
	bot := byLayer(ents, LayerCompBottom, "TEXT")
	if len(bot) != 1 || bot[0].codes["1"][0] != "U2" || !near(bot[0].f(t, "11"), 8) || !near(bot[0].f(t, "21"), 24) {
		t.Fatalf("bottom parts: texts %+v", bot)
	}
}

func TestWriteSharpAndPolyOutline(t *testing.T) {
	b := core.NewBoard()
	b.Outline = &core.Rect{Min: pt(0, 0), Max: pt(10, 5)}
	text, _ := Write(b)
	_, ents, _ := parse(t, text)
	if l, a := len(byLayer(ents, LayerOutline, "LINE")), len(byLayer(ents, LayerOutline, "ARC")); l != 4 || a != 0 {
		t.Fatalf("sharp rect: %d lines %d arcs", l, a)
	}

	b.OutlinePoly = []core.Point{pt(0, 0), pt(10, 0), pt(5, 8)}
	text, st := Write(b)
	_, ents, _ = parse(t, text)
	if n := len(byLayer(ents, LayerOutline, "LINE")); n != 3 {
		t.Fatalf("poly outline: %d lines, want 3", n)
	}
	if st.OutlineMM != [2]float64{10, 8} {
		t.Fatalf("poly bbox = %v", st.OutlineMM)
	}
}

func TestWriteIsDeterministic(t *testing.T) {
	b := testBoard()
	a, _ := Write(b)
	for i := 0; i < 5; i++ {
		if c, _ := Write(b); c != a {
			t.Fatalf("output differs between runs")
		}
	}
}
