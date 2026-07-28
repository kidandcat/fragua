//! Escape reachability — the pads no search on this escape plan can ever
//! reach, and the barrels that wall them in.
//!
//! # Why
//!
//! On the RP2040 stress board a routing pass spends most of its wall clock
//! *failing*. A search that cannot reach its target explores the whole grid
//! before it gives up (the A\* pop cap is `grid_cells × 8`), and in the
//! clean-board rip-up pass each such failure then buys a rip-up cascade —
//! up to four blockers ripped, rerouted and restored, two levels deep,
//! every one of them another full search. Measured on the 36 × 30 mm board
//! at a 180 s budget: a rip-up-enabled pass costs 30–44 s against 6–8 s for
//! a plain one, and the difference is almost entirely nets that were never
//! going to route.
//!
//! Some of those failures are *provable* before the search runs. Strip the
//! board back to the copper no pass can move — pads, bodies, keep-outs, and
//! the escape plan's own barrels and stubs — and flood the cells a net may
//! legally stand on. Routed traces only ever *remove* cells from that set
//! (a net is exempt from its own copper, never from anyone else's), so a
//! pad in a closed component that holds none of its net's other pads is
//! unreachable in **every** pass built on this escape plan, at any budget,
//! in any net order.
//!
//! # What it costs
//!
//! Only the pads a pass actually failed on are floods, and each flood stops
//! at [`POCKET_CELL_CAP`] cells. An entombed pad's pocket closes in a few
//! dozen cells; a pad on open board hits the cap and is reported as
//! reachable. So the analysis is milliseconds, and it is deliberately
//! one-sided: it only ever claims "entombed" when it has enumerated the
//! *entire* component the pad lives in.
//!
//! # What it buys
//!
//! 1. A provably dead spoke fails instantly instead of burning a pop cap
//!    and a rip-up cascade — that budget goes to nets that can still route.
//! 2. The wall is named. The copper walling an entombed pad in is mostly
//!    *other pads' escape barrels*, and those the escape planner can move:
//!    the rip-and-reassign lever gets told which barrels to move rather
//!    than guessing from failure streaks.
//! 3. The route report can finally separate "this needs more time" from
//!    "this needs different geometry" — the classification the stress
//!    campaign was doing by hand with the test-only flood autopsy.

use std::collections::{BTreeMap, BTreeSet, VecDeque};



use crate::grid::{self, Cell, Grid, GridPoint};
use crate::router::{NetPadInfo, RouteOptions, RuleCtx};

/// Cells a single pocket flood may visit before we stop calling it a
/// pocket. Every entombed pad measured on the stress board closes in 8–40
/// cells; the open board is ~30 000. 512 cells is ~20 mm² of free copper
/// at the default 0.20 mm cell — far more room than any escape needs, and
/// the cap is what keeps a round of plan legalisation at milliseconds
/// instead of half a second.
///
/// The cap only ever errs toward *under*-reporting: a component larger
/// than this is called reachable, so the search still gets its chance. It
/// can never invent an entombment.
const POCKET_CELL_CAP: usize = 512;

/// How far (in cells) around a pocket cell we look for the copper that
/// bounds it. Four cells is one clearance radius plus a margin at the
/// default 0.20 mm cell.
const BLAME_RADIUS_CELLS: i32 = 4;

/// Verdict for one escape plan.
#[derive(Debug, Default, Clone)]
pub(crate) struct Reach {
    /// Pads that are provably unreachable from the rest of their net.
    /// Keyed by pad ref (`U1.52`), valued with the net name.
    pub entombed: BTreeMap<String, String>,
    /// Escape barrels (by the pad ref that owns them) whose copper bounds
    /// at least one entombed pocket, most-blame-first. These are the sites
    /// the escape planner can actually move.
    pub blamed_barrels: Vec<String>,
}

impl Reach {
    pub fn is_entombed(&self, pad_ref: &str) -> bool {
        self.entombed.contains_key(pad_ref)
    }

    /// Nets with at least one entombed pad, in name order.
    pub fn entombed_nets(&self) -> BTreeSet<&str> {
        self.entombed.values().map(String::as_str).collect()
    }
}

/// Flood the cells `net_id` may legally stand on, starting at `start`,
/// giving up after `cap` cells. `None` = the component is bigger than the
/// cap (so: not a pocket). Mirrors the A\* move rules — 8-neighbour planar
/// steps gated by the trace clearance disk, layer changes gated by the via
/// disk and the no-drilled-pad rule — without any cost model.
fn flood_pocket(
    grid: &Grid,
    start: GridPoint,
    net_id: u32,
    clr: &grid::ClearanceModel<'_>,
    via: &grid::ClearanceModel<'_>,
    cap: usize,
) -> Option<BTreeSet<(u8, i32, i32)>> {
    let mut seen: BTreeSet<(u8, i32, i32)> = BTreeSet::new();
    let mut q: VecDeque<GridPoint> = VecDeque::new();
    seen.insert((start.layer, start.col, start.row));
    q.push_back(start);
    while let Some(p) = q.pop_front() {
        if seen.len() > cap {
            return None;
        }
        let mut cands: Vec<(GridPoint, bool)> = Vec::with_capacity(8 + grid.layer_count as usize);
        for (dc, dr) in [
            (1, 0),
            (-1, 0),
            (0, 1),
            (0, -1),
            (1, 1),
            (1, -1),
            (-1, 1),
            (-1, -1),
        ] {
            cands.push((
                GridPoint {
                    layer: p.layer,
                    col: p.col + dc,
                    row: p.row + dr,
                },
                false,
            ));
        }
        for l in 0..grid.layer_count {
            if l != p.layer {
                cands.push((
                    GridPoint {
                        layer: l,
                        col: p.col,
                        row: p.row,
                    },
                    true,
                ));
            }
        }
        for (n, is_via) in cands {
            if !grid.in_bounds(n) || seen.contains(&(n.layer, n.col, n.row)) {
                continue;
            }
            if !grid::walkable(grid.get(n), net_id) {
                continue;
            }
            if is_via {
                let drilled = (0..grid.layer_count).any(|l| {
                    matches!(
                        grid.get(GridPoint {
                            layer: l,
                            col: n.col,
                            row: n.row
                        }),
                        Cell::DrilledPad(_)
                    )
                });
                if drilled || !grid.via_clearance_ok(n, net_id, via) {
                    continue;
                }
            } else {
                let own =
                    matches!(grid.get(n), Cell::NetPad(i) | Cell::DrilledPad(i) if i == net_id);
                if !own && !grid.clearance_ok(n, net_id, clr) {
                    continue;
                }
            }
            seen.insert((n.layer, n.col, n.row));
            q.push_back(n);
        }
    }
    Some(seen)
}

/// Escape barrels whose copper is inside `radius` cells of any cell of
/// `pocket`, with a blame count each. Only `DrilledPad` cells count: those
/// are the fanout's barrels, the one class of walling copper the escape
/// planner can move. Pads and bodies bound the pocket too, but naming them
/// would only produce advice nobody can act on.
fn blame_barrels(
    grid: &Grid,
    pocket: &BTreeSet<(u8, i32, i32)>,
    net_id: u32,
    barrel_of_cell: &BTreeMap<(i32, i32), String>,
) -> BTreeMap<String, usize> {
    let mut hits: BTreeMap<String, usize> = BTreeMap::new();
    let r = BLAME_RADIUS_CELLS;
    for &(layer, col, row) in pocket {
        for dc in -r..=r {
            for dr in -r..=r {
                if dc * dc + dr * dr > r * r {
                    continue;
                }
                let q = GridPoint {
                    layer,
                    col: col + dc,
                    row: row + dr,
                };
                if !grid.in_bounds(q) {
                    continue;
                }
                let Cell::DrilledPad(i) = grid.get(q) else {
                    continue;
                };
                if i == net_id {
                    continue;
                }
                if let Some(owner) = barrel_of_cell.get(&(q.col, q.row)) {
                    *hits.entry(owner.clone()).or_insert(0) += 1;
                }
            }
        }
    }
    hits
}

/// Classify the pads of `suspect_nets` against the bare copper of this
/// escape plan.
///
/// `grid` must be the pass grid of a board with **no routed copper** —
/// i.e. exactly what `build_pass_grid` produces after `clear_routing`.
/// Anything routed on top can only take cells away, which is what makes a
/// verdict here hold for every pass.
pub(crate) fn analyse(
    grid: &Grid,
    rules: &RuleCtx<'_>,
    opts: &RouteOptions,
    order: &[String],
    nets: &BTreeMap<String, Vec<NetPadInfo>>,
    fanout: &crate::fanout::FanoutPlan,
    suspect_nets: &[String],
) -> Reach {
    // Which barrel owns which grid cell, so the blame can name a pad ref
    // instead of a net. Built once for the whole analysis.
    let mut barrel_of_cell: BTreeMap<(i32, i32), String> = BTreeMap::new();
    for (pad_ref, at) in &fanout.via_positions {
        let gp = grid.snap(*at, pcb_core::CopperLayer::Top);
        let r = crate::router::via_copper_cells(opts);
        for dc in -r..=r {
            for dr in -r..=r {
                if dc * dc + dr * dr > r * r {
                    continue;
                }
                barrel_of_cell
                    .entry((gp.col + dc, gp.row + dr))
                    .or_insert_with(|| pad_ref.clone());
            }
        }
    }

    let net_id_of: BTreeMap<&str, u32> = order
        .iter()
        .enumerate()
        .map(|(i, n)| (n.as_str(), i as u32))
        .collect();

    let mut out = Reach::default();
    let mut blame_total: BTreeMap<String, usize> = BTreeMap::new();

    for net in suspect_nets {
        let (Some(pads), Some(&net_id)) = (nets.get(net), net_id_of.get(net.as_str())) else {
            continue;
        };
        if pads.len() < 2 {
            continue;
        }
        let clr = rules.trace_model(net, opts.trace_width, opts.cell);
        let via = rules.via_model(net, opts.cell);
        let route_point = |p: &NetPadInfo| {
            fanout
                .via_positions
                .get(&p.pad_ref)
                .copied()
                .unwrap_or(p.center)
        };

        // Component per pad, sharing floods: a pad already inside a known
        // pocket does not get its own. `None` marks "bigger than the cap",
        // i.e. the open-board component every reachable pad shares.
        let mut pockets: Vec<BTreeSet<(u8, i32, i32)>> = Vec::new();
        let mut owner: Vec<Option<usize>> = Vec::with_capacity(pads.len());
        let mut open_board = false;
        for p in pads {
            let gp = grid.snap(route_point(p), p.layer);
            let key = (gp.layer, gp.col, gp.row);
            if let Some(k) = pockets.iter().position(|c| c.contains(&key)) {
                owner.push(Some(k));
                continue;
            }
            match flood_pocket(grid, gp, net_id, &clr, &via, POCKET_CELL_CAP) {
                Some(cells) => {
                    owner.push(Some(pockets.len()));
                    pockets.push(cells);
                }
                None => {
                    owner.push(None);
                    open_board = true;
                }
            }
        }

        // The trunk is the component the router can grow a tree in: the
        // open-board one when any pad reaches it, else the largest pocket.
        // Pads outside it can never join, whatever the budget.
        let trunk: Option<usize> = if open_board {
            None
        } else {
            let mut best: Option<usize> = None;
            for (i, _) in pockets.iter().enumerate() {
                let size = owner
                    .iter()
                    .filter(|o| **o == Some(i))
                    .count()
                    .saturating_mul(1_000_000)
                    + pockets[i].len();
                if best.is_none_or(|b| {
                    let bsize = owner.iter().filter(|o| **o == Some(b)).count() * 1_000_000
                        + pockets[b].len();
                    size > bsize
                }) {
                    best = Some(i);
                }
            }
            best
        };
        // With no pocket at all (every pad on open board) there is nothing
        // to report — the net fails for congestion, not geometry.
        if open_board && pockets.is_empty() {
            continue;
        }

        for (i, p) in pads.iter().enumerate() {
            let mine = owner[i];
            let stuck = match (trunk, mine) {
                // Open-board trunk: any pad in a closed pocket is stuck.
                (None, Some(_)) => true,
                (None, None) => false,
                // No pad reached open board: everything outside the
                // biggest pocket is stuck.
                (Some(t), Some(k)) => k != t,
                (Some(_), None) => false,
            };
            if !stuck {
                continue;
            }
            out.entombed.insert(p.pad_ref.clone(), net.clone());
            if let Some(k) = mine {
                for (owner_ref, n) in blame_barrels(grid, &pockets[k], net_id, &barrel_of_cell) {
                    *blame_total.entry(owner_ref).or_insert(0) += n;
                }
            }
        }
    }

    // Most-blamed barrel first; ties broken by pad ref so the order — and
    // therefore every downstream reassignment — is deterministic.
    let mut ranked: Vec<(usize, String)> = blame_total.into_iter().map(|(k, n)| (n, k)).collect();
    ranked.sort_by(|a, b| b.0.cmp(&a.0).then_with(|| a.1.cmp(&b.1)));
    out.blamed_barrels = ranked.into_iter().map(|(_, k)| k).collect();
    out
}
