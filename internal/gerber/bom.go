package gerber

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mentasystems/fragua/internal/core"
)

// BOM columns an EE / JLCPCB SMT importer expects.
// Comment is the value; Designator is the joined refs; LCSC Part # is
// present even when empty (never invented).
const bomHeader = "Comment,Designator,Footprint,LCSC Part #,Manufacturer,MPN,Quantity"

// CPL / JLC "Coordinate file" columns. Matches the KiCad JLCPCB plugin:
// Designator,Val,Package,Mid X,Mid Y,Rotation,Layer
//
// Convention (documented in README.txt):
//   - Origin: board lower-left, X right, Y up, millimetres
//   - Mid X/Y: footprint origin (same as Fragua placement)
//   - Rotation: degrees counterclockwise
//   - Bottom-side rotation is (180 − placement) mod 360, matching the
//     KiCad JLC plugin (jlcpcb-tools / Fabrication Toolkit)
const posHeader = "Designator,Val,Package,Mid X,Mid Y,Rotation,Layer"

// writeBOM groups footprints by (value, package, lcsc, manufacturer, mpn).
func writeBOM(board *core.Board) string {
	type key struct {
		value, pkg, lcsc, mfr, mpn string
	}
	groups := map[key][]string{}
	for _, fp := range footprintsInOrder(board) {
		k := key{
			value: fp.Value,
			pkg:   core.FootprintPackageName(fp),
			lcsc:  fp.LcscID,
			mfr:   fp.Manufacturer,
			mpn:   fp.MPN,
		}
		groups[k] = append(groups[k], fp.Reference)
	}
	keys := make([]key, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].value != keys[j].value {
			return keys[i].value < keys[j].value
		}
		if keys[i].pkg != keys[j].pkg {
			return keys[i].pkg < keys[j].pkg
		}
		if keys[i].lcsc != keys[j].lcsc {
			return keys[i].lcsc < keys[j].lcsc
		}
		return keys[i].mpn < keys[j].mpn
	})

	var b strings.Builder
	b.WriteString(bomHeader + "\n")
	for _, k := range keys {
		refs := append([]string(nil), groups[k]...)
		sort.Strings(refs)
		joined := strings.Join(refs, ",")
		fmt.Fprintf(&b, "%s,%s,%s,%s,%s,%s,%d\n",
			csvField(k.value),
			csvField(joined),
			csvField(k.pkg),
			csvField(k.lcsc),
			csvField(k.mfr),
			csvField(k.mpn),
			len(refs),
		)
	}
	return b.String()
}

// writePos emits a JLC-friendly pick-and-place / coordinate CSV.
func writePos(board *core.Board) string {
	var b strings.Builder
	b.WriteString(posHeader + "\n")
	for _, fp := range footprintsInOrder(board) {
		layer := "top"
		rot := fp.Rotation
		if !fp.Layer.IsTop() {
			layer = "bottom"
			// KiCad JLCPCB plugin: bottom rotation as seen from the top.
			rot = 180 - rot
		}
		rot = normalizeDeg(rot)
		fmt.Fprintf(&b, "%s,%s,%s,%.4f,%.4f,%.2f,%s\n",
			csvField(fp.Reference),
			csvField(fp.Value),
			csvField(core.FootprintPackageName(fp)),
			fp.Position.X.ToMM(),
			fp.Position.Y.ToMM(),
			rot,
			layer,
		)
	}
	return b.String()
}

func normalizeDeg(deg float64) float64 {
	deg = deg - 360*float64(int(deg/360))
	if deg < 0 {
		deg += 360
	}
	if deg == 360 {
		return 0
	}
	return deg
}

func csvField(s string) string {
	if strings.ContainsAny(s, ",\"\n") {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}
