//! Global escape-slot assignment for fine-pitch footprints.
//!
//! # Why this exists
//!
//! On a 0.4 mm-pitch QFN no trace can pass between two pins (a 0.2 mm
//! channel where a 0.25 mm trace at a 0.12 mm rule needs 0.245 mm from its
//! centreline to foreign copper), so **every** inner escape is a via. The
//! escape *positions* — not the corridors between them — are the scarce
//! resource.
//!
//! The historical fanout picked one dogbone per pad, in pad order, from a
//! short ladder of on-axis depths, and never revisited the choice. Each
//! committed barrel is stamped on every layer and cannot be ripped, so a
//! pin that grabbed a convenient slot walled off the two pins beside it.
//! The v6 negotiation autopsy proved this is what the plateau is made of:
//! a dozen nets could not reach their U1 pad even when allowed to route
//! straight through every other net's copper.
//!
//! # What this does instead
//!
//! 1. **Slot generation** — a lattice of candidate barrel sites per package
//!    side: several depth ranks **outward** (open board, effectively
//!    unbounded room) and **inward** (the channel left by the shrunk
//!    thermal pad), each sampled at sub-pitch lateral offsets so a barrel
//!    may sit between two pins rather than only on a pin's own axis.
//! 2. **Assignment** — pad → slot is solved as a **min-cost max-cardinality
//!    bipartite matching** (successive shortest paths / MCMF), so the set is
//!    chosen together instead of first-come-first-served. An edge exists
//!    only when the barrel fits *and* the pad → barrel copper stub is
//!    layable (`fanout::dogbone_stub`, the same legality check the greedy
//!    path used).
//! 3. **Exit planning** — a slot is only useful if the router can leave it.
//!    The assignment is iterated: after each round the bottom-layer exit ray
//!    from every assigned barrel toward its net's partner is probed against
//!    the other assigned barrels, and the cost of blocking / blocked slots
//!    is raised for the next round (PathFinder's idea, applied to escape
//!    slots rather than to routing cells).
//! 4. **Legalisation** — matching edges are scored against *static* geometry,
//!    so two chosen slots can still conflict with each other. Assignments
//!    are committed cheapest-first against the live board; whatever fails is
//!    re-matched against the copper that did land, for a few rounds. Pads
//!    that survive that with no legal slot are reported as **stranded** —
//!    known before routing starts instead of silently dropped.
//!
//! # Deviation from a single footprint-wide matching
//!
//! The matching runs **per package side**, sides committed in order. A
//! pad's candidate slots never leave its own side (they are reach-limited to
//! ~1.2 pitches laterally), so the decomposition is exact apart from
//! via-to-via clearance across a corner — and that is handled because each
//! side is legalised against the copper the previous sides committed. It
//! keeps the flow instances tiny (≤ 14 pads × a few hundred slots).

use std::collections::{BTreeMap, HashMap};

use pcb_core::{Board, Id, Length, Point, RuleResolver, Trace, Via};

use crate::fanout::{
    dogbone_stub, fanout_via_fits, inward_axis, point_seg_dist, FanoutPlan, PadRect, PadRules,
};
use crate::router::RouteOptions;

/// A footprint with at least this many pads that need a DOGBONE escape
/// (i.e. a via-in-pad does not fit) goes through the global assignment.
/// Below it the greedy per-pad ladder is both adequate and cheaper — a
/// 2-pin passive has no assignment problem to solve.
pub(crate) const SLOT_ASSIGN_MIN_PADS: usize = 6;

/// Depth ranks generated OUTWARD from the pad row (away from the package
/// centre). Outward is open board on a QFN, so this is where most escapes
/// should land; four ranks is enough for a 14-pin side to fan out with
/// every third pin on a rank.
const RANKS_OUT: usize = 5;
/// Depth ranks generated INWARD, into the channel a shrunk thermal pad
/// leaves. Bounded by the exposed pad, so fewer.
const RANKS_IN: usize = 3;

/// Lateral offsets (as a multiple of the side's pad pitch) tried for every
/// depth rank. Sub-pitch offsets are what let a barrel sit *between* two
/// pins, which is the only way a stub from the pin behind it can thread the
/// gap: a straight stub passing a barrel at exactly one pitch (0.4 mm) needs
/// a width of ≤ 0.11 mm, under the 0.12 mm floor.
const LATERAL_OFFSETS: [f64; 13] = [
    0.0, 0.25, -0.25, 0.5, -0.5, 0.75, -0.75, 1.0, -1.0, 0.125, -0.125, 0.375, -0.375,
];

/// Cost weight per mm of pad → barrel stub. Dominant term: short stubs keep
/// copper off the neighbours' lanes.
const W_LEN: f64 = 10.0;
/// Cost weight on `1 - cos θ` between the escape direction and the direction
/// the net actually has to travel. 0 when the barrel is dead ahead of the
/// net's partner, `2 × W_DIR` when it points the opposite way.
const W_DIR: f64 = 1.2;
/// Cost per already-assigned barrel sitting in this slot's bottom-layer exit
/// ray (this slot cannot get out).
const W_BLOCKED: f64 = 3.0;
/// Cost per other net's exit ray this slot would sit in (this slot boxes
/// someone else in). Symmetric partner of `W_BLOCKED`.
const W_CROWD: f64 = 2.0;
/// How far (mm) the bottom-layer exit ray is probed from a barrel.
const EXIT_PROBE_MM: f64 = 3.0;
/// Cost per unit of "tightness" between two chosen barrels. Two barrels
/// that merely satisfy via-to-via CLEARANCE (≈ 0.57 mm at the stress
/// board's fine area) still leave no lane a trace can be routed through —
/// on the coarse grid they are one solid wall, which is precisely how the
/// old greedy fanout boxed its own pins in. Tightness is the shortfall
/// against `lane_mm` (the width a trace plus two clearances actually
/// needs), normalised to 1 at coincident barrels.
const W_TIGHT: f64 = 8.0;
/// Assignment rounds. Round 1 has no congestion information; rounds 2..N
/// price the previous round's exit blockage.
const ASSIGN_ROUNDS: usize = 3;
/// How many times pads that lost their slot during legalisation are
/// re-matched against the copper that did land.
const REPAIR_ROUNDS: usize = 3;

/// A pad that needs a dogbone escape, with everything the assignment needs.
pub(crate) struct SlotTarget {
    pub pad_ref: String,
    pub pad_number: String,
    pub net: String,
    pub layer: pcb_core::CopperLayer,
    pub cx: f64,
    pub cy: f64,
    pub hw: f64,
    pub hh: f64,
    /// Where this pad's net has to travel: centroid of the net's pads
    /// outside this footprint (or, failing that, straight out of the
    /// package). Drives the escape-direction cost term.
    pub tx: f64,
    pub ty: f64,
    /// Widest stub this pad may grow (capped by the pad's own width).
    pub max_stub_w: f64,
}

/// Candidate barrel site.
#[derive(Clone, Copy)]
struct Slot {
    x: f64,
    y: f64,
}

/// One feasible pad → slot pairing, priced against static geometry.
struct Edge {
    slot: usize,
    /// Static part of the cost (length + direction). Congestion terms are
    /// added per round.
    base_cost: f64,
    /// Unit vector of this pad's travel direction, cached for the exit probe.
    dir: (f64, f64),
}

/// Outcome of the assignment for one footprint.
pub(crate) struct SlotOutcome {
    /// Pads that got no legal slot at all — geometrically stranded.
    pub stranded: Vec<String>,
}

/// Assign escape slots for `targets` and commit the winning barrels and
/// stubs into `plan` / `work`.
#[allow(clippy::too_many_arguments)]
pub(crate) fn assign_escape_slots(
    fp: &pcb_core::Footprint,
    targets: &[SlotTarget],
    foreign: &[&PadRect],
    work: &mut Board,
    opts: &RouteOptions,
    resolver: &RuleResolver<'_>,
    fab: Option<&pcb_core::FabRules>,
    fp_cx: f64,
    fp_cy: f64,
    // Barrel sites this assignment may NOT use, in mm. The driver fills
    // this when it rips a barrel whose net kept failing: handing the same
    // site straight back would reproduce the board we just gave up on.
    banned: &[(f64, f64)],
    plan: &mut FanoutPlan,
) -> SlotOutcome {
    let mut stranded = Vec::new();
    // Group the pads by the package side they sit on (their escape axis).
    let mut sides: BTreeMap<(i8, i8), Vec<usize>> = BTreeMap::new();
    for (i, t) in targets.iter().enumerate() {
        let (dx, dy) = inward_axis(t.cx, t.cy, t.hw, t.hh, fp_cx, fp_cy);
        sides.entry((dx as i8, dy as i8)).or_default().push(i);
    }
    for (key, mut group) in sides {
        // Along-side coordinate: the axis perpendicular to the escape axis.
        let (inx, iny) = (key.0 as f64, key.1 as f64);
        let (px, py) = (-iny, inx);
        let along = |t: &SlotTarget| t.cx * px + t.cy * py;
        group.sort_by(|&a, &b| {
            along(&targets[a])
                .partial_cmp(&along(&targets[b]))
                .unwrap_or(std::cmp::Ordering::Equal)
                .then(targets[a].pad_number.cmp(&targets[b].pad_number))
        });
        let pitch = side_pitch(targets, &group, px, py);
        let (slots, lane_mm) = generate_slots(
            targets, &group, work, opts, resolver, fab, inx, iny, px, py, pitch,
        );
        let mut pending: Vec<usize> = group.clone();
        for round in 0..=REPAIR_ROUNDS {
            if pending.is_empty() {
                break;
            }
            let mut taken = collect_taken(&slots, work, resolver, opts);
            for (i, s) in slots.iter().enumerate() {
                if banned
                    .iter()
                    .any(|&(bx, by)| (s.x - bx).abs() < 1e-6 && (s.y - by).abs() < 1e-6)
                {
                    taken[i] = true;
                }
            }
            let edges = build_edges(
                targets, &pending, &slots, &taken, foreign, work, opts, resolver, fab,
            );
            let standing: Vec<(f64, f64, String)> = work
                .vias
                .iter()
                .map(|v| (v.position.x.to_mm(), v.position.y.to_mm(), v.net.clone()))
                .collect();
            let chosen = solve_rounds(
                &edges,
                &slots,
                slots.len(),
                &standing,
                targets,
                &pending,
                lane_mm,
            );
            let (committed, left) = commit(
                targets, &pending, &chosen, &edges, &slots, foreign, work, opts, resolver, fab,
                plan,
            );
            if committed == 0 || round == REPAIR_ROUNDS {
                stranded.extend(left.iter().map(|&i| targets[i].pad_ref.clone()));
                pending.clear();
                break;
            }
            pending = left;
        }
        stranded.extend(pending.iter().map(|&i| targets[i].pad_ref.clone()));
    }
    stranded.sort();
    let _ = fp;
    SlotOutcome { stranded }
}

/// Median spacing between consecutive pads along a package side. Falls back
/// to 0.4 mm (a standard fine QFN pitch) for degenerate groups.
fn side_pitch(targets: &[SlotTarget], group: &[usize], px: f64, py: f64) -> f64 {
    let mut gaps: Vec<f64> = group
        .windows(2)
        .map(|w| {
            let a = targets[w[0]].cx * px + targets[w[0]].cy * py;
            let b = targets[w[1]].cx * px + targets[w[1]].cy * py;
            (b - a).abs()
        })
        .filter(|g| *g > 1e-6)
        .collect();
    if gaps.is_empty() {
        return 0.4;
    }
    gaps.sort_by(|a, b| a.partial_cmp(b).unwrap_or(std::cmp::Ordering::Equal));
    gaps[gaps.len() / 2]
}

/// Build the candidate barrel lattice for one package side.
///
/// Depth is measured from the pad row's outer / inner face so a whole side
/// shares one set of ranks (barrels line up in clean rows, which is what
/// leaves lanes between them). Lateral positions come from each pad's own
/// along-side coordinate plus `LATERAL_OFFSETS`, deduped to 1 µm.
#[allow(clippy::too_many_arguments)]
fn generate_slots(
    targets: &[SlotTarget],
    group: &[usize],
    work: &Board,
    opts: &RouteOptions,
    resolver: &RuleResolver<'_>,
    fab: Option<&pcb_core::FabRules>,
    inx: f64,
    iny: f64,
    px: f64,
    py: f64,
    pitch: f64,
) -> (Vec<Slot>, f64) {
    // Representative barrel geometry for the side: every pad of one package
    // resolves the same rule area in practice, and the per-pad rules are
    // re-derived exactly when the barrel is actually placed.
    let rep = &targets[group[0]];
    let rules = PadRules::at_pad(
        resolver,
        &rep.net,
        Point::new(Length::from_mm(rep.cx), Length::from_mm(rep.cy)),
        rep.layer,
        fab,
    );
    let via_r = rules.via_diameter / 2.0;
    let step = 2.0 * via_r + rules.base();
    // Outer face of the pad row (largest outward extent) and inner face.
    let mut face_out = f64::NEG_INFINITY;
    let mut face_in = f64::INFINITY;
    for &i in group {
        let t = &targets[i];
        let depth = -(t.cx * inx + t.cy * iny); // outward is -inward
        let extent = inx.abs() * t.hw + iny.abs() * t.hh;
        face_out = face_out.max(depth + extent);
        face_in = face_in.min(depth - extent);
    }
    let base = via_r + 0.05;
    let mut depths: Vec<f64> = Vec::with_capacity(RANKS_OUT + RANKS_IN);
    for k in 0..RANKS_OUT {
        depths.push(face_out + base + k as f64 * step);
    }
    for k in 0..RANKS_IN {
        depths.push(face_in - base - k as f64 * step);
    }
    let mut seen: BTreeMap<(i64, i64), Slot> = BTreeMap::new();
    for &i in group {
        let t = &targets[i];
        let a = t.cx * px + t.cy * py;
        for off in LATERAL_OFFSETS {
            let lat = a + off * pitch;
            for &d in &depths {
                // Outward depth `d` means the point sits at -inward * d.
                let x = -inx * d + px * lat;
                let y = -iny * d + py * lat;
                if !fanout_via_fits(x, y, &t.net, via_r, &rules, &[], work) {
                    // Cheap board-edge / existing-via reject only; the real
                    // per-pad test (which needs the pad's own net) happens
                    // when the edge is built.
                    continue;
                }
                seen.entry((
                    (x * 1_000_000.0).round() as i64,
                    (y * 1_000_000.0).round() as i64,
                ))
                .or_insert(Slot { x, y });
            }
        }
    }
    // The lane a routed trace needs between two barrels: both barrel radii,
    // both clearances and the trace itself, plus one search cell of
    // quantisation slack (a lane the grid cannot represent does not exist).
    let lane = 2.0 * (via_r + rules.base() + opts.trace_width.to_mm() / 2.0) + opts.cell.to_mm();
    (seen.into_values().collect(), lane)
}

/// Slots already occupied by copper on `work` (barrels committed by an
/// earlier side or footprint) — they can never be handed out again.
fn collect_taken(
    slots: &[Slot],
    work: &Board,
    resolver: &RuleResolver<'_>,
    opts: &RouteOptions,
) -> Vec<bool> {
    let _ = (resolver, opts);
    slots
        .iter()
        .map(|s| {
            work.vias.iter().any(|v| {
                (v.position.x.to_mm() - s.x).abs() < 1e-6
                    && (v.position.y.to_mm() - s.y).abs() < 1e-6
            })
        })
        .collect()
}

/// Price every legal pad → slot pairing against the copper currently on
/// `work`. This is the expensive part (one `dogbone_stub` per candidate),
/// so candidates are reach-limited first.
#[allow(clippy::too_many_arguments)]
fn build_edges(
    targets: &[SlotTarget],
    pending: &[usize],
    slots: &[Slot],
    taken: &[bool],
    foreign: &[&PadRect],
    work: &Board,
    opts: &RouteOptions,
    resolver: &RuleResolver<'_>,
    fab: Option<&pcb_core::FabRules>,
) -> Vec<Vec<Edge>> {
    let mut out: Vec<Vec<Edge>> = Vec::with_capacity(pending.len());
    for &i in pending {
        let t = &targets[i];
        let rules = PadRules::at_pad(
            resolver,
            &t.net,
            Point::new(Length::from_mm(t.cx), Length::from_mm(t.cy)),
            t.layer,
            fab,
        );
        let via_r = rules.via_diameter / 2.0;
        // Longest stub worth considering: past this the copper is thin,
        // long, and crosses everybody else's lanes.
        let max_reach =
            t.hw.max(t.hh) + via_r + 0.1 + RANKS_OUT as f64 * (2.0 * via_r + rules.base());
        let (mut dx, mut dy) = (t.tx - t.cx, t.ty - t.cy);
        let dlen = (dx * dx + dy * dy).sqrt();
        if dlen > 1e-9 {
            dx /= dlen;
            dy /= dlen;
        } else {
            dx = 0.0;
            dy = 0.0;
        }
        let mut edges = Vec::new();
        for (si, s) in slots.iter().enumerate() {
            if taken[si] {
                continue;
            }
            let (ex, ey) = (s.x - t.cx, s.y - t.cy);
            let len = (ex * ex + ey * ey).sqrt();
            if len > max_reach + 1e-9 || len < 1e-9 {
                continue;
            }
            if !fanout_via_fits(s.x, s.y, &t.net, via_r, &rules, foreign, work) {
                continue;
            }
            if dogbone_stub(
                t.layer,
                t.cx,
                t.cy,
                s.x,
                s.y,
                &t.net,
                t.max_stub_w,
                &rules,
                foreign,
                work,
            )
            .is_none()
            {
                continue;
            }
            let cos = if dlen > 1e-9 {
                (ex * dx + ey * dy) / len
            } else {
                1.0
            };
            let base_cost = W_LEN * len + W_DIR * (1.0 - cos);
            edges.push(Edge {
                slot: si,
                base_cost,
                dir: (dx, dy),
            });
        }
        out.push(edges);
    }
    let _ = opts;
    out
}

/// Run the matching `ASSIGN_ROUNDS` times, pricing the previous round's
/// exit blockage into the next. Returns the best assignment found
/// (most pads matched, then lowest congested cost).
fn solve_rounds(
    edges: &[Vec<Edge>],
    slots: &[Slot],
    n_slots: usize,
    standing: &[(f64, f64, String)],
    targets: &[SlotTarget],
    pending: &[usize],
    lane_mm: f64,
) -> Vec<Option<usize>> {
    let mut penalty: HashMap<usize, f64> = HashMap::new();
    let mut best: Vec<Option<usize>> = Vec::new();
    let mut best_key = (usize::MAX, f64::INFINITY);
    for _ in 0..ASSIGN_ROUNDS {
        let mut adj: Vec<Vec<(usize, i64)>> = Vec::with_capacity(edges.len());
        for pad_edges in edges {
            let mut row: Vec<(usize, i64)> = pad_edges
                .iter()
                .map(|e| {
                    let c = e.base_cost + penalty.get(&e.slot).copied().unwrap_or(0.0);
                    (e.slot, (c * 10_000.0).round() as i64)
                })
                .collect();
            row.sort_by_key(|&(s, c)| (c, s));
            adj.push(row);
        }
        let assign = min_cost_matching(edges.len(), n_slots, &adj);
        // Score it: matched count, then true cost + exit blockage.
        let placed: Vec<(usize, usize)> = assign
            .iter()
            .enumerate()
            .filter_map(|(p, s)| s.map(|s| (p, s)))
            .collect();
        let (blocked, crowd, tight) =
            exit_pressure(&placed, edges, slots, standing, targets, pending, lane_mm);
        let mut cost = 0.0;
        for &(p, s) in &placed {
            if let Some(e) = edges[p].iter().find(|e| e.slot == s) {
                cost += e.base_cost;
            }
        }
        let total: f64 = cost
            + blocked.values().sum::<f64>() * W_BLOCKED
            + crowd.values().sum::<f64>() * W_CROWD
            + tight.values().sum::<f64>() * W_TIGHT;
        let unmatched = assign.iter().filter(|a| a.is_none()).count();
        if (unmatched, total) < best_key {
            best_key = (unmatched, total);
            best = assign;
        }
        // Price this round's pressure into the next.
        for (slot, n) in blocked.iter().chain(crowd.iter()) {
            *penalty.entry(*slot).or_insert(0.0) += n * W_BLOCKED.max(W_CROWD) * 0.5;
        }
        for (slot, n) in tight.iter() {
            *penalty.entry(*slot).or_insert(0.0) += n * W_TIGHT;
        }
    }
    best
}

/// For an assignment, how much every chosen slot is blocked (barrels in its
/// own bottom-layer exit ray) and how much it blocks (other nets' exit rays
/// it sits in). Keyed by slot index so the next round can price it.
fn exit_pressure(
    placed: &[(usize, usize)],
    edges: &[Vec<Edge>],
    slots: &[Slot],
    standing: &[(f64, f64, String)],
    targets: &[SlotTarget],
    pending: &[usize],
    lane_mm: f64,
) -> (
    HashMap<usize, f64>,
    HashMap<usize, f64>,
    HashMap<usize, f64>,
) {
    let mut blocked: HashMap<usize, f64> = HashMap::new();
    let mut crowd: HashMap<usize, f64> = HashMap::new();
    let mut tight: HashMap<usize, f64> = HashMap::new();
    // Barrels packed tighter than one routable lane wall each other (and
    // everyone behind them) off. Charge both ends of every tight pair.
    for (i, &(_, s)) in placed.iter().enumerate() {
        for &(_, t) in placed.iter().skip(i + 1) {
            if s == t {
                continue;
            }
            let d = ((slots[s].x - slots[t].x).powi(2) + (slots[s].y - slots[t].y).powi(2)).sqrt();
            if d < lane_mm {
                let pressure = (lane_mm - d) / lane_mm;
                *tight.entry(s).or_insert(0.0) += pressure;
                *tight.entry(t).or_insert(0.0) += pressure;
            }
        }
        // …and against barrels already committed, which cannot move.
        for (ox, oy, _) in standing {
            let d = ((slots[s].x - ox).powi(2) + (slots[s].y - oy).powi(2)).sqrt();
            if d < lane_mm {
                *tight.entry(s).or_insert(0.0) += (lane_mm - d) / lane_mm;
            }
        }
    }
    // Barrel radius + trace half width + clearance is board-specific; the
    // exit probe only needs an order-of-magnitude corridor, so use a fixed
    // 0.45 mm — a 0.45 mm barrel and a 0.25 mm trace at a 0.12 mm rule.
    const CORRIDOR_MM: f64 = 0.45;
    for &(p, s) in placed {
        let Some(e) = edges[p].iter().find(|e| e.slot == s) else {
            continue;
        };
        let (dx, dy) = e.dir;
        if dx.abs() < 1e-9 && dy.abs() < 1e-9 {
            continue;
        }
        let a = slots[s];
        let bx = a.x + dx * EXIT_PROBE_MM;
        let by = a.y + dy * EXIT_PROBE_MM;
        for &(q, t) in placed {
            if q == p || t == s {
                continue;
            }
            let o = slots[t];
            if point_seg_dist(o.x, o.y, a.x, a.y, bx, by) < CORRIDOR_MM {
                *blocked.entry(s).or_insert(0.0) += 1.0;
                *crowd.entry(t).or_insert(0.0) += 1.0;
            }
        }
        // Barrels already committed (earlier sides, earlier footprints,
        // via-in-pads) block just as hard and cannot be moved — they only
        // ever add to THIS slot's blocked score.
        let net = &targets[pending[p]].net;
        for (ox, oy, onet) in standing {
            if onet == net {
                continue;
            }
            if point_seg_dist(*ox, *oy, a.x, a.y, bx, by) < CORRIDOR_MM {
                *blocked.entry(s).or_insert(0.0) += 1.0;
            }
        }
    }
    (blocked, crowd, tight)
}

/// Commit an assignment against the LIVE board, cheapest first. Returns the
/// number of pads that landed and the indices of those that did not.
#[allow(clippy::too_many_arguments)]
fn commit(
    targets: &[SlotTarget],
    pending: &[usize],
    chosen: &[Option<usize>],
    edges: &[Vec<Edge>],
    slots: &[Slot],
    foreign: &[&PadRect],
    work: &mut Board,
    opts: &RouteOptions,
    resolver: &RuleResolver<'_>,
    fab: Option<&pcb_core::FabRules>,
    plan: &mut FanoutPlan,
) -> (usize, Vec<usize>) {
    let mut order: Vec<(i64, usize, usize, usize)> = Vec::new(); // (cost, pad_number tiebreak, local, slot)
    let mut left: Vec<usize> = Vec::new();
    for (li, &gi) in pending.iter().enumerate() {
        match chosen.get(li).copied().flatten() {
            Some(s) => {
                let c = edges[li]
                    .iter()
                    .find(|e| e.slot == s)
                    .map_or(f64::INFINITY, |e| e.base_cost);
                order.push(((c * 10_000.0).round() as i64, gi, li, s));
            }
            None => left.push(gi),
        }
    }
    order.sort();
    let mut committed = 0usize;
    for (_, gi, _, s) in order {
        let t = &targets[gi];
        let rules = PadRules::at_pad(
            resolver,
            &t.net,
            Point::new(Length::from_mm(t.cx), Length::from_mm(t.cy)),
            t.layer,
            fab,
        );
        let via_r = rules.via_diameter / 2.0;
        let slot = slots[s];
        if !fanout_via_fits(slot.x, slot.y, &t.net, via_r, &rules, foreign, work) {
            left.push(gi);
            continue;
        }
        let Some(stub) = dogbone_stub(
            t.layer,
            t.cx,
            t.cy,
            slot.x,
            slot.y,
            &t.net,
            t.max_stub_w,
            &rules,
            foreign,
            work,
        ) else {
            left.push(gi);
            continue;
        };
        let via_pos = Point::new(Length::from_mm(slot.x), Length::from_mm(slot.y));
        let via = Via {
            id: Id::new(),
            position: via_pos,
            drill: Length::from_mm(rules.via_drill),
            diameter: Length::from_mm(rules.via_diameter),
            net: t.net.clone(),
        };
        work.vias.push(via.clone());
        plan.vias.push(via);
        work.traces.push(stub.clone());
        plan.stubs.push(stub);
        plan.dogbone_pads.insert(t.pad_ref.clone());
        plan.through_pads.insert(t.pad_ref.clone());
        plan.via_positions.insert(t.pad_ref.clone(), via_pos);
        committed += 1;
    }
    let _ = opts;
    left.sort();
    (committed, left)
}

// ---------------------------------------------------------------------------
// Min-cost max-cardinality bipartite matching (successive shortest paths).
// ---------------------------------------------------------------------------

/// Min-cost maximum-cardinality matching on a sparse bipartite graph.
///
/// Successive shortest paths over an explicit min-cost-flow network with an
/// SPFA (Bellman-Ford queue) shortest path, so negative reduced costs on the
/// residual arcs need no potentials. Instances here are tiny (≤ 14 pads ×
/// a few hundred slots), and SPFA keeps the implementation short and exactly
/// reproducible: edge insertion order is the caller's sorted order, and SPFA
/// relaxes in FIFO order, so the result is deterministic.
///
/// Returns, for each left node, the right node it was matched to.
fn min_cost_matching(
    n_left: usize,
    n_right: usize,
    adj: &[Vec<(usize, i64)>],
) -> Vec<Option<usize>> {
    if n_left == 0 || n_right == 0 {
        return vec![None; n_left];
    }
    let src = n_left + n_right;
    let sink = src + 1;
    let n_nodes = sink + 1;
    let mut to: Vec<usize> = Vec::new();
    let mut cap: Vec<i32> = Vec::new();
    let mut cost: Vec<i64> = Vec::new();
    let mut head: Vec<Vec<usize>> = vec![Vec::new(); n_nodes];
    let add = |u: usize,
               v: usize,
               c: i32,
               w: i64,
               to: &mut Vec<usize>,
               cap: &mut Vec<i32>,
               cost: &mut Vec<i64>,
               head: &mut Vec<Vec<usize>>| {
        head[u].push(to.len());
        to.push(v);
        cap.push(c);
        cost.push(w);
        head[v].push(to.len());
        to.push(u);
        cap.push(0);
        cost.push(-w);
    };
    for i in 0..n_left {
        add(src, i, 1, 0, &mut to, &mut cap, &mut cost, &mut head);
        for &(j, w) in &adj[i] {
            add(i, n_left + j, 1, w, &mut to, &mut cap, &mut cost, &mut head);
        }
    }
    for j in 0..n_right {
        add(
            n_left + j,
            sink,
            1,
            0,
            &mut to,
            &mut cap,
            &mut cost,
            &mut head,
        );
    }

    const INF: i64 = i64::MAX / 4;
    loop {
        let mut dist = vec![INF; n_nodes];
        let mut in_q = vec![false; n_nodes];
        let mut prev_edge = vec![usize::MAX; n_nodes];
        dist[src] = 0;
        let mut queue = std::collections::VecDeque::new();
        queue.push_back(src);
        in_q[src] = true;
        while let Some(u) = queue.pop_front() {
            in_q[u] = false;
            for &e in &head[u] {
                if cap[e] <= 0 {
                    continue;
                }
                let v = to[e];
                let nd = dist[u] + cost[e];
                if nd < dist[v] {
                    dist[v] = nd;
                    prev_edge[v] = e;
                    if !in_q[v] {
                        in_q[v] = true;
                        queue.push_back(v);
                    }
                }
            }
        }
        if dist[sink] >= INF {
            break;
        }
        // Augment one unit along the path.
        let mut v = sink;
        while v != src {
            let e = prev_edge[v];
            cap[e] -= 1;
            cap[e ^ 1] += 1;
            v = to[e ^ 1];
        }
    }

    let mut out = vec![None; n_left];
    for i in 0..n_left {
        for &e in &head[i] {
            // Saturated forward arc into a right node.
            if e % 2 == 0 && cap[e] == 0 && to[e] >= n_left && to[e] < n_left + n_right {
                out[i] = Some(to[e] - n_left);
                break;
            }
        }
    }
    out
}

/// Recompute a `Trace`'s length in mm (test/debug helper).
#[allow(dead_code)]
pub(crate) fn trace_len_mm(t: &Trace) -> f64 {
    let dx = t.end.x.to_mm() - t.start.x.to_mm();
    let dy = t.end.y.to_mm() - t.start.y.to_mm();
    (dx * dx + dy * dy).sqrt()
}

#[cfg(test)]
mod tests {
    //! Acceptance tests for the global escape-slot assignment.
    //!
    //! They live in the crate (not in `tests/`) because the point of the
    //! first one is to run the SAME footprint down both paths, and the
    //! greedy ladder is unreachable from outside once a part is fine-pitch
    //! enough to qualify for the matching (`SLOT_ASSIGN_MIN_PADS`).

    use super::*;
    use crate::fanout::{pad_rects, plan_footprint, plan_footprint_greedy};
    use pcb_core::{CopperLayer, Footprint, Pad, Rect};

    fn pad(num: &str, off_x: f64, off_y: f64, w: f64, h: f64, net: &str) -> Pad {
        Pad {
            number: num.into(),
            name: String::new(),
            offset: Point::new(Length::from_mm(off_x), Length::from_mm(off_y)),
            size: (Length::from_mm(w), Length::from_mm(h)),
            layer: CopperLayer::Top,
            net: Some(net.to_string()),
            drill: None,
        }
    }

    fn footprint(reference: &str, x_mm: f64, y_mm: f64, pads: Vec<Pad>) -> Footprint {
        Footprint {
            id: Id::new(),
            reference: reference.into(),
            value: String::new(),
            library: "test".into(),
            position: Point::new(Length::from_mm(x_mm), Length::from_mm(y_mm)),
            rotation: 0.0,
            layer: CopperLayer::Top,
            pads,
            key: String::new(),
            description: String::new(),
            edge_mounted: false,
            edge_side: None,
            silk: Vec::new(),
        }
    }

    /// The script layer's fine-pitch numbers (see `tests/fine_pitch_fanout.rs`):
    /// a 0.20 mm cell, 0.25 mm traces and a 0.20 mm clearance. The default
    /// 0.40 mm clearance no fine-pitch board ever passes through.
    fn opts() -> RouteOptions {
        RouteOptions {
            cell: Length::from_mm(0.20),
            trace_width: Length::from_mm(0.25),
            clearance: Length::from_mm(0.20),
            ..RouteOptions::default()
        }
    }

    /// One 2-pin landing part per pin net, on a ring around the package, so
    /// every escape has a real direction to travel in (the assignment's
    /// direction term reads the net's partner centroid).
    fn landing_ring(board: &mut Board, count: usize) {
        for i in 0..count {
            let angle = std::f64::consts::TAU * i as f64 / count as f64;
            let (x, y) = (20.0 + 13.0 * angle.cos(), 20.0 + 13.0 * angle.sin());
            board.add_footprint(footprint(
                &format!("R{i}"),
                x,
                y,
                vec![
                    pad("1", -0.8, 0.0, 0.9, 0.9, &format!("N{i}")),
                    pad("2", 0.8, 0.0, 0.9, 0.9, "GND"),
                ],
            ));
        }
    }

    fn empty_board() -> Board {
        let mut board = Board::new();
        board.outline = Some(Rect::from_corners(
            Point::new(Length::from_mm(0.0), Length::from_mm(0.0)),
            Point::new(Length::from_mm(40.0), Length::from_mm(40.0)),
        ));
        board
    }

    /// Two facing rows of `per_row` pads at `pitch`, 0.50 × 0.20 mm each —
    /// the classic fine-pitch escape problem: the barrel does not fit in the
    /// pad (0.30 mm via vs a 0.20 mm-wide pad) so every pin needs a dogbone,
    /// and the sites those dogbones can occupy are the scarce resource.
    fn two_facing_rows(per_row: usize, pitch: f64) -> Board {
        let mut board = empty_board();
        let (pad_len, pad_w) = (0.50, 0.20);
        let ring = 3.5 - pad_len / 2.0; // pad centre, half a pad inside the body edge
        let span = (per_row as f64 - 1.0) * pitch;
        let mut pads = Vec::new();
        let mut n = 0usize;
        for side in 0..2 {
            for i in 0..per_row {
                let along = -span / 2.0 + i as f64 * pitch;
                let net = format!("N{n}");
                n += 1;
                let x = if side == 0 { -ring } else { ring };
                pads.push(pad(&format!("{n}"), x, along, pad_len, pad_w, &net));
            }
        }
        pads.push(pad("EP", 0.0, 0.0, 1.6, 1.6, "GND"));
        board.add_footprint(footprint("U1", 20.0, 20.0, pads));
        landing_ring(&mut board, per_row * 2);
        board
    }

    /// A 0.4 mm-pitch QFN: `per_side` pins on all four sides plus a shrunk
    /// thermal pad. Exercises the per-side decomposition (four independent
    /// matchings, each legalised against the copper the previous ones left).
    fn qfn(per_side: usize) -> Board {
        let mut board = empty_board();
        let (pitch, pad_len, pad_w) = (0.40, 0.50, 0.20);
        let ring = 3.5 - pad_len / 2.0;
        let span = (per_side as f64 - 1.0) * pitch;
        let mut pads = Vec::new();
        let mut n = 0usize;
        for side in 0..4 {
            for i in 0..per_side {
                let along = -span / 2.0 + i as f64 * pitch;
                let net = format!("N{n}");
                n += 1;
                let num = format!("{n}");
                pads.push(match side {
                    0 => pad(&num, -ring, along, pad_len, pad_w, &net),
                    1 => pad(&num, ring, along, pad_len, pad_w, &net),
                    2 => pad(&num, along, -ring, pad_w, pad_len, &net),
                    _ => pad(&num, along, ring, pad_w, pad_len, &net),
                });
            }
        }
        pads.push(pad("EP", 0.0, 0.0, 1.6, 1.6, "GND"));
        board.add_footprint(footprint("U1", 20.0, 20.0, pads));
        landing_ring(&mut board, per_side * 4);
        board
    }

    /// Plan the escape of ONE footprint exactly the way `escape::plan_escapes`
    /// does (same foreign-pad set, same rule resolver, same working board),
    /// with a choice of assignment path. Nothing else runs — no grid, no
    /// search, no clock — so the result is a pure function of the geometry.
    fn plan_one(board: &Board, reference: &str, greedy: bool) -> FanoutPlan {
        let o = opts();
        let rects = pad_rects(board);
        let foreign: Vec<&PadRect> = rects.iter().collect();
        let resolver = pcb_core::RuleResolver::new(
            &board.rule_areas,
            pcb_core::RuleDefaults {
                clearance: o.clearance,
                trace_width: o.trace_width,
                via_diameter: o.via_diameter,
                via_drill: o.via_drill,
            },
        );
        let fp = board
            .footprints_in_order()
            .find(|f| f.reference == reference)
            .expect("footprint not on the board");
        let mut work = board.clone();
        let fab = board.fab_rules.as_ref();
        if greedy {
            plan_footprint_greedy(fp, &foreign, &mut work, &o, &resolver, fab)
        } else {
            plan_footprint(fp, &foreign, &mut work, &o, &resolver, fab)
        }
    }

    /// The feature's reason to exist: choosing the escape sites TOGETHER
    /// beats choosing them one pad at a time.
    ///
    /// Both runs see byte-identical geometry; the only difference is that
    /// the greedy one commits the first barrel each pad happens to fit
    /// (which walls off its neighbours) while the matched one solves the
    /// whole side at once.
    #[test]
    fn matching_beats_greedy() {
        for (per_row, pitch) in [(8usize, 0.40), (12, 0.40)] {
            let board = two_facing_rows(per_row, pitch);
            let greedy = plan_one(&board, "U1", true);
            let matched = plan_one(&board, "U1", false);
            assert!(
                matched.through_pads.len() > greedy.through_pads.len(),
                "{per_row}x2 @ {pitch} mm: matching escaped {} pad(s), greedy {} — \
                 the fixture stopped exercising the assignment problem",
                matched.through_pads.len(),
                greedy.through_pads.len()
            );
            // Every escape the matching claims has to be real copper: a
            // barrel plus the stub that ties the pad to it. A dogbone with
            // no stub is a via the pad is not connected to.
            assert_eq!(
                matched.vias.len(),
                matched.through_pads.len(),
                "escaped pads and barrels disagree"
            );
            assert_eq!(
                matched.stubs.len(),
                matched.dogbone_pads.len(),
                "a dogbone barrel with no pad -> via stub"
            );
            // There IS room on this board for every pin — the greedy path
            // just spends it badly. The matching escapes them all, so it
            // has nothing to strand; the pads greedy left behind are not
            // reported anywhere at all (the old path has no such list).
            assert!(
                matched.stranded_pads.is_empty(),
                "matching stranded {:?} on a board with room for every pin",
                matched.stranded_pads
            );
        }
    }

    /// The assignment must be a pure function of the board. It runs before
    /// any search, so nothing here is allowed to depend on wall-clock time,
    /// on hash iteration order, or on `Id`s (which are fresh UUIDs every
    /// run — hence the comparison on geometry, not on identity).
    #[test]
    fn slot_assignment_is_deterministic() {
        /// Everything the assignment decided, in an exactly comparable
        /// form: barrels as (pad ref, x nm, y nm) integers — no epsilon,
        /// a sub-nanometre drift is still a difference — plus the stub
        /// segments and the stranded list.
        type Signature = (Vec<(String, i64, i64)>, Vec<String>, Vec<String>);
        let signature = |plan: &FanoutPlan| -> Signature {
            let mut vias: Vec<(String, i64, i64)> = plan
                .via_positions
                .iter()
                .map(|(k, p)| (k.clone(), p.x.0, p.y.0))
                .collect();
            vias.sort();
            let mut stubs: Vec<String> = plan
                .stubs
                .iter()
                .map(|t| {
                    format!(
                        "{} L{} {},{} -> {},{} w{}",
                        t.net,
                        t.layer.index,
                        t.start.x.0,
                        t.start.y.0,
                        t.end.x.0,
                        t.end.y.0,
                        t.width.0
                    )
                })
                .collect();
            stubs.sort();
            let mut stranded = plan.stranded_pads.clone();
            stranded.sort();
            (vias, stubs, stranded)
        };

        // Two shapes: the QFN exercises the four-sided decomposition (each
        // side legalised against the previous sides' copper), the jammed
        // part exercises the stranded list.
        for (name, build) in [
            ("qfn", (|| qfn(8)) as fn() -> Board),
            ("jammed", (|| jammed_package(4)) as fn() -> Board),
        ] {
            // Rebuilt from scratch each time: fresh `Id`s, fresh maps, fresh
            // insertion order.
            let first = signature(&plan_one(&build(), "U1", false));
            let second = signature(&plan_one(&build(), "U1", false));
            assert_eq!(first, second, "{name}: slot assignment is not reproducible");
            assert!(
                !first.0.is_empty() || !first.2.is_empty(),
                "{name}: the fixture produced neither an escape nor a strand"
            );
        }
    }

    /// A package whose escape sites are all occupied: a ground shield frame
    /// 0.15 mm off the pad row kills every OUTWARD barrel site, and a
    /// thermal pad grown to 0.05 mm from the pads' inner faces kills every
    /// INWARD one. Nothing is left for a 0.4 mm-pitch pin that cannot host a
    /// via-in-pad. Mirrored by `tests/escape_slots.rs`, which drives the
    /// same geometry through the public `route()` to check the strand
    /// reaches the report's hints.
    fn jammed_package(per_side: usize) -> Board {
        let mut board = empty_board();
        let (pitch, pad_len, pad_w) = (0.40, 0.50, 0.20);
        let ring = 3.5 - pad_len / 2.0;
        let span = (per_side as f64 - 1.0) * pitch;
        let mut pads = Vec::new();
        let mut n = 0usize;
        for side in 0..4 {
            for i in 0..per_side {
                let along = -span / 2.0 + i as f64 * pitch;
                let net = format!("N{n}");
                n += 1;
                let num = format!("{n}");
                pads.push(match side {
                    0 => pad(&num, -ring, along, pad_len, pad_w, &net),
                    1 => pad(&num, ring, along, pad_len, pad_w, &net),
                    2 => pad(&num, along, -ring, pad_w, pad_len, &net),
                    _ => pad(&num, along, ring, pad_w, pad_len, &net),
                });
            }
        }
        // Unshrunk thermal pad: its edge sits 0.05 mm from the pad row's
        // inner face, so the inward channel the assignment normally uses
        // does not exist.
        pads.push(pad("EP", 0.0, 0.0, 5.9, 5.9, "GND"));
        board.add_footprint(footprint("U1", 20.0, 20.0, pads));
        // Shield frame: four bars from 3.65 mm to 6.25 mm out, i.e. starting
        // 0.15 mm past the pad row's outer face — closer than a 0.15 mm
        // barrel plus 0.20 mm of clearance, so no outward rank fits either.
        let (inner, outer) = (3.65, 6.25);
        let mid = f64::midpoint(inner, outer);
        let (thick, long) = (outer - inner, 2.0 * outer);
        board.add_footprint(footprint(
            "SH1",
            20.0,
            20.0,
            vec![
                pad("1", -mid, 0.0, thick, long, "SHIELD"),
                pad("2", mid, 0.0, thick, long, "SHIELD"),
                pad("3", 0.0, -mid, long, thick, "SHIELD"),
                pad("4", 0.0, mid, long, thick, "SHIELD"),
            ],
        ));
        landing_ring(&mut board, per_side * 4);
        board
    }
}
