//! Organic post-pass: turn the router's grid-born polylines into
//! smooth, any-angle, TopoR-style geometry.
//!
//! The Theta* search already produces any-angle *segments*, but they
//! carry grid artefacts: staircase kinks, needless bends, and corners
//! sharp enough to read as "autorouted". This pass rewrites each routed
//! chain in continuous space:
//!
//!   1. **String-pulling** — greedily replace any sub-path with a
//!      straight segment when the segment keeps full clearance from
//!      every other-net obstacle (pads, traces, vias, board edge). This
//!      is the rubber-band contraction: the trace tightens like an
//!      elastic cord around the obstacles that actually block it.
//!   2. **Arc filleting** — every remaining corner becomes a tangent
//!      arc with the largest radius that stays clear, discretised well
//!      under DRC resolution. Traces flow around parts instead of
//!      cornering at them.
//!
//! Every rewrite is validated against the same clearance model before
//! being accepted, so the pass is DRC-neutral by construction: a chain
//! either comes out cleaner or is left exactly as the router made it.
//! Chain endpoints (pads, vias, junctions with other chains) are never
//! moved, which keeps net topology and connectivity untouched.

use std::collections::HashMap;

use pcb_core::{Board, CopperLayer, Length, Point, Trace};

use crate::router::RouteOptions;

/// Tunables for the organic pass.
#[derive(Debug, Clone)]
pub struct OrganicOptions {
    /// Largest fillet radius attempted at a corner, mm.
    pub max_fillet_radius_mm: f64,
    /// Max chord deviation when discretising an arc, mm. 0.02 mm is far
    /// below any fab tolerance while keeping segment counts low.
    pub chord_tol_mm: f64,
}

impl Default for OrganicOptions {
    fn default() -> Self {
        Self {
            max_fillet_radius_mm: 3.0,
            chord_tol_mm: 0.02,
        }
    }
}

/// What the pass did, for the route report.
#[derive(Debug, Clone, Default)]
pub struct OrganicReport {
    pub chains: usize,
    pub segments_before: usize,
    pub segments_after: usize,
    pub length_before_mm: f64,
    pub length_after_mm: f64,
    /// The whole pass was discarded because the smoothed board did not
    /// pass the final legality sweep — see `organic_pass`.
    pub rolled_back: bool,
}

pub(crate) type P2 = [f64; 2];

fn sub(a: P2, b: P2) -> P2 {
    [a[0] - b[0], a[1] - b[1]]
}
fn dot(a: P2, b: P2) -> f64 {
    a[0] * b[0] + a[1] * b[1]
}
fn norm(a: P2) -> f64 {
    dot(a, a).sqrt()
}
pub(crate) fn dist(a: P2, b: P2) -> f64 {
    norm(sub(a, b))
}

/// Distance from point `p` to segment `ab`.
fn point_seg_dist(p: P2, a: P2, b: P2) -> f64 {
    let ab = sub(b, a);
    let len2 = dot(ab, ab);
    if len2 <= 1e-18 {
        return dist(p, a);
    }
    let t = (dot(sub(p, a), ab) / len2).clamp(0.0, 1.0);
    dist(p, [a[0] + t * ab[0], a[1] + t * ab[1]])
}

/// True if segments `ab` and `cd` properly intersect or touch.
fn segs_intersect(a: P2, b: P2, c: P2, d: P2) -> bool {
    let orient = |p: P2, q: P2, r: P2| -> f64 {
        (q[0] - p[0]) * (r[1] - p[1]) - (q[1] - p[1]) * (r[0] - p[0])
    };
    let (o1, o2) = (orient(a, b, c), orient(a, b, d));
    let (o3, o4) = (orient(c, d, a), orient(c, d, b));
    if ((o1 > 0.0) != (o2 > 0.0)) && ((o3 > 0.0) != (o4 > 0.0)) {
        return true;
    }
    // Collinear-touch cases resolve through the distance checks below;
    // exact zero orientation with separation > 0 is handled there too.
    false
}

/// Distance between segments `ab` and `cd` (0 when they intersect).
fn seg_seg_dist(a: P2, b: P2, c: P2, d: P2) -> f64 {
    if segs_intersect(a, b, c, d) {
        return 0.0;
    }
    point_seg_dist(a, c, d)
        .min(point_seg_dist(b, c, d))
        .min(point_seg_dist(c, a, b))
        .min(point_seg_dist(d, a, b))
}

/// Distance from segment `ab` to an axis-aligned rect (0 when the
/// segment enters it).
fn seg_rect_dist(a: P2, b: P2, min: P2, max: P2) -> f64 {
    let inside = |p: P2| p[0] >= min[0] && p[0] <= max[0] && p[1] >= min[1] && p[1] <= max[1];
    if inside(a) || inside(b) {
        return 0.0;
    }
    let corners = [
        [min[0], min[1]],
        [max[0], min[1]],
        [max[0], max[1]],
        [min[0], max[1]],
    ];
    let mut best = f64::INFINITY;
    for i in 0..4 {
        let c = corners[i];
        let d = corners[(i + 1) % 4];
        best = best.min(seg_seg_dist(a, b, c, d));
        if best == 0.0 {
            return 0.0;
        }
    }
    best
}

/// One other-net obstacle on a layer, with the clearance its own net
/// class demands. The final required distance to a chain is
/// `chain_half_width + max(chain_clearance, self.clearance) + self
/// copper reach` — computed in `Obstacles::polyline_clear`.
pub(crate) enum Shape {
    /// Pad copper as the DRC sees it: an AABB.
    Rect { min: P2, max: P2 },
    /// Another net's trace segment: centreline + half-width.
    Capsule { a: P2, b: P2, half_w: f64 },
    /// A via barrel: centre + radius.
    Circle { c: P2, r: f64 },
}

pub(crate) struct Obstacle {
    pub(crate) shape: Shape,
    /// Clearance the obstacle's own net class demands. This is only half
    /// the rule: a `RuleArea` covering the point where the two coppers
    /// actually come closest REPLACES it outright, and that is resolved
    /// per approach in `Obstacles::approach` — exactly where DRC
    /// resolves it. Sampling the area at the obstacle's centre instead
    /// (what this used to do) disagrees with DRC for every pair whose
    /// closest approach lands on the other side of an area border.
    pub(crate) clearance_mm: f64,
    /// Net owning the copper, when it has one — the topo engine's
    /// targeted rip-up wants to know WHO is in the way.
    pub(crate) net: Option<String>,
}

/// All other-net obstacles a given net's chains must clear on a layer,
/// plus the outline band the centreline must stay inside.
pub(crate) struct Obstacles {
    pub(crate) items: Vec<Obstacle>,
    /// The board's rule areas, cloned so the checker can ask the ONE
    /// resolver for the rule at an arbitrary point without borrowing a
    /// resolver that outlives its builder (the topo engine builds one
    /// per call). A board carries a handful of areas.
    areas: Vec<pcb_core::RuleArea>,
    defaults: pcb_core::RuleDefaults,
    /// Layer these obstacles live on — rule areas can be layer-scoped.
    layer: CopperLayer,
    /// Largest clearance any area on this board can demand. Upper-bounds
    /// the required distance, so an obstacle further away than that can
    /// be discarded without resolving the rule at all.
    worst_area_mm: f64,
    /// Board outline, unshrunk: the centreline band folds in the chain's
    /// own half-width at check time, because one net's chains can carry
    /// different widths (a narrowed fine-pitch stub and its 0.25 mm run).
    pub(crate) outline: pcb_core::Rect,
    /// Edge clearance the centreline band adds on top of the half-width.
    edge_mm: f64,
}

impl Obstacles {
    /// Clearance a rule area imposes at `site`, if any. Goes through
    /// `RuleResolver` so area priority/specificity is derived in exactly
    /// one place (see `pcb_core::rules`).
    fn area_clearance_at(&self, site: P2) -> Option<f64> {
        if self.areas.is_empty() {
            return None;
        }
        pcb_core::RuleResolver::new(&self.areas, self.defaults)
            .area_clearance(to_point(site), Some(self.layer))
            .map(|l| l.to_mm())
    }

    /// Centreline band for a chain of half-width `hw`.
    fn band(&self, hw: f64) -> (P2, P2) {
        let m = hw + self.edge_mm;
        (
            [
                self.outline.min.x.to_mm() + m,
                self.outline.min.y.to_mm() + m,
            ],
            [
                self.outline.max.x.to_mm() - m,
                self.outline.max.y.to_mm() - m,
            ],
        )
    }

    /// Copper-edge distance from segment `ab` to `ob` and the clearance
    /// required there, or `None` when no rule on this board could be
    /// violated at that distance (the fast path — it skips resolving the
    /// area, which needs the point of closest approach).
    fn approach(&self, a: P2, b: P2, ob: &Obstacle, hw: f64, clr: f64) -> Option<(f64, f64)> {
        let d = match &ob.shape {
            Shape::Rect { min, max } => seg_rect_dist(a, b, *min, *max),
            Shape::Capsule {
                a: c,
                b: d2,
                half_w,
            } => seg_seg_dist(a, b, *c, *d2) - half_w,
            Shape::Circle { c, r } => point_seg_dist(*c, a, b) - r,
        };
        let class_need = hw + clr.max(ob.clearance_mm);
        if d >= class_need.max(hw + self.worst_area_mm) {
            return None;
        }
        let site = match &ob.shape {
            Shape::Rect { min, max } => seg_rect_site(a, b, *min, *max),
            Shape::Capsule { a: c, b: d2, .. } => seg_seg_site(a, b, *c, *d2),
            Shape::Circle { c, r } => seg_circle_site(a, b, *c, *r),
        };
        let need = self.area_clearance_at(site).map_or(class_need, |c| hw + c);
        Some((d, need))
    }

    fn describe(ob: &Obstacle) -> String {
        match &ob.shape {
            Shape::Rect { min, max } => format!(
                "pad rect ({:.2},{:.2})-({:.2},{:.2})",
                min[0], min[1], max[0], max[1]
            ),
            Shape::Capsule { a, b, .. } => {
                format!("trace ({:.2},{:.2})->({:.2},{:.2})", a[0], a[1], b[0], b[1])
            }
            Shape::Circle { c, .. } => format!("via ({:.2},{:.2})", c[0], c[1]),
        }
    }

    /// Debug twin of `polyline_clear`: description of the first
    /// violation, or `None` when the polyline is clear.
    pub(crate) fn first_violation(&self, pts: &[P2], hw: f64, clr: f64) -> Option<String> {
        let (lo, hi) = self.band(hw);
        for w in pts.windows(2) {
            let (a, b) = (w[0], w[1]);
            for p in [a, b] {
                if p[0] < lo[0] || p[1] < lo[1] || p[0] > hi[0] || p[1] > hi[1] {
                    return Some(format!("outline band at ({:.2},{:.2})", p[0], p[1]));
                }
            }
            for ob in &self.items {
                let Some((d, need)) = self.approach(a, b, ob, hw, clr) else {
                    continue;
                };
                if d < need - 1e-6 {
                    return Some(format!(
                        "{}: d {d:.3} < need {need:.3} on seg ({:.2},{:.2})->({:.2},{:.2})",
                        Self::describe(ob),
                        a[0],
                        a[1],
                        b[0],
                        b[1]
                    ));
                }
            }
        }
        None
    }

    /// Net of the first blocking TRACE/VIA obstacle (pads return None —
    /// they cannot be ripped up).
    pub(crate) fn first_blocking_net(&self, pts: &[P2], hw: f64, clr: f64) -> Option<String> {
        for w in pts.windows(2) {
            let (a, b) = (w[0], w[1]);
            for ob in &self.items {
                let Some((d, need)) = self.approach(a, b, ob, hw, clr) else {
                    continue;
                };
                if d < need - 1e-6 {
                    if let Some(n) = &ob.net {
                        return Some(n.clone());
                    }
                }
            }
        }
        None
    }

    /// True when every segment of `pts` keeps clearance. `hw` is the
    /// chain's half-width, `clr` its net clearance.
    pub(crate) fn polyline_clear(&self, pts: &[P2], hw: f64, clr: f64) -> bool {
        let (lo, hi) = self.band(hw);
        for w in pts.windows(2) {
            let (a, b) = (w[0], w[1]);
            // Stay inside the outline band.
            for p in [a, b] {
                if p[0] < lo[0] || p[1] < lo[1] || p[0] > hi[0] || p[1] > hi[1] {
                    return false;
                }
            }
            for ob in &self.items {
                let Some((d, need)) = self.approach(a, b, ob, hw, clr) else {
                    continue;
                };
                // Micro-epsilon: an exactly-at-clearance geometry (a
                // trace laid tangent to the window) must count as
                // clear, or float noise flips borderline fits.
                if d < need - 1e-6 {
                    return false;
                }
            }
        }
        true
    }
}

/// Point on segment `ab` closest to `p`.
fn closest_on_seg(p: P2, a: P2, b: P2) -> P2 {
    let ab = sub(b, a);
    let len2 = dot(ab, ab);
    if len2 <= 1e-18 {
        return a;
    }
    let t = (dot(sub(p, a), ab) / len2).clamp(0.0, 1.0);
    [a[0] + t * ab[0], a[1] + t * ab[1]]
}

fn mid(a: P2, b: P2) -> P2 {
    [f64::midpoint(a[0], b[0]), f64::midpoint(a[1], b[1])]
}

/// Midpoint of the closest approach between two segments — the same
/// site `pcb_drc`'s trace/trace check resolves the rule at.
fn seg_seg_site(a0: P2, a1: P2, b0: P2, b1: P2) -> P2 {
    let cands = [
        (a0, closest_on_seg(a0, b0, b1)),
        (a1, closest_on_seg(a1, b0, b1)),
        (closest_on_seg(b0, a0, a1), b0),
        (closest_on_seg(b1, a0, a1), b1),
    ];
    let mut best = cands[0];
    let mut best_d = f64::INFINITY;
    for (p, q) in cands {
        let d = dot(sub(p, q), sub(p, q));
        if d < best_d {
            best_d = d;
            best = (p, q);
        }
    }
    mid(best.0, best.1)
}

fn clamp_to_rect(p: P2, min: P2, max: P2) -> P2 {
    [p[0].clamp(min[0], max[0]), p[1].clamp(min[1], max[1])]
}

/// Midpoint of the closest approach between a segment and a rect — the
/// same site `pcb_drc`'s trace/pad check resolves the rule at.
fn seg_rect_site(a: P2, b: P2, min: P2, max: P2) -> P2 {
    let centre = mid(min, max);
    let on_seg = closest_on_seg(centre, a, b);
    let on_rect = clamp_to_rect(on_seg, min, max);
    let on_seg = closest_on_seg(on_rect, a, b);
    mid(on_seg, on_rect)
}

/// Midpoint of the closest approach between a segment and a via barrel.
fn seg_circle_site(a: P2, b: P2, c: P2, r: f64) -> P2 {
    let on_seg = closest_on_seg(c, a, b);
    let v = sub(on_seg, c);
    let l = norm(v);
    let on_circle = if l < 1e-9 {
        c
    } else {
        [c[0] + v[0] / l * r, c[1] + v[1] / l * r]
    };
    mid(on_seg, on_circle)
}

/// Map key for exact endpoint matching (nm-resolution fixed point).
fn key(p: Point) -> (i64, i64) {
    (p.x.0, p.y.0)
}

pub(crate) fn to_mm(p: Point) -> P2 {
    [p.x.to_mm(), p.y.to_mm()]
}

pub(crate) fn to_point(p: P2) -> Point {
    Point::new(Length::from_mm(p[0]), Length::from_mm(p[1]))
}

/// Run the organic pass over every routed net. `rules` resolves a net
/// name to its `(trace_width, clearance)` — the router's
/// `effective_net_rules` partially applied.
pub(crate) fn organic_pass<F>(
    board: &mut Board,
    opts: &OrganicOptions,
    route_opts: &RouteOptions,
    rules: F,
) -> OrganicReport
where
    F: Fn(&RouteOptions, &str) -> (Length, Length),
{
    let Some(outline) = board.outline else {
        return OrganicReport::default();
    };
    let mut report = OrganicReport::default();

    // Net names with any copper, insertion-ordered for determinism.
    let mut nets: Vec<String> = Vec::new();
    for t in &board.traces {
        if !nets.contains(&t.net) {
            nets.push(t.net.clone());
        }
    }

    // Shared rule resolver so the smoothing pass judges clearance the
    // same way the grid search and DRC do — including rule areas.
    let areas = board.rule_areas.clone();
    let schematic = route_opts.schematic.clone();
    let resolver = pcb_core::RuleResolver::new(
        &areas,
        pcb_core::RuleDefaults {
            clearance: route_opts.clearance,
            trace_width: route_opts.trace_width,
            via_diameter: route_opts.via_diameter,
            via_drill: route_opts.via_drill,
        },
    )
    .with_schematic(schematic.as_deref());

    // The copper as the router committed it. The smoothing below is
    // cosmetic — it must never be the reason a board fails DRC — so if the
    // final sweep finds a violation the whole pass is discarded and this is
    // what stands. See the sweep at the end for why per-chain validation is
    // not enough on its own.
    let before: Vec<Trace> = board.traces.clone();

    for net in &nets {
        let (_class_width, clearance) = rules(route_opts, net);
        let clr = clearance.to_mm();

        for layer in [CopperLayer::Top, CopperLayer::Bottom] {
            let obstacles =
                collect_obstacles(board, net, layer, route_opts, &rules, clr, outline, &resolver);
            let chains = extract_chains(board, net, layer);
            for (chain, width) in chains {
                // The chain keeps the width the ROUTER gave it. Rewriting
                // it at the net's class width used to silently fatten the
                // fine-pitch escape stubs the fanout had narrowed on
                // purpose (0.15/0.20 mm → 0.25 mm), which ate 0.025–0.100
                // mm of clearance that nothing ever re-checked.
                let hw = width.to_mm() / 2.0;
                report.chains += 1;
                report.segments_before += chain.len() - 1;
                let pts: Vec<P2> = chain.iter().map(|p| to_mm(*p)).collect();
                report.length_before_mm += polyline_len(&pts);

                let pulled = string_pull(&pts, &obstacles, hw, clr);
                let smooth = fillet(&pulled, &obstacles, hw, clr, opts);
                // Last gate: what gets committed must pass the same test
                // DRC runs. `string_pull`/`fillet` return the input
                // untouched when they find no improvement, so without
                // this an input that was never itself validated could
                // ride through — geometry that fails the test is never
                // committed (the v6 principle).
                let smooth = if obstacles.polyline_clear(&smooth, hw, clr) {
                    smooth
                } else {
                    pts.clone()
                };

                report.segments_after += smooth.len() - 1;
                report.length_after_mm += polyline_len(&smooth);
                replace_chain(board, net, layer, &chain, &smooth, width);
            }
        }
    }

    // Final legality sweep over the FINISHED board.
    //
    // Each rewrite is validated against the obstacles collected when its
    // net's turn began, and that is not the same thing as being legal at
    // the end: a net smoothed early is judged against copper that later
    // nets then move. On a sparse board the difference never shows;
    // on the compact RP2040 escape ring it produced overlapping copper and
    // a NetShort that the router itself had never laid. Re-collect against
    // the finished geometry and check every chain again — and if anything
    // fails, drop the whole pass rather than ship prettier illegal copper.
    let mut legal = true;
    'sweep: for net in &nets {
        let (_class_width, clearance) = rules(route_opts, net);
        let clr = clearance.to_mm();
        for layer in [CopperLayer::Top, CopperLayer::Bottom] {
            let obstacles =
                collect_obstacles(board, net, layer, route_opts, &rules, clr, outline, &resolver);
            for (chain, width) in extract_chains(board, net, layer) {
                let pts: Vec<P2> = chain.iter().map(|p| to_mm(*p)).collect();
                if !obstacles.polyline_clear(&pts, width.to_mm() / 2.0, clr) {
                    legal = false;
                    break 'sweep;
                }
            }
        }
    }
    if !legal {
        board.traces = before;
        // Truthful, not silent: the pass ran and looked at every chain, it
        // just kept none of them. Zeroing the counters here would make the
        // driver report `organic: None` — indistinguishable from "smoothing
        // was disabled or skipped for budget" — and that is exactly the
        // signal a caller needs to see.
        report.rolled_back = true;
        report.segments_after = report.segments_before;
        report.length_after_mm = report.length_before_mm;
    }
    report
}

fn polyline_len(pts: &[P2]) -> f64 {
    pts.windows(2).map(|w| dist(w[0], w[1])).sum()
}

/// Everything on `layer` that does not belong to `net`.
#[allow(clippy::too_many_arguments)]
pub(crate) fn collect_obstacles<F>(
    board: &Board,
    net: &str,
    layer: CopperLayer,
    route_opts: &RouteOptions,
    rules: &F,
    clr: f64,
    outline: pcb_core::Rect,
    resolver: &pcb_core::RuleResolver<'_>,
) -> Obstacles
where
    F: Fn(&RouteOptions, &str) -> (Length, Length),
{
    let mut items: Vec<Obstacle> = Vec::new();
    // Cache per-net rule lookups; boards have few distinct nets.
    let mut rule_cache: HashMap<String, (f64, f64)> = HashMap::new();
    let mut rules_of = |n: &str| -> (f64, f64) {
        if let Some(v) = rule_cache.get(n) {
            return *v;
        }
        let (w, c) = rules(route_opts, n);
        let v = (w.to_mm(), c.to_mm());
        rule_cache.insert(n.to_string(), v);
        v
    };

    for fp in board.footprints_in_order() {
        for pad in &fp.pads {
            if pad.net.as_deref() == Some(net) {
                continue;
            }
            // A pad blocks this layer if it's on it or is through-hole.
            if pad.drill.is_none() && pad.layer != layer {
                continue;
            }
            let c = fp.pad_world_center(pad);
            let (w, h) = fp.pad_world_size(pad);
            let cm = to_mm(c);
            items.push(Obstacle {
                shape: Shape::Rect {
                    min: [cm[0] - w.to_mm() / 2.0, cm[1] - h.to_mm() / 2.0],
                    max: [cm[0] + w.to_mm() / 2.0, cm[1] + h.to_mm() / 2.0],
                },
                clearance_mm: pad.net.as_deref().map_or(clr, |n| rules_of(n).1),
                net: None, // pads are immovable — never rip-up targets
            });
        }
    }
    for t in &board.traces {
        if t.net == net || t.layer != layer {
            continue;
        }
        // The obstacle's copper is its OWN width, whatever the router
        // laid — a fine-pitch stub is deliberately narrower than the
        // class width and the checker must not fatten it back.
        let c_o = rules_of(&t.net).1;
        items.push(Obstacle {
            shape: Shape::Capsule {
                a: to_mm(t.start),
                b: to_mm(t.end),
                half_w: t.width.to_mm() / 2.0,
            },
            clearance_mm: c_o,
            net: Some(t.net.clone()),
        });
    }
    for v in &board.vias {
        if v.net == net {
            continue;
        }
        let c_o = rules_of(&v.net).1;
        items.push(Obstacle {
            shape: Shape::Circle {
                c: to_mm(v.position),
                r: v.diameter.to_mm() / 2.0,
            },
            clearance_mm: c_o,
            net: Some(v.net.clone()),
        });
    }

    // Board edge: centreline band. Matches the DRC edge check (0.2 mm
    // default); the chain's own half width is folded in at check time.
    let worst_area_mm = resolver
        .areas()
        .iter()
        .filter(|a| a.covers_layer(Some(layer)))
        .filter_map(|a| a.clearance_mm)
        .fold(0.0f64, f64::max);
    Obstacles {
        items,
        areas: resolver.areas().to_vec(),
        defaults: resolver.defaults(),
        layer,
        worst_area_mm,
        outline,
        edge_mm: 0.3,
    }
}

/// Split `net`'s copper on `layer` into maximal chains whose interior
/// points have degree exactly 2 and coincide with nothing else. Chain
/// ends are pads' connection points, vias, junctions (degree ≥ 3) — the
/// points the pass must not move. A width change also ends a chain, so
/// every chain carries ONE width (returned with it) and the smoothing
/// never has to pick between the widths it merged.
fn extract_chains(board: &Board, net: &str, layer: CopperLayer) -> Vec<(Vec<Point>, Length)> {
    // Adjacency over exact endpoints.
    let mut adj: HashMap<(i64, i64), Vec<usize>> = HashMap::new();
    let segs: Vec<&Trace> = board
        .traces
        .iter()
        .filter(|t| t.net == net && t.layer == layer)
        .collect();
    for (i, t) in segs.iter().enumerate() {
        adj.entry(key(t.start)).or_default().push(i);
        adj.entry(key(t.end)).or_default().push(i);
    }
    // Points that must anchor a chain end even at degree 2: vias and
    // pad centres of this net (a chain that passes straight through a
    // pad still must keep hitting it).
    let mut hard: std::collections::HashSet<(i64, i64)> = std::collections::HashSet::new();
    for v in &board.vias {
        if v.net == net {
            hard.insert(key(v.position));
        }
    }
    for fp in board.footprints_in_order() {
        for pad in &fp.pads {
            if pad.net.as_deref() == Some(net) {
                hard.insert(key(fp.pad_world_center(pad)));
            }
        }
    }

    let is_anchor = |k: &(i64, i64), adj: &HashMap<(i64, i64), Vec<usize>>| -> bool {
        if hard.contains(k) {
            return true;
        }
        match adj.get(k) {
            Some(v) if v.len() == 2 => segs[v[0]].width.0 != segs[v[1]].width.0,
            _ => true,
        }
    };

    let mut used = vec![false; segs.len()];
    let mut chains: Vec<(Vec<Point>, Length)> = Vec::new();

    // Walk from every anchor endpoint.
    let mut anchor_keys: Vec<(i64, i64)> =
        adj.keys().filter(|k| is_anchor(k, &adj)).copied().collect();
    anchor_keys.sort_unstable();
    for start_key in anchor_keys {
        let Some(start_segs) = adj.get(&start_key) else {
            continue;
        };
        for &first in start_segs {
            if used[first] {
                continue;
            }
            let mut chain: Vec<Point> = Vec::new();
            let mut cur_key = start_key;
            let mut cur_seg = first;
            chain.push(point_of(segs[first], start_key));
            loop {
                used[cur_seg] = true;
                let t = segs[cur_seg];
                let nxt_key = if key(t.start) == cur_key {
                    key(t.end)
                } else {
                    key(t.start)
                };
                chain.push(point_of(t, nxt_key));
                if is_anchor(&nxt_key, &adj) {
                    break;
                }
                // Degree exactly 2: continue on the other segment.
                let Some(cands) = adj.get(&nxt_key) else {
                    break;
                };
                let Some(&next_seg) = cands.iter().find(|&&s| s != cur_seg && !used[s]) else {
                    break;
                };
                cur_key = nxt_key;
                cur_seg = next_seg;
            }
            if chain.len() >= 2 {
                chains.push((chain, segs[first].width));
            }
        }
    }
    chains
}

/// The endpoint of `t` whose key is `k`.
fn point_of(t: &Trace, k: (i64, i64)) -> Point {
    if key(t.start) == k {
        t.start
    } else {
        t.end
    }
}

/// Greedy rubber-band contraction: repeatedly replace the longest
/// clear-line-of-sight sub-path with a straight segment.
pub(crate) fn string_pull(pts: &[P2], obs: &Obstacles, hw: f64, clr: f64) -> Vec<P2> {
    if pts.len() <= 2 {
        return pts.to_vec();
    }
    let mut out: Vec<P2> = Vec::with_capacity(pts.len());
    let mut i = 0usize;
    out.push(pts[0]);
    while i + 1 < pts.len() {
        // Farthest j > i with a clear straight shot from i.
        let mut j = i + 1;
        for cand in ((i + 2)..pts.len()).rev() {
            if obs.polyline_clear(&[pts[i], pts[cand]], hw, clr) {
                j = cand;
                break;
            }
        }
        out.push(pts[j]);
        i = j;
    }
    out
}

/// Replace polyline corners with tangent arcs where a clear arc fits.
fn fillet(pts: &[P2], obs: &Obstacles, hw: f64, clr: f64, opts: &OrganicOptions) -> Vec<P2> {
    if pts.len() < 3 {
        return pts.to_vec();
    }
    let mut out: Vec<P2> = Vec::with_capacity(pts.len() * 4);
    out.push(pts[0]);
    for k in 1..pts.len() - 1 {
        let a = *out.last().unwrap();
        let b = pts[k];
        let c = pts[k + 1];
        let v1 = sub(a, b);
        let v2 = sub(c, b);
        let (l1, l2) = (norm(v1), norm(v2));
        if l1 < 1e-9 || l2 < 1e-9 {
            continue;
        }
        let u1 = [v1[0] / l1, v1[1] / l1];
        let u2 = [v2[0] / l2, v2[1] / l2];
        let cosang = dot(u1, u2).clamp(-1.0, 1.0);
        let ang = cosang.acos(); // corner interior angle
                                 // Nearly straight (or degenerate reversal): keep the corner.
        if !(0.05..=std::f64::consts::PI - 0.05).contains(&ang) {
            out.push(b);
            continue;
        }
        // Tangent offset t from the corner along both legs; keep less
        // than half of either leg so consecutive fillets never overlap.
        let half = ang / 2.0;
        let mut r = opts.max_fillet_radius_mm;
        let mut placed = false;
        for _ in 0..3 {
            let t_full = r / half.tan();
            let t = t_full.min(0.45 * l1).min(0.45 * l2);
            let r_eff = t * half.tan();
            if r_eff < 0.05 {
                break; // corner too tight to be worth an arc
            }
            let p1 = [b[0] + u1[0] * t, b[1] + u1[1] * t];
            let p2 = [b[0] + u2[0] * t, b[1] + u2[1] * t];
            // Arc centre along the bisector.
            let bis = [u1[0] + u2[0], u1[1] + u2[1]];
            let bl = norm(bis);
            if bl < 1e-9 {
                break;
            }
            let centre = [
                b[0] + bis[0] / bl * (r_eff / half.sin()),
                b[1] + bis[1] / bl * (r_eff / half.sin()),
            ];
            let arc = sample_arc(centre, p1, p2, r_eff, opts.chord_tol_mm);
            if obs.polyline_clear(&arc, hw, clr) {
                out.extend_from_slice(&arc);
                placed = true;
                break;
            }
            r /= 2.0;
        }
        if !placed {
            out.push(b);
        }
    }
    out.push(pts[pts.len() - 1]);
    // Drop consecutive duplicates the construction can produce.
    out.dedup_by(|a, b| dist(*a, *b) < 1e-6);
    out
}

/// Points along the arc of radius `r` centred at `c` from `p1` to `p2`
/// (the short way), endpoints included.
fn sample_arc(c: P2, p1: P2, p2: P2, r: f64, chord_tol: f64) -> Vec<P2> {
    let a1 = (p1[1] - c[1]).atan2(p1[0] - c[0]);
    let a2 = (p2[1] - c[1]).atan2(p2[0] - c[0]);
    let mut sweep = a2 - a1;
    while sweep > std::f64::consts::PI {
        sweep -= 2.0 * std::f64::consts::PI;
    }
    while sweep < -std::f64::consts::PI {
        sweep += 2.0 * std::f64::consts::PI;
    }
    // Chord tolerance → max step angle.
    let max_step = 2.0 * (1.0 - (chord_tol / r).min(0.5)).acos().max(0.05);
    let steps = ((sweep.abs() / max_step).ceil() as usize).clamp(1, 64);
    let mut out = Vec::with_capacity(steps + 1);
    for s in 0..=steps {
        let a = a1 + sweep * (s as f64 / steps as f64);
        out.push([c[0] + r * a.cos(), c[1] + r * a.sin()]);
    }
    out
}

/// Swap a chain's old segments for the smoothed polyline. Endpoints are
/// identical by construction, so connectivity is untouched.
fn replace_chain(
    board: &mut Board,
    net: &str,
    layer: CopperLayer,
    old_chain: &[Point],
    new_pts: &[P2],
    width: Length,
) {
    // Remove the exact old segments (by endpoint pairs, order-agnostic).
    let mut old_pairs: std::collections::HashSet<((i64, i64), (i64, i64))> =
        std::collections::HashSet::new();
    for w in old_chain.windows(2) {
        let (a, b) = (key(w[0]), key(w[1]));
        old_pairs.insert((a.min(b), a.max(b)));
    }
    board.traces.retain(|t| {
        if t.net != net || t.layer != layer {
            return true;
        }
        let (a, b) = (key(t.start), key(t.end));
        !old_pairs.contains(&(a.min(b), a.max(b)))
    });
    // Insert the new geometry, preserving the EXACT original endpoints
    // (mm round-trip must not perturb junction matching).
    let n = new_pts.len();
    for (i, w) in new_pts.windows(2).enumerate() {
        let start = if i == 0 { old_chain[0] } else { to_point(w[0]) };
        let end = if i + 2 == n {
            *old_chain.last().unwrap()
        } else {
            to_point(w[1])
        };
        if key(start) == key(end) {
            continue;
        }
        board.traces.push(Trace {
            id: pcb_core::Id::new(),
            layer,
            start,
            end,
            width,
            net: net.to_string(),
        });
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn obs_none(outline_mm: f64) -> Obstacles {
        Obstacles {
            items: Vec::new(),
            areas: Vec::new(),
            defaults: pcb_core::RuleDefaults::default(),
            layer: CopperLayer::Top,
            worst_area_mm: 0.0,
            outline: pcb_core::Rect {
                min: Point::new(Length::from_mm(-1.0), Length::from_mm(-1.0)),
                max: Point::new(
                    Length::from_mm(outline_mm + 1.0),
                    Length::from_mm(outline_mm + 1.0),
                ),
            },
            edge_mm: 0.0,
        }
    }

    /// A staircase with line-of-sight collapses to one segment.
    #[test]
    fn string_pull_collapses_staircase() {
        let pts = vec![
            [1.0, 1.0],
            [2.0, 1.0],
            [2.0, 2.0],
            [3.0, 2.0],
            [3.0, 3.0],
            [4.0, 3.0],
        ];
        let out = string_pull(&pts, &obs_none(10.0), 0.125, 0.2);
        assert_eq!(out.len(), 2, "clear staircase must collapse: {out:?}");
        assert_eq!(out[0], [1.0, 1.0]);
        assert_eq!(out[1], [4.0, 3.0]);
    }

    /// An obstacle in the line of sight keeps the detour point.
    #[test]
    fn string_pull_respects_obstacles() {
        let pts = vec![[1.0, 1.0], [5.0, 4.5], [9.0, 1.0]];
        let mut obs = obs_none(10.0);
        // Block the straight shot y=1 between the endpoints.
        obs.items.push(Obstacle {
            shape: Shape::Rect {
                min: [4.0, 0.0],
                max: [6.0, 2.0],
            },
            clearance_mm: 0.2,
            net: None,
        });
        let out = string_pull(&pts, &obs, 0.125, 0.2);
        assert_eq!(out.len(), 3, "blocked path must keep its bend: {out:?}");
    }

    /// Filleting a right angle inserts an arc and shortens the path,
    /// and every arc point keeps clearance.
    #[test]
    fn fillet_rounds_a_right_angle() {
        let pts = vec![[1.0, 1.0], [5.0, 1.0], [5.0, 5.0]];
        let opts = OrganicOptions::default();
        let out = fillet(&pts, &obs_none(10.0), 0.125, 0.2, &opts);
        assert!(out.len() > 3, "arc points expected: {}", out.len());
        assert!(
            polyline_len(&out) < polyline_len(&pts) - 0.5,
            "fillet should shorten the corner: {:.3} vs {:.3}",
            polyline_len(&out),
            polyline_len(&pts)
        );
        // Endpoints preserved exactly.
        assert_eq!(out[0], [1.0, 1.0]);
        assert_eq!(*out.last().unwrap(), [5.0, 5.0]);
    }

    /// Segment-rect distance sanity.
    #[test]
    fn seg_rect_distance_basics() {
        // Passing above the rect at y=3, rect top at y=2 → distance 1.
        let d = seg_rect_dist([0.0, 3.0], [10.0, 3.0], [4.0, 1.0], [6.0, 2.0]);
        assert!((d - 1.0).abs() < 1e-9, "got {d}");
        // Piercing the rect → 0.
        let d = seg_rect_dist([0.0, 1.5], [10.0, 1.5], [4.0, 1.0], [6.0, 2.0]);
        assert_eq!(d, 0.0);
    }

    /// v8 regression: the pass must not FATTEN the copper it smooths.
    ///
    /// A fine-pitch escape stub is laid narrower than its net's class
    /// width on purpose — that is the only way copper leaves a 0.20 mm
    /// pad at 0.4 mm pitch. `replace_chain` used to rewrite every chain
    /// at the class width, so a stub sitting exactly on a rule area's
    /// 0.12 mm line lost 0.05 mm of clearance, and nothing re-checked
    /// it: `string_pull`/`fillet` hand back their input untouched when
    /// they find no improvement, so the widened geometry was committed
    /// without ever being tested against anything.
    #[test]
    fn smoothing_never_widens_a_chain_into_a_violation() {
        use pcb_core::{Footprint, Id, Pad, Rect, RuleArea};

        let mut board = Board::new();
        board.outline = Some(Rect::from_corners(
            Point::new(Length::from_mm(0.0), Length::from_mm(0.0)),
            Point::new(Length::from_mm(12.0), Length::from_mm(12.0)),
        ));
        // The whole board is a tight-clearance zone, as a fine-pitch
        // escape pocket is.
        let mut area = RuleArea::new(
            "fine",
            Rect::from_corners(
                Point::new(Length::from_mm(0.0), Length::from_mm(0.0)),
                Point::new(Length::from_mm(12.0), Length::from_mm(12.0)),
            ),
        );
        area.clearance_mm = Some(0.12);
        board.rule_areas.push(area);

        // Foreign copper: a 0.2 x 4.0 mm bar of net B at x = 5.9..6.1.
        board.add_footprint(Footprint {
            id: Id::new(),
            reference: "U1".into(),
            value: String::new(),
            library: "test".into(),
            position: Point::new(Length::from_mm(6.0), Length::from_mm(6.0)),
            rotation: 0.0,
            layer: CopperLayer::Top,
            pads: vec![Pad {
                number: "1".into(),
                name: String::new(),
                offset: Point::new(Length::from_mm(0.0), Length::from_mm(0.0)),
                size: (Length::from_mm(0.2), Length::from_mm(4.0)),
                layer: CopperLayer::Top,
                net: Some("B".into()),
                drill: None,
            }],
            key: String::new(),
            description: String::new(),
            edge_mounted: false,
            edge_side: None,
            silk: Vec::new(),
        });
        // Net A: a 0.15 mm stub 0.20 mm off the bar's edge — legal at the
        // area's 0.12 mm rule (0.20 - 0.075 = 0.125 mm of gap), illegal
        // the moment anything rewrites it at the 0.25 mm class width.
        board.add_trace(Trace {
            id: pcb_core::Id::new(),
            layer: CopperLayer::Top,
            start: Point::new(Length::from_mm(6.30), Length::from_mm(4.5)),
            end: Point::new(Length::from_mm(6.30), Length::from_mm(7.5)),
            width: Length::from_mm(0.15),
            net: "A".into(),
        });

        let route_opts = RouteOptions {
            cell: Length::from_mm(0.20),
            trace_width: Length::from_mm(0.25),
            clearance: Length::from_mm(0.20),
            organic: true,
            ..RouteOptions::default()
        };
        organic_pass(
            &mut board,
            &OrganicOptions::default(),
            &route_opts,
            crate::router::effective_net_rules,
        );

        for t in board.traces.iter().filter(|t| t.net == "A") {
            assert_eq!(
                t.width.to_mm(),
                0.15,
                "smoothing rewrote a 0.15 mm stub at {:.3} mm",
                t.width.to_mm()
            );
        }
        let drc = pcb_drc::run(&board, &pcb_drc::DrcOptions::default());
        let bad: Vec<&str> = drc
            .violations
            .iter()
            .filter(|v| v.severity == pcb_drc::Severity::Error)
            .filter(|v| {
                matches!(
                    v.kind,
                    pcb_drc::ViolationKind::TraceTraceClearance
                        | pcb_drc::ViolationKind::TracePadClearance
                )
            })
            .map(|v| v.message.as_str())
            .collect();
        assert!(
            bad.is_empty(),
            "organic pass committed copper DRC rejects: {bad:?}"
        );
    }
}
