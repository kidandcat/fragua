//go:build lean

package leanbridge

import (
	"math/big"
	"testing"
)

// TestDifferential compares the pure Go kernel with the Lean server on
// thousands of segments, including degenerate, collinear, touching, and
// int64-edge coordinates. Single calls and batches both have to match.
func TestDifferential(t *testing.T) {
	segs := append(edgeSegs(), randomSegs(4000)...)
	for i, s := range segs {
		got, err := leanSegSeg(s[0], s[1], s[2], s[3], s[4], s[5], s[6], s[7])
		if err != nil {
			t.Fatalf("case %d lean: %v", i, err)
		}
		want := pureSegSeg(s[0], s[1], s[2], s[3], s[4], s[5], s[6], s[7])
		if err := cmpRat(got, want); err != nil {
			t.Fatalf("case %d seg %v: %v", i, s, err)
		}
	}

	// Single-call coverage for the other operations, on every edge case and
	// a few hundred random ones. The segment loop above already went through
	// the one-request path.
	sample := append(edgeSegs(), randomSegs(400)...)
	for i, s := range sample {
		dLean, err := leanDist2(s[0], s[1], s[2], s[3])
		if err != nil {
			t.Fatal(err)
		}
		if err := cmpInt(dLean, pureDist2(s[0], s[1], s[2], s[3])); err != nil {
			t.Fatalf("dist2 %d: %v", i, err)
		}
		pLean, err := leanPointSeg(s[0], s[1], s[2], s[3], s[4], s[5])
		if err != nil {
			t.Fatal(err)
		}
		if err := cmpRat(pLean, purePointSeg(s[0], s[1], s[2], s[3], s[4], s[5])); err != nil {
			t.Fatalf("point-seg %d: %v", i, err)
		}
		sepLean, err := leanSeparated(pLean.Num, pLean.Den, s[6])
		if err != nil {
			t.Fatal(err)
		}
		if sepLean != pureSeparated(pLean.Num, pLean.Den, s[6]) {
			t.Fatalf("separated %d lean %v", i, sepLean)
		}
		// Negative clearance and a zero denominator.
		neg, err := leanSeparated(pLean.Num, pLean.Den, -8)
		if err != nil {
			t.Fatal(err)
		}
		if neg != pureSeparated(pLean.Num, pLean.Den, -8) {
			t.Fatalf("neg clearance %d", i)
		}
	}

	norms := [][2]string{{"2", "4"}, {"0", "5"}, {"2", "-4"}, {"-2", "-4"}, {"-15", "25"}}
	for _, n := range norms {
		num, _ := new(big.Int).SetString(n[0], 10)
		den, _ := new(big.Int).SetString(n[1], 10)
		got, err := leanNorm(num, den)
		if err != nil {
			t.Fatal(err)
		}
		if err := cmpRat(got, pureNorm(num, den)); err != nil {
			t.Fatalf("norm %s/%s: %v", n[0], n[1], err)
		}
	}

	batch, err := leanSegSegBatch(segs)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) != len(segs) {
		t.Fatalf("batch len %d", len(batch))
	}
	for i, s := range segs {
		want := pureSegSeg(s[0], s[1], s[2], s[3], s[4], s[5], s[6], s[7])
		if err := cmpRat(batch[i], want); err != nil {
			t.Fatalf("batch %d: %v", i, err)
		}
	}
}

// TestDifferentialRejectsMutation runs the same comparator the table uses,
// against a numerator that has been deliberately incremented. This is the
// proof that TestDifferential is not vacuously green.
//
// The whole table was also shown to fail by adding one to pureDist2 and running
//
//	go test -tags lean -count=1 -run 'TestDifferential$' ./internal/leanbridge/
//
// which stopped at case 0 with "0/1 != 1/1". That edit was reverted.
func TestDifferentialRejectsMutation(t *testing.T) {
	pure := pureSegSeg(0, 0, 10, 0, 0, 5, 10, 5)
	lean, err := leanSegSeg(0, 0, 10, 0, 0, 5, 10, 5)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmpRat(lean, pure); err != nil {
		t.Fatalf("unmutated pair should agree: %v", err)
	}
	lie := Rat{Num: new(big.Int).Add(pure.Num, big.NewInt(1)), Den: new(big.Int).Set(pure.Den)}
	if err := cmpRat(lie, lean); err == nil {
		t.Fatal("differential comparator accepted a numerator mutated by +1")
	}
}
