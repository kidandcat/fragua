//! PathFinder-style negotiated congestion (handoff issue O1).
//!
//! # Why
//!
//! The classic driver (`router::route_pass`) routes nets one after another
//! against a grid where every already-laid net is a WALL. Whoever runs first
//! owns the corridor for the rest of the pass, and rip-up-and-reroute only
//! ever re-shuffles the order — on a 0.4 mm QFN escape that plateaus, because
//! the fat `+3V3` ring claims the escape channels before the QSPI/USB/SWD
//! nets ever get to ask for them.
//!
//! # What this does instead
//!
//! The FPGA router's answer (McMurchie & Ebeling's PathFinder, 1995):
//!
//! 1. Let every net route through whatever it likes, *sharing* resources,
//!    and pay a price for the sharing.
//! 2. After each iteration, find the over-subscribed resources, raise their
//!    **history** cost, rip up only the nets involved, and reroute them.
//! 3. Repeat until nobody shares anything — that solution is legal by
//!    construction — or until the wall-clock budget runs out.
//!
//! The first iteration is therefore "every net gets its shortest path", and
//! congestion (not routing order) decides who detours. That is exactly the
//! failure mode the plateau was made of.
//!
//! # Clearance versus sharing — the PCB-specific part
//!
//! FPGA PathFinder shares *wires*: a resource is either used or not. On a
//! PCB the resource a net consumes is its copper **plus the pairwise
//! clearance halo around it**, and that halo depends on which two nets meet
//! (strictest class wins) and on where they meet (a `RuleArea` overrides
//! outright). Inflating every net's claim by its own halo and then forbidding
//! claim-vs-claim overlap would be far too conservative: two nets whose halos
//! touch can still be perfectly legal.
//!
//! So we keep the exact, asymmetric test the DRC and the classic search
//! already agree on — copper is stamped BARE (its own half-width and nothing
//! more) and each net scans its own `clearance + half-width` disk for foreign
//! copper — and change only the verdict:
//!
//! - foreign **trace/via** copper inside the disk → **shareable**, priced at
//!   `present_factor` per foreign net per cell (see [`Negotiate`]);
//! - foreign **pad** copper inside the disk → **still hard**. Pads cannot be
//!   ripped up, so a pad-halo violation can never be negotiated away; buying
//!   one would only produce solutions that can never be legalised.
//!
//! Because the requirement `w_a/2 + clearance + w_b/2` is symmetric, both
//! nets see the same overlap from their own side, so the negotiation is
//! symmetric too. And because the price comes from the very same disk test
//! that decides legality, **"zero conflicts anywhere" is literally "legal
//! copper"** — convergence needs no separate geometry pass.
//!
//! # Legality of the answer, at any deadline
//!
//! A shared state is NOT a solution. Every candidate this module offers the
//! driver is produced by [`extract_legal`], which lays negotiated geometry
//! into a fresh grid only when it passes the ordinary hard clearance test,
//! and hard-routes whatever is left over. So the "best so far" comparison in
//! `route()` only ever sees legal boards, exactly like the classic loop.

use std::collections::{BTreeMap, BTreeSet, HashMap, HashSet};
use std::time::Instant;

use pcb_core::{Board, Length, Point, Trace};

use crate::astar::{search, Limits, Negotiate};
use crate::grid::{self, Cell, CostMap, Grid, GridPoint};
use crate::router::{
    build_pass_grid, ceil_cells, compute_region, count_failed, effective_net_rules, hpwl_mm,
    lay_path, pour_nets, progress, report_is_better, route_one_net, timed_out, via_copper_cells,
    NetPadInfo, NetRoute, Outcome, RouteOptions, RouteReport, RuleCtx,
};

/// Fraction of the wall-clock budget the negotiation loop may spend. The
/// remainder goes to the classic rip-up-and-reroute loop, which runs
/// afterwards as a fallback whenever negotiation did NOT converge — so the
/// driver's answer is always the better of the two, never only the
/// negotiated one.
const NEGOTIATION_BUDGET_FRACTION: f64 = 0.4;

/// Pop cap for one negotiated search. Much tighter than the driver's
/// default (which scales to millions): with sharing allowed there are no
/// walls to prune the frontier, so a net that CANNOT be routed — blocked by
/// pad halos or fanout landings, which no amount of negotiating moves —
/// would otherwise sweep the whole window every iteration and eat the
/// budget the nets that can still move need.
const NEG_MAX_POPS: u64 = 1_000_000;

/// Greedy weight for negotiated searches (`f = g + W·h`). Negotiation is an
/// iterative process: a few percent of extra path length on one iteration is
/// irrelevant next to getting more iterations in, and the weighting is
/// size-gated inside the search so short escapes stay optimal.
const NEG_HEURISTIC_WEIGHT: f64 = 1.35;

/// Hard cap on negotiation iterations, so an oscillating board cannot spin
/// forever on an unlimited budget.
const MAX_ITERATIONS: usize = 40;

/// History added to a cell each iteration it stays over-subscribed, in
/// whole grid cells of detour. Persistent across iterations: this is the
/// term that eventually makes a fought-over corridor unattractive to
/// everyone, which is what breaks a deadlock the present price alone
/// cannot (both nets would otherwise keep preferring the same lane).
const HISTORY_INC: u32 = 2;
/// Cells around a conflicting cell that also get the history bump. Zero
/// would let a trace dodge the price by shifting one cell sideways and
/// conflicting all over again; a wide smear stops distinguishing corridors
/// at all once hundreds of cells are contended.
const HISTORY_SMEAR_CELLS: i32 = 1;
/// Ceiling on accumulated history, so a hot cell stays expensive without
/// swamping the heuristic (A* must still be willing to use it when it is
/// the only way through).
const HISTORY_MAX: u32 = 96;

/// A net that cannot be routed even with sharing allowed is blocked by
/// something non-negotiable (pad halos, bodies, the board edge). Retrying it
/// costs a full exhaustive A* sweep every iteration, so give it this many
/// attempts and then leave it alone.
const MAX_NET_ATTEMPTS: usize = 2;

/// Present-congestion price at iteration `i` (1-based), in whole grid cells
/// of detour per shared cell. Tripling each round is a steeper ramp than
/// textbook PathFinder uses, because a PCB budget buys single-digit
/// iterations, not hundreds: cheap at first (so iteration 1 really is
/// "every net takes its shortest path"), then expensive enough that only
/// nets with no alternative keep sharing.
fn present_factor(iteration: usize) -> u32 {
    3u32.saturating_pow(
        u32::try_from(iteration.saturating_sub(1))
            .unwrap_or(5)
            .min(5),
    )
}

/// One routed spoke of a net: the grid path, the width it was laid at
/// (fine-pitch entries neck down), and the exact world point the last
/// segment must land on.
#[derive(Clone)]
struct Spoke {
    path: Vec<GridPoint>,
    width: Length,
    target_world: Point,
}

/// A net's negotiated geometry.
#[derive(Clone, Default)]
struct NetPaths {
    spokes: Vec<Spoke>,
    lower_bound_mm: f64,
    /// `Some` when the net could not be completed even with sharing —
    /// the spokes present are a partial tree.
    failed: Option<String>,
}

/// What the negotiation loop hands back to the driver.
pub(crate) struct Negotiated {
    /// A LEGAL board (coarse routing only — the driver adds fanout copper,
    /// stitching and the organic pass afterwards).
    pub board: Board,
    pub report: RouteReport,
    /// Negotiation iterations actually run.
    pub iterations: usize,
    /// True when an iteration ended with zero sharing anywhere.
    pub converged: bool,
    /// Human-readable autopsy of the cells that stayed over-subscribed —
    /// the corridor congestion, named in board coordinates.
    pub autopsy: Vec<String>,
}

/// Run the negotiation loop and return the best legal board it produced.
#[allow(clippy::too_many_arguments)]
pub(crate) fn negotiate_route(
    board: &Board,
    nets: &BTreeMap<String, Vec<NetPadInfo>>,
    order: &[String],
    opts: &RouteOptions,
    fanout: &crate::fanout::FanoutPlan,
    escape_stubs: &[Trace],
    deadline: Option<Instant>,
) -> Negotiated {
    let started = Instant::now();
    // The negotiation loop stops early so the final legal extraction (which
    // may need real routing work) always gets its share of the budget.
    let neg_deadline = match (deadline, opts.max_seconds) {
        (Some(d), Some(s)) if s > 0.0 => Some(
            (started + std::time::Duration::from_secs_f64(s * NEGOTIATION_BUDGET_FRACTION)).min(d),
        ),
        (d, _) => d,
    };
    let limits = Limits {
        deadline,
        max_pops: Some(NEG_MAX_POPS),
    };

    let areas = board.rule_areas.clone();
    let schematic = opts.schematic.clone();
    let via_cells = via_copper_cells(opts);
    let pours = pour_nets(board);
    let region = compute_region(board, opts);
    let layer_count = board.stackup.layer_count();
    let mut history = Grid::with_layers(region, opts.cell, layer_count).new_cost_map();
    let ids: HashMap<&str, u32> = order
        .iter()
        .enumerate()
        .map(|(i, n)| (n.as_str(), i as u32))
        .collect();

    // Nets the search actually has to solve: everything that is not a pour
    // net and has at least two pads. The rest are trivially "Ok" and are
    // folded back into the report by `extract_legal`.
    let routable: Vec<String> = order
        .iter()
        .filter(|n| !pours.contains(*n) && nets.get(*n).is_some_and(|p| p.len() >= 2))
        .cloned()
        .collect();

    let mut routes: BTreeMap<String, NetPaths> = BTreeMap::new();
    let mut attempts: BTreeMap<String, usize> = BTreeMap::new();
    let mut to_route: Vec<String> = routable.clone();
    let mut best: Option<(Board, RouteReport)> = None;
    let mut hot_total: BTreeMap<(u8, i32, i32), u32> = BTreeMap::new();
    let mut iterations = 0usize;
    let mut converged = false;

    while iterations < MAX_ITERATIONS && !to_route.is_empty() {
        if timed_out(neg_deadline) {
            break;
        }
        iterations += 1;
        let pf = present_factor(iterations);

        // Rebuild the shared substrate from scratch: fixed obstacles plus
        // the routes we are KEEPING. Rebuilding (instead of un-stamping) is
        // what keeps cell ownership unambiguous — `stamp_cell_copper` only
        // claims free cells, so after sharing a cell's recorded owner is
        // whoever got there first, and rewinding that incrementally would
        // desync the grid from the routes.
        let mut grid = build_pass_grid(board, opts, order, fanout, escape_stubs);
        let rules = RuleCtx::new(&areas, schematic.as_deref(), opts, order, &grid);
        let ripped: HashSet<&str> = to_route.iter().map(String::as_str).collect();
        for (name, paths) in &routes {
            if ripped.contains(name.as_str()) {
                continue;
            }
            stamp_paths(&mut grid, paths, ids[name.as_str()], opts, via_cells);
        }

        // Route the pending nets, sharing allowed. Order follows `order`
        // (deterministic); within one iteration a later net still sees the
        // earlier ones' copper, so it pays to share — but at iteration 1 the
        // price is 1 cell, which is why everybody gets their direct path and
        // the congestion, not the ordering, decides the outcome.
        let pending: Vec<String> = order
            .iter()
            .filter(|n| ripped.contains(n.as_str()))
            .cloned()
            .collect();
        for name in &pending {
            if timed_out(neg_deadline) {
                break;
            }
            routes.remove(name);
            let id = ids[name.as_str()];
            let pads = nets.get(name).map_or(&[][..], Vec::as_slice);
            let paths = negotiate_one_net(
                &mut grid, name, id, pads, opts, &rules, &history, fanout, via_cells, limits, pf,
            );
            *attempts.entry(name.clone()).or_default() += 1;
            routes.insert(name.clone(), paths);
        }

        // Conflict scan: whose copper still sits inside whose halo?
        let conflicts = detect_conflicts(&grid, &routes, &rules, opts, &ids);
        for (cell, _) in &conflicts.hot {
            *hot_total
                .entry((cell.layer, cell.col, cell.row))
                .or_default() += 1;
        }

        let unresolved: BTreeSet<String> = routes
            .iter()
            .filter(|(n, p)| {
                p.failed.is_some() && attempts.get(*n).copied().unwrap_or(0) < MAX_NET_ATTEMPTS
            })
            .map(|(n, _)| n.clone())
            .collect();

        if conflicts.nets.is_empty() && unresolved.is_empty() {
            // Nothing shared and nothing left to retry: the negotiated
            // geometry is legal as it stands.
            converged = conflicts.nets.is_empty();
            let (b, r) = extract_legal(
                board,
                opts,
                order,
                nets,
                fanout,
                escape_stubs,
                &routes,
                &conflicts.per_net,
                &history,
                &ids,
                deadline,
                true,
            );
            if best
                .as_ref()
                .is_none_or(|(_, prev)| report_is_better(&r, prev))
            {
                best = Some((b, r));
            }
            break;
        }

        // Raise the history cost of every over-subscribed neighbourhood, so
        // next iteration BOTH sides of the fight see the corridor as
        // expensive and whoever has the cheaper alternative moves.
        for (cell, _) in &conflicts.hot {
            history.bump_box_on(
                cell.layer,
                cell.col - HISTORY_SMEAR_CELLS,
                cell.row - HISTORY_SMEAR_CELLS,
                cell.col + HISTORY_SMEAR_CELLS,
                cell.row + HISTORY_SMEAR_CELLS,
                HISTORY_INC,
                HISTORY_MAX,
            );
        }

        // Cheap legal fallback: keep the mutually-legal subset of the
        // negotiated routes, with no re-routing at all (that is the
        // expensive part and it is saved for the final extraction). Costs
        // O(copper) and guarantees the driver always holds a legal board
        // even if the budget dies mid-negotiation.
        let (cand_board, cand_report) = extract_legal(
            board,
            opts,
            order,
            nets,
            fanout,
            escape_stubs,
            &routes,
            &conflicts.per_net,
            &history,
            &ids,
            deadline,
            false,
        );
        let blocked: Vec<&str> = routes
            .iter()
            .filter(|(_, p)| p.failed.is_some())
            .map(|(n, _)| n.as_str())
            .collect();
        progress(
            opts,
            format!(
                "route: negotiation iteration {iterations} — {} sharing, {} blocked even when sharing{}, {} hot cell(s), price {pf}x, {} legal net(s)",
                conflicts.nets.len(),
                blocked.len(),
                if blocked.is_empty() {
                    String::new()
                } else {
                    format!(" ({})", blocked.join(" "))
                },
                conflicts.hot.len(),
                cand_report.per_net.len() - count_failed(&cand_report),
            ),
        );
        if best
            .as_ref()
            .is_none_or(|(_, prev)| report_is_better(&cand_report, prev))
        {
            best = Some((cand_board, cand_report));
        }

        // Rip up exactly the nets that share something (plus the ones still
        // owed a retry) — everybody else keeps their copper, which is what
        // makes later iterations far cheaper than the first.
        to_route = order
            .iter()
            .filter(|n| conflicts.nets.contains(*n) || unresolved.contains(*n))
            .cloned()
            .collect();
    }

    let (work, report) = best.unwrap_or_else(|| {
        let mut w = board.clone();
        w.clear_routing();
        let per_net = order
            .iter()
            .map(|n| {
                (
                    n.clone(),
                    Outcome::Failed {
                        reason: "timeout (route max_seconds budget) before negotiation".into(),
                    },
                )
            })
            .collect();
        (
            w,
            RouteReport {
                per_net,
                ..RouteReport::default()
            },
        )
    });

    let mut autopsy_lines = autopsy(&hot_total, region, opts, board);
    // Nets that could not be routed even with every foreign trace treated as
    // shareable are blocked by copper no negotiation can move — fixed pads,
    // fanout landings, bodies, the board edge. Naming them separates "lost
    // the negotiation" from "there is no lane at all", which is the
    // difference between a router problem and an escape-planning problem.
    for (name, paths) in &routes {
        if let Some(reason) = paths.failed.as_deref() {
            autopsy_lines.push(format!(
                "congestion: net {name} is blocked even when allowed to share every foreign trace — {reason}"
            ));
        }
    }
    Negotiated {
        board: work,
        report,
        iterations,
        converged,
        autopsy: autopsy_lines,
    }
}

/// Route one net with sharing allowed. Mirrors `router::route_one_net`'s
/// seed/spoke construction, but lays nothing on the board: the copper is
/// stamped into the shared grid (so the next net can see it and price it)
/// and the paths are kept for the legal extraction.
#[allow(clippy::too_many_arguments)]
fn negotiate_one_net(
    grid: &mut Grid,
    net_name: &str,
    net_id: u32,
    pad_points: &[NetPadInfo],
    opts: &RouteOptions,
    rules: &RuleCtx<'_>,
    history: &CostMap,
    fanout: &crate::fanout::FanoutPlan,
    via_cells: i32,
    limits: Limits,
    present_factor: u32,
) -> NetPaths {
    let mut out = NetPaths {
        lower_bound_mm: hpwl_mm(pad_points),
        ..NetPaths::default()
    };
    if pad_points.len() < 2 {
        return out;
    }
    let neg = Some(Negotiate { present_factor });

    // Seed: prefer a pad the fanout did NOT take over, then the
    // geographically central one — identical to the classic driver.
    let eligible: Vec<usize> = {
        let non_fanout: Vec<usize> = (0..pad_points.len())
            .filter(|&i| !fanout.through_pads.contains(&pad_points[i].pad_ref))
            .collect();
        if non_fanout.is_empty() {
            (0..pad_points.len()).collect()
        } else {
            non_fanout
        }
    };
    let seed_idx = *eligible
        .iter()
        .min_by_key(|&&i| {
            pad_points
                .iter()
                .enumerate()
                .filter(|(j, _)| *j != i)
                .map(|(_, q)| {
                    let p = pad_points[i].center;
                    (p.x.0 - q.center.x.0).unsigned_abs() + (p.y.0 - q.center.y.0).unsigned_abs()
                })
                .sum::<u64>()
        })
        .unwrap_or(&0);
    let seed = pad_points[seed_idx].clone();
    let route_point = |p: &NetPadInfo| -> Point {
        fanout
            .via_positions
            .get(&p.pad_ref)
            .copied()
            .unwrap_or(p.center)
    };
    let seed_grid = grid.snap(route_point(&seed), seed.layer);
    let seed_is_fanout = fanout.through_pads.contains(&seed.pad_ref);
    let (net_trace_width, _) = effective_net_rules(opts, net_name);
    let via_model = rules.via_model(net_name, opts.cell);

    let mut net_trace_cells: Vec<GridPoint> = Vec::new();
    let mut spokes_sorted: Vec<NetPadInfo> = pad_points
        .iter()
        .enumerate()
        .filter(|(i, _)| *i != seed_idx)
        .map(|(_, p)| p.clone())
        .collect();
    spokes_sorted.sort_by_key(|q| {
        (seed.center.x.0 - q.center.x.0).unsigned_abs()
            + (seed.center.y.0 - q.center.y.0).unsigned_abs()
    });

    for spoke in spokes_sorted {
        let spoke_grid = grid.snap(route_point(&spoke), spoke.layer);
        let spoke_is_fanout = fanout.through_pads.contains(&spoke.pad_ref);
        let spoke_width = if seed_is_fanout || spoke_is_fanout {
            Length(net_trace_width.0.min(opts.trace_width.0))
        } else {
            net_trace_width
        };
        let clr_model = rules.trace_model(net_name, spoke_width, opts.cell);
        let copper_cells = ceil_cells(spoke_width.0 / 2, opts.cell.0).max(0);
        let Some(result) = search(
            grid,
            seed_grid,
            net_id,
            opts.via_cost,
            spoke_grid,
            Some(&via_model),
            &clr_model,
            history,
            &net_trace_cells,
            opts.heuristic_weight.max(NEG_HEURISTIC_WEIGHT),
            limits,
            neg,
        ) else {
            out.failed = Some(if timed_out(limits.deadline) {
                "timeout (route max_seconds budget)".to_string()
            } else {
                format!(
                    "no path to pad {} at ({:.2}, {:.2}) mm even with negotiated sharing",
                    spoke.pad_ref,
                    spoke.center.x.to_mm(),
                    spoke.center.y.to_mm(),
                )
            });
            return out;
        };
        stamp_path(grid, &result.path, net_id, copper_cells, via_cells);
        for w in result.path.windows(2) {
            if w[0].layer == w[1].layer {
                net_trace_cells.extend(grid.line_cells(w[0], w[1]));
            }
        }
        net_trace_cells.push(spoke_grid);
        out.spokes.push(Spoke {
            path: result.path,
            width: spoke_width,
            target_world: route_point(&spoke),
        });
    }
    out
}

/// Stamp one path's bare copper (and via barrels) into the shared grid.
/// Cells already claimed by another net keep that net's identity — the
/// overlap is exactly what the conflict scan then finds from the other
/// side.
fn stamp_path(grid: &mut Grid, path: &[GridPoint], net_id: u32, copper: i32, via_copper: i32) {
    for w in path.windows(2) {
        if w[0].layer == w[1].layer {
            grid.stamp_trace(w[0], w[1], net_id, copper);
        } else {
            grid.stamp_via(w[0], net_id, via_copper);
        }
    }
}

fn stamp_paths(
    grid: &mut Grid,
    paths: &NetPaths,
    net_id: u32,
    opts: &RouteOptions,
    via_copper: i32,
) {
    for s in &paths.spokes {
        let copper = ceil_cells(s.width.0 / 2, opts.cell.0).max(0);
        stamp_path(grid, &s.path, net_id, copper, via_copper);
    }
}

/// Every net that shares copper or a clearance halo with another, plus the
/// cells where it happens.
#[derive(Default)]
struct Conflicts {
    /// Nets involved in at least one sharing incident.
    nets: BTreeSet<String>,
    /// Sharing incidents per net (used to order the legal extraction:
    /// cleanest nets get to keep their geometry first).
    per_net: BTreeMap<String, u32>,
    /// `(cell, smear radius)` for the history bump.
    hot: Vec<(GridPoint, i32)>,
}

/// Scan every negotiated path against the shared grid. A cell is a conflict
/// when the net's own clearance disk (pairwise, area-aware — the exact test
/// the DRC uses) finds foreign TRACE copper, or when it is hard-blocked.
///
/// Own-pad cells are exempt, mirroring the search: a fine-pitch pad's
/// clearance to its neighbours is fixed placement/fanout geometry, not
/// something the router can negotiate, so requiring it here would make
/// convergence impossible on exactly the boards this pass exists for.
fn detect_conflicts(
    grid: &Grid,
    routes: &BTreeMap<String, NetPaths>,
    rules: &RuleCtx<'_>,
    opts: &RouteOptions,
    ids: &HashMap<&str, u32>,
) -> Conflicts {
    let mut out = Conflicts::default();
    for (name, paths) in routes {
        let Some(&id) = ids.get(name.as_str()) else {
            continue;
        };
        let via_model = rules.via_model(name, opts.cell);
        let mut count = 0u32;
        for spoke in &paths.spokes {
            let model = rules.trace_model(name, spoke.width, opts.cell);
            let radius = model.max_radius_cells().max(1);
            for w in spoke.path.windows(2) {
                if w[0].layer != w[1].layer {
                    if grid
                        .via_soft_clearance(w[0], id, &via_model)
                        .is_none_or(|k| k > 0)
                    {
                        count += 1;
                        out.hot.push((w[0], radius));
                    }
                    continue;
                }
                for cell in grid.line_cells(w[0], w[1]) {
                    if matches!(grid.get(cell), Cell::NetPad(n) | Cell::DrilledPad(n) if n == id) {
                        continue;
                    }
                    if grid
                        .soft_clearance(cell, id, &model, false)
                        .is_none_or(|k| k > 0)
                    {
                        count += 1;
                        out.hot.push((cell, radius));
                    }
                }
            }
        }
        if count > 0 {
            out.nets.insert(name.clone());
        }
        out.per_net.insert(name.clone(), count);
    }
    out
}

/// True if `path` can be laid on `grid` under the ORDINARY hard rules —
/// the same test the classic search applies at every expansion.
fn path_is_legal(
    grid: &Grid,
    path: &[GridPoint],
    net_id: u32,
    trace_model: &crate::grid::ClearanceModel<'_>,
    via_model: &crate::grid::ClearanceModel<'_>,
) -> bool {
    for w in path.windows(2) {
        if w[0].layer != w[1].layer {
            // A via may not land in any drilled pad (it would collide with
            // the existing fab drill) and must keep clearance on every layer.
            for l in 0..grid.layer_count {
                if matches!(
                    grid.get(GridPoint {
                        layer: l,
                        col: w[0].col,
                        row: w[0].row
                    }),
                    Cell::DrilledPad(_)
                ) {
                    return false;
                }
            }
            if !grid.via_clearance_ok(w[0], net_id, via_model) {
                return false;
            }
            continue;
        }
        for cell in grid.line_cells(w[0], w[1]) {
            let c = grid.get(cell);
            if !grid::walkable(c, net_id) {
                return false;
            }
            if matches!(c, Cell::NetPad(n) | Cell::DrilledPad(n) if n == net_id) {
                continue;
            }
            if !grid.clearance_ok(cell, net_id, trace_model) {
                return false;
            }
        }
    }
    true
}

/// Turn the (possibly still-sharing) negotiated state into a LEGAL board.
///
/// Nets are considered cleanest-first (fewest sharing incidents, then board
/// order — deterministic). A net keeps its negotiated geometry when that
/// geometry passes the ordinary hard clearance test against everything
/// already laid; otherwise it goes on the redo list. With `reroute` set, the
/// redo list is then routed the classic way into the space that remains,
/// biased by the negotiation's history map.
#[allow(clippy::too_many_arguments)]
fn extract_legal(
    board: &Board,
    opts: &RouteOptions,
    order: &[String],
    nets: &BTreeMap<String, Vec<NetPadInfo>>,
    fanout: &crate::fanout::FanoutPlan,
    escape_stubs: &[Trace],
    routes: &BTreeMap<String, NetPaths>,
    conflict_counts: &BTreeMap<String, u32>,
    history: &CostMap,
    ids: &HashMap<&str, u32>,
    deadline: Option<Instant>,
    reroute: bool,
) -> (Board, RouteReport) {
    let mut work = board.clone();
    work.clear_routing();
    let mut grid = build_pass_grid(board, opts, order, fanout, escape_stubs);
    let areas = board.rule_areas.clone();
    let schematic = opts.schematic.clone();
    let rules = RuleCtx::new(&areas, schematic.as_deref(), opts, order, &grid);
    let via_cells = via_copper_cells(opts);
    let pours = pour_nets(board);
    let limits = Limits {
        deadline,
        ..Limits::default()
    };

    // Cleanest first: a net with no sharing keeps the path it negotiated,
    // and the contended ones fill in around it.
    let mut candidates: Vec<(u32, usize, &String)> = order
        .iter()
        .enumerate()
        .filter_map(|(i, n)| {
            routes
                .get(n)
                .filter(|p| p.failed.is_none() && !p.spokes.is_empty())
                .map(|_| (conflict_counts.get(n).copied().unwrap_or(0), i, n))
        })
        .collect();
    candidates.sort_unstable();

    let mut outcomes: BTreeMap<String, Outcome> = BTreeMap::new();
    let mut redo: BTreeSet<String> = BTreeSet::new();
    for (_, _, name) in candidates {
        let id = ids[name.as_str()];
        let paths = &routes[name];
        let via_model = rules.via_model(name, opts.cell);
        let legal = paths.spokes.iter().all(|s| {
            let model = rules.trace_model(name, s.width, opts.cell);
            path_is_legal(&grid, &s.path, id, &model, &via_model)
        });
        if !legal {
            redo.insert((*name).clone());
            continue;
        }
        let (mut segs, mut vias, mut len) = (0usize, 0usize, 0.0f64);
        for s in &paths.spokes {
            let copper = ceil_cells(s.width.0 / 2, opts.cell.0).max(0);
            let (sg, v, l) = lay_path(
                &mut work,
                &mut grid,
                &s.path,
                name,
                id,
                opts,
                copper,
                via_cells,
                s.width,
                Some(s.target_world),
            );
            segs += sg;
            vias += v;
            len += l;
        }
        outcomes.insert(
            (*name).clone(),
            Outcome::Ok {
                trace_segments: segs,
                vias,
                length_mm: len,
                lower_bound_mm: paths.lower_bound_mm,
            },
        );
    }

    // Everything the negotiation never completed, plus the nets whose
    // geometry did not survive legalisation.
    let mut leftover: Vec<&String> = order
        .iter()
        .filter(|n| {
            !outcomes.contains_key(*n)
                && !pours.contains(*n)
                && nets.get(*n).is_some_and(|p| p.len() >= 2)
        })
        .collect();
    leftover.sort_by_key(|n| nets.get(*n).map_or(0, Vec::len));
    for name in leftover {
        let id = ids[name.as_str()];
        let pads = nets.get(name).map_or(&[][..], Vec::as_slice);
        if !reroute || timed_out(deadline) {
            let reason = if timed_out(deadline) {
                "timeout (route max_seconds budget)".to_string()
            } else {
                routes
                    .get(name)
                    .and_then(|p| p.failed.clone())
                    .unwrap_or_else(|| {
                        if redo.contains(name) {
                            "negotiated route still shares copper with another net".into()
                        } else {
                            "not routed in this negotiation iteration".into()
                        }
                    })
            };
            outcomes.insert(name.clone(), Outcome::Failed { reason });
            continue;
        }
        let nr = route_one_net(
            &mut work, &mut grid, name, id, pads, opts, &rules, history, fanout, via_cells, &pours,
            limits,
            // Negotiation is its own driver and runs before any pass has
            // failures to analyse, so it carries no reachability verdict.
            &crate::reach::Reach::default(),
        );
        outcomes.insert(
            name.clone(),
            match nr {
                NetRoute::Ok {
                    trace_segments,
                    vias,
                    length_mm,
                    lower_bound_mm,
                } => Outcome::Ok {
                    trace_segments,
                    vias,
                    length_mm,
                    lower_bound_mm,
                },
                NetRoute::Failed { reason, .. } => Outcome::Failed { reason },
            },
        );
    }

    // Trivial nets (pours, single-pad) report Ok exactly like the classic
    // pass does, so the two drivers' reports stay comparable.
    let mut per_net = Vec::with_capacity(order.len());
    let (mut traces, mut vias, mut len, mut lb) = (0usize, 0usize, 0.0f64, 0.0f64);
    for name in order {
        let outcome = outcomes.remove(name).unwrap_or(Outcome::Ok {
            trace_segments: 0,
            vias: 0,
            length_mm: 0.0,
            lower_bound_mm: 0.0,
        });
        if let Outcome::Ok {
            trace_segments,
            vias: v,
            length_mm,
            lower_bound_mm,
        } = &outcome
        {
            traces += trace_segments;
            vias += v;
            len += length_mm;
            lb += lower_bound_mm;
        }
        per_net.push((name.clone(), outcome));
    }

    (
        work,
        RouteReport {
            per_net,
            trace_count: traces,
            via_count: vias,
            total_length_mm: len,
            total_lower_bound_mm: lb,
            ..RouteReport::default()
        },
    )
}

/// Which corridors stayed over-subscribed, in board coordinates — the
/// autopsy the handoff asks for when the plateau survives negotiation.
fn autopsy(
    hot: &BTreeMap<(u8, i32, i32), u32>,
    region: pcb_core::Rect,
    opts: &RouteOptions,
    board: &Board,
) -> Vec<String> {
    if hot.is_empty() {
        return Vec::new();
    }
    let grid = Grid::with_layers(region, opts.cell, board.stackup.layer_count());
    // Cluster the hot cells into 1 mm buckets so the report names corridors,
    // not thousands of individual cells.
    let bucket_nm = Length::from_mm(1.0).0;
    let mut clusters: BTreeMap<(u8, i64, i64), u32> = BTreeMap::new();
    for (&(layer, col, row), &n) in hot {
        let p = grid.unsnap(GridPoint { layer, col, row });
        *clusters
            .entry((layer, p.x.0 / bucket_nm, p.y.0 / bucket_nm))
            .or_default() += n;
    }
    let mut ranked: Vec<((u8, i64, i64), u32)> = clusters.into_iter().collect();
    ranked.sort_by(|a, b| b.1.cmp(&a.1).then(a.0.cmp(&b.0)));
    ranked
        .into_iter()
        .take(8)
        .map(|((layer, bx, by), n)| {
            format!(
                "congestion: layer {layer} around ({:.1}, {:.1}) mm — {n} over-subscribed cell-iteration(s)",
                (bx * bucket_nm) as f64 / 1e6,
                (by * bucket_nm) as f64 / 1e6,
            )
        })
        .collect()
}
