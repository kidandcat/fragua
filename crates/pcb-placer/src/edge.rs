//! Edge planning — WHICH board edge each edge-mounted part goes to.
//!
//! Edge-mounted parts (USB receptacles, screw terminals, pin headers a
//! ribbon plugs into) are only legal touching the outline, so the placer
//! used to snap them to the NEAREST edge and let the SA refine from
//! there. That freezes the single most consequential decision on the
//! board at whatever the spawn position happened to be: once a header is
//! on the left edge, every move that would take it to the right edge is
//! hard-rejected as an edge-mount violation, so SA can never fix a wrong
//! side — it can only slide the part along the wrong one.
//!
//! This pass makes that decision on purpose, BEFORE the global stage.
//! For each movable edge-mounted part it enumerates the four sides,
//! seeds the along-edge coordinate from the parts it wires to, spreads
//! parts that land on the same side so they do not overlap, and scores
//! the whole assignment with the same objective the SA uses:
//!
//! ```text
//! cost = Σ net_hpwl(affected nets) + crossing_penalty_factor × bundle crossings
//! ```
//!
//! Assignments are enumerated EXACTLY while there are at most
//! `EXACT_MAX_PARTS` movable edge parts (4^n combinations, ≤ 256), and
//! greedily in a deterministic order beyond that — a board with five or
//! more independent edge connectors is rare, and greedy on this objective
//! is still far better than "nearest edge".
//!
//! No randomness anywhere: the same board always produces the same plan.

use std::collections::HashSet;

use pcb_core::{Board, EdgeSide, Footprint, Id, Length, Point, Rect};

use crate::{
    bundle::crossings_involving, margin_for_fp, net_hpwl, pads_inside_outline, probe_min_gap,
    MarginMap, PlaceOptions,
};

/// Sides in the order the planner enumerates them. Fixed, so combination
/// order — and therefore tie-breaking — is reproducible.
const SIDES: [EdgeSide; 4] = [
    EdgeSide::Left,
    EdgeSide::Right,
    EdgeSide::Bottom,
    EdgeSide::Top,
];

/// Up to this many movable edge parts, every side combination is scored
/// (4^4 = 256 evaluations). Above it, sides are chosen greedily in
/// reference order.
const EXACT_MAX_PARTS: usize = 4;

/// What the planner decided for one part.
#[derive(Debug, Clone)]
pub struct EdgePlacement {
    pub reference: String,
    pub side: EdgeSide,
    /// Coordinate of the part's bbox centre along the chosen edge, mm
    /// (x on the top/bottom edges, y on the left/right ones).
    pub along_mm: f64,
    pub position: Point,
    pub rotation: f32,
}

/// Outcome of a planning pass.
#[derive(Debug, Clone, Default)]
pub struct EdgePlanReport {
    /// One entry per part the planner placed, in reference order.
    pub placed: Vec<EdgePlacement>,
    /// `"REF: reason"` for every requested ref the planner could not
    /// touch (unknown, no pads, not edge-mounted, no legal side).
    pub skipped: Vec<String>,
    /// Objective of the layout the planner started from, mm-equivalent.
    pub initial_cost: f64,
    /// Objective of the chosen assignment, mm-equivalent.
    pub final_cost: f64,
}

/// Plan the sides of the named edge-mounted footprints on `board` and
/// apply the winning assignment. Refs that are unknown, pad-less or not
/// edge-mounted are reported in `skipped` rather than failing the call.
///
/// This is the whole of the `edge-plan` verb: no SA, no global stage, so
/// nothing else on the board moves.
pub fn plan_edge_sides(
    board: &mut Board,
    movable: &[String],
    opts: &PlaceOptions,
    margins: &MarginMap,
) -> Result<EdgePlanReport, String> {
    let outline = board
        .outline
        .ok_or_else(|| "edge-plan needs a board outline; set one with `outline W H`".to_string())?;
    let mut ids: Vec<Id> = Vec::new();
    let mut skipped: Vec<String> = Vec::new();
    for r in movable {
        match board.footprints_in_order().find(|fp| fp.reference == *r) {
            None => skipped.push(format!("{r}: not on the board")),
            Some(fp) if fp.bounds().is_none() => {
                skipped.push(format!("{r}: footprint has no pads"));
            }
            Some(fp) if !fp.edge_mounted => skipped.push(format!(
                "{r}: not edge-mounted — use `edge-mount KEY <side>` on its library entry first"
            )),
            Some(fp) => ids.push(fp.id),
        }
    }
    let mut report = plan_movable_edges(board, &ids, opts, margins, outline);
    report.skipped.splice(0..0, skipped);
    Ok(report)
}

/// Core pass, id-based: used both by the verb wrapper above and by
/// `place()` before its global stage.
pub(crate) fn plan_movable_edges(
    board: &mut Board,
    movable_ids: &[Id],
    opts: &PlaceOptions,
    margins: &MarginMap,
    outline: Rect,
) -> EdgePlanReport {
    let hard_clearance = opts.min_clearance_mm.max(opts.solder_gap_mm);
    let mut report = EdgePlanReport::default();

    // Reference order is the planner's deterministic input order (and the
    // greedy path's decision order).
    let mut parts: Vec<Part> = Vec::new();
    for id in movable_ids {
        let Some(fp) = board.footprints.get(id) else {
            continue;
        };
        if !fp.edge_mounted || fp.bounds().is_none() {
            continue;
        }
        parts.push(Part {
            id: *id,
            reference: fp.reference.clone(),
            poses: [None, None, None, None],
        });
    }
    if parts.is_empty() {
        return report;
    }
    parts.sort_by(|a, b| a.reference.cmp(&b.reference));

    let ids: Vec<Id> = parts.iter().map(|p| p.id).collect();
    let planned: HashSet<Id> = ids.iter().copied().collect();
    // Nets the objective can actually change: every net any planned part
    // has a pad on. Everything else contributes a constant, so leaving it
    // out makes the numbers comparable and the evaluation cheap.
    let nets = affected_nets(board, &planned);
    let originals: Vec<(Id, Point, f32)> = ids
        .iter()
        .filter_map(|id| {
            board
                .footprints
                .get(id)
                .map(|fp| (*id, fp.position, fp.rotation))
        })
        .collect();
    report.initial_cost = cost(board, &nets, &ids, opts.crossing_penalty_factor);

    // --- Per (part, side): the pose we would use -----------------------------
    // Rotation is decided here, once, so the assignment search below is
    // over sides only (4^n, not 16^n). For a part that declares its
    // wire/plug side there is exactly one legal rotation per edge; for an
    // undeclared one we keep the orientations that run its long axis
    // ALONG the edge (a connector across the board edge is never right)
    // and pick between them on the same objective, evaluated with every
    // other part still where it is.
    #[allow(clippy::needless_range_loop)] // `parts[pi]` is written inside
    for pi in 0..parts.len() {
        for (si, side) in SIDES.iter().enumerate() {
            let Some(fp) = board.footprints.get(&parts[pi].id).cloned() else {
                continue;
            };
            let mut best: Option<(bool, f64, Pose)> = None;
            for rot in rotation_candidates(&fp, *side) {
                let Some(pose) = pose_for(&fp, rot, *side, board, &planned, outline) else {
                    continue;
                };
                let along = clamp_along(pose.seed_along, &pose, *side, outline);
                let pos = position_for(*side, along, &pose, outline);
                let legal = apply_probe(board, parts[pi].id, pos, rot);
                let ok =
                    legal && is_legal(board, parts[pi].id, outline, margins, hard_clearance, opts);
                let c = cost(board, &nets, &ids, opts.crossing_penalty_factor);
                restore(board, &originals);
                // Prefer legal poses; among equals, the cheaper one; ties
                // go to the first candidate (lowest rotation).
                let better = match &best {
                    None => true,
                    Some((bl, bc, _)) => (ok, -c) > (*bl, -*bc),
                };
                if better {
                    best = Some((ok, c, pose));
                }
            }
            parts[pi].poses[si] = best.map(|(_, _, p)| p);
        }
    }

    // --- Choose the assignment ----------------------------------------------
    let n = parts.len();
    let mut best_assign: Option<(f64, Vec<usize>)> = None;
    if n <= EXACT_MAX_PARTS {
        let total = 4usize.pow(n as u32);
        for combo in 0..total {
            let mut assign = vec![0usize; n];
            let mut c = combo;
            for a in &mut assign {
                *a = c % 4;
                c /= 4;
            }
            let Some(score) = evaluate(
                board,
                &parts,
                &assign,
                &originals,
                &nets,
                &ids,
                outline,
                margins,
                hard_clearance,
                opts,
            ) else {
                continue;
            };
            if best_assign.as_ref().is_none_or(|(b, _)| score < *b - 1e-9) {
                best_assign = Some((score, assign));
            }
        }
    } else {
        // Greedy: fix one part at a time, in reference order. Parts not
        // decided yet stay where they are, so each decision is made
        // against the best information available at that point.
        let mut assign: Vec<usize> = vec![usize::MAX; n];
        for k in 0..n {
            let mut best_side: Option<(f64, usize)> = None;
            for si in 0..4 {
                let mut trial = assign.clone();
                trial[k] = si;
                let decided: Vec<usize> = (0..=k).collect();
                let Some(score) = evaluate_subset(
                    board,
                    &parts,
                    &trial,
                    &decided,
                    &originals,
                    &nets,
                    &ids,
                    outline,
                    margins,
                    hard_clearance,
                    opts,
                ) else {
                    continue;
                };
                if best_side.is_none_or(|(b, _)| score < b - 1e-9) {
                    best_side = Some((score, si));
                }
            }
            match best_side {
                Some((_, si)) => assign[k] = si,
                // No legal side for this part: leave it out of the plan.
                None => assign[k] = usize::MAX,
            }
        }
        let decided: Vec<usize> = (0..n).filter(|k| assign[*k] != usize::MAX).collect();
        let score = evaluate_subset(
            board,
            &parts,
            &assign,
            &decided,
            &originals,
            &nets,
            &ids,
            outline,
            margins,
            hard_clearance,
            opts,
        );
        if let Some(score) = score {
            best_assign = Some((score, assign));
        }
    }

    let Some((score, assign)) = best_assign else {
        // Nothing legal anywhere: leave the board untouched and say so.
        restore(board, &originals);
        report.final_cost = report.initial_cost;
        for p in &parts {
            report
                .skipped
                .push(format!("{}: no legal edge found", p.reference));
        }
        return report;
    };

    // --- Apply the winner ----------------------------------------------------
    let decided: Vec<usize> = (0..parts.len())
        .filter(|k| assign[*k] != usize::MAX)
        .collect();
    let placements = poses_for_assignment(&parts, &assign, &decided, outline, hard_clearance);
    restore(board, &originals);
    if let Some(placements) = placements {
        for (k, side, along, pos, rot) in placements {
            if let Some(fp) = board.footprints.get_mut(&parts[k].id) {
                fp.position = pos;
                fp.rotation = rot;
            }
            report.placed.push(EdgePlacement {
                reference: parts[k].reference.clone(),
                side,
                along_mm: along,
                position: pos,
                rotation: rot,
            });
        }
    }
    for k in 0..parts.len() {
        if assign[k] == usize::MAX {
            report
                .skipped
                .push(format!("{}: no legal edge found", parts[k].reference));
        }
    }
    report.final_cost = score;
    report
}

/// A part being planned, with the pose it would take on each side.
struct Part {
    id: Id,
    reference: String,
    /// Indexed by `SIDES`. `None` = that side has no usable pose.
    poses: [Option<Pose>; 4],
}

/// Geometry of one (part, side) candidate, in mm.
#[derive(Clone)]
struct Pose {
    rot: f32,
    /// Half width / half height of the pad bbox at `rot`.
    half: [f64; 2],
    /// Offset from `Footprint::position` to the bbox centre at `rot`.
    bb_off: [f64; 2],
    /// Where along the edge the part WANTS to be: the projection of the
    /// centroid of the pads it connects to (excluding other planned
    /// parts, whose positions are still in flux).
    seed_along: f64,
}

impl Pose {
    /// Half extent along the edge (the axis the part slides on).
    fn half_along(&self, side: EdgeSide) -> f64 {
        match side {
            EdgeSide::Left | EdgeSide::Right => self.half[1],
            EdgeSide::Top | EdgeSide::Bottom => self.half[0],
        }
    }
}

/// Absolute rotations worth trying for `fp` on `side`.
///
/// A part that declares `edge_side` (the wire entry / plug face) has
/// exactly one: the rotation that turns that local side toward this board
/// edge — same arithmetic `Project::place_edge_from_palette` uses, so the
/// planner and the manual verb agree. Otherwise we keep the orientations
/// whose long axis is parallel to the edge; if the bbox is square, all
/// four are candidates.
fn rotation_candidates(fp: &Footprint, side: EdgeSide) -> Vec<f32> {
    if let Some(local) = fp.edge_side {
        let q = (side.ccw_index() + 4 - local.ccw_index()) % 4;
        return vec![(q as f32) * 90.0];
    }
    let mut out: Vec<f32> = Vec::new();
    for q in 0..4u32 {
        let rot = (q as f32) * 90.0;
        let mut probe = fp.clone();
        probe.rotation = rot;
        let Some(b) = probe.bounds() else { continue };
        let w = b.max.x.to_mm() - b.min.x.to_mm();
        let h = b.max.y.to_mm() - b.min.y.to_mm();
        let along_edge = match side {
            EdgeSide::Left | EdgeSide::Right => h >= w - 1e-6,
            EdgeSide::Top | EdgeSide::Bottom => w >= h - 1e-6,
        };
        if along_edge {
            out.push(rot);
        }
    }
    if out.is_empty() {
        out = (0..4).map(|q| (q as f32) * 90.0).collect();
    }
    out
}

/// Geometry + along-edge seed of one (part, rotation, side) candidate.
fn pose_for(
    fp: &Footprint,
    rot: f32,
    side: EdgeSide,
    board: &Board,
    planned: &HashSet<Id>,
    outline: Rect,
) -> Option<Pose> {
    let mut probe = fp.clone();
    probe.rotation = rot;
    let b = probe.bounds()?;
    let px = probe.position.x.to_mm();
    let py = probe.position.y.to_mm();
    let half = [
        (b.max.x.to_mm() - b.min.x.to_mm()) / 2.0,
        (b.max.y.to_mm() - b.min.y.to_mm()) / 2.0,
    ];
    let bb_off = [
        f64::midpoint(b.min.x.to_mm(), b.max.x.to_mm()) - px,
        f64::midpoint(b.min.y.to_mm(), b.max.y.to_mm()) - py,
    ];
    // Seed: centroid of the pads this part wires to on parts that are NOT
    // being planned (their positions are fixed, so they are the only
    // trustworthy signal), projected onto the edge axis. With nothing to
    // go on, the middle of the edge.
    let centroid = connected_centroid(board, fp, planned);
    let seed_along = match (side, centroid) {
        (EdgeSide::Left | EdgeSide::Right, Some(c)) => c[1],
        (EdgeSide::Top | EdgeSide::Bottom, Some(c)) => c[0],
        (EdgeSide::Left | EdgeSide::Right, None) => {
            f64::midpoint(outline.min.y.to_mm(), outline.max.y.to_mm())
        }
        (EdgeSide::Top | EdgeSide::Bottom, None) => {
            f64::midpoint(outline.min.x.to_mm(), outline.max.x.to_mm())
        }
    };
    Some(Pose {
        rot,
        half,
        bb_off,
        seed_along,
    })
}

/// Centroid (mm) of every pad that shares a net with `fp` and sits on a
/// footprint that is neither `fp` itself nor another planned part.
fn connected_centroid(board: &Board, fp: &Footprint, planned: &HashSet<Id>) -> Option<[f64; 2]> {
    let mut nets: Vec<&str> = fp.pads.iter().filter_map(|p| p.net.as_deref()).collect();
    nets.sort_unstable();
    nets.dedup();
    if nets.is_empty() {
        return None;
    }
    let mut sx = 0.0;
    let mut sy = 0.0;
    let mut n = 0.0;
    for other in board.footprints_in_order() {
        if other.id == fp.id || planned.contains(&other.id) {
            continue;
        }
        for pad in &other.pads {
            let Some(net) = pad.net.as_deref() else {
                continue;
            };
            if !nets.contains(&net) {
                continue;
            }
            let c = other.pad_world_center(pad);
            sx += c.x.to_mm();
            sy += c.y.to_mm();
            n += 1.0;
        }
    }
    if n < 1.0 {
        return None;
    }
    Some([sx / n, sy / n])
}

/// Clamp an along-edge coordinate so the part's bbox stays inside the
/// outline on that axis.
fn clamp_along(along: f64, pose: &Pose, side: EdgeSide, outline: Rect) -> f64 {
    let h = pose.half_along(side);
    let (lo, hi) = match side {
        EdgeSide::Left | EdgeSide::Right => (outline.min.y.to_mm() + h, outline.max.y.to_mm() - h),
        EdgeSide::Top | EdgeSide::Bottom => (outline.min.x.to_mm() + h, outline.max.x.to_mm() - h),
    };
    if lo > hi {
        f64::midpoint(lo, hi) // part wider than the board: centre it
    } else {
        along.clamp(lo, hi)
    }
}

/// Footprint position that puts `pose`'s bbox against `side` at
/// along-coordinate `along`.
fn position_for(side: EdgeSide, along: f64, pose: &Pose, outline: Rect) -> Point {
    let (cx, cy) = match side {
        EdgeSide::Left => (outline.min.x.to_mm() + pose.half[0], along),
        EdgeSide::Right => (outline.max.x.to_mm() - pose.half[0], along),
        EdgeSide::Bottom => (along, outline.min.y.to_mm() + pose.half[1]),
        EdgeSide::Top => (along, outline.max.y.to_mm() - pose.half[1]),
    };
    Point::new(
        Length::from_mm(cx - pose.bb_off[0]),
        Length::from_mm(cy - pose.bb_off[1]),
    )
}

/// Spread the parts assigned to one side along it so no two bodies come
/// closer than `clearance`, staying as near their seeds as possible.
/// `None` = they do not fit on that edge at all.
fn spread(
    items: &mut [(usize, f64, f64)], // (part index, seed, half extent along)
    lo: f64,
    hi: f64,
    clearance: f64,
) -> Option<Vec<(usize, f64)>> {
    // Seed order (tie: part index) is the along-edge order we keep, so
    // parts do not leapfrog each other and the result is deterministic.
    items.sort_by(|a, b| a.1.total_cmp(&b.1).then(a.0.cmp(&b.0)));
    let mut out: Vec<(usize, f64)> = Vec::with_capacity(items.len());
    // Forward pass: push each part up to clear the previous one.
    let mut prev: Option<(f64, f64)> = None; // (along, half)
    for (idx, seed, half) in items.iter() {
        let mut a = seed.max(lo + half);
        if let Some((pa, ph)) = prev {
            a = a.max(pa + ph + clearance + half);
        }
        prev = Some((a, *half));
        out.push((*idx, a));
    }
    // Backward pass: if the last one overshot the far end, pull the whole
    // chain back down and fail if that breaks the near end.
    if let Some(((_, last_a), (_, _, last_half))) = out.last().zip(items.last()) {
        if *last_a > hi - last_half {
            let n = out.len();
            if hi - last_half < lo + last_half {
                return None; // this part alone is longer than the edge
            }
            out[n - 1].1 = hi - last_half;
            for i in (0..n - 1).rev() {
                let (_, next_a) = out[i + 1];
                let next_half = items[i + 1].2;
                let half = items[i].2;
                let limit = next_a - next_half - clearance - half;
                if out[i].1 > limit {
                    out[i].1 = limit;
                }
                if out[i].1 < lo + half {
                    return None;
                }
            }
        }
    }
    Some(out)
}

/// One decided pose: `(part index, side, along-edge coordinate, footprint
/// position, rotation)`.
type Placement = (usize, EdgeSide, f64, Point, f32);

/// Poses for a whole assignment. `None` when some side cannot hold the
/// parts assigned to it.
fn poses_for_assignment(
    parts: &[Part],
    assign: &[usize],
    decided: &[usize],
    outline: Rect,
    clearance: f64,
) -> Option<Vec<Placement>> {
    let mut out: Vec<(usize, EdgeSide, f64, Point, f32)> = Vec::new();
    for (si, side) in SIDES.iter().enumerate() {
        let mut items: Vec<(usize, f64, f64)> = Vec::new();
        for k in decided {
            if assign[*k] != si {
                continue;
            }
            let pose = parts[*k].poses[si].as_ref()?;
            items.push((*k, pose.seed_along, pose.half_along(*side)));
        }
        if items.is_empty() {
            continue;
        }
        let (lo, hi) = match side {
            EdgeSide::Left | EdgeSide::Right => (outline.min.y.to_mm(), outline.max.y.to_mm()),
            EdgeSide::Top | EdgeSide::Bottom => (outline.min.x.to_mm(), outline.max.x.to_mm()),
        };
        let spread = spread(&mut items, lo, hi, clearance)?;
        for (k, along) in spread {
            let pose = parts[k].poses[si].as_ref()?;
            out.push((
                k,
                *side,
                along,
                position_for(*side, along, pose, outline),
                pose.rot,
            ));
        }
    }
    // Reference order (== part index order) keeps the report stable.
    out.sort_by_key(|(k, _, _, _, _)| *k);
    Some(out)
}

/// Score one full assignment; `None` = infeasible (does not fit, or some
/// part ends up illegal). Leaves the board as it found it.
#[allow(clippy::too_many_arguments)]
fn evaluate(
    board: &mut Board,
    parts: &[Part],
    assign: &[usize],
    originals: &[(Id, Point, f32)],
    nets: &[String],
    ids: &[Id],
    outline: Rect,
    margins: &MarginMap,
    clearance: f64,
    opts: &PlaceOptions,
) -> Option<f64> {
    let decided: Vec<usize> = (0..parts.len()).collect();
    evaluate_subset(
        board, parts, assign, &decided, originals, nets, ids, outline, margins, clearance, opts,
    )
}

/// Like `evaluate` but only `decided` parts are moved; the rest stay put.
/// This is what makes the greedy path meaningful.
#[allow(clippy::too_many_arguments)]
fn evaluate_subset(
    board: &mut Board,
    parts: &[Part],
    assign: &[usize],
    decided: &[usize],
    originals: &[(Id, Point, f32)],
    nets: &[String],
    ids: &[Id],
    outline: Rect,
    margins: &MarginMap,
    clearance: f64,
    opts: &PlaceOptions,
) -> Option<f64> {
    let placements = poses_for_assignment(parts, assign, decided, outline, clearance)?;
    for (k, _, _, pos, rot) in &placements {
        if let Some(fp) = board.footprints.get_mut(&parts[*k].id) {
            fp.position = *pos;
            fp.rotation = *rot;
        }
    }
    let mut ok = true;
    for (k, _, _, _, _) in &placements {
        if !is_legal(board, parts[*k].id, outline, margins, clearance, opts) {
            ok = false;
            break;
        }
    }
    let score = if ok {
        Some(cost(board, nets, ids, opts.crossing_penalty_factor))
    } else {
        None
    };
    restore(board, originals);
    score
}

/// Objective: weighted HPWL over the nets a plan can move, plus the
/// bundle-crossing penalty of the bundles the planned parts sit on.
fn cost(board: &Board, nets: &[String], ids: &[Id], crossing_factor: f64) -> f64 {
    let hpwl: f64 = nets.iter().map(|n| net_hpwl(board, n)).sum();
    hpwl + crossing_factor * crossings_involving(board, ids) as f64
}

/// Every net with a pad on a planned part, sorted and deduped.
fn affected_nets(board: &Board, planned: &HashSet<Id>) -> Vec<String> {
    let mut nets: Vec<String> = Vec::new();
    for fp in board.footprints_in_order() {
        if !planned.contains(&fp.id) {
            continue;
        }
        for pad in &fp.pads {
            if let Some(net) = pad.net.clone() {
                nets.push(net);
            }
        }
    }
    nets.sort();
    nets.dedup();
    nets
}

/// Move a footprint to a probe pose. False when the id is gone.
fn apply_probe(board: &mut Board, id: Id, pos: Point, rot: f32) -> bool {
    match board.footprints.get_mut(&id) {
        Some(fp) => {
            fp.position = pos;
            fp.rotation = rot;
            true
        }
        None => false,
    }
}

/// The same hard constraints the SA enforces, applied to the footprint as
/// it now sits on the board: pads on copper, body inside the outline, the
/// hard body-to-body clearance against everything else (including the
/// other planned parts, which are already moved at this point), and the
/// edge-mount rule that makes an edge part legal at all.
fn is_legal(
    board: &Board,
    id: Id,
    outline: Rect,
    margins: &MarginMap,
    clearance: f64,
    opts: &PlaceOptions,
) -> bool {
    let Some(fp) = board.footprints.get(&id) else {
        return false;
    };
    if !pads_inside_outline(fp, outline, opts.edge_clearance_mm) {
        return false;
    }
    if board
        .body_outline_violation(fp, margin_for_fp(fp, margins))
        .is_some()
    {
        return false;
    }
    if probe_min_gap(board, fp, margins) < clearance {
        return false;
    }
    if board.edge_mount_violation(fp).is_some() {
        return false;
    }
    true
}

/// Put every planned part back where the pass found it.
fn restore(board: &mut Board, originals: &[(Id, Point, f32)]) {
    for (id, pos, rot) in originals {
        if let Some(fp) = board.footprints.get_mut(id) {
            fp.position = *pos;
            fp.rotation = *rot;
        }
    }
}
