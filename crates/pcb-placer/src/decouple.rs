//! Decoupling ring — put each small passive AT a specific pin, not near
//! the average of several.
//!
//! The previous rule pulled a 2-pad passive toward the CENTROID of every
//! fixed same-net pad. On a board where `+3V3` reaches eight IOVDD pins
//! that centroid is the middle of the package, so five decoupling caps
//! all converge on the same meaningless point, the legality checks push
//! them into whatever gap is left, and none of them ends up short-loop
//! coupled to a pin. Which defeats the purpose of a decoupling cap: what
//! matters is the loop area between THIS cap and THE pin it serves.
//!
//! So we do what a human does:
//!
//! 1. Pick one specific anchor pin per passive — a same-net pad on a
//!    non-passive part (IC/connector) — preferring the passive's rail or
//!    signal net over GND (a decoupling cap's interesting end is
//!    `+3V3`/`+1V1`; its GND end goes to the pour whatever we do).
//! 2. Assign passives to DISTINCT pins, so five caps on one rail ring
//!    five different IOVDD pins instead of piling on the first one.
//!    Deterministic order (passives sorted by reference), greedy
//!    nearest-free pin, reusing a pin only when none is free.
//! 3. Place the passive on a radial ring outside the anchor pad: walk
//!    outward along the body-centre → pad direction, with small lateral
//!    offsets when the radial line is blocked, and rotate the part in 90°
//!    steps so its OWN connected pad faces the anchor pad (that is the
//!    short loop).
//!
//! Every legality check the SA enforces still applies to each candidate
//! (pads inside the outline, body inside the outline, the hard
//! body-to-body clearance, edge-mount). When no anchor pin exists at all
//! — a passive between two other passives, an RC filter — we fall back to
//! the old centroid pull, which is the right answer for that shape.

use std::collections::{BTreeMap, HashSet};

use pcb_core::{Board, Id, Length, Point, Rect};

use crate::{margin_for_fp, pads_inside_outline, probe_min_gap, MarginMap, PlaceOptions};

/// Radial step of the ring ladder, mm. Small on purpose: the first legal
/// rank should be the CLOSEST legal rank, and 0.5 mm is finer than the
/// hard clearance so we never overshoot a whole millimetre past the pin.
const RING_STEP_MM: f64 = 0.5;

/// How many radial ranks to try before giving up on this anchor.
/// 8 × 0.5 mm = 4 mm of outward travel — past that the cap is no longer
/// "at the pin" and the centroid fallback is no worse.
const RING_RANKS: usize = 8;

/// Slack (mm) added to the first rank so the nanometre rounding in
/// `Length` cannot make an exactly-at-clearance candidate read as one
/// nanometre too close and cost a whole rank.
const RING_SLACK_MM: f64 = 0.02;

/// Lateral offsets (in units of the part's own perpendicular extent plus
/// the hard clearance) tried at each rank, in order. 0 first: dead ahead
/// of the pin is always the best loop.
const RING_LATERAL: [f64; 5] = [0.0, 1.0, -1.0, 2.0, -2.0];

/// Ring placement is run twice over the same (fixed) pin assignment.
/// The first pass places passives in reference order against a board
/// where the LATER passives are still wherever SA dropped them, so an
/// early cap can be pushed a rank or a lateral step out by a neighbour
/// that is about to move away. The second pass re-seats everyone against
/// the settled layout and recovers those ranks. Deterministic: same
/// order, same assignment, and a pass only ever moves a part closer to
/// its own pin.
const RING_PASSES: usize = 2;

/// One anchor pin: a pad of a non-passive footprint.
#[derive(Clone)]
struct AnchorPin {
    fp: Id,
    pad: usize,
    /// `(reference, pad number)` — the deterministic tie-break for
    /// "nearest free pin", so two equidistant pins always resolve the
    /// same way regardless of map iteration order.
    sort_key: (String, String),
    centre: [f64; 2],
}

/// True for the conventional ground names. Used only to RANK a passive's
/// own nets when choosing which end anchors it: the ground end of a
/// decoupling cap tells us nothing about where the cap belongs.
fn is_ground_net(net: &str) -> bool {
    let u = net.to_ascii_uppercase();
    matches!(
        u.as_str(),
        "GND" | "AGND" | "DGND" | "PGND" | "VSS" | "VSSA" | "0V" | "GROUND"
    ) || u.starts_with("GND")
}

/// Pull small multi-pin passives onto the decoupling ring of a specific
/// same-net pin. Skips edge-mounted parts and anything with more than
/// 4 pads (ICs / connectors stay where SA left them).
pub(crate) fn pull_passives_to_anchors(
    board: &mut Board,
    movable_ids: &[Id],
    outline: Rect,
    margins: &MarginMap,
    opts: &PlaceOptions,
) {
    let movable_set: HashSet<Id> = movable_ids.iter().copied().collect();
    let hard_clearance = opts.min_clearance_mm.max(opts.solder_gap_mm);

    // Snapshot passive candidates first (id + reference + nets) so we
    // don't hold a board borrow while mutating. Sorted by reference: the
    // pin assignment below is greedy, so its input order IS part of the
    // result and must not depend on the footprint map.
    let mut candidates: Vec<(Id, String, Vec<String>)> = Vec::new();
    for id in movable_ids {
        let Some(fp) = board.footprints.get(id) else {
            continue;
        };
        if fp.edge_mounted || fp.pads.len() > 4 || fp.pads.len() < 2 {
            continue;
        }
        let mut nets: Vec<String> = fp.pads.iter().filter_map(|p| p.net.clone()).collect();
        nets.sort();
        nets.dedup();
        if nets.is_empty() {
            continue;
        }
        candidates.push((*id, fp.reference.clone(), nets));
    }
    if candidates.is_empty() {
        return;
    }
    candidates.sort_by(|a, b| a.1.cmp(&b.1));

    // Anchor pin catalogue, keyed by net: every pad of every NON-passive
    // footprint. "Passive" = movable and ≤ 4 pads, i.e. exactly the parts
    // this pass moves — anchoring to those would make caps chase caps.
    let mut pins: BTreeMap<String, Vec<AnchorPin>> = BTreeMap::new();
    let mut net_pad_count: BTreeMap<String, usize> = BTreeMap::new();
    for fp in board.footprints_in_order() {
        let is_passive = movable_set.contains(&fp.id) && fp.pads.len() <= 4;
        for (i, pad) in fp.pads.iter().enumerate() {
            let Some(net) = pad.net.clone() else {
                continue;
            };
            *net_pad_count.entry(net.clone()).or_insert(0) += 1;
            if is_passive {
                continue;
            }
            let c = fp.pad_world_center(pad);
            pins.entry(net).or_default().push(AnchorPin {
                fp: fp.id,
                pad: i,
                sort_key: (fp.reference.clone(), pad.number.clone()),
                centre: [c.x.to_mm(), c.y.to_mm()],
            });
        }
    }
    for v in pins.values_mut() {
        v.sort_by(|a, b| a.sort_key.cmp(&b.sort_key));
    }

    // Pins already handed out. Distinct-pin assignment is the whole point
    // of the pass, so this is consulted before distance.
    let mut used: HashSet<(Id, usize)> = HashSet::new();

    // Phase 1: assignment. Decided for every passive BEFORE anything
    // moves, so the greedy "nearest free pin" reads the layout SA settled
    // on rather than a board half-rearranged by this same pass.
    let mut assignments: Vec<Assignment> = Vec::new();
    for (id, _reference, nets) in candidates {
        let Some(here) = board.footprints.get(&id).map(|f| f.position) else {
            continue;
        };
        let here = [here.x.to_mm(), here.y.to_mm()];

        // Rank the passive's own nets: non-ground first (that is the rail
        // or signal it serves), then the sparser net (a 3-pad rail says
        // more about position than a 20-pad one), then by name so the
        // order is total.
        let mut ranked: Vec<(bool, usize, String)> = nets
            .iter()
            .map(|n| {
                (
                    is_ground_net(n),
                    net_pad_count.get(n).copied().unwrap_or(0),
                    n.clone(),
                )
            })
            .collect();
        ranked.sort();

        // The NET is chosen first (best-ranked one that has any pin at
        // all), then the pin within it: nearest free pin, and only if the
        // whole rail is already spoken for do we double up on the nearest
        // used one. Choosing the net first is deliberate — eight caps on a
        // four-pin rail should pair up on the rail, not wander off to the
        // IC's GND pads just because those are still free.
        let mut chosen: Option<(AnchorPin, String)> = None;
        for (_, _, net) in &ranked {
            let Some(list) = pins.get(net) else { continue };
            let pick = nearest_pin(list, here, &used, true)
                .or_else(|| nearest_pin(list, here, &used, false));
            if let Some(p) = pick {
                chosen = Some((p.clone(), net.clone()));
                break;
            }
        }
        if let Some((pin, _)) = &chosen {
            used.insert((pin.fp, pin.pad));
        }
        assignments.push(Assignment {
            id,
            nets,
            pin: chosen,
            placed: false,
        });
    }

    // Phase 2: seat every passive on its pin's ring. Repeated so a cap
    // that had to give way to a neighbour still sitting on its SA position
    // gets the close rank back once that neighbour has moved.
    for _pass in 0..RING_PASSES {
        for a in &mut assignments {
            let Some((pin, net)) = &a.pin else { continue };
            if ring_place(
                board,
                a.id,
                pin,
                net,
                outline,
                margins,
                hard_clearance,
                opts,
            ) {
                a.placed = true;
            }
        }
    }

    // Phase 3: whatever had no anchor pin (or no legal ring position at
    // all) still gets the old centroid pull — the right answer for the
    // shapes with no single anchor.
    for a in &assignments {
        if a.placed {
            continue;
        }
        pull_to_centroid(board, a.id, &a.nets, &movable_set, outline, margins, opts);
    }
}

/// A passive and the pin it was assigned to.
struct Assignment {
    id: Id,
    /// The passive's own nets, for the centroid fallback.
    nets: Vec<String>,
    /// `(pin, net)`; `None` when no non-passive part shares any of its nets.
    pin: Option<(AnchorPin, String)>,
    /// Whether some ring pass found a legal pose.
    placed: bool,
}

/// Nearest pin to `here`, either restricted to unused pins (`free_only`)
/// or over all of them. Ties break on `sort_key`, which `pins` is already
/// sorted by, so `<` (strictly better) keeps the first of any tie.
fn nearest_pin<'a>(
    list: &'a [AnchorPin],
    here: [f64; 2],
    used: &HashSet<(Id, usize)>,
    free_only: bool,
) -> Option<&'a AnchorPin> {
    let mut best: Option<(f64, &AnchorPin)> = None;
    for p in list {
        if free_only && used.contains(&(p.fp, p.pad)) {
            continue;
        }
        let d = (p.centre[0] - here[0]).hypot(p.centre[1] - here[1]);
        if best.is_none_or(|(bd, _)| d < bd) {
            best = Some((d, p));
        }
    }
    best.map(|(_, p)| p)
}

/// Try the radial ring around `pin` for the passive `id`. Returns true if
/// a legal pose was found and applied (position + rotation).
#[allow(clippy::too_many_arguments)]
fn ring_place(
    board: &mut Board,
    id: Id,
    pin: &AnchorPin,
    net: &str,
    outline: Rect,
    margins: &MarginMap,
    hard_clearance: f64,
    opts: &PlaceOptions,
) -> bool {
    let Some(anchor) = board.footprints.get(&pin.fp) else {
        return false;
    };
    let Some(anchor_bb) = anchor.bounds() else {
        return false;
    };
    let anchor_centre = [
        f64::midpoint(anchor_bb.min.x.to_mm(), anchor_bb.max.x.to_mm()),
        f64::midpoint(anchor_bb.min.y.to_mm(), anchor_bb.max.y.to_mm()),
    ];
    let Some(original) = board.footprints.get(&id).cloned() else {
        return false;
    };
    // The passive's own end of the wire: the pad carrying the anchor net.
    let Some(own_pad) = original
        .pads
        .iter()
        .position(|p| p.net.as_deref() == Some(net))
    else {
        return false;
    };

    // Outward direction: body centre → anchor pad, so the passive lands on
    // the pin's OWN side of the package rather than around a corner.
    // Degenerate case (a single centred pad) falls back to the direction
    // the passive already lies in, which at least does not teleport it
    // across the board.
    let mut dx = pin.centre[0] - anchor_centre[0];
    let mut dy = pin.centre[1] - anchor_centre[1];
    if dx.hypot(dy) < 1e-6 {
        dx = original.position.x.to_mm() - anchor_centre[0];
        dy = original.position.y.to_mm() - anchor_centre[1];
    }
    if dx.hypot(dy) < 1e-6 {
        return false;
    }
    // Quantised to the body side the pad sits on. Clearance is measured
    // between axis-aligned bounding boxes, so a part pushed out on a
    // diagonal ray has to travel ~40 % further to clear the same body and
    // ends up sitting off the corner instead of in front of its pin —
    // exactly what a human would not do. The lateral offsets then run
    // ALONG that body side, which is where the free room is anyway.
    // Ties (a pad exactly on the diagonal) resolve to x, for repeatability.
    let (ux, uy) = if dx.abs() >= dy.abs() {
        (dx.signum(), 0.0)
    } else {
        (0.0, dy.signum())
    };
    // Perpendicular, for the lateral offsets.
    let (vx, vy) = (-uy, ux);

    // Rotation: the passive's connected pad must face the anchor pad, so
    // pick the 90° step whose rotated pad offset points most against the
    // outward ray. `<` keeps the smallest quarter on a tie (a symmetric
    // 0603 is 180°-degenerate; both are equally good, so be repeatable).
    let mut best_rot = original.rotation;
    let mut best_dot = f64::INFINITY;
    for q in 0..4 {
        let rot = (original.rotation + 90.0 * q as f32).rem_euclid(360.0);
        let mut probe = original.clone();
        probe.rotation = rot;
        probe.position = Point::new(Length::ZERO, Length::ZERO);
        let c = probe.pad_world_center(&probe.pads[own_pad]);
        let dot = c.x.to_mm() * ux + c.y.to_mm() * uy;
        if dot < best_dot - 1e-9 {
            best_dot = dot;
            best_rot = rot;
        }
    }

    // Own half-extents at the chosen rotation, and the offset from the
    // footprint position to its bbox centre (pad-only footprints are
    // rarely centred on their origin).
    let mut oriented = original.clone();
    oriented.rotation = best_rot;
    let Some(ob) = oriented.bounds() else {
        return false;
    };
    let px = oriented.position.x.to_mm();
    let py = oriented.position.y.to_mm();
    let half = [
        (ob.max.x.to_mm() - ob.min.x.to_mm()) / 2.0,
        (ob.max.y.to_mm() - ob.min.y.to_mm()) / 2.0,
    ];
    let bb_off = [
        f64::midpoint(ob.min.x.to_mm(), ob.max.x.to_mm()) - px,
        f64::midpoint(ob.min.y.to_mm(), ob.max.y.to_mm()) - py,
    ];
    // Support of the own bbox along the ray / the perpendicular: how far
    // the body reaches in each of those directions.
    let reach_u = half[0] * ux.abs() + half[1] * uy.abs();
    let reach_v = half[0] * vx.abs() + half[1] * vy.abs();
    // First rank: the passive's body exactly one hard clearance outside
    // the ANCHOR's body — not one clearance outside the anchor PAD, which
    // would still overlap the package whenever the pad sits inboard of the
    // body edge. Measured along the ray from the pad centre, since that is
    // where the ladder starts. `probe_min_gap` remains the real arbiter;
    // this only decides where to start looking.
    let anchor_reach = (anchor_bb.max.x.to_mm() - anchor_bb.min.x.to_mm()) / 2.0 * ux.abs()
        + (anchor_bb.max.y.to_mm() - anchor_bb.min.y.to_mm()) / 2.0 * uy.abs();
    let pad_along =
        (pin.centre[0] - anchor_centre[0]) * ux + (pin.centre[1] - anchor_centre[1]) * uy;
    let base = (anchor_reach - pad_along).max(0.0) + reach_u + hard_clearance + RING_SLACK_MM;
    let lateral_step = 2.0 * reach_v + hard_clearance;

    for rank in 0..RING_RANKS {
        let d = base + RING_STEP_MM * rank as f64;
        for l in RING_LATERAL {
            let cx = pin.centre[0] + ux * d + vx * l * lateral_step;
            let cy = pin.centre[1] + uy * d + vy * l * lateral_step;
            let pos = Point::new(
                Length::from_mm(cx - bb_off[0]),
                Length::from_mm(cy - bb_off[1]),
            );
            let mut probe = oriented.clone();
            probe.position = pos;
            if !pads_inside_outline(&probe, outline, opts.edge_clearance_mm) {
                continue;
            }
            if board
                .body_outline_violation(&probe, margin_for_fp(&probe, margins))
                .is_some()
            {
                continue;
            }
            if probe_min_gap(board, &probe, margins) < hard_clearance {
                continue;
            }
            if board.edge_mount_violation(&probe).is_some() {
                continue;
            }
            if let Some(fp) = board.footprints.get_mut(&id) {
                fp.position = pos;
                fp.rotation = best_rot;
            }
            return true;
        }
    }
    false
}

/// Historical behaviour, kept as the fallback: walk the passive to the
/// centroid of the non-passive same-net pads, trying a few offsets when
/// the exact spot is blocked. Correct for the shapes that have no single
/// anchor pin (RC filters, divider chains, LED + series R).
fn pull_to_centroid(
    board: &mut Board,
    id: Id,
    nets: &[String],
    movable_set: &HashSet<Id>,
    outline: Rect,
    margins: &MarginMap,
    opts: &PlaceOptions,
) {
    let hard_clearance = opts.min_clearance_mm.max(opts.solder_gap_mm);
    let mut ax = 0.0_f64;
    let mut ay = 0.0_f64;
    let mut n = 0.0_f64;
    for fp in board.footprints_in_order() {
        if fp.id == id {
            continue;
        }
        let is_passive = movable_set.contains(&fp.id) && fp.pads.len() <= 4;
        if is_passive {
            continue;
        }
        for pad in &fp.pads {
            let Some(net) = pad.net.as_deref() else {
                continue;
            };
            if !nets.iter().any(|n| n == net) {
                continue;
            }
            let c = fp.pad_world_center(pad);
            ax += c.x.to_mm();
            ay += c.y.to_mm();
            n += 1.0;
        }
    }
    if n < 1.0 {
        return;
    }
    let target = [ax / n, ay / n];

    // Try the target, then a few radial offsets if the exact spot is
    // blocked (common next to a dense QFN pad ring).
    let offsets: [(f64, f64); 9] = [
        (0.0, 0.0),
        (1.5, 0.0),
        (-1.5, 0.0),
        (0.0, 1.5),
        (0.0, -1.5),
        (1.2, 1.2),
        (1.2, -1.2),
        (-1.2, 1.2),
        (-1.2, -1.2),
    ];
    for (dx, dy) in offsets {
        let candidate = Point::new(
            Length::from_mm(target[0] + dx),
            Length::from_mm(target[1] + dy),
        );
        let Some(mut probe) = board.footprints.get(&id).cloned() else {
            return;
        };
        probe.position = candidate;
        if !pads_inside_outline(&probe, outline, opts.edge_clearance_mm) {
            continue;
        }
        if board
            .body_outline_violation(&probe, margin_for_fp(&probe, margins))
            .is_some()
        {
            continue;
        }
        if probe_min_gap(board, &probe, margins) < hard_clearance {
            continue;
        }
        if board.edge_mount_violation(&probe).is_some() {
            continue;
        }
        if let Some(fp) = board.footprints.get_mut(&id) {
            fp.position = candidate;
        }
        return;
    }
    // Nothing legal anywhere: leave the SA result untouched — better a
    // scattered passive than one overlapping its neighbour.
}
