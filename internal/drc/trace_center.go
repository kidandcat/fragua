package drc

import (
	"github.com/mentasystems/fragua/internal/core"
	"github.com/mentasystems/fragua/internal/leanbridge"
)

// traceCenterMM is the centreline distance in millimetres between two traces.
// It goes through leanbridge so a build with `-tags lean` uses the proved
// integer kernel; the default build uses the same rational arithmetic in Go.
// The float conversion lives in one place, which is what keeps the two DRC
// reports identical.
func traceCenterMM(a0, a1, b0, b1 core.Point) float64 {
	return leanbridge.SegmentSegmentMM(
		int64(a0.X), int64(a0.Y), int64(a1.X), int64(a1.Y),
		int64(b0.X), int64(b0.Y), int64(b1.X), int64(b1.Y),
	)
}
