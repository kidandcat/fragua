// Package gerber writes RS-274X Gerbers, Excellon drills, BOM and CPL.
package gerber

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mentasystems/fragua/internal/core"
)

// WriteFabPack writes the full manufacturing pack into outDir.
// Returns paths of written files.
func WriteFabPack(board *core.Board, name, outDir string) ([]string, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	files := []string{
		name + "-F_Cu.gbr",
		name + "-B_Cu.gbr",
		name + "-F_Mask.gbr",
		name + "-B_Mask.gbr",
		name + "-F_SilkS.gbr",
		name + "-B_SilkS.gbr",
		name + "-Edge_Cuts.gbr",
		name + "-PTH.drl",
		name + "-NPTH.drl",
		name + "-bom.csv",
		name + "-pos.csv",
	}
	var paths []string
	for _, f := range files {
		p := filepath.Join(outDir, f)
		var content string
		switch {
		case strings.HasSuffix(f, "F_Cu.gbr"):
			content = copperGerber(board, 0)
		case strings.HasSuffix(f, "B_Cu.gbr"):
			content = copperGerber(board, 1)
		case strings.HasSuffix(f, "Edge_Cuts.gbr"):
			content = edgeGerber(board)
		case strings.HasSuffix(f, "PTH.drl"):
			content = excellon(board, true)
		case strings.HasSuffix(f, "NPTH.drl"):
			content = excellon(board, false)
		case strings.HasSuffix(f, "bom.csv"):
			content = bomCSV(board)
		case strings.HasSuffix(f, "pos.csv"):
			content = posCSV(board)
		default:
			content = gerberHeader() + "M02*\n"
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return paths, err
		}
		paths = append(paths, p)
	}
	return paths, nil
}

func gerberHeader() string {
	return `%FSLAX46Y46*%
%MOMM*%
G04 Fragua Go gerber*
G01*
`
}

func copperGerber(board *core.Board, layer uint8) string {
	var b strings.Builder
	b.WriteString(gerberHeader())
	// aperture for pads / traces
	b.WriteString("%ADD10C,0.150000*%\n")
	b.WriteString("%ADD11R,1.000000X1.200000*%\n")
	// traces
	for _, tr := range board.Traces {
		if tr.Layer.Index != layer {
			continue
		}
		w := tr.Width.ToMM()
		fmt.Fprintf(&b, "%%ADD20C,%.6f*%%\n", w)
		b.WriteString("D20*\n")
		fmt.Fprintf(&b, "X%dY%dD02*\n", nmToGerber(tr.Start.X), nmToGerber(tr.Start.Y))
		fmt.Fprintf(&b, "X%dY%dD01*\n", nmToGerber(tr.End.X), nmToGerber(tr.End.Y))
	}
	// pads
	for _, id := range board.FootprintOrder {
		fp := board.Footprints[id]
		if fp == nil {
			continue
		}
		for i := range fp.Pads {
			pad := &fp.Pads[i]
			// top pads on layer 0; bottom-mounted flip
			pl := pad.Layer.Index
			if fp.Layer.Index == 1 {
				if pl == 0 {
					pl = 1
				} else if pl == 1 {
					pl = 0
				}
			}
			if pl != layer {
				continue
			}
			c := core.PadWorldCenter(fp, pad)
			fmt.Fprintf(&b, "%%ADD21R,%.6fX%.6f*%%\n", pad.Size[0].ToMM(), pad.Size[1].ToMM())
			b.WriteString("D21*\n")
			fmt.Fprintf(&b, "X%dY%dD03*\n", nmToGerber(c.X), nmToGerber(c.Y))
		}
	}
	b.WriteString("M02*\n")
	return b.String()
}

func edgeGerber(board *core.Board) string {
	var b strings.Builder
	b.WriteString(gerberHeader())
	b.WriteString("%ADD10C,0.100000*%\nD10*\n")
	if board.Outline != nil {
		o := board.Outline
		pts := []core.Point{
			o.Min,
			{X: o.Max.X, Y: o.Min.Y},
			o.Max,
			{X: o.Min.X, Y: o.Max.Y},
			o.Min,
		}
		fmt.Fprintf(&b, "X%dY%dD02*\n", nmToGerber(pts[0].X), nmToGerber(pts[0].Y))
		for _, p := range pts[1:] {
			fmt.Fprintf(&b, "X%dY%dD01*\n", nmToGerber(p.X), nmToGerber(p.Y))
		}
	}
	b.WriteString("M02*\n")
	return b.String()
}

func excellon(board *core.Board, plated bool) string {
	var b strings.Builder
	b.WriteString("M48\nMETRIC,TZ\n")
	if plated {
		// vias
		tools := map[int64]int{}
		next := 1
		for _, v := range board.Vias {
			d := int64(v.Drill)
			if _, ok := tools[d]; !ok {
				tools[d] = next
				fmt.Fprintf(&b, "T%02dC%.3f\n", next, v.Drill.ToMM())
				next++
			}
		}
		b.WriteString("%\n")
		// stable order
		type via struct {
			x, y core.Length
			t    int
		}
		var list []via
		for _, v := range board.Vias {
			list = append(list, via{v.Position.X, v.Position.Y, tools[int64(v.Drill)]})
		}
		sort.Slice(list, func(i, j int) bool {
			if list[i].y != list[j].y {
				return list[i].y < list[j].y
			}
			return list[i].x < list[j].x
		})
		cur := -1
		for _, v := range list {
			if v.t != cur {
				fmt.Fprintf(&b, "T%02d\n", v.t)
				cur = v.t
			}
			fmt.Fprintf(&b, "X%.3fY%.3f\n", v.x.ToMM(), v.y.ToMM())
		}
	} else {
		b.WriteString("%\n")
		for _, h := range board.Holes {
			fmt.Fprintf(&b, "X%.3fY%.3f\n", h.Position.X.ToMM(), h.Position.Y.ToMM())
		}
	}
	b.WriteString("M30\n")
	return b.String()
}

func bomCSV(board *core.Board) string {
	// group by value+library
	type key struct{ value, lib string }
	groups := map[key][]string{}
	for _, id := range board.FootprintOrder {
		fp := board.Footprints[id]
		if fp == nil {
			continue
		}
		k := key{fp.Value, fp.Library}
		groups[k] = append(groups[k], fp.Reference)
	}
	var b strings.Builder
	b.WriteString("Comment,Designator,Footprint,LCSC Part #\n")
	keys := make([]key, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].value != keys[j].value {
			return keys[i].value < keys[j].value
		}
		return keys[i].lib < keys[j].lib
	})
	for _, k := range keys {
		refs := groups[k]
		sort.Strings(refs)
		fmt.Fprintf(&b, "%s,%s,%s,\n", k.value, strings.Join(refs, ","), k.lib)
	}
	return b.String()
}

func posCSV(board *core.Board) string {
	var b strings.Builder
	b.WriteString("Designator,Mid X,Mid Y,Layer,Rotation\n")
	ids := append([]string{}, board.FootprintOrder...)
	if len(ids) == 0 {
		for id := range board.Footprints {
			ids = append(ids, id)
		}
		sort.Strings(ids)
	}
	for _, id := range ids {
		fp := board.Footprints[id]
		if fp == nil {
			continue
		}
		layer := "T"
		if fp.Layer.Index == 1 {
			layer = "B"
		}
		fmt.Fprintf(&b, "%s,%.4f,%.4f,%s,%.1f\n",
			fp.Reference, fp.Position.X.ToMM(), fp.Position.Y.ToMM(), layer, fp.Rotation)
	}
	return b.String()
}

func nmToGerber(l core.Length) int {
	// 4.6 format in mm → 1 unit = 1e-6 mm = nm when MOMM? 
	// With FSLAX46Y46 and MOMM, coordinates are in 0.000001 mm = nm.
	return int(l)
}
