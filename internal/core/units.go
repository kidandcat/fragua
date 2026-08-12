// Package core holds the in-memory project model: geometry (nm fixed-point),
// board, schematic, library, rules, and Project with change events.
package core

import "math"

// Length is a fixed-point length in nanometres.
// All board coordinates use Length so equality and hashing stay exact.
type Length int64

const (
	NM  Length = 1
	UM  Length = 1_000
	MM  Length = 1_000_000
	MIL Length = 25_400
)

// FromMM converts millimetres to Length (rounded to nearest nm).
func FromMM(mm float64) Length {
	return Length(math.Round(mm * 1_000_000.0))
}

// FromMil converts mils (1/1000 inch) to Length.
func FromMil(mil float64) Length {
	return Length(math.Round(mil * 25_400.0))
}

// ToMM converts Length to millimetres.
func (l Length) ToMM() float64 {
	return float64(l) / 1_000_000.0
}

// Abs returns the absolute value.
func (l Length) Abs() Length {
	if l < 0 {
		return -l
	}
	return l
}

// Min returns the smaller of a and b.
func Min(a, b Length) Length {
	if a < b {
		return a
	}
	return b
}

// Max returns the larger of a and b.
func Max(a, b Length) Length {
	if a > b {
		return a
	}
	return b
}
