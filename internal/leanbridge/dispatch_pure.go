//go:build !lean

package leanbridge

import "math/big"

func dist2(ax, ay, bx, by int64) *big.Int {
	return pureDist2(ax, ay, bx, by)
}

func pointSegDist2(px, py, ax, ay, bx, by int64) Rat {
	return purePointSeg(px, py, ax, ay, bx, by)
}

func segSegDist2(a0x, a0y, a1x, a1y, b0x, b0y, b1x, b1y int64) Rat {
	return pureSegSeg(a0x, a0y, a1x, a1y, b0x, b0y, b1x, b1y)
}

func separated(num, den *big.Int, clearanceNm int64) bool {
	return pureSeparated(num, den, clearanceNm)
}
