// Package leanbridge is the integer-geometry kernel shared by DRC and the
// optional Lean prover.
//
// Coordinates are nanometres, the same unit as core.Length. Squared distances
// are exact rationals computed with arbitrary-precision integers: a difference
// of two int64 endpoints does not fit in int64, and a squared distance does
// not fit in 128 bits, so neither implementation uses wrapping arithmetic.
//
// The pure Go functions are always available. Building with -tags lean makes
// the exported operations call the proved Lean implementation
// (lean/fragua-geom) instead. Both sides normalise fractions the same way
// (positive denominator, gcd 1) so results compare as integers.
package leanbridge

import (
	"math/big"
)

// Rat is a rational squared distance. Den is positive for every distance
// this package returns. Num and Den are reduced by their gcd.
type Rat struct {
	Num *big.Int
	Den *big.Int
}

// Dist2 is the squared Euclidean distance between two points.
func Dist2(ax, ay, bx, by int64) *big.Int {
	return dist2(ax, ay, bx, by)
}

// PointSegDist2 is the squared distance from (px, py) to the closed segment
// (ax, ay)–(bx, by).
func PointSegDist2(px, py, ax, ay, bx, by int64) Rat {
	return pointSegDist2(px, py, ax, ay, bx, by)
}

// SegSegDist2 is the squared distance between two closed segments.
func SegSegDist2(a0x, a0y, a1x, a1y, b0x, b0y, b1x, b1y int64) Rat {
	return segSegDist2(a0x, a0y, a1x, a1y, b0x, b0y, b1x, b1y)
}

// Separated reports whether the distance whose square is num/den is at least
// clearanceNm nanometres. A non-positive denominator or a negative numerator
// is not a squared distance and answers false. A negative clearance is treated
// as zero.
func Separated(num, den *big.Int, clearanceNm int64) bool {
	return separated(num, den, clearanceNm)
}

// SegmentSegmentMM is the Euclidean distance in millimetres. The squared
// length is exact; only this final conversion to the float DRC still prints
// rounds, and both builds share it.
func SegmentSegmentMM(a0x, a0y, a1x, a1y, b0x, b0y, b1x, b1y int64) float64 {
	return ratToMM(SegSegDist2(a0x, a0y, a1x, a1y, b0x, b0y, b1x, b1y))
}

func ratToMM(r Rat) float64 {
	if r.Num.Sign() == 0 {
		return 0
	}
	nf := new(big.Float).SetPrec(256).SetInt(r.Num)
	df := new(big.Float).SetPrec(256).SetInt(r.Den)
	q := new(big.Float).SetPrec(256).Quo(nf, df)
	s := new(big.Float).SetPrec(256).Sqrt(q)
	scale := new(big.Float).SetPrec(256).SetInt64(1_000_000)
	mm := new(big.Float).SetPrec(256).Quo(s, scale)
	f, _ := mm.Float64()
	return f
}

func pureDist2(ax, ay, bx, by int64) *big.Int {
	dx := new(big.Int).Sub(big.NewInt(ax), big.NewInt(bx))
	dy := new(big.Int).Sub(big.NewInt(ay), big.NewInt(by))
	dx.Mul(dx, dx)
	dy.Mul(dy, dy)
	return dx.Add(dx, dy)
}

func purePointSeg(px, py, ax, ay, bx, by int64) Rat {
	abx := new(big.Int).Sub(big.NewInt(bx), big.NewInt(ax))
	aby := new(big.Int).Sub(big.NewInt(by), big.NewInt(ay))
	apx := new(big.Int).Sub(big.NewInt(px), big.NewInt(ax))
	apy := new(big.Int).Sub(big.NewInt(py), big.NewInt(ay))
	len2 := new(big.Int).Add(new(big.Int).Mul(abx, abx), new(big.Int).Mul(aby, aby))
	tn := new(big.Int).Add(new(big.Int).Mul(apx, abx), new(big.Int).Mul(apy, aby))
	if len2.Sign() == 0 || tn.Sign() <= 0 {
		return normInt(pureDist2(px, py, ax, ay))
	}
	if tn.Cmp(len2) >= 0 {
		return normInt(pureDist2(px, py, bx, by))
	}
	ap2 := new(big.Int).Add(new(big.Int).Mul(apx, apx), new(big.Int).Mul(apy, apy))
	num := new(big.Int).Mul(ap2, len2)
	num.Sub(num, new(big.Int).Mul(tn, tn))
	return normalize(num, len2)
}

func pureSegSeg(a0x, a0y, a1x, a1y, b0x, b0y, b1x, b1y int64) Rat {
	if properCross(a0x, a0y, a1x, a1y, b0x, b0y, b1x, b1y) {
		return Rat{Num: big.NewInt(0), Den: big.NewInt(1)}
	}
	return minRat(
		minRat(purePointSeg(a0x, a0y, b0x, b0y, b1x, b1y), purePointSeg(a1x, a1y, b0x, b0y, b1x, b1y)),
		minRat(purePointSeg(b0x, b0y, a0x, a0y, a1x, a1y), purePointSeg(b1x, b1y, a0x, a0y, a1x, a1y)),
	)
}

func pureSeparated(num, den *big.Int, clearance int64) bool {
	if den.Sign() <= 0 || num.Sign() < 0 {
		return false
	}
	c := clearance
	if c < 0 {
		c = 0
	}
	cc := new(big.Int).Mul(big.NewInt(c), big.NewInt(c))
	rhs := new(big.Int).Mul(cc, den)
	return num.Cmp(rhs) >= 0
}

// pureSegAt is one iteration of the hot-loop workload: the index changes the
// endpoints, so a loop of these calls is not a repeated identical query.
func pureSegAt(i int) Rat {
	k := int64(i)
	return pureSegSeg(k, 0, k+5_000_000, int64(i%17), 0, 250_000, 8_000_000, 250_000+int64(i%5))
}

// pureSegSum is the Go twin of the Lean `seg_sum` operation: the whole loop
// runs in this process and returns the sum of the normalized numerators.
func pureSegSum(n int) *big.Int {
	acc := big.NewInt(0)
	for i := 0; i < n; i++ {
		acc.Add(acc, pureSegAt(i).Num)
	}
	return acc
}

func pureNorm(num, den *big.Int) Rat {
	return normalize(num, den)
}

func normInt(n *big.Int) Rat {
	return normalize(n, big.NewInt(1))
}

func normalize(num, den *big.Int) Rat {
	num = new(big.Int).Set(num)
	den = new(big.Int).Set(den)
	if den.Sign() < 0 {
		num.Neg(num)
		den.Neg(den)
	}
	if den.Sign() == 0 {
		return Rat{Num: big.NewInt(0), Den: big.NewInt(0)}
	}
	g := new(big.Int).GCD(nil, nil, num, den)
	if g.Sign() == 0 {
		return Rat{Num: big.NewInt(0), Den: big.NewInt(0)}
	}
	return Rat{
		Num: new(big.Int).Quo(num, g),
		Den: new(big.Int).Quo(den, g),
	}
}

func minRat(a, b Rat) Rat {
	left := new(big.Int).Mul(a.Num, b.Den)
	right := new(big.Int).Mul(b.Num, a.Den)
	if left.Cmp(right) <= 0 {
		return a
	}
	return b
}

func orient(ax, ay, bx, by, cx, cy int64) *big.Int {
	bax := new(big.Int).Sub(big.NewInt(bx), big.NewInt(ax))
	bay := new(big.Int).Sub(big.NewInt(by), big.NewInt(ay))
	cax := new(big.Int).Sub(big.NewInt(cx), big.NewInt(ax))
	cay := new(big.Int).Sub(big.NewInt(cy), big.NewInt(ay))
	return new(big.Int).Sub(new(big.Int).Mul(bax, cay), new(big.Int).Mul(bay, cax))
}

func oppositeStrict(x, y *big.Int) bool {
	return (x.Sign() > 0 && y.Sign() < 0) || (x.Sign() < 0 && y.Sign() > 0)
}

func properCross(a0x, a0y, a1x, a1y, b0x, b0y, b1x, b1y int64) bool {
	return oppositeStrict(orient(a0x, a0y, a1x, a1y, b0x, b0y), orient(a0x, a0y, a1x, a1y, b1x, b1y)) &&
		oppositeStrict(orient(b0x, b0y, b1x, b1y, a0x, a0y), orient(b0x, b0y, b1x, b1y, a1x, a1y))
}
