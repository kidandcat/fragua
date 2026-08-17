package gerber

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mentasystems/fragua/internal/core"
)

// writeExcellon emits an Excellon drill file.
// plated=true → PTH (vias + pad drills); plated=false → NPTH (mount holes).
// Format: METRIC, leading-zero suppression, 3.3 fixed-point in mm.
func writeExcellon(board *core.Board, plated bool) string {
	var b strings.Builder
	kind := "NPTH"
	if plated {
		kind = "PTH"
	}
	fmt.Fprintf(&b, "M48\n")
	fmt.Fprintf(&b, "; Fragua %s %s drills\n", core.Version, kind)
	b.WriteString("FMAT,2\n")
	b.WriteString("METRIC,LZ,000.000\n")

	// Group holes by drill diameter (nm key, BTreeMap order).
	groups := map[int64][][2]float64{}
	if plated {
		for i := range board.Vias {
			v := &board.Vias[i]
			d := int64(v.Drill)
			groups[d] = append(groups[d], [2]float64{v.Position.X.ToMM(), v.Position.Y.ToMM()})
		}
		for _, fp := range footprintsInOrder(board) {
			for i := range fp.Pads {
				pad := &fp.Pads[i]
				if pad.Drill == nil {
					continue
				}
				c := core.PadWorldCenter(fp, pad)
				d := int64(*pad.Drill)
				groups[d] = append(groups[d], [2]float64{c.X.ToMM(), c.Y.ToMM()})
			}
		}
	} else {
		for _, h := range allMountHoles(board) {
			d := int64(h.Diameter)
			groups[d] = append(groups[d], [2]float64{h.Center.X.ToMM(), h.Center.Y.ToMM()})
		}
	}

	// Sorted tool definitions (ascending drill size).
	keys := make([]int64, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	for i, drillNm := range keys {
		toolID := i + 1
		drillMM := core.Length(drillNm).ToMM()
		fmt.Fprintf(&b, "T%dC%.3f\n", toolID, drillMM)
	}
	b.WriteString("%\n")
	b.WriteString("G90\n")
	for i, drillNm := range keys {
		toolID := i + 1
		fmt.Fprintf(&b, "T%d\n", toolID)
		for _, pt := range groups[drillNm] {
			fmt.Fprintf(&b, "X%.3fY%.3f\n", pt[0], pt[1])
		}
	}
	b.WriteString("M30\n")
	return b.String()
}

// allMountHoles merges MountHoles with the legacy Holes alias.
func allMountHoles(board *core.Board) []core.MountHole {
	if len(board.MountHoles) == 0 {
		return board.Holes
	}
	if len(board.Holes) == 0 {
		return board.MountHoles
	}
	out := make([]core.MountHole, 0, len(board.MountHoles)+len(board.Holes))
	out = append(out, board.MountHoles...)
	out = append(out, board.Holes...)
	return out
}
