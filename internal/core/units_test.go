package core

import "testing"

func TestFromMMRoundTrip(t *testing.T) {
	l := FromMM(1.25)
	if l != Length(1_250_000) {
		t.Fatalf("FromMM(1.25) = %d, want 1250000", l)
	}
	if d := l.ToMM(); d < 1.249 || d > 1.251 {
		t.Fatalf("ToMM = %v", d)
	}
}

func TestRectIntersects(t *testing.T) {
	a := RectFromCorners(NewPoint(0, 0), NewPoint(FromMM(10), FromMM(10)))
	b := RectFromCorners(NewPoint(FromMM(5), FromMM(5)), NewPoint(FromMM(15), FromMM(15)))
	if !a.Intersects(b) {
		t.Fatal("expected intersection")
	}
	// edge-touching: max of a == min of c → no interior overlap
	c := RectFromCorners(NewPoint(FromMM(10), 0), NewPoint(FromMM(20), FromMM(10)))
	if a.Intersects(c) {
		t.Fatal("edge-touching should not count as intersect")
	}
}

func TestPointInPolygonSquare(t *testing.T) {
	sq := []Point{
		NewPoint(0, 0),
		NewPoint(FromMM(10), 0),
		NewPoint(FromMM(10), FromMM(10)),
		NewPoint(0, FromMM(10)),
	}
	if !PointInPolygon(NewPoint(FromMM(5), FromMM(5)), sq) {
		t.Fatal("center should be inside")
	}
	if PointInPolygon(NewPoint(FromMM(15), FromMM(5)), sq) {
		t.Fatal("outside should be false")
	}
}
