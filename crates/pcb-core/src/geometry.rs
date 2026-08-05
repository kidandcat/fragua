//! 2D geometry primitives over `Length`.
//!
//! Everything stays in nanometres so comparisons, hashing, and bounding
//! boxes are exact. Floating-point only appears in `to_mm` conversions
//! at the user/render boundary.

use serde::{Deserialize, Serialize};

use crate::units::Length;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize)]
pub struct Point {
    pub x: Length,
    pub y: Length,
}

impl Point {
    pub const ORIGIN: Self = Self {
        x: Length::ZERO,
        y: Length::ZERO,
    };

    #[must_use]
    pub fn new(x: Length, y: Length) -> Self {
        Self { x, y }
    }

    #[must_use]
    pub fn translate(self, dx: Length, dy: Length) -> Self {
        Self {
            x: self.x + dx,
            y: self.y + dy,
        }
    }
}

/// Axis-aligned rectangle in board coordinates.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize)]
pub struct Rect {
    pub min: Point,
    pub max: Point,
}

impl Rect {
    #[must_use]
    pub fn from_corners(a: Point, b: Point) -> Self {
        Self {
            min: Point {
                x: a.x.min(b.x),
                y: a.y.min(b.y),
            },
            max: Point {
                x: a.x.max(b.x),
                y: a.y.max(b.y),
            },
        }
    }

    #[must_use]
    pub fn from_center(center: Point, w: Length, h: Length) -> Self {
        Self {
            min: Point {
                x: center.x - w / 2,
                y: center.y - h / 2,
            },
            max: Point {
                x: center.x + w / 2,
                y: center.y + h / 2,
            },
        }
    }

    #[must_use]
    pub fn width(self) -> Length {
        self.max.x - self.min.x
    }

    #[must_use]
    pub fn height(self) -> Length {
        self.max.y - self.min.y
    }

    /// Rectangle that contains both `self` and `other`.
    #[must_use]
    pub fn union(self, other: Self) -> Self {
        Self {
            min: Point {
                x: self.min.x.min(other.min.x),
                y: self.min.y.min(other.min.y),
            },
            max: Point {
                x: self.max.x.max(other.max.x),
                y: self.max.y.max(other.max.y),
            },
        }
    }

    /// Expand outward by `margin` on every side.
    #[must_use]
    pub fn expand(self, margin: Length) -> Self {
        Self {
            min: self.min.translate(-margin, -margin),
            max: self.max.translate(margin, margin),
        }
    }

    /// True if the two rectangles share any interior area. Touching
    /// edges (zero-area overlap) counts as non-intersecting so a tight
    /// edge-to-edge layout passes the check.
    #[must_use]
    pub fn intersects(&self, other: &Self) -> bool {
        self.min.x.0 < other.max.x.0
            && self.max.x.0 > other.min.x.0
            && self.min.y.0 < other.max.y.0
            && self.max.y.0 > other.min.y.0
    }
}

/// Axis-aligned bounding box of a closed polygon (3+ vertices).
/// Returns `None` if the vertex list is empty.
#[must_use]
pub fn polygon_bbox(poly: &[Point]) -> Option<Rect> {
    let first = *poly.first()?;
    let mut min_x = first.x;
    let mut min_y = first.y;
    let mut max_x = first.x;
    let mut max_y = first.y;
    for p in poly.iter().skip(1) {
        min_x = min_x.min(p.x);
        min_y = min_y.min(p.y);
        max_x = max_x.max(p.x);
        max_y = max_y.max(p.y);
    }
    Some(Rect {
        min: Point::new(min_x, min_y),
        max: Point::new(max_x, max_y),
    })
}

/// True if `p` lies on any edge of `poly` (inclusive endpoints).
#[must_use]
pub fn point_on_polygon_boundary(p: Point, poly: &[Point]) -> bool {
    if poly.len() < 2 {
        return false;
    }
    let n = poly.len();
    for i in 0..n {
        if point_on_segment(p, poly[i], poly[(i + 1) % n]) {
            return true;
        }
    }
    false
}

/// Point-in-polygon (even-odd ray cast). Boundary counts as inside.
/// Degenerate polygons (< 3 verts) never contain a point.
#[must_use]
pub fn point_in_polygon(p: Point, poly: &[Point]) -> bool {
    if poly.len() < 3 {
        return false;
    }
    // On-edge check first (exact integer coords).
    if point_on_polygon_boundary(p, poly) {
        return true;
    }
    let n = poly.len();
    let mut inside = false;
    let mut j = n - 1;
    for i in 0..n {
        let pi = poly[i];
        let pj = poly[j];
        let yi = pi.y.0;
        let yj = pj.y.0;
        let py = p.y.0;
        if (yi > py) != (yj > py) {
            // x intersect of edge with horizontal ray from p
            let xi = pi.x.0 as i128;
            let xj = pj.x.0 as i128;
            // Avoid div by zero: yi != yj because of the branch above.
            let x_int = xi + ((py as i128 - yi as i128) * (xj - xi)) / (yj as i128 - yi as i128);
            if (p.x.0 as i128) < x_int {
                inside = !inside;
            }
        }
        j = i;
    }
    inside
}

/// True if `p` lies on the closed segment `a–b` (inclusive endpoints).
fn point_on_segment(p: Point, a: Point, b: Point) -> bool {
    let cross = (p.y.0 - a.y.0) as i128 * (b.x.0 - a.x.0) as i128
        - (p.x.0 - a.x.0) as i128 * (b.y.0 - a.y.0) as i128;
    if cross != 0 {
        return false;
    }
    let dot = (p.x.0 - a.x.0) as i128 * (b.x.0 - a.x.0) as i128
        + (p.y.0 - a.y.0) as i128 * (b.y.0 - a.y.0) as i128;
    if dot < 0 {
        return false;
    }
    let len2 = (b.x.0 - a.x.0) as i128 * (b.x.0 - a.x.0) as i128
        + (b.y.0 - a.y.0) as i128 * (b.y.0 - a.y.0) as i128;
    dot <= len2
}

/// True if `p` is inside the outer polygon and outside every cutout.
#[must_use]
pub fn point_in_board_shape(p: Point, outer: &[Point], cutouts: &[Vec<Point>]) -> bool {
    if !point_in_polygon(p, outer) {
        return false;
    }
    for c in cutouts {
        if point_in_polygon(p, c) {
            return false;
        }
    }
    true
}

#[cfg(test)]
mod poly_tests {
    use super::*;
    use crate::units::Length;

    fn p(x: f64, y: f64) -> Point {
        Point::new(Length::from_mm(x), Length::from_mm(y))
    }

    #[test]
    fn square_contains_center() {
        let poly = [p(0.0, 0.0), p(10.0, 0.0), p(10.0, 10.0), p(0.0, 10.0)];
        assert!(point_in_polygon(p(5.0, 5.0), &poly));
        assert!(!point_in_polygon(p(15.0, 5.0), &poly));
        assert!(point_in_polygon(p(0.0, 0.0), &poly)); // corner
    }

    #[test]
    fn cutout_punches_hole() {
        let outer = [p(0.0, 0.0), p(20.0, 0.0), p(20.0, 20.0), p(0.0, 20.0)];
        let cut = vec![p(8.0, 8.0), p(12.0, 8.0), p(12.0, 12.0), p(8.0, 12.0)];
        assert!(point_in_board_shape(p(2.0, 2.0), &outer, &[cut.clone()]));
        assert!(!point_in_board_shape(p(10.0, 10.0), &outer, &[cut]));
    }
}
