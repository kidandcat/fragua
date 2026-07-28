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

use pcb_core::{Board, CopperLayer, Id, Length, Point, RuleResolver, Trace, Via};

use crate::router::RouteOptions;

/// The rules in force at ONE pad, resolved through the shared
/// `RuleResolver` (see `pcb_core::rules`).
///
/// A fine-pitch escape zone is exactly where a fab-legal design uses a
/// tighter clearance and a smaller barrel, so every geometric test in
/// this pass asks here instead of reading a single global number. When a
/// rule area covers the pad its clearance is ABSOLUTE (same gap to every
/// foreign net); outside one, the gap is the pairwise rule — the
/// strictest of the two nets' classes.
pub(crate) struct PadRules<'a> {
    resolver: Option<&'a RuleResolver<'a>>,
    net: &'a str,
    /// `Some` when a rule area covers this pad: an absolute override.
    fixed: Option<f64>,
    /// Clearance against copper carrying no net / an unknown net.
    base: f64,
    /// Via geometry to use for a barrel dropped at this pad.
    pub via_diameter: f64,
    pub via_drill: f64,
    /// Narrowest copper the pad → via stub may use here.
    pub min_stub_width: f64,
}

impl<'a> PadRules<'a> {
    /// Single global clearance, no areas — what callers with no rule
    /// context (fine-grid escape, tests) get.
    pub(crate) fn uniform(clearance: f64) -> Self {
        Self {
            resolver: None,
            net: "",
            fixed: Some(clearance),
            base: clearance,
            via_diameter: FANOUT_VIA_DIAMETER_MM,
            via_drill: FANOUT_VIA_DRILL_MM,
            min_stub_width: STUB_MIN_WIDTH_MM,
        }
    }

    /// Rules at `at` for a pad on `net`.
    pub(crate) fn at_pad(
        resolver: &'a RuleResolver<'a>,
        net: &'a str,
        at: Point,
        layer: CopperLayer,
        fab: Option<&pcb_core::FabRules>,
    ) -> Self {
        let layer = Some(layer);
        let fixed = resolver.area_clearance(at, layer).map(|l| l.to_mm());
        // Smallest via that is legal here: an area override wins, else
        // the fab's floor (a 0.30/0.15 barrel is below JLCPCB's standard
        // tier, so a board that adopted a preset must not fan out with
        // one), else the historical minimum.
        let via_drill = resolver.area_via_drill(at, layer).map_or_else(
            || FANOUT_VIA_DRILL_MM.max(fab.map_or(0.0, |f| f.min_via_drill_mm)),
            |l| l.to_mm(),
        );
        // The pad must satisfy the fab's annular ring around that drill,
        // not just its headline minimum via diameter — a 0.45 mm pad on a
        // 0.20 mm drill is a 0.125 mm ring, which JLCPCB's own 0.13 mm
        // rule rejects.
        let fab_dia = fab.map_or(0.0, |f| {
            f.min_via_diameter_mm
                .max(via_drill + 2.0 * f.min_annular_ring_mm)
        });
        let via_diameter = resolver
            .area_via_diameter(at, layer)
            .map_or_else(|| FANOUT_VIA_DIAMETER_MM.max(fab_dia), |l| l.to_mm());
        // Narrowest legal stub here: an area override wins, else the
        // fab's floor — a 0.12 mm stub on a board that adopted JLCPCB's
        // 0.127 mm tier is copper the fab would reject.
        let min_stub_width = resolver.area_trace_width(at, layer).map_or_else(
            || STUB_MIN_WIDTH_MM.max(fab.map_or(0.0, |f| f.min_trace_width_mm)),
            |l| l.to_mm(),
        );
        Self {
            resolver: Some(resolver),
            net,
            fixed,
            base: resolver.pair_clearance(Some(net), None).to_mm(),
            via_diameter,
            via_drill,
            min_stub_width,
        }
    }

    /// Required clearance to copper belonging to `other`.
    pub(crate) fn to(&self, other: Option<&str>) -> f64 {
        match (self.fixed, self.resolver) {
            (Some(c), _) => c,
            (None, Some(r)) => r.pair_clearance(Some(self.net), other).to_mm(),
            (None, None) => self.base,
        }
    }

    /// Clearance to use when the other net is not known yet (ray probes,
    /// via-to-via spacing ladders).
    pub(crate) fn base(&self) -> f64 {
        self.fixed.unwrap_or(self.base)
    }
}

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
///
/// Deliberately foreign-net only. Extending it to "any pad of the same
/// footprint" (so a power pin flanked by two more pins of its own rail
/// also escapes) was measured on the RP2040 stress board and is a clear
/// LOSS: 4 extra barrels, 14 → 19 failed nets, 1199 → 984 traces. Escape
/// slots are the scarce resource — spending them on pads that do not
/// strictly need one costs more than it buys.
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
const STUB_WIDTHS_MM: [f64; 5] = [0.25, 0.20, 0.15, 0.12, 0.10];

/// Narrowest stub allowed without a rule area saying otherwise — the
/// DRC's own default minimum trace width.
const STUB_MIN_WIDTH_MM: f64 = 0.12;

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
    /// `"ref.num"` of every pad that NEEDED an escape and could not get
    /// one: no candidate barrel site clears its neighbours, or none of the
    /// sites that do can be reached by a legal copper stub. These pads are
    /// geometrically stranded — the router will fail their nets no matter
    /// how long it searches, so they are reported up front rather than
    /// silently dropped. See `crate::slots`.
    pub stranded_pads: Vec<String>,
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
pub(crate) fn point_seg_dist(px: f64, py: f64, ax: f64, ay: f64, bx: f64, by: f64) -> f64 {
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

/// The gap a pad's surface escape needs before the planner believes it.
///
/// Exact geometry, deliberately. The router measures on a `cell`-pitch
/// grid and rounds its clearance radius up, so it is stricter than this —
/// but MEASURED (RP2040 stress board, 180 s): charging the planner the
/// full quantisation escapes 46 pads instead of 32, packs the extra
/// barrels into the QFN ring, and the copper the router then lays through
/// its own landing exemption is illegal (a short and eleven sub-rule gaps
/// — see `Grid::stamp_drilled_disk`). Half a cell was no better on
/// connectivity. The honest cost of exact geometry is that a pad whose
/// only lane exists below the grid pitch gets neither an escape nor a
/// route; the honest alternative is a finer `cell`, not a barrel wedged
/// against its neighbour.
fn escape_gap(need: f64, cell: f64) -> f64 {
    let _ = cell;
    need
}

/// Can a trace of width `tw` leave this pad on its own layer in *some*
/// direction without coming within `clearance` of a foreign-net pad for
/// `ESCAPE_LEN_MM`? We probe the four cardinal and four diagonal
/// directions from the pad centre.
///
/// `outline` is the board edge: a ray that leaves the board is not an
/// escape. Without it an edge-mounted connector (USB-C, castellated
/// header) reads as trivially escapable — its outward ray meets no pad
/// because there is no board out there — and the fanout then skips the
/// very pins that need it most, leaving them entombed by their
/// neighbours' copper. That was the whole USB group on the RP2040 stress
/// board.
pub(crate) fn can_escape_surface(
    cx: f64,
    cy: f64,
    hw: f64,
    hh: f64,
    net: &str,
    foreign: &[&PadRect],
    tw: f64,
    rules: &PadRules<'_>,
    cell: f64,
    outline: Option<pcb_core::Rect>,
) -> bool {
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
            if let Some(o) = outline {
                let edge = (px - o.min.x.to_mm())
                    .min(o.max.x.to_mm() - px)
                    .min(py - o.min.y.to_mm())
                    .min(o.max.y.to_mm() - py);
                if edge < tw / 2.0 + EDGE_CLEARANCE_MM {
                    blocked = true;
                    break;
                }
            }
            for r in foreign {
                if r.net.as_deref() == Some(net) {
                    continue;
                }
                if point_rect_dist(px, py, r)
                    < escape_gap(tw / 2.0 + rules.to(r.net.as_deref()), cell)
                {
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

/// Is the pad at `(cx, cy)` inside a fine-pitch cluster — i.e. is honest
/// clearance to its foreign-net neighbours impossible on the surface?
/// See `CLUSTER_NEIGHBOURS` for why this stays foreign-net only.
pub(crate) fn in_fine_cluster(cx: f64, cy: f64, net: &str, foreign: &[&PadRect]) -> bool {
    foreign
        .iter()
        .filter(|r| r.net.as_deref() != Some(net))
        .filter(|r| point_rect_dist(cx, cy, r) < CLUSTER_DIST_MM)
        .count()
        >= CLUSTER_NEIGHBOURS
}

/// Would a fanout via at `(cx,cy)` on `net` keep `clearance` to every
/// foreign-net pad and to every existing via? (Same-net pad is the pad
/// we sit in — that's the point.)
pub(crate) fn fanout_via_fits(
    cx: f64,
    cy: f64,
    net: &str,
    via_r: f64,
    rules: &PadRules<'_>,
    foreign: &[&PadRect],
    board: &Board,
) -> bool {
    for r in foreign {
        if r.net.as_deref() == Some(net) {
            continue;
        }
        if point_rect_dist(cx, cy, r) < via_r + rules.to(r.net.as_deref()) - 1e-9 {
            return false;
        }
    }
    for v in &board.vias {
        let dx = cx - v.position.x.to_mm();
        let dy = cy - v.position.y.to_mm();
        let other_r = v.diameter.to_mm() / 2.0;
        if (dx * dx + dy * dy).sqrt() < via_r + other_r + rules.to(Some(v.net.as_str())) - 1e-9 {
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
    rules: &PadRules<'_>,
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
        .filter(|v| (v.diameter.to_mm() - rules.via_diameter).abs() < 1e-6)
        .map(|v| (v.position.x.to_mm(), v.position.y.to_mm()))
        .collect();

    let mut best: Option<(f64, f64, f64, f64)> = None; // (score, |off|, vx, vy)
    for off in candidate_offsets {
        if off.abs() > max_off + 1e-9 {
            continue;
        }
        let vx = cx + dx * off;
        let vy = cy + dy * off;
        if !fanout_via_fits(vx, vy, net, via_r, rules, foreign, work) {
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
/// a via-in-pad (or dogbone fallback) if it fits.
///
/// On 2-layer boards the "other" layer is Bottom — vias still open a
/// corridor under the package body. Via-in-pad only lands when the pad is
/// large enough to host the 0.30 mm barrel; finer pads get a dogbone.
/// Fold `sub` (one footprint's fanout) into `plan`.
pub(crate) fn merge_plan(plan: &mut FanoutPlan, sub: FanoutPlan) {
    plan.vias.extend(sub.vias);
    plan.stubs.extend(sub.stubs);
    plan.dogbone_pads.extend(sub.dogbone_pads);
    plan.through_pads.extend(sub.through_pads);
    plan.via_positions.extend(sub.via_positions);
    plan.stranded_pads.extend(sub.stranded_pads);
}

/// Centroid of a footprint's pads — the interior reference the escape
/// geometry is oriented against.
pub(crate) fn pad_centroid(fp: &pcb_core::Footprint) -> (f64, f64) {
    let (mut sx, mut sy, mut n) = (0.0_f64, 0.0_f64, 0.0_f64);
    for pad in &fp.pads {
        let c = fp.pad_world_center(pad);
        sx += c.x.to_mm();
        sy += c.y.to_mm();
        n += 1.0;
    }
    if n > 0.0 {
        (sx / n, sy / n)
    } else {
        (0.0, 0.0)
    }
}

/// Build the escape-slot request for ONE pad — the same description
/// `plan_footprint_inner` defers to the global assignment. Shared so the
/// driver can re-request a slot for a pad whose barrel it ripped
/// (`escape::reassign_escapes`) without duplicating the geometry.
#[allow(clippy::too_many_arguments)]
pub(crate) fn slot_target_of(
    fp: &pcb_core::Footprint,
    pad: &pcb_core::Pad,
    opts: &RouteOptions,
    resolver: &RuleResolver<'_>,
    fab: Option<&pcb_core::FabRules>,
    net_targets: &HashMap<String, (f64, f64)>,
    fp_cx: f64,
    fp_cy: f64,
    outline: Option<pcb_core::Rect>,
) -> Option<crate::slots::SlotTarget> {
    let net = pad.net.as_deref()?;
    let c = fp.pad_world_center(pad);
    let (w, h) = fp.pad_world_size(pad);
    let (cx, cy) = (c.x.to_mm(), c.y.to_mm());
    let (hw, hh) = (w.to_mm() / 2.0, h.to_mm() / 2.0);
    let rules = PadRules::at_pad(resolver, net, c, pad.layer, fab);
    let via_r = rules.via_diameter / 2.0;
    let (dirx, diry) = escape_axis(cx, cy, hw, hh, fp_cx, fp_cy, via_r, outline);
    let (tx, ty) = net_targets
        .get(net)
        .copied()
        .unwrap_or((cx - dirx * 5.0, cy - diry * 5.0));
    Some(crate::slots::SlotTarget {
        pad_ref: format!("{}.{}", fp.reference, pad.number),
        pad_number: pad.number.clone(),
        net: net.to_string(),
        layer: pad.layer,
        cx,
        cy,
        hw,
        hh,
        tx,
        ty,
        max_stub_w: opts.trace_width.to_mm().min(2.0 * hw.min(hh)),
    })
}

/// Plan the VIP / dogbone fanout of ONE footprint against the copper
/// already committed in `work` (previous footprints' barrels and stubs),
/// appending its own vias and stubs to `work` as it goes.
///
/// Split out of `plan_fanout` so the fine-grid escape (`escape.rs`) can
/// use this proven path as its per-footprint baseline and only keep its
/// own result when it demonstrably escapes at least as many pads.
pub(crate) fn plan_footprint(
    fp: &pcb_core::Footprint,
    foreign: &[&PadRect],
    work: &mut Board,
    opts: &RouteOptions,
    resolver: &RuleResolver<'_>,
    fab: Option<&pcb_core::FabRules>,
) -> FanoutPlan {
    plan_footprint_inner(fp, foreign, work, opts, resolver, fab, false)
}

/// Test hook: plan one footprint with the HISTORICAL greedy per-pad
/// dogbone ladder, regardless of how many pads need a dogbone.
///
/// The greedy path is still production code below
/// `slots::SLOT_ASSIGN_MIN_PADS`; this only lets a test drive a
/// fine-pitch footprint (which production sends to the global
/// assignment) down the same ladder, so the two can be compared on
/// identical geometry. No production call site passes `true`.
#[cfg(test)]
pub(crate) fn plan_footprint_greedy(
    fp: &pcb_core::Footprint,
    foreign: &[&PadRect],
    work: &mut Board,
    opts: &RouteOptions,
    resolver: &RuleResolver<'_>,
    fab: Option<&pcb_core::FabRules>,
) -> FanoutPlan {
    plan_footprint_inner(fp, foreign, work, opts, resolver, fab, true)
}

#[allow(clippy::too_many_arguments)]
fn plan_footprint_inner(
    fp: &pcb_core::Footprint,
    foreign: &[&PadRect],
    work: &mut Board,
    opts: &RouteOptions,
    resolver: &RuleResolver<'_>,
    fab: Option<&pcb_core::FabRules>,
    force_greedy: bool,
) -> FanoutPlan {
    let mut plan = FanoutPlan::default();
    let tw = opts.trace_width.to_mm();
    // Board edge: an escape ray that leaves the board is not an escape,
    // and a barrel outside it never fits. Read once for the footprint.
    let outline = work.outline;
    {
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
            let via_r = PadRules::at_pad(
                resolver,
                pad.net.as_deref().unwrap_or(""),
                c,
                pad.layer,
                fab,
            )
            .via_diameter
                / 2.0;
            let (dx, dy) = escape_axis(
                cx,
                cy,
                w.to_mm() / 2.0,
                h.to_mm() / 2.0,
                fp_cx,
                fp_cy,
                via_r,
                outline,
            );
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
        // Pre-scan: how many pads need an escape that a via-in-pad cannot
        // provide? A fine-pitch package with a whole row of those has a
        // real ASSIGNMENT problem (which pad takes which barrel site), and
        // solving it per-pad in pad order is exactly what walls the later
        // pins off — see `crate::slots`. Below the threshold the greedy
        // ladder is adequate and cheaper, so coarse-pitch and lone pads
        // keep the historical path bit for bit.
        let dogbone_needed = fp
            .pads
            .iter()
            .filter(|pad| {
                let Some(net) = pad.net.as_deref() else {
                    return false;
                };
                if pad.drill.is_some() {
                    return false;
                }
                let c = fp.pad_world_center(pad);
                let (w, h) = fp.pad_world_size(pad);
                let (cx, cy) = (c.x.to_mm(), c.y.to_mm());
                let (hw, hh) = (w.to_mm() / 2.0, h.to_mm() / 2.0);
                let rules = PadRules::at_pad(resolver, net, c, pad.layer, fab);
                if rules.via_diameter / 2.0 <= hw.min(hh) + 1e-9 {
                    return false; // a true via-in-pad fits: no slot contest
                }
                in_fine_cluster(cx, cy, net, foreign)
                    || !can_escape_surface(
                        cx,
                        cy,
                        hw,
                        hh,
                        net,
                        foreign,
                        tw,
                        &rules,
                        opts.cell.to_mm(),
                        outline,
                    )
            })
            .count();
        let use_slots = !force_greedy && dogbone_needed >= crate::slots::SLOT_ASSIGN_MIN_PADS;
        // Where each net has to travel, for the escape-direction cost term.
        let net_targets = if use_slots {
            net_partner_centroids(work, &fp.reference)
        } else {
            HashMap::new()
        };
        let mut slot_targets: Vec<crate::slots::SlotTarget> = Vec::new();

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
            // Rules AT THIS PAD: a fine-pitch rule area gives a tighter
            // clearance and (typically) a smaller barrel exactly where
            // the escape is impossible at board-default rules.
            let rules = PadRules::at_pad(resolver, net, c, pad.layer, fab);
            let via_r = rules.via_diameter / 2.0;
            // Is this pad crowded enough that the surface is not an option?
            let in_cluster = in_fine_cluster(cx, cy, net, foreign);
            // Fan out if the pad is in a fine-pitch cluster (parallel
            // escape impossible) OR simply can't escape in any direction.
            if !in_cluster
                && can_escape_surface(
                    cx,
                    cy,
                    hw,
                    hh,
                    net,
                    foreign,
                    tw,
                    &rules,
                    opts.cell.to_mm(),
                    outline,
                )
            {
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
            // A via-in-pad has to sit INSIDE the pad's copper — that is
            // what makes it a VIP rather than a barrel floating over the
            // solder joint. Without this the fit test alone would accept
            // a 0.30 mm barrel centred on a 0.20 mm-wide QFN pad as soon
            // as a tight rule area let it clear the neighbours, and the
            // whole row would drop dogbones for overhanging via-in-pads
            // that block every approach lane.
            let vip = if via_r <= hw.min(hh) + 1e-9 {
                pick_via_position(
                    cx, cy, hw, hh, fp_cx, fp_cy, net, via_r, &rules, foreign, work,
                )
            } else {
                None
            };
            // Dogbone fallback: pad too small for a true via-in-pad
            // (typical 0.4 mm-pitch QFN pads are ~0.20 mm wide — a
            // 0.30 mm barrel does not fit). Place the via just past the
            // pad tip along the pad's inward axis, and lay the copper
            // stub that actually ties the pad to that barrel.
            let mut stub: Option<Trace> = None;
            let max_stub_w = tw.min(2.0 * hw.min(hh));
            if vip.is_none() && use_slots {
                // Defer: this pad's barrel site is chosen by the global
                // assignment once every competing pad is known.
                let (dirx, diry) = escape_axis(cx, cy, hw, hh, fp_cx, fp_cy, via_r, outline);
                let (tx, ty) = net_targets
                    .get(net)
                    .copied()
                    .unwrap_or((cx - dirx * 5.0, cy - diry * 5.0));
                slot_targets.push(crate::slots::SlotTarget {
                    pad_ref: format!("{}.{}", fp.reference, pad.number),
                    pad_number: pad.number.clone(),
                    net: net.to_string(),
                    layer: pad.layer,
                    cx,
                    cy,
                    hw,
                    hh,
                    tx,
                    ty,
                    max_stub_w,
                });
                continue;
            }
            let via_xy = match vip {
                Some(p) => Some(p),
                None => {
                    let (dirx, diry) = escape_axis(cx, cy, hw, hh, fp_cx, fp_cy, via_r, outline);
                    let rank = pad_rank.get(&pad.number).copied().unwrap_or(0);
                    // A dogbone barrel the pad cannot reach with legal copper
                    // is worse than no dogbone at all: the coarse router would
                    // treat the pad as escaped while DRC (rightly) reports it
                    // unconnected. So depth slot and stub are chosen TOGETHER —
                    // walk the candidate depths and keep the first whose stub
                    // is layable, instead of committing to the first fitting
                    // barrel and then discovering its stub is boxed in.
                    dogbone_via_candidates(
                        cx, cy, hw, hh, dirx, diry, rank, net, via_r, &rules, foreign, work,
                    )
                    .into_iter()
                    .find_map(|(vx, vy)| {
                        dogbone_stub(
                            pad.layer, cx, cy, vx, vy, net, max_stub_w, &rules, foreign, work,
                        )
                        .map(|t| {
                            stub = Some(t);
                            (vx, vy)
                        })
                    })
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
                drill: Length::from_mm(rules.via_drill),
                diameter: Length::from_mm(rules.via_diameter),
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
        // Global escape-slot assignment for the deferred fine-pitch pads.
        if !slot_targets.is_empty() {
            let outcome = crate::slots::assign_escape_slots(
                fp,
                &slot_targets,
                foreign,
                work,
                opts,
                resolver,
                fab,
                fp_cx,
                fp_cy,
                &[],
                &mut plan,
            );
            plan.stranded_pads.extend(outcome.stranded);
        }
    }
    plan
}

/// Centroid of every pad on each net that lives OUTSIDE `fp_ref` — i.e.
/// where a pad of `fp_ref` on that net actually has to travel. Used to aim
/// the escape: a barrel on the far side of the package is a legal escape
/// and a terrible one.
pub(crate) fn net_partner_centroids(board: &Board, fp_ref: &str) -> HashMap<String, (f64, f64)> {
    let mut acc: HashMap<String, (f64, f64, f64)> = HashMap::new();
    for fp in board.footprints_in_order() {
        if fp.reference == fp_ref {
            continue;
        }
        for pad in &fp.pads {
            let Some(net) = pad.net.as_deref() else {
                continue;
            };
            let c = fp.pad_world_center(pad);
            let e = acc.entry(net.to_string()).or_insert((0.0, 0.0, 0.0));
            e.0 += c.x.to_mm();
            e.1 += c.y.to_mm();
            e.2 += 1.0;
        }
    }
    acc.into_iter()
        .filter(|(_, v)| v.2 > 0.0)
        .map(|(k, v)| (k, (v.0 / v.2, v.1 / v.2)))
        .collect()
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

/// The direction a pad's escape copper should actually grow in: the
/// pad's long axis (as [`inward_axis`] picks it), flipped when that
/// direction runs off the board and the opposite one does not.
///
/// [`inward_axis`] aims at the footprint's own pad centroid, which is the
/// right answer for a package sitting in open board — the interior
/// channel is where the room is. For an EDGE-MOUNTED part it is exactly
/// backwards: a USB-C receptacle's signal pins sit outboard of its
/// mounting pads, so the centroid points at the board edge, every barrel
/// site fails edge clearance, and the pin gets no escape at all while the
/// open board 1 mm inboard goes unused.
pub(crate) fn escape_axis(
    cx: f64,
    cy: f64,
    hw: f64,
    hh: f64,
    fp_cx: f64,
    fp_cy: f64,
    via_r: f64,
    outline: Option<pcb_core::Rect>,
) -> (f64, f64) {
    let (dx, dy) = inward_axis(cx, cy, hw, hh, fp_cx, fp_cy);
    let Some(o) = outline else {
        return (dx, dy);
    };
    // Where this pad's shallowest barrel would land, on either side.
    let reach = (dx.abs() * hw + dy.abs() * hh).max(hw.min(hh)) + via_r + 0.05;
    let fits = |sx: f64, sy: f64| -> bool {
        let edge = (sx - o.min.x.to_mm())
            .min(o.max.x.to_mm() - sx)
            .min(sy - o.min.y.to_mm())
            .min(o.max.y.to_mm() - sy);
        edge >= via_r + EDGE_CLEARANCE_MM
    };
    if !fits(cx + dx * reach, cy + dy * reach) && fits(cx - dx * reach, cy - dy * reach) {
        return (-dx, -dy);
    }
    (dx, dy)
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
/// escape at all.
///
/// Returns EVERY candidate that clears its neighbours, in preference
/// order, because via feasibility alone is not enough: the caller also
/// has to lay the pad → via copper stub, and a barrel sitting beside a
/// shallower slot can veto that slot's stub while a deeper one still
/// works. Empty when nothing fits.
#[allow(clippy::too_many_arguments)]
pub(crate) fn dogbone_via_candidates(
    cx: f64,
    cy: f64,
    hw: f64,
    hh: f64,
    dirx: f64,
    diry: f64,
    rank: usize,
    net: &str,
    via_r: f64,
    rules: &PadRules<'_>,
    foreign: &[&PadRect],
    work: &Board,
) -> Vec<(f64, f64)> {
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
    let step = 2.0 * via_r + rules.base();
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
    let mut out = Vec::new();
    for dist in depths {
        if dist < base - 0.1 {
            continue;
        }
        let vx = cx + dx * dist;
        let vy = cy + dy * dist;
        if fanout_via_fits(vx, vy, net, via_r, rules, foreign, work) {
            out.push((vx, vy));
        }
    }
    // Last resort: allow a perpendicular nudge. It bends the stub, so it
    // is only tried once the clean on-axis depths are all taken.
    let (px, py) = (-dy, dx);
    for dist in [base, base + step, base + 2.0 * step] {
        for perp_sign in [1.0_f64, -1.0] {
            let nudge = perp_sign * (via_r + rules.base() * 0.5);
            let vx = cx + dx * dist + px * nudge;
            let vy = cy + dy * dist + py * nudge;
            if fanout_via_fits(vx, vy, net, via_r, rules, foreign, work) {
                out.push((vx, vy));
            }
        }
    }
    out
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
    rules: &PadRules<'_>,
    foreign: &[&PadRect],
    work: &Board,
) -> Option<Trace> {
    for &w in &STUB_WIDTHS_MM {
        if w > max_width + 1e-9 || w + 1e-9 < rules.min_stub_width {
            continue;
        }
        let half = w / 2.0;
        let mut ok = true;
        for r in foreign {
            if r.net.as_deref() == Some(net) {
                continue;
            }
            if seg_rect_dist(cx, cy, vx, vy, r) < half + rules.to(r.net.as_deref()) - 1e-9 {
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
            let d = point_seg_dist(v.position.x.to_mm(), v.position.y.to_mm(), cx, cy, vx, vy);
            if d < half + v.diameter.to_mm() / 2.0 + rules.to(Some(v.net.as_str())) - 1e-9 {
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
            if d < half + t.width.to_mm() / 2.0 + rules.to(Some(t.net.as_str())) - 1e-9 {
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
