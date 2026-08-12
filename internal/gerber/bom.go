package gerber

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mentasystems/fragua/internal/core"
)

// writeBOM groups footprints by (value, library); references joined by space.
// Columns: Reference,Value,Footprint,Quantity (matches Rust pcb-gerber).
func writeBOM(board *core.Board) string {
	type key struct{ value, lib string }
	groups := map[key][]string{}
	for _, fp := range footprintsInOrder(board) {
		k := key{fp.Value, fp.Library}
		groups[k] = append(groups[k], fp.Reference)
	}
	keys := make([]key, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	// BTreeMap order: value then library.
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].value != keys[j].value {
			return keys[i].value < keys[j].value
		}
		return keys[i].lib < keys[j].lib
	})

	var b strings.Builder
	b.WriteString("Reference,Value,Footprint,Quantity\n")
	for _, k := range keys {
		refs := append([]string(nil), groups[k]...)
		sort.Strings(refs)
		joined := strings.Join(refs, " ")
		fmt.Fprintf(&b, "%s,%s,%s,%d\n",
			csvField(joined), csvField(k.value), csvField(k.lib), len(refs))
	}
	return b.String()
}

// writePos emits pick-and-place CSV.
// Columns: Reference,Value,Footprint,X,Y,Rotation,Side
func writePos(board *core.Board) string {
	var b strings.Builder
	b.WriteString("Reference,Value,Footprint,X,Y,Rotation,Side\n")
	for _, fp := range footprintsInOrder(board) {
		side := "top"
		if !fp.Layer.IsTop() {
			side = "bottom"
		}
		fmt.Fprintf(&b, "%s,%s,%s,%.4f,%.4f,%.2f,%s\n",
			csvField(fp.Reference),
			csvField(fp.Value),
			csvField(fp.Library),
			fp.Position.X.ToMM(),
			fp.Position.Y.ToMM(),
			fp.Rotation,
			side,
		)
	}
	return b.String()
}

func csvField(s string) string {
	if strings.ContainsAny(s, ",\"\n") {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}
