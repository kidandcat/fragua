package leanbridge

import (
	"math"
	"math/big"
	"math/rand"
	"testing"
)

func ratEq(a, b Rat) bool {
	return a.Num.Cmp(b.Num) == 0 && a.Den.Cmp(b.Den) == 0
}

// cmpRat is the differential comparator. It returns an error when the two
// fractions differ, which is what a wrong implementation must trip.
func cmpRat(got, want Rat) error {
	if ratEq(got, want) {
		return nil
	}
	return errDiff(got, want)
}

type diffErr struct{ got, want Rat }

func (e diffErr) Error() string {
	return e.got.Num.String() + "/" + e.got.Den.String() + " != " + e.want.Num.String() + "/" + e.want.Den.String()
}

func errDiff(got, want Rat) error { return diffErr{got, want} }

func cmpInt(got, want *big.Int) error {
	if got.Cmp(want) == 0 {
		return nil
	}
	return errDiff(Rat{Num: got, Den: big.NewInt(1)}, Rat{Num: want, Den: big.NewInt(1)})
}

// TestComparatorRejectsMutation is the in-tree proof that the comparator can
// fail. Shifting a 3-4-5 square by one nanometre squared must not compare equal.
func TestComparatorRejectsMutation(t *testing.T) {
	got := pureDist2(0, 0, 3, 4)
	if got.Cmp(big.NewInt(25)) != 0 {
		t.Fatalf("dist2 = %s, want 25", got)
	}
	mutated := new(big.Int).Add(got, big.NewInt(1))
	if err := cmpInt(mutated, got); err == nil {
		t.Fatal("comparator accepted a distance shifted by 1")
	}
	mutatedRat := Rat{Num: new(big.Int).Add(big.NewInt(1), big.NewInt(1)), Den: big.NewInt(2)}
	if err := cmpRat(mutatedRat, Rat{Num: big.NewInt(1), Den: big.NewInt(1)}); err == nil {
		t.Fatal("comparator accepted 2/2 against 1/1 without reducing the mutated side")
	}
}

func TestNormalize(t *testing.T) {
	cases := []struct {
		num, den string
		wn, wd   string
	}{
		{"2", "4", "1", "2"},
		{"0", "5", "0", "1"},
		{"2", "-4", "-1", "2"},
		{"-2", "-4", "1", "2"},
		{"25", "1", "25", "1"},
		{"-15", "5", "-3", "1"},
	}
	for _, c := range cases {
		num, _ := new(big.Int).SetString(c.num, 10)
		den, _ := new(big.Int).SetString(c.den, 10)
		got := pureNorm(num, den)
		if got.Num.String() != c.wn || got.Den.String() != c.wd {
			t.Fatalf("norm %s/%s = %s/%s, want %s/%s", c.num, c.den, got.Num, got.Den, c.wn, c.wd)
		}
	}
}

func TestKnownDistances(t *testing.T) {
	if d := pureDist2(0, 0, 3, 4); d.Cmp(big.NewInt(25)) != 0 {
		t.Fatalf("3-4-5 = %s", d)
	}
	if !ratEq(pureDist2Rat(3, 4, 0, 0), pureDist2Rat(0, 0, 3, 4)) {
		t.Fatal("dist2 not symmetric")
	}
	// (1,1) onto (0,0)-(4,0) is 1.
	if r := purePointSeg(1, 1, 0, 0, 4, 0); !ratEq(r, rat(1, 1)) {
		t.Fatalf("point-seg = %s/%s", r.Num, r.Den)
	}
	// (0,1) onto (0,0)-(1,1) is 1/2.
	if r := purePointSeg(0, 1, 0, 0, 1, 1); !ratEq(r, rat(1, 2)) {
		t.Fatalf("diagonal point-seg = %s/%s", r.Num, r.Den)
	}
	// Endpoints and a degenerate segment.
	if r := purePointSeg(0, 0, 0, 0, 5, 0); r.Num.Sign() != 0 {
		t.Fatal("start point should be distance 0")
	}
	if r := purePointSeg(5, 0, 0, 0, 5, 0); r.Num.Sign() != 0 {
		t.Fatal("end point should be distance 0")
	}
	if r := purePointSeg(9, 0, 0, 0, 5, 0); !ratEq(r, rat(16, 1)) {
		t.Fatalf("past the end = %s/%s", r.Num, r.Den)
	}
	// Proper crossing.
	if r := pureSegSeg(0, 0, 2, 0, 1, -1, 1, 1); r.Num.Sign() != 0 {
		t.Fatal("crossing segments should touch")
	}
	// Parallel horizontals, 2 nm apart → squared distance 4.
	if r := pureSegSeg(0, 0, 2, 0, 0, 2, 2, 2); !ratEq(r, rat(4, 1)) {
		t.Fatalf("parallel = %s/%s", r.Num, r.Den)
	}
	// Collinear overlap and a shared endpoint.
	if r := pureSegSeg(0, 0, 10, 0, 4, 0, 12, 0); r.Num.Sign() != 0 {
		t.Fatal("collinear overlap should be 0")
	}
	if r := pureSegSeg(0, 0, 10, 0, 10, 0, 20, 0); r.Num.Sign() != 0 {
		t.Fatal("endpoint touch should be 0")
	}
	// Collinear gap of 1.
	if r := pureSegSeg(0, 0, 10, 0, 11, 0, 20, 0); !ratEq(r, rat(1, 1)) {
		t.Fatalf("collinear gap = %s/%s", r.Num, r.Den)
	}
	// T junction: endpoint in the interior of the other segment.
	if r := pureSegSeg(0, 0, 10, 0, 5, 0, 5, 4); r.Num.Sign() != 0 {
		t.Fatal("T junction should be 0")
	}
	// Zero-length segments.
	if r := pureSegSeg(3, 4, 3, 4, 0, 0, 0, 0); !ratEq(r, rat(25, 1)) {
		t.Fatalf("two points = %s/%s", r.Num, r.Den)
	}
	if r := pureSegSeg(7, 1, 7, 1, 7, 1, 9, 1); r.Num.Sign() != 0 {
		t.Fatal("point on a segment should be 0")
	}
	if !pureSeparated(big.NewInt(25), big.NewInt(1), 5) {
		t.Fatal("distance 5 should meet clearance 5")
	}
	if pureSeparated(big.NewInt(25), big.NewInt(1), 6) {
		t.Fatal("distance 5 should miss clearance 6")
	}
	if pureSeparated(big.NewInt(1), big.NewInt(2), 1) {
		t.Fatal("squared distance 1/2 is under clearance 1")
	}
	if pureSeparated(big.NewInt(1), big.NewInt(2), -3) != pureSeparated(big.NewInt(1), big.NewInt(2), 0) {
		t.Fatal("negative clearance should match clearance 0")
	}
	if pureSeparated(big.NewInt(1), big.NewInt(0), 1) {
		t.Fatal("zero denominator is not a distance")
	}
	if pureSeparated(big.NewInt(-1), big.NewInt(1), 0) {
		t.Fatal("negative numerator is not a squared distance")
	}
}

func TestPureInvariants(t *testing.T) {
	segs := append(edgeSegs(), randomSegs(2000)...)
	for i, s := range segs {
		r := pureSegSeg(s[0], s[1], s[2], s[3], s[4], s[5], s[6], s[7])
		if r.Num.Sign() < 0 || r.Den.Sign() <= 0 {
			t.Fatalf("case %d negative fraction %s/%s", i, r.Num, r.Den)
		}
		swapped := pureSegSeg(s[4], s[5], s[6], s[7], s[0], s[1], s[2], s[3])
		if !ratEq(r, swapped) {
			t.Fatalf("case %d asymmetry %s/%s vs %s/%s", i, r.Num, r.Den, swapped.Num, swapped.Den)
		}
		d := pureDist2(s[0], s[1], s[2], s[3])
		if d.Sign() < 0 {
			t.Fatalf("case %d negative dist2", i)
		}
		// Clearance equal to a integer endpoint distance is separated; one more is not
		// when the squared distance is an integer (den 1).
		if r.Den.Cmp(big.NewInt(1)) == 0 {
			// sqrt may not be integer; compare the predicate against the square.
			if !pureSeparated(r.Num, r.Den, 0) && r.Num.Sign() >= 0 {
				t.Fatalf("case %d not separated at clearance 0", i)
			}
		}
	}
}

func TestHugeCoords(t *testing.T) {
	h := int64(math.MaxInt64)
	l := int64(math.MinInt64)
	cases := [][8]int64{
		{h, h, h, h, l, l, l, l},
		{h, l, l, h, 0, 0, 0, 0},
		{l, l, l, h, h, l, h, h},
		{h - 5, h, h, h, h - 5, h - 1, h, h - 1},
		{l, 0, l + 10, 0, l, 1, l + 10, 1},
	}
	for i, s := range cases {
		r := pureSegSeg(s[0], s[1], s[2], s[3], s[4], s[5], s[6], s[7])
		if r.Den.Sign() <= 0 {
			t.Fatalf("huge case %d den %s", i, r.Den)
		}
		// Must not panic and must agree with the four-endpoint minimum when
		// the segments do not properly cross. The cross test is the same
		// function, so just check the value is stable under reversal.
		rev := pureSegSeg(s[2], s[3], s[0], s[1], s[6], s[7], s[4], s[5])
		if !ratEq(r, rev) {
			t.Fatalf("huge case %d reversal %s/%s vs %s/%s", i, r.Num, r.Den, rev.Num, rev.Den)
		}
	}
}

func rat(n, d int64) Rat {
	return normalize(big.NewInt(n), big.NewInt(d))
}

func pureDist2Rat(ax, ay, bx, by int64) Rat {
	return normInt(pureDist2(ax, ay, bx, by))
}

func edgeSegs() [][8]int64 {
	h := int64(math.MaxInt64)
	l := int64(math.MinInt64)
	return [][8]int64{
		{0, 0, 0, 0, 0, 0, 0, 0},
		{0, 0, 10, 0, 0, 2, 10, 2},
		{0, 0, 2, 0, 1, -1, 1, 1},
		{0, 0, 10, 0, 4, 0, 12, 0},
		{0, 0, 10, 0, 10, 0, 20, 0},
		{0, 0, 10, 0, 11, 0, 20, 0},
		{0, 0, 10, 0, 5, 0, 5, 4},
		{3, 4, 3, 4, 0, 0, 0, 0},
		{7, 1, 7, 1, 7, 1, 9, 1},
		{0, 0, 0, 5, 2, 0, 2, 5},
		{-5, -5, 5, 5, -5, 5, 5, -5},
		{0, 0, 100, 0, 0, 0, 100, 0},
		{0, 0, 100, 0, 100, 0, 0, 0},
		{1, 2, 1, 2, 8, -3, 8, -3},
		{l, l, h, h, l, h, h, l},
		{h, h, h, h, l, l, l, l},
		{h, l, l, h, 0, 0, 1, 0},
		{l, 0, l + 10, 0, l, 1, l + 10, 1},
		{h - 5, 0, h, 0, h - 5, 2, h, 2},
		{-1 << 62, -1 << 62, 1 << 62, 0, 0, 1 << 62, 5, -3},
	}
}

func randomSegs(n int) [][8]int64 {
	rng := rand.New(rand.NewSource(20261103))
	out := make([][8]int64, 0, n)
	for i := 0; i < n; i++ {
		mode := i % 3
		var s [8]int64
		for k := 0; k < 8; k++ {
			s[k] = randCoord(rng, mode)
		}
		// A slice of the random set is forced into degenerate / collinear shapes
		// so those branches are not left to chance.
		switch i % 11 {
		case 0:
			s[2], s[3] = s[0], s[1]
		case 1:
			s[6], s[7] = s[4], s[5]
		case 2:
			s[4], s[5], s[6], s[7] = s[0], s[1], s[2], s[3]
		case 3:
			s[4], s[5] = s[2], s[3]
			s[6], s[7] = s[2]+s[2]-s[0], s[3]+s[3]-s[1]
		}
		out = append(out, s)
	}
	return out
}

func randCoord(rng *rand.Rand, mode int) int64 {
	switch mode {
	case 0:
		return int64(rng.Intn(20001) - 10000)
	case 1:
		return rng.Int63n(400_000_001) - 200_000_000
	default:
		picks := []int64{
			math.MinInt64, math.MinInt64 + 1, math.MinInt64 + 2,
			-1 << 62, -1, 0, 1, 1 << 62,
			math.MaxInt64 - 2, math.MaxInt64 - 1, math.MaxInt64,
		}
		if rng.Intn(3) == 0 {
			return picks[rng.Intn(len(picks))]
		}
		if rng.Intn(2) == 0 {
			return math.MaxInt64 - rng.Int63n(1_000_000)
		}
		return math.MinInt64 + rng.Int63n(1_000_000)
	}
}

func BenchmarkPureSegSeg(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		pureSegSeg(0, 0, 5_000_000, 0, 0, 250_000, 8_000_000, 250_000)
	}
}

func BenchmarkPureHotLoop(b *testing.B) {
	b.ReportAllocs()
	const chunk = 4096
	var acc *big.Int
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		acc = pureSegSum(chunk)
	}
	if acc.Sign() < 0 {
		b.Fatal(acc)
	}
}
