//! Fanout pre-pass: escape dense fine-pitch pads to the inner layers
//! with a via-in-pad, so the rest of the router can pick the net up on a
//! layer that actually has room.
//!
//! Why this exists: a 0.5 mm-pitch part (USB-C, QFN) leaves a ~0.2 mm gap
//! between adjacent pads. A routed trace plus honest clearance needs far
//! more than that, so parallel surface escape is geometrically
//! impossible. Real boards solve it the same way this pass does — drop a
//! small via *inside* the pad (a "via-in-pad" / POFV) down to an inner
//! layer where the pins can spread out. A 0.30 mm / 0.15 mm via (JLCPCB's
//! minimum) sits centred on a 0.5 mm-pitch pad and still keeps a full
//! 0.20 mm to the neighbouring pad, so the result passes DRC.
//!
//! The pass only fans a pad out when it genuinely cannot escape on its
//! own layer — ordinary 2-pin passives keep routing on the surface and
//! never grow a needless via.

use std::collections::{HashMap, HashSet};

use pcb_core::{Board, CopperLayer, Id, Length, Point, Trace, Via};

use crate::router::RouteOptions;

/// JLCPCB minimum via — 0.30 mm pad, 0.15 mm drill. Small enough to sit
/// centred in a 0.30 mm-wide, 0.5 mm-pitch pad and still clear the
/// neighbour by the default 0.20 mm.
pub(crate) const FANOUT_VIA_DIAMETER_MM: f64 = 0.30;
pub(crate) const FANOUT_VIA_DRILL_MM: f64 = 0.15;

/// Board-edge copper clearance enforced by the DRC (`EdgeClearance`),
/// stricter than inter-copper clearance. A staggered via must respect it
/// or the slid-out position trips the edge rule even though it clears
/// every pad.
pub(crate) const EDGE_CLEARANCE_MM: f64 = 0.30;

/// How far (mm) a trace must be able to run away from a pad edge, in some
/// direction, for the pad to count as "able to escape on the surface".
/// About two trace pitches — enough to clear the neighbouring pads.
const ESCAPE_LEN_MM: f64 = 0.9;

/// A pad flanked by at least this many foreign-net pads within
/// `CLUSTER_DIST_MM` is in a fine-pitch cluster (USB-C row, QFN edge).
/// Even though *one* trace can slip out along its long axis, the whole
/// row can't escape in parallel at sub-0.65 mm pitch, so every clustered
/// pad gets a fanout via and routes on an inner layer instead.
pub(crate) const CLUSTER_NEIGHBOURS: usize = 2;
pub(crate) const CLUSTER_DIST_MM: f64 = 0.55;

/// How many distinct depth "ranks" a row of dogbones staggers across.
/// Consecutive fine-pitch pins on one package side get depths
/// `base + (rank % DOGBONE_RANKS) * step`, so neighbouring barrels never
/// sit at the same distance from the pad row and each pin keeps a clear
/// approach lane. Three ranks is enough to break up a 0.4 mm-pitch row
/// (0.4 mm lateral pitch vs a 0.5 mm barrel-to-barrel requirement) while
/// keeping the deepest via well clear of the package's thermal pad.
const DOGBONE_RANKS: usize = 3;

/// Candidate widths (mm) for the pad → dogbone-via stub, widest first.
/// The stub is real copper, so it must clear the neighbouring pins; on a
/// 0.4 mm-pitch QFN only the narrow end of this list survives validation.
/// Everything here is ≥ the DRC's 0.1 mm minimum trace width.
const STUB_WIDTHS_MM: [f64; 4] = [0.25, 0.20, 0.15, 0.12];

/// Result of the fanout pass.
#[derive(Debug, Default, Clone)]
pub struct FanoutPlan {
    /// Vias to add to the board (one per fanned-out pad).
    pub vias: Vec<Via>,
    /// Short pad → via copper stubs for the dogbone escapes. A dogbone
    /// via sits OUTSIDE its pad, so without this copper the pad is not
    /// actually connected to the barrel the router lands on — DRC would
    /// (correctly) report the pad as unconnected. Laid on the pad's own
    /// layer and validated against real geometry before being emitted.
    pub stubs: Vec<Trace>,
    /// `"ref.num"` of every pad that got a DOGBONE via (barrel outside
    /// the pad) rather than a true via-in-pad. Reported so agents can see
    /// how much of the escape depends on the dogbone path.
    pub dogbone_pads: HashSet<String>,
    /// `"ref.num"` of every pad that was fanned out (matches
    /// `NetPadInfo::pad_ref`). The grid stamps these as through-hole so
    /// the search can land on them from any layer (the via already ties
    /// the surface pad to the inner copper).
    pub through_pads: HashSet<String>,
    /// `"ref.num"` → the via-in-pad world position. The via may be slid
    /// along the pad's long axis away from the pad centre (staggering),
    /// so the router must aim the inner-layer pickup at the VIA, not the
    /// pad centre — that's the only point where the inner copper exists.
    pub via_positions: HashMap<String, Point>,
}

/// Axis-aligned pad rectangle in world mm.
pub(crate) struct PadRect {
    pub cx: f64,
    pub cy: f64,
    pub hw: f64,
    pub hh: f64,
    pub net: Option<String>,
}

pub(crate) fn pad_rects(board: &Board) -> Vec<PadRect> {
    let mut out = Vec::new();
    for fp in board.footprints_in_order() {
        for pad in &fp.pads {
            let c = fp.pad_world_center(pad);
            let (w, h) = fp.pad_world_size(pad);
            out.push(PadRect {
                cx: c.x.to_mm(),
                cy: c.y.to_mm(),
                hw: w.to_mm() / 2.0,
                hh: h.to_mm() / 2.0,
                net: pad.net.clone(),
            });
        }
    }
    out
}

/// Distance (mm) from point to an axis-aligned rectangle (0 inside).
pub(crate) fn point_rect_dist(px: f64, py: f64, r: &PadRect) -> f64 {
    let dx = (px - r.cx).abs() - r.hw;
    let dy = (py - r.cy).abs() - r.hh;
    let dx = dx.max(0.0);
    let dy = dy.max(0.0);
    (dx * dx + dy * dy).sqrt()
}

/// Distance (mm) from a point to a segment.
fn point_seg_dist(px: f64, py: f64, ax: f64, ay: f64, bx: f64, by: f64) -> f64 {
    let (dx, dy) = (bx - ax, by - ay);
    let len2 = dx * dx + dy * dy;
    let t = if len2 < 1e-12 {
        0.0
    } else {
        (((px - ax) * dx + (py - ay) * dy) / len2).clamp(0.0, 1.0)
    };
    let (qx, qy) = (ax + dx * t, ay + dy * t);
    ((px - qx).powi(2) + (py - qy).powi(2)).sqrt()
}

/// Distance (mm) from a segment to an axis-aligned rectangle (0 when they
/// intersect). Sampling-free: the minimum over the rect's four edges plus
/// an inside test for the segment endpoints.
fn seg_rect_dist(ax: f64, ay: f64, bx: f64, by: f64, r: &PadRect) -> f64 {
    let (x0, y0) = (r.cx - r.hw, r.cy - r.hh);
    let (x1, y1) = (r.cx + r.hw, r.cy + r.hh);
    let inside = |x: f64, y: f64| x >= x0 && x <= x1 && y >= y0 && y <= y1;
    if inside(ax, ay) || inside(bx, by) {
        return 0.0;
    }
    let corners = [(x0, y0), (x1, y0), (x1, y1), (x0, y1)];
    let mut best = f64::INFINITY;
    for i in 0..4 {
        let (cx0, cy0) = corners[i];
        let (cx1, cy1) = corners[(i + 1) % 4];
        // Segment-to-segment distance = min of the four point-to-segment
        // distances when the segments do not cross; a crossing means one
        // endpoint of one lies inside the other's span, which the
        // point-to-segment minima already drive to ~0.
        best = best
            .min(point_seg_dist(cx0, cy0, ax, ay, bx, by))
            .min(point_seg_dist(cx1, cy1, ax, ay, bx, by))
            .min(point_seg_dist(ax, ay, cx0, cy0, cx1, cy1))
            .min(point_seg_dist(bx, by, cx0, cy0, cx1, cy1));
    }
    best
}

/// Can a trace of width `tw` leave this pad on its own layer in *some*
/// direction without coming within `clearance` of a foreign-net pad for
/// `ESCAPE_LEN_MM`? We probe the four cardinal and four diagonal
/// directions from the pad centre.
pub(crate) fn can_escape_surface(
    cx: f64,
    cy: f64,
    hw: f64,
    hh: f64,
    net: &str,
    foreign: &[&PadRect],
    tw: f64,
    clearance: f64,
) -> bool {
    let need = tw / 2.0 + clearance;
    let dirs = [
        (1.0, 0.0),
        (-1.0, 0.0),
        (0.0, 1.0),
        (0.0, -1.0),
        (0.707, 0.707),
        (0.707, -0.707),
        (-0.707, 0.707),
        (-0.707, -0.707),
    ];
    let start = hw.max(hh); // step out past the pad body
    for (dx, dy) in dirs {
        let mut blocked = false;
        // Sample along the escape ray.
        let mut d = start;
        let end = start + ESCAPE_LEN_MM;
        while d <= end {
            let px = cx + dx * d;
            let py = cy + dy * d;
            for r in foreign {
                if r.net.as_deref() == Some(net) {
                    continue;
                }
                if point_rect_dist(px, py, r) < need {
                    blocked = true;
                    break;
                }
            }
            if blocked {
                break;
            }
            d += 0.1;
        }
        if !blocked {
            return true;
        }
    }
    false
}

/// Would a fanout via at `(cx,cy)` on `net` keep `clearance` to every
/// foreign-net pad and to every existing via? (Same-net pad is the pad
/// we sit in — that's the point.)
pub(crate) fn fanout_via_fits(
    cx: f64,
    cy: f64,
    net: &str,
    via_r: f64,
    clearance: f64,
    foreign: &[&PadRect],
    board: &Board,
) -> bool {
    let need = via_r + clearance;
    for r in foreign {
        if r.net.as_deref() == Some(net) {
            continue;
        }
        if point_rect_dist(cx, cy, r) < need - 1e-9 {
            return false;
        }
    }
    for v in &board.vias {
        let dx = cx - v.position.x.to_mm();
        let dy = cy - v.position.y.to_mm();
        let other_r = v.diameter.to_mm() / 2.0;
        if (dx * dx + dy * dy).sqrt() < via_r + other_r + clearance - 1e-9 {
            return false;
        }
    }
    // Keep the via inside the board, clear of the edge. Use the DRC's
    // board-edge clearance (0.30 mm), not the inter-copper `clearance`:
    // the EdgeClearance rule is stricter than copper-to-copper, and a via
    // slid toward a pad end can otherwise pass the copper check yet still
    // fail edge clearance.
    if let Some(o) = board.outline {
        let edge = (cx - o.min.x.to_mm())
            .min(o.max.x.to_mm() - cx)
            .min(cy - o.min.y.to_mm())
            .min(o.max.y.to_mm() - cy);
        if edge < via_r + EDGE_CLEARANCE_MM {
            return false;
        }
    }
    true
}

/// Choose where inside a pad to drop its fanout via. Slides the via along
/// the pad's LONG axis and returns the candidate world position that (a)
/// geometrically fits (`fanout_via_fits`) and (b) is the most isolated from
/// the fanout vias already placed — staggering adjacent fine-pitch pins to
/// opposite pad ends so their through-barrels stop blocking each other's
/// escape lanes. Returns `None` when no offset fits at all.
#[allow(clippy::too_many_arguments)]
pub(crate) fn pick_via_position(
    cx: f64,
    cy: f64,
    hw: f64,
    hh: f64,
    fp_cx: f64,
    fp_cy: f64,
    net: &str,
    via_r: f64,
    clearance: f64,
    foreign: &[&PadRect],
    work: &Board,
) -> Option<(f64, f64)> {
    // Long axis + how far the via centre may slide while staying fully
    // inside the pad copper (a via-in-pad must sit on its own pad).
    let (dx, dy, half_len) = if hw >= hh {
        (1.0, 0.0, hw)
    } else {
        (0.0, 1.0, hh)
    };
    let max_off = (half_len - via_r).max(0.0);
    // Slide along the long axis, biased toward the footprint INTERIOR (its
    // central channel) but allowed toward the outer end too. A row of N
    // consecutive fine-pitch escapes needs N distinct long-axis offsets to
    // keep every pin's approach lane clear (a neighbour barrel at the same
    // offset boxes the pin on every layer, since the barrel shorts them
    // all); two offsets aren't enough for 3+ in a row. The interior
    // direction is the sign of (fp_centre − pad) along the long axis; we
    // try interior offsets first (so the bias wins ties) then the outer
    // ones. `fanout_via_fits` rejects any position that nears the board
    // edge or a shield pad, so the outer candidates are self-policing.
    let along = dx * (fp_cx - cx) + dy * (fp_cy - cy);
    let inner = if along >= 0.0 { 1.0 } else { -1.0 };
    let candidate_offsets = [
        0.0,
        inner * max_off,
        inner * 0.5 * max_off,
        -inner * max_off,
        inner * 0.75 * max_off,
        -inner * 0.5 * max_off,
        inner * 0.25 * max_off,
    ];
    // Existing fanout vias (the 0.30 mm ones this pass places) — score
    // isolation against these so a row of pins spreads out.
    let fanout_via_centers: Vec<(f64, f64)> = work
        .vias
        .iter()
        .filter(|v| (v.diameter.to_mm() - FANOUT_VIA_DIAMETER_MM).abs() < 1e-6)
        .map(|v| (v.position.x.to_mm(), v.position.y.to_mm()))
        .collect();

    let mut best: Option<(f64, f64, f64, f64)> = None; // (score, |off|, vx, vy)
    for off in candidate_offsets {
        if off.abs() > max_off + 1e-9 {
            continue;
        }
        let vx = cx + dx * off;
        let vy = cy + dy * off;
        if !fanout_via_fits(vx, vy, net, via_r, clearance, foreign, work) {
            continue;
        }
        let score = fanout_via_centers
            .iter()
            .map(|(ox, oy)| ((vx - ox).powi(2) + (vy - oy).powi(2)).sqrt())
            .fold(f64::INFINITY, f64::min);
        // Prefer the most isolated spot; tie-break toward the pad centre
        // (smaller |offset|) so isolated pads stay centred.
        let take = match best {
            None => true,
            Some((bscore, boff, _, _)) => {
                score > bscore + 1e-9 || (score >= bscore - 1e-9 && off.abs() < boff - 1e-9)
            }
        };
        if take {
            best = Some((score, off.abs(), vx, vy));
        }
    }
    best.map(|(_, _, vx, vy)| (vx, vy))
}

/// Plan the fanout: for every pad that can't escape on the surface, drop
/// a via-in-pad (or dogbone fallback via the escape pass) if it fits.
///
/// On 2-layer boards the "other" layer is Bottom — vias still open a
/// corridor under the package body. Via-in-pad only lands when the pad is
/// large enough to host the 0.30 mm barrel; finer pads rely on the
/// localized escape stubs in `escape.rs`.
pub fn plan_fanout(board: &Board, opts: &RouteOptions) -> FanoutPlan {
    let mut plan = FanoutPlan::default();
    if board.stackup.layer_count() < 2 {
        return plan;
    }
    let rects = pad_rects(board);
    let foreign: Vec<&PadRect> = rects.iter().collect();
    let tw = opts.trace_width.to_mm();
    let clearance = opts.clearance.to_mm();
    let via_r = FANOUT_VIA_DIAMETER_MM / 2.0;

    // Mutable copy of board vias so successively-placed fanout vias also
    // respect each other's spacing.
    let mut work = board.clone();

    for fp in board.footprints_in_order() {
        // Footprint centre (pad centroid) — the interior reference the via
        // staggering slides toward, so escapes head into the part's central
        // channel rather than out toward the board edge / shield pads.
        let (mut sum_x, mut sum_y, mut n) = (0.0_f64, 0.0_f64, 0.0_f64);
        for pad in &fp.pads {
            let c = fp.pad_world_center(pad);
            sum_x += c.x.to_mm();
            sum_y += c.y.to_mm();
            n += 1.0;
        }
        let (fp_cx, fp_cy) = if n > 0.0 {
            (sum_x / n, sum_y / n)
        } else {
            (0.0, 0.0)
        };
        // Rank every pad along its package SIDE. Pads sharing an inward
        // axis form one side; sorting them by the coordinate that runs
        // ALONG that side gives each pin a stable index, which is what the
        // dogbone depth stagger keys off. Without it a whole 0.4 mm-pitch
        // row aims at one depth and mutual via clearance rejects most of
        // the row.
        let mut sides: HashMap<(i8, i8), Vec<(f64, String)>> = HashMap::new();
        for pad in &fp.pads {
            let c = fp.pad_world_center(pad);
            let (w, h) = fp.pad_world_size(pad);
            let (cx, cy) = (c.x.to_mm(), c.y.to_mm());
            let (dx, dy) = inward_axis(cx, cy, w.to_mm() / 2.0, h.to_mm() / 2.0, fp_cx, fp_cy);
            let along = if dx.abs() > 0.0 { cy } else { cx };
            sides
                .entry((dx as i8, dy as i8))
                .or_default()
                .push((along, pad.number.clone()));
        }
        let mut pad_rank: HashMap<String, usize> = HashMap::new();
        for group in sides.values_mut() {
            group.sort_by(|a, b| a.0.partial_cmp(&b.0).unwrap_or(std::cmp::Ordering::Equal));
            for (i, (_, num)) in group.iter().enumerate() {
                pad_rank.insert(num.clone(), i);
            }
        }
        for pad in &fp.pads {
            let Some(net) = pad.net.as_deref() else {
                continue;
            };
            // Through-hole pads already reach every layer — no fanout.
            if pad.drill.is_some() {
                continue;
            }
            let c = fp.pad_world_center(pad);
            let (w, h) = fp.pad_world_size(pad);
            let (cx, cy) = (c.x.to_mm(), c.y.to_mm());
            let (hw, hh) = (w.to_mm() / 2.0, h.to_mm() / 2.0);
            // Count foreign-net pads crowding this one.
            let neighbours = foreign
                .iter()
                .filter(|r| r.net.as_deref() != Some(net))
                .filter(|r| point_rect_dist(cx, cy, r) < CLUSTER_DIST_MM)
                .count();
            let in_cluster = neighbours >= CLUSTER_NEIGHBOURS;
            // Fan out if the pad is in a fine-pitch cluster (parallel
            // escape impossible) OR simply can't escape in any direction.
            if !in_cluster && can_escape_surface(cx, cy, hw, hh, net, &foreign, tw, clearance) {
                continue;
            }
            // Pick the via-in-pad position. A via-in-pad may sit anywhere
            // inside the pad copper, so on a LONG fine-pitch pad we slide it
            // along the pad's long axis to the spot that is most ISOLATED
            // from the via-in-pads already placed on neighbouring pins.
            // Centring every connector via leaves them all at one x (0.5 mm
            // apart in y) — their through-barrels then block every layer, so
            // the coarse router has no lane to approach any of them and the
            // pins fail to land. Staggering adjacent pins to opposite ends of
            // their pads opens a clear approach lane down the pad's long axis.
            let vip = pick_via_position(
                cx, cy, hw, hh, fp_cx, fp_cy, net, via_r, clearance, &foreign, &work,
            );
            // Dogbone fallback: pad too small for a true via-in-pad
            // (typical 0.4 mm-pitch QFN pads are ~0.20 mm wide — a
            // 0.30 mm barrel does not fit). Place the via just past the
            // pad tip along the pad's inward axis, and lay the copper
            // stub that actually ties the pad to that barrel.
            let mut stub: Option<Trace> = None;
            let via_xy = match vip {
                Some(p) => Some(p),
                None => {
                    let (dirx, diry) = inward_axis(cx, cy, hw, hh, fp_cx, fp_cy);
                    let rank = pad_rank.get(&pad.number).copied().unwrap_or(0);
                    match dogbone_via_position(
                        cx, cy, hw, hh, dirx, diry, rank, net, via_r, clearance, &foreign, &work,
                    ) {
                        // A dogbone barrel the pad cannot reach with legal
                        // copper is worse than no dogbone at all: the coarse
                        // router would treat the pad as escaped while DRC
                        // (rightly) reports it unconnected. Only keep the
                        // via when its stub is layable.
                        Some((vx, vy)) => dogbone_stub(
                            pad.layer,
                            cx,
                            cy,
                            vx,
                            vy,
                            net,
                            tw.min(2.0 * hw.min(hh)),
                            clearance,
                            &foreign,
                            &work,
                        )
                        .map(|t| {
                            stub = Some(t);
                            (vx, vy)
                        }),
                        None => None,
                    }
                }
            };
            let Some((vx, vy)) = via_xy else {
                // No VIP and no dogbone fits — leave it for the surface
                // router (it will likely fail; the report will flag it).
                continue;
            };
            let via_pos = Point::new(Length::from_mm(vx), Length::from_mm(vy));
            let via = Via {
                id: Id::new(),
                position: via_pos,
                drill: Length::from_mm(FANOUT_VIA_DRILL_MM),
                diameter: Length::from_mm(FANOUT_VIA_DIAMETER_MM),
                net: net.to_string(),
            };
            work.vias.push(via.clone());
            plan.vias.push(via);
            let key = format!("{}.{}", fp.reference, pad.number);
            if let Some(t) = stub {
                work.traces.push(t.clone());
                plan.stubs.push(t);
                plan.dogbone_pads.insert(key.clone());
            }
            plan.through_pads.insert(key.clone());
            plan.via_positions.insert(key, via_pos);
        }
    }
    plan
}

/// The inward escape axis for a fine-pitch pad: the pad's own LONG axis,
/// signed toward the footprint centroid. Using the pad axis (rather than
/// the raw pad→centroid vector) keeps the dogbone — and the copper stub
/// that feeds it — inside the pad's own lateral lane, which is the only
/// corridor that clears the neighbouring pins on a 0.4 mm-pitch row.
pub(crate) fn inward_axis(
    cx: f64,
    cy: f64,
    hw: f64,
    hh: f64,
    fp_cx: f64,
    fp_cy: f64,
) -> (f64, f64) {
    if hw >= hh {
        (if fp_cx >= cx { 1.0 } else { -1.0 }, 0.0)
    } else {
        (0.0, if fp_cy >= cy { 1.0 } else { -1.0 })
    }
}

/// Place a via just outside a fine-pitch pad, along the pad's inward axis
/// (dogbone fanout). Used when the pad copper is too small to host a
/// via-in-pad barrel.
///
/// `rank` is the pad's index along its package side. Depths are staggered
/// as `base + (rank % DOGBONE_RANKS) * step`, so a run of consecutive pins
/// drops its barrels at alternating distances from the row instead of all
/// piling onto one arc — at 0.4 mm pitch a single-depth row cannot even
/// satisfy via-to-via clearance, so without the stagger most pins get no
/// escape at all. Returns `None` if no candidate clears neighbours.
#[allow(clippy::too_many_arguments)]
pub(crate) fn dogbone_via_position(
    cx: f64,
    cy: f64,
    hw: f64,
    hh: f64,
    dirx: f64,
    diry: f64,
    rank: usize,
    net: &str,
    via_r: f64,
    clearance: f64,
    foreign: &[&PadRect],
    work: &Board,
) -> Option<(f64, f64)> {
    let (dx, dy) = if dirx.abs() < 1e-9 && diry.abs() < 1e-9 {
        if hw >= hh {
            (1.0, 0.0)
        } else {
            (0.0, 1.0)
        }
    } else {
        (dirx, diry)
    };
    // Half-extent of the pad along the dogbone direction + via radius +
    // a hair so the barrel sits fully outside the pad copper (DRC would
    // otherwise see via-on-foreign-pad if we clip the edge).
    let pad_extent = (dx.abs() * hw + dy.abs() * hh).max(hw.min(hh));
    let base = pad_extent + via_r + 0.05;
    // One rank step must separate two barrels end-to-end.
    let step = 2.0 * via_r + clearance;
    let slot = rank % DOGBONE_RANKS;
    // Preferred depth first (this pad's rank), then the other rank slots,
    // then fine adjustments — a blocked first choice still has options but
    // the staggered layout is what we aim for.
    let mut depths: Vec<f64> = Vec::with_capacity(DOGBONE_RANKS + 4);
    depths.push(base + slot as f64 * step);
    for k in 0..DOGBONE_RANKS {
        if k != slot {
            depths.push(base + k as f64 * step);
        }
    }
    depths.extend([
        base + DOGBONE_RANKS as f64 * step,
        base + slot as f64 * step + 0.5 * step,
        base + 0.2,
        base - 0.05,
    ]);
    for dist in depths {
        if dist < base - 0.1 {
            continue;
        }
        let vx = cx + dx * dist;
        let vy = cy + dy * dist;
        if fanout_via_fits(vx, vy, net, via_r, clearance, foreign, work) {
            return Some((vx, vy));
        }
    }
    // Last resort: allow a perpendicular nudge. It bends the stub, so it
    // is only tried once the clean on-axis depths are all taken.
    let (px, py) = (-dy, dx);
    for dist in [base, base + step, base + 2.0 * step] {
        for perp_sign in [1.0_f64, -1.0] {
            let nudge = perp_sign * (via_r + clearance * 0.5);
            let vx = cx + dx * dist + px * nudge;
            let vy = cy + dy * dist + py * nudge;
            if fanout_via_fits(vx, vy, net, via_r, clearance, foreign, work) {
                return Some((vx, vy));
            }
        }
    }
    None
}

/// Build the short pad → dogbone-via copper stub, if one fits.
///
/// The stub runs from the pad centre to the via centre on the pad's own
/// layer. It is validated against the true geometry — foreign pads,
/// foreign vias and already-laid stubs — at honest clearance, trying
/// progressively narrower widths. `None` means no legal stub exists, in
/// which case the caller should NOT place the dogbone: a barrel the pad
/// cannot reach is copper that lies to the connectivity report.
#[allow(clippy::too_many_arguments)]
pub(crate) fn dogbone_stub(
    layer: CopperLayer,
    cx: f64,
    cy: f64,
    vx: f64,
    vy: f64,
    net: &str,
    max_width: f64,
    clearance: f64,
    foreign: &[&PadRect],
    work: &Board,
) -> Option<Trace> {
    for &w in &STUB_WIDTHS_MM {
        if w > max_width + 1e-9 {
            continue;
        }
        let half = w / 2.0;
        let need = half + clearance;
        let mut ok = true;
        for r in foreign {
            if r.net.as_deref() == Some(net) {
                continue;
            }
            if seg_rect_dist(cx, cy, vx, vy, r) < need - 1e-9 {
                ok = false;
                break;
            }
        }
        if !ok {
            continue;
        }
        for v in &work.vias {
            if v.net == net {
                continue;
            }
            let d = point_seg_dist(
                v.position.x.to_mm(),
                v.position.y.to_mm(),
                cx,
                cy,
                vx,
                vy,
            );
            if d < half + v.diameter.to_mm() / 2.0 + clearance - 1e-9 {
                ok = false;
                break;
            }
        }
        if !ok {
            continue;
        }
        for t in &work.traces {
            if t.net == net || t.layer != layer {
                continue;
            }
            let (tax, tay) = (t.start.x.to_mm(), t.start.y.to_mm());
            let (tbx, tby) = (t.end.x.to_mm(), t.end.y.to_mm());
            let d = point_seg_dist(tax, tay, cx, cy, vx, vy)
                .min(point_seg_dist(tbx, tby, cx, cy, vx, vy))
                .min(point_seg_dist(cx, cy, tax, tay, tbx, tby))
                .min(point_seg_dist(vx, vy, tax, tay, tbx, tby));
            if d < half + t.width.to_mm() / 2.0 + clearance - 1e-9 {
                ok = false;
                break;
            }
        }
        if !ok {
            continue;
        }
        return Some(Trace {
            id: Id::new(),
            layer,
            start: Point::new(Length::from_mm(cx), Length::from_mm(cy)),
            end: Point::new(Length::from_mm(vx), Length::from_mm(vy)),
            width: Length::from_mm(w),
            net: net.to_string(),
        });
    }
    None
}
