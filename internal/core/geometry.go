package core

// Point is a 2D point in board coordinates (nanometres).
type Point struct {
	X Length `json:"x"`
	Y Length `json:"y"`
}

// Origin is (0, 0).
var Origin = Point{}

// NewPoint constructs a Point.
func NewPoint(x, y Length) Point {
	return Point{X: x, Y: y}
}

// Translate returns p + (dx, dy).
func (p Point) Translate(dx, dy Length) Point {
	return Point{X: p.X + dx, Y: p.Y + dy}
}

// Rect is an axis-aligned rectangle.
type Rect struct {
	Min Point `json:"min"`
	Max Point `json:"max"`
}

// RectFromCorners builds a Rect from any two corners.
func RectFromCorners(a, b Point) Rect {
	return Rect{
		Min: Point{X: Min(a.X, b.X), Y: Min(a.Y, b.Y)},
		Max: Point{X: Max(a.X, b.X), Y: Max(a.Y, b.Y)},
	}
}

// RectFromCenter builds a Rect of size w×h centered at center.
func RectFromCenter(center Point, w, h Length) Rect {
	return Rect{
		Min: Point{X: center.X - w/2, Y: center.Y - h/2},
		Max: Point{X: center.X + w/2, Y: center.Y + h/2},
	}
}

// Width returns max.x - min.x.
func (r Rect) Width() Length { return r.Max.X - r.Min.X }

// Height returns max.y - min.y.
func (r Rect) Height() Length { return r.Max.Y - r.Min.Y }

// Union returns the AABB containing both rectangles.
func (r Rect) Union(other Rect) Rect {
	return Rect{
		Min: Point{X: Min(r.Min.X, other.Min.X), Y: Min(r.Min.Y, other.Min.Y)},
		Max: Point{X: Max(r.Max.X, other.Max.X), Y: Max(r.Max.Y, other.Max.Y)},
	}
}

// Expand grows the rect by margin on every side.
func (r Rect) Expand(margin Length) Rect {
	return Rect{
		Min: r.Min.Translate(-margin, -margin),
		Max: r.Max.Translate(margin, margin),
	}
}

// Intersects reports whether the two rects share interior area.
// Touching edges (zero-area overlap) count as non-intersecting.
func (r Rect) Intersects(other Rect) bool {
	return r.Min.X < other.Max.X && r.Max.X > other.Min.X &&
		r.Min.Y < other.Max.Y && r.Max.Y > other.Min.Y
}

// ContainsPoint reports whether p is inside or on the boundary of r.
func (r Rect) ContainsPoint(p Point) bool {
	return p.X >= r.Min.X && p.X <= r.Max.X && p.Y >= r.Min.Y && p.Y <= r.Max.Y
}

// PolygonBBox returns the AABB of a closed polygon, or false if empty.
func PolygonBBox(verts []Point) (Rect, bool) {
	if len(verts) == 0 {
		return Rect{}, false
	}
	r := Rect{Min: verts[0], Max: verts[0]}
	for _, v := range verts[1:] {
		r.Min.X = Min(r.Min.X, v.X)
		r.Min.Y = Min(r.Min.Y, v.Y)
		r.Max.X = Max(r.Max.X, v.X)
		r.Max.Y = Max(r.Max.Y, v.Y)
	}
	return r, true
}

// PointInPolygon is ray-casting even-odd fill rule.
func PointInPolygon(p Point, poly []Point) bool {
	n := len(poly)
	if n < 3 {
		return false
	}
	inside := false
	j := n - 1
	for i := 0; i < n; i++ {
		yi, yj := poly[i].Y, poly[j].Y
		xi, xj := poly[i].X, poly[j].X
		if (yi > p.Y) != (yj > p.Y) {
			// x of intersection with horizontal ray
			x := xi + (p.Y-yi)*(xj-xi)/(yj-yi)
			if p.X < x {
				inside = !inside
			}
		}
		j = i
	}
	return inside
}

// PointInBoardShape: rectangular outline or polygon; cutouts exclude.
func PointInBoardShape(p Point, outline *Rect, poly []Point, cutouts [][]Point) bool {
	in := false
	if len(poly) >= 3 {
		in = PointInPolygon(p, poly)
	} else if outline != nil {
		in = outline.ContainsPoint(p)
	}
	if !in {
		return false
	}
	for _, c := range cutouts {
		if PointInPolygon(p, c) {
			return false
		}
	}
	return true
}
