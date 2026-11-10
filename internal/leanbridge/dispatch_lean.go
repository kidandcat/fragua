//go:build lean

package leanbridge

import "math/big"

func dist2(ax, ay, bx, by int64) *big.Int {
	n, err := leanDist2(ax, ay, bx, by)
	if err != nil {
		panic("fragua-geom: " + err.Error())
	}
	return n
}

func pointSegDist2(px, py, ax, ay, bx, by int64) Rat {
	r, err := leanPointSeg(px, py, ax, ay, bx, by)
	if err != nil {
		panic("fragua-geom: " + err.Error())
	}
	return r
}

func segSegDist2(a0x, a0y, a1x, a1y, b0x, b0y, b1x, b1y int64) Rat {
	r, err := leanSegSeg(a0x, a0y, a1x, a1y, b0x, b0y, b1x, b1y)
	if err != nil {
		panic("fragua-geom: " + err.Error())
	}
	return r
}

func separated(num, den *big.Int, clearanceNm int64) bool {
	ok, err := leanSeparated(num, den, clearanceNm)
	if err != nil {
		panic("fragua-geom: " + err.Error())
	}
	return ok
}
