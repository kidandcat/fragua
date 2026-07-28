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
//! Two rules keep a real (crowded) board from making every assignment
//! infeasible, which would hand the decision back to "nearest edge":
//!
//! * **The seed is a preference, not the only candidate.** If the seeded
//!   along-edge position collides with something, the part scans the whole
//!   edge on a 1 mm grid, nearest-to-seed first, and only when NO along
//!   position is legal does the side become infeasible.
//! * **Parts the SA is about to move do not get a veto.** Run from
//!   `place()`, the planner ignores body clearance against movable
//!   non-edge footprints (`ignore`): they are re-placed by the global
//!   stage seconds later, so their stale scatter must not decide which
//!   edge a connector lives on. Fixed parts and the other planned edge
//!   parts are still checked in full. The standalone `edge-plan` verb
//!   passes an empty `ignore`: there the rest of the board is settled and
//!   every collision is real.
//!
//! No randomness anywhere: the same board always produces the same plan.

use std::collections::HashSet;

use pcb_core::{Board, EdgeSide, Footprint, Id, Length, Point, Rect};

use crate::{
    aabb_gap_mm, bundle::crossings_involving, fp_bounds_with_margin, margin_for_fp, net_hpwl,
    pads_inside_outline, MarginMap, PlaceOptions,
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

/// Grid step (mm) of the along-edge scan used when the seeded position is
/// blocked. 1 mm is finer than any body-to-body clearance we enforce, so
/// if a legal slot exists on the edge the scan lands in it; an 80 mm edge
/// is still only ~80 candidates, each a bbox sweep.
const ALONG_STEP_MM: f64 = 1.0;

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
    /// The footprint's `edge_side` after planning: the LOCAL side now
    /// against the outline. The planner re-declares it when the library's
    /// value would have forced an end-on pose (a 1×10 header declared
    /// `left`), so `edge_mount_violation` — the rule the SA and the DRC
    /// enforce — agrees with the pose that was committed.
    pub edge_side: Option<EdgeSide>,
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
    // Empty `ignore`: on the verb path the caller asked for these refs and
    // nothing else, so every other footprint on the board is settled and
    // its clearance is a real constraint.
    let mut report = plan_movable_edges(board, &ids, opts, margins, outline, &HashSet::new());
    report.skipped.splice(0..0, skipped);
    Ok(report)
}

/// Core pass, id-based: used both by the verb wrapper above and by
/// `place()` before its global stage.
///
/// `ignore` lists footprints whose body clearance must NOT block a
/// candidate pose — the movable non-edge parts `place()` is about to
/// re-place. Everything else (fixed parts, and the planned edge parts
/// against each other) is checked in full.
pub(crate) fn plan_movable_edges(
    board: &mut Board,
    movable_ids: &[Id],
    opts: &PlaceOptions,
    margins: &MarginMap,
    outline: Rect,
    ignore: &HashSet<Id>,
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
    let originals: Vec<Original> = ids
        .iter()
        .filter_map(|id| {
            board
                .footprints
                .get(id)
                .map(|fp| (*id, fp.position, fp.rotation, fp.edge_side))
        })
        .collect();
    report.initial_cost = cost(board, &nets, &ids, opts.crossing_penalty_factor);

    // --- Per (part, side): the pose we would use -----------------------------
    // Rotation is decided here, once, so the assignment search below is
    // over sides only (4^n, not 16^n). All four 90° steps are candidates
    // and GEOMETRY picks between them: the winner is the one with the
    // smallest extent PERPENDICULAR to the edge, i.e. the one that lays the
    // part's pad row ALONG the cut. A 1×10 header spanning 23 mm must never
    // be planned end-on, sticking through the middle of the board — which
    // is exactly what deriving the rotation from the declared `edge_side`
    // alone produces for a row authored along local x (its local "left" is
    // the END of the row). The declaration is only a tie-break here: among
    // the equally-parallel rotations we keep the one that honours it, which
    // is what puts a USB-C's plug face outward rather than inward.
    #[allow(clippy::needless_range_loop)] // `parts[pi]` is written inside
    for pi in 0..parts.len() {
        for (si, side) in SIDES.iter().enumerate() {
            let Some(fp) = board.footprints.get(&parts[pi].id).cloned() else {
                continue;
            };
            let mut best: Option<Cand> = None;
            for q in 0..4u32 {
                let rot = (q as f32) * 90.0;
                let Some(mut pose) = pose_for(&fp, rot, *side, board, &planned, outline) else {
                    continue;
                };
                // When the declared local side would NOT be the one against
                // this edge, the pose stays legal only if the footprint's
                // declaration is updated to name the side that actually
                // faces the cut — `edge_mount_violation` (SA, DRC) checks
                // exactly that. `None` = nothing to change.
                let honoured = fp
                    .edge_side
                    .is_none_or(|local| local.world_side(rot) == *side);
                pose.local_side = if fp.edge_side.is_some() && !honoured {
                    Some(EdgeSide::local_facing(*side, rot))
                } else {
                    None
                };
                // Legality here means "legal SOMEWHERE on this edge", so a
                // blocked seed does not make every rotation look equally
                // bad; the cost is still read at the position the part
                // would actually take.
                let along = scan_along(
                    board,
                    parts[pi].id,
                    &pose,
                    *side,
                    outline,
                    margins,
                    hard_clearance,
                    opts,
                    ignore,
                );
                let ok = along.is_some();
                let along =
                    along.unwrap_or_else(|| clamp_along(pose.seed_along, &pose, *side, outline));
                let pos = position_for(*side, along, &pose, outline);
                apply_probe(board, parts[pi].id, pos, rot, pose.local_side);
                let c = cost(board, &nets, &ids, opts.crossing_penalty_factor);
                restore(board, &originals);
                let cand = Cand {
                    ok,
                    perp: pose.half_perp(*side) * 2.0,
                    honoured,
                    cost: c,
                    rot,
                    pose,
                };
                if best.as_ref().is_none_or(|b| cand.beats(b)) {
                    best = Some(cand);
                }
            }
            parts[pi].poses[si] = best.map(|c| c.pose);
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
                ignore,
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
                    ignore,
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
            ignore,
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
            report.skipped.push(no_legal_edge(&p.reference));
        }
        return report;
    };

    // --- Apply the winner ----------------------------------------------------
    // `realise` is the SAME routine the search scored with (spread + along
    // repair), so what lands on the board is exactly what won — no second,
    // subtly different layout pass.
    let decided: Vec<usize> = (0..parts.len())
        .filter(|k| assign[*k] != usize::MAX)
        .collect();
    restore(board, &originals);
    let placements = realise(
        board,
        &parts,
        &assign,
        &decided,
        outline,
        margins,
        hard_clearance,
        opts,
        ignore,
    );
    if let Some(placements) = placements {
        for p in placements {
            report.placed.push(EdgePlacement {
                reference: parts[p.part].reference.clone(),
                side: p.side,
                along_mm: p.along,
                position: p.pos,
                rotation: p.rot,
                // What the footprint's declaration IS now, so a caller
                // holding its own copy of the board (the script tools do)
                // can keep it in sync.
                edge_side: board
                    .footprints
                    .get(&parts[p.part].id)
                    .and_then(|f| f.edge_side),
            });
        }
    }
    for k in 0..parts.len() {
        if assign[k] == usize::MAX {
            report.skipped.push(no_legal_edge(&parts[k].reference));
        }
    }
    report.final_cost = score;
    report
}

/// Skip line for a part with no legal pose anywhere. The scan already
/// walked every along position on all four edges, so the only way out is
/// to free room — say so instead of leaving the agent guessing.
fn no_legal_edge(reference: &str) -> String {
    format!(
        "{reference}: no legal edge found — every along position on all four edges collides \
         with a part that is not being placed; run `auto-place` (which lets the interior parts \
         move too) or clear the edge first"
    )
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
    /// New value for the footprint's `edge_side` (the LOCAL side that ends
    /// up against the cut), or `None` to leave the declaration alone.
    /// Written whenever a part declares a side that this rotation does not
    /// put on the outline — the geometry wins, and the declaration is
    /// corrected so `edge_mount_violation` still passes for the pose we
    /// commit. See the pre-pass in `plan_movable_edges`.
    local_side: Option<EdgeSide>,
}

impl Pose {
    /// Half extent along the edge (the axis the part slides on).
    fn half_along(&self, side: EdgeSide) -> f64 {
        match side {
            EdgeSide::Left | EdgeSide::Right => self.half[1],
            EdgeSide::Top | EdgeSide::Bottom => self.half[0],
        }
    }

    /// Half extent PERPENDICULAR to the edge — how far the part reaches
    /// into the board. Minimising it is what keeps a pin header's pad row
    /// parallel to the cut instead of driving it through the middle of the
    /// board.
    fn half_perp(&self, side: EdgeSide) -> f64 {
        match side {
            EdgeSide::Left | EdgeSide::Right => self.half[0],
            EdgeSide::Top | EdgeSide::Bottom => self.half[1],
        }
    }
}

/// One rotation candidate for a (part, side), with everything the ranking
/// needs. See `Cand::beats` for the order.
struct Cand {
    /// Some along position on this edge is legal at this rotation.
    ok: bool,
    /// Full extent perpendicular to the edge, mm (smaller = more parallel).
    perp: f64,
    /// The footprint's declared `edge_side` already faces this edge, so no
    /// re-declaration is needed.
    honoured: bool,
    cost: f64,
    rot: f32,
    pose: Pose,
}

impl Cand {
    /// Ranking, most significant first: the pose that reaches LEAST far into
    /// the board (pad row along the cut); then legality; then the one that
    /// honours the library's declared side (this is what flips a USB-C's
    /// plug face outward); then cost; then the lowest rotation, so ties are
    /// always resolved the same way.
    ///
    /// Geometry outranks legality ON PURPOSE. Legality here is measured with
    /// the OTHER planned parts still at their old positions, so a connector
    /// that will happily share the top edge once both are spread looks
    /// "blocked" now — and ranking legality first then buys a legal-looking
    /// pose by turning a 24 mm header end-on through the middle of the
    /// board. Orientation relative to the cut is a hard design property;
    /// collisions are what the spread and the along-scan in `realise` exist
    /// to resolve, and a side that truly cannot hold the part is rejected
    /// there (or, in the last resort, reported as skipped).
    fn beats(&self, other: &Self) -> bool {
        if (self.perp - other.perp).abs() > 1e-6 {
            return self.perp < other.perp;
        }
        if self.ok != other.ok {
            return self.ok;
        }
        if self.honoured != other.honoured {
            return self.honoured;
        }
        if (self.cost - other.cost).abs() > 1e-9 {
            return self.cost < other.cost;
        }
        self.rot < other.rot
    }
}

/// A planned part's pose before the pass touched it: `(id, position,
/// rotation, declared edge side)`. All four are restored between probes.
type Original = (Id, Point, f32, Option<EdgeSide>);

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
        local_side: None,
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

/// Along-edge range the part's bbox centre may take and still fit inside
/// the outline on that axis.
fn along_limits(pose: &Pose, side: EdgeSide, outline: Rect) -> (f64, f64) {
    let h = pose.half_along(side);
    match side {
        EdgeSide::Left | EdgeSide::Right => (outline.min.y.to_mm() + h, outline.max.y.to_mm() - h),
        EdgeSide::Top | EdgeSide::Bottom => (outline.min.x.to_mm() + h, outline.max.x.to_mm() - h),
    }
}

/// Clamp an along-edge coordinate so the part's bbox stays inside the
/// outline on that axis.
fn clamp_along(along: f64, pose: &Pose, side: EdgeSide, outline: Rect) -> f64 {
    let (lo, hi) = along_limits(pose, side, outline);
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

/// One decided pose, as laid out on the board.
#[derive(Clone)]
struct Placement {
    /// Index into `parts`.
    part: usize,
    side: EdgeSide,
    /// Bbox-centre coordinate along the edge, mm.
    along: f64,
    pos: Point,
    rot: f32,
    /// `edge_side` re-declaration this pose needs, if any.
    local_side: Option<EdgeSide>,
}

/// Poses for a whole assignment. `None` when some side cannot hold the
/// parts assigned to it.
fn poses_for_assignment(
    parts: &[Part],
    assign: &[usize],
    decided: &[usize],
    outline: Rect,
    clearance: f64,
) -> Option<Vec<Placement>> {
    let mut out: Vec<Placement> = Vec::new();
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
            out.push(Placement {
                part: k,
                side: *side,
                along,
                pos: position_for(*side, along, pose, outline),
                rot: pose.rot,
                local_side: pose.local_side,
            });
        }
    }
    // Reference order (== part index order) keeps the report stable.
    out.sort_by_key(|p| p.part);
    Some(out)
}

/// Lay an assignment out on the board for real: spread the parts on each
/// side, then repair — with an along-edge scan — anyone the spread left
/// illegal. Returns the placements ACTUALLY used (their `along` reflects
/// any repair) and leaves the board holding them; `None` = infeasible.
///
/// Search and apply both go through here, so what the winner scored is
/// exactly what lands on the board.
#[allow(clippy::too_many_arguments)]
fn realise(
    board: &mut Board,
    parts: &[Part],
    assign: &[usize],
    decided: &[usize],
    outline: Rect,
    margins: &MarginMap,
    clearance: f64,
    opts: &PlaceOptions,
    ignore: &HashSet<Id>,
) -> Option<Vec<Placement>> {
    let mut placements = poses_for_assignment(parts, assign, decided, outline, clearance)?;
    for p in &placements {
        apply_probe(board, parts[p.part].id, p.pos, p.rot, p.local_side);
    }
    // Repair in part order. Moving a part can only ever free space for the
    // ones after it (clearance is symmetric), so a single pass suffices and
    // a part whose blocker has already moved away needs no scan at all.
    for p in &mut placements {
        let id = parts[p.part].id;
        if is_legal(board, id, outline, margins, clearance, opts, ignore) {
            continue;
        }
        let pose = parts[p.part].poses[assign[p.part]].as_ref()?;
        let along = scan_along(
            board, id, pose, p.side, outline, margins, clearance, opts, ignore,
        )?;
        p.along = along;
        p.pos = position_for(p.side, along, pose, outline);
        apply_probe(board, id, p.pos, p.rot, p.local_side);
    }
    Some(placements)
}

/// First legal along-edge position for `id` at `pose` on `side`, searched
/// nearest-to-seed first. Leaves the board holding that pose when it finds
/// one (the caller re-applies anyway); `None` = the whole edge is blocked.
///
/// This is what stops a crowded board from silently falling back to
/// "nearest edge": one blocked seed used to make the entire side — and
/// therefore, often, every assignment — infeasible.
#[allow(clippy::too_many_arguments)]
fn scan_along(
    board: &mut Board,
    id: Id,
    pose: &Pose,
    side: EdgeSide,
    outline: Rect,
    margins: &MarginMap,
    clearance: f64,
    opts: &PlaceOptions,
    ignore: &HashSet<Id>,
) -> Option<f64> {
    let (before_pos, before_rot, before_side) = {
        let fp = board.footprints.get(&id)?;
        (fp.position, fp.rotation, fp.edge_side)
    };
    let (lo, hi) = along_limits(pose, side, outline);
    let mut found = None;
    for along in along_candidates(pose.seed_along, lo, hi) {
        let pos = position_for(side, along, pose, outline);
        apply_probe(board, id, pos, pose.rot, pose.local_side);
        if is_legal(board, id, outline, margins, clearance, opts, ignore) {
            found = Some(along);
            break;
        }
    }
    if found.is_none() {
        apply_probe(board, id, before_pos, before_rot, None);
        if let Some(fp) = board.footprints.get_mut(&id) {
            fp.edge_side = before_side;
        }
    }
    found
}

/// Along-edge candidates: the seed first, then a `ALONG_STEP_MM` grid over
/// the legal range, ordered by distance from the seed (ties resolve to the
/// lower coordinate). Deterministic and independent of where the part
/// currently sits.
fn along_candidates(seed: f64, lo: f64, hi: f64) -> Vec<f64> {
    if lo > hi {
        // Part longer than the edge: only the centred pose exists, and the
        // legality check will decide whether it is usable.
        return vec![f64::midpoint(lo, hi)];
    }
    let mut out = vec![seed.clamp(lo, hi)];
    let steps = ((hi - lo) / ALONG_STEP_MM).floor().max(0.0) as usize;
    for i in 0..=steps {
        out.push((lo + i as f64 * ALONG_STEP_MM).min(hi));
    }
    out.push(hi);
    out.sort_by(|a, b| {
        (a - seed)
            .abs()
            .total_cmp(&(b - seed).abs())
            .then(a.total_cmp(b))
    });
    out.dedup_by(|a, b| (*a - *b).abs() < 1e-9);
    out
}

/// Score one full assignment; `None` = infeasible (does not fit, or some
/// part has no legal along position anywhere on its side). Leaves the
/// board as it found it.
#[allow(clippy::too_many_arguments)]
fn evaluate(
    board: &mut Board,
    parts: &[Part],
    assign: &[usize],
    originals: &[Original],
    nets: &[String],
    ids: &[Id],
    outline: Rect,
    margins: &MarginMap,
    clearance: f64,
    opts: &PlaceOptions,
    ignore: &HashSet<Id>,
) -> Option<f64> {
    let decided: Vec<usize> = (0..parts.len()).collect();
    evaluate_subset(
        board, parts, assign, &decided, originals, nets, ids, outline, margins, clearance, opts,
        ignore,
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
    originals: &[Original],
    nets: &[String],
    ids: &[Id],
    outline: Rect,
    margins: &MarginMap,
    clearance: f64,
    opts: &PlaceOptions,
    ignore: &HashSet<Id>,
) -> Option<f64> {
    let score = realise(
        board, parts, assign, decided, outline, margins, clearance, opts, ignore,
    )
    .map(|_| cost(board, nets, ids, opts.crossing_penalty_factor));
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

/// Move a footprint to a probe pose, re-declaring its `edge_side` when the
/// pose needs it (`local_side = Some`). False when the id is gone.
fn apply_probe(
    board: &mut Board,
    id: Id,
    pos: Point,
    rot: f32,
    local_side: Option<EdgeSide>,
) -> bool {
    match board.footprints.get_mut(&id) {
        Some(fp) => {
            fp.position = pos;
            fp.rotation = rot;
            if local_side.is_some() {
                fp.edge_side = local_side;
            }
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
///
/// Footprints in `ignore` are exempt from the clearance check only — the
/// outline and edge-mount rules are about the board itself and always
/// hold. See the module header for why a movable non-edge part must not
/// get a veto here.
#[allow(clippy::too_many_arguments)]
fn is_legal(
    board: &Board,
    id: Id,
    outline: Rect,
    margins: &MarginMap,
    clearance: f64,
    opts: &PlaceOptions,
    ignore: &HashSet<Id>,
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
    if min_body_gap(board, fp, margins, ignore) < clearance {
        return false;
    }
    if board.edge_mount_violation(fp).is_some() {
        return false;
    }
    true
}

/// Smallest body-to-body gap (mm, margins folded in) between `fp` and
/// every other footprint except the ones in `ignore`. Same measure as the
/// placer's `probe_min_gap`, with the exemption set added.
fn min_body_gap(board: &Board, fp: &Footprint, margins: &MarginMap, ignore: &HashSet<Id>) -> f64 {
    let Some(b) = fp_bounds_with_margin(fp, margins) else {
        return f64::INFINITY;
    };
    board
        .footprints_in_order()
        .filter(|o| {
            o.id != fp.id && !ignore.contains(&o.id) && crate::bodies_interact(fp, o, margins)
        })
        .filter_map(|o| fp_bounds_with_margin(o, margins).map(|ob| aabb_gap_mm(b, ob)))
        .fold(f64::INFINITY, f64::min)
}

/// Put every planned part back exactly as the pass found it — pose AND
/// declared edge side, since a probe may have re-declared the latter.
fn restore(board: &mut Board, originals: &[Original]) {
    for (id, pos, rot, side) in originals {
        if let Some(fp) = board.footprints.get_mut(id) {
            fp.position = *pos;
            fp.rotation = *rot;
            fp.edge_side = *side;
        }
    }
}
