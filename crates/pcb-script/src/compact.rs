//! Board compaction — feasibility-gated outline shrink.
//!
//! Given a routed, DRC-clean board, `compact` searches for the smallest
//! rectangular outline that still lets the placer + router produce a
//! layout with **no more failed nets than the caller allows and 0 DRC
//! errors** (`allow_failed = 0` — the default — means fully routed). The
//! baseline-relative gate exists because a hard "0 failed nets" bar makes
//! compaction useless on a board that does not fully route at ANY size
//! (a fine-pitch QFN board may plateau at 28/39): such a board must still
//! be shrinkable without pretending connectivity got worse. The search
//! never
//! trusts a candidate size on geometry alone: every candidate is proven
//! by cloning the board, re-placing every footprint into the smaller
//! outline, re-routing, and re-running DRC with the exact options the
//! `route` / `drc` verbs use. A size is "feasible" only when that whole
//! pipeline comes back clean, so the result is always manufacturable.
//!
//! Two search phases (both share one feasibility oracle):
//!   1. Binary-search a uniform scale factor `s ∈ [s_min, 1]` applied to
//!      both dimensions (aspect = keep). Converges to 0.5 mm.
//!   2. Greedy per-dimension shrink from the binary-search result:
//!      repeatedly try `W-step` / `H-step` while feasible. This is the
//!      whole of aspect = free, and a cheap refinement for aspect = keep.
//!
//! The core takes a `&Board` (+ a schematic `Arc` and margin maps) and
//! returns a `CompactOutcome` with the best routed board and metrics, so
//! the `compact` verb and the headless `examples/compact.rs` binary share
//! one implementation.

use std::collections::{BTreeSet, HashMap};
use std::sync::Arc;
use std::time::{Duration, Instant};

use pcb_core::{Board, Length, PlacementMargin, Point, Rect, Schematic, SilkText};
use pcb_placer::{place, MarginMap, PlaceOptions};
use pcb_router::{Outcome, RouteOptions};

/// Tunables for a compaction run. Defaults are calibrated for a ~15
/// footprint board finishing within a couple of minutes.
#[derive(Debug, Clone)]
pub struct CompactOptions {
    /// Lower bound on the compacted width (mm). `None` = derive from
    /// component geometry only.
    pub min_w_mm: Option<f64>,
    /// Lower bound on the compacted height (mm).
    pub min_h_mm: Option<f64>,
    /// Greedy per-dimension shrink step (mm).
    pub step_mm: f64,
    /// PRNG seed for the placer. Per-iteration seeds are derived from
    /// this deterministically, so a fixed seed → a fixed result.
    pub seed: u64,
    /// Placer iterations per feasibility check.
    pub place_iters: usize,
    /// `true` = shrink each dimension independently (aspect = free);
    /// `false` = keep aspect ratio in the binary-search phase, then
    /// still run a bounded greedy refinement.
    pub aspect_free: bool,
    /// Base soft body-to-body gap (mm) at full size. Scaled down as the
    /// outline shrinks, but never below the hard solder-access gap
    /// (`solder_gap_mm`). `None` = use the placer default (2.0 mm).
    pub min_gap_mm: Option<f64>,
    /// Hard solder-access floor (mm) plumbed straight to the placer's
    /// `solder_gap_mm`. Even the tightest compaction never leaves two
    /// component bodies closer than this — the user hand-solders and needs
    /// iron-tip access, so parts must never end up nearly touching.
    /// Default 1.0 mm.
    pub solder_gap_mm: f64,
    /// Binary-search iterations in phase 1.
    pub binary_steps: usize,
    /// Hard cap on total feasibility checks (placer+router+DRC runs)
    /// across both phases, so a pathological board can't run forever.
    pub max_checks: usize,
    /// Wall-clock budget. When exceeded the search stops and returns the
    /// best feasible result found so far.
    pub time_budget: Duration,
    /// Board-edge copper clearance (mm) folded into the per-dimension
    /// lower bound. Matches the DRC `edge_clearance` default.
    pub edge_clearance_mm: f64,
    /// Packing allowance on the summed component area used as an
    /// absolute area floor: `area_min = packing_factor * Σ component
    /// area`. > 1 leaves room for routing channels and imperfect packing.
    pub packing_factor: f64,
    /// How many nets a candidate may leave unrouted and still count as
    /// feasible. `0` (default) = the strict "everything routes" bar.
    ///
    /// Raise it for a board that cannot fully route at any size: the gate
    /// then accepts up to `allow_failed` failed nets and, correspondingly,
    /// ignores the `NetSplit` DRC errors those unrouted nets *necessarily*
    /// produce — while still demanding zero clearance / edge / short
    /// errors. Set it to the failure count the board already has at full
    /// size so compaction can never make connectivity worse than the
    /// baseline it started from.
    pub allow_failed: usize,
    /// Per-candidate router budget (seconds). Every feasibility check runs
    /// one bounded route, so total wall clock ≈ `checks × route_seconds`
    /// (still capped by `time_budget`). The 30 s default suits a ~15
    /// footprint board; a fine-pitch QFN needs considerably more before a
    /// probe is a fair verdict rather than a timeout.
    pub route_seconds: f64,
}

impl Default for CompactOptions {
    // `from_secs` reads fine here; `from_mins` is not on our MSRV.
    #[allow(clippy::duration_suboptimal_units)]
    fn default() -> Self {
        Self {
            min_w_mm: None,
            min_h_mm: None,
            step_mm: 1.0,
            seed: 1,
            place_iters: 8000,
            aspect_free: false,
            min_gap_mm: None,
            solder_gap_mm: 1.0,
            binary_steps: 7,
            max_checks: 40,
            time_budget: Duration::from_secs(240),
            edge_clearance_mm: 0.3,
            packing_factor: 1.3,
            allow_failed: 0,
            route_seconds: 30.0,
        }
    }
}

/// Metrics describing what a compaction run achieved.
#[derive(Debug, Clone)]
pub struct CompactMetrics {
    pub old_w_mm: f64,
    pub old_h_mm: f64,
    pub old_area_mm2: f64,
    pub new_w_mm: f64,
    pub new_h_mm: f64,
    pub new_area_mm2: f64,
    /// Percentage area reduction: `(old - new) / old * 100`.
    pub area_reduction_pct: f64,
    pub trace_count: usize,
    pub via_count: usize,
    pub total_length_mm: f64,
    /// Failed nets on the accepted candidate. 0 with the default
    /// `allow_failed = 0`; otherwise the actual count the gate accepted,
    /// so the report never claims a board routes when it does not.
    pub failed_nets: usize,
    /// DRC errors the gate had to reject. Always 0: a candidate is only
    /// accepted with no error other than the `NetSplit` opens implied by
    /// its own (allowed) failed nets.
    pub drc_errors: usize,
    /// Per-dimension geometric lower bound the search was clamped to.
    pub lower_bound_w_mm: f64,
    pub lower_bound_h_mm: f64,
    /// How many feasibility checks (full placer+router+DRC runs) ran.
    pub checks: usize,
}

/// Result of a compaction run.
#[derive(Debug, Clone)]
pub struct CompactOutcome {
    /// The best feasible board (outline shrunk, re-placed, re-routed).
    /// When `shrunk == false` this is a clone of the input, untouched.
    pub board: Board,
    pub metrics: CompactMetrics,
    /// `true` when a smaller feasible outline was found and applied to
    /// `board`; `false` when no shrink was feasible (board untouched).
    pub shrunk: bool,
}

/// A single feasible candidate: the fully re-placed, re-routed board and
/// its headline metrics.
struct Feasible {
    board: Board,
    trace_count: usize,
    via_count: usize,
    total_length_mm: f64,
    /// Nets the router gave up on for this candidate (empty unless
    /// `allow_failed > 0`). Kept as names, not a count, because the trim
    /// phase needs to know exactly which `NetSplit` errors it may excuse.
    failed: BTreeSet<String>,
}

/// Map of `REF.PAD` → net name, matching the pad labels DRC puts in
/// `Violation::involved`. Position-independent (references and nets do not
/// change during compaction), so it is built once per run.
fn pad_net_map(board: &Board) -> HashMap<String, String> {
    let mut out = HashMap::new();
    for fp in board.footprints_in_order() {
        for p in &fp.pads {
            if let Some(net) = p.net.as_ref() {
                out.insert(format!("{}.{}", fp.reference, p.number), net.clone());
            }
        }
    }
    out
}

/// Nets the router reported as `Failed` on this pass.
fn failed_nets_of(report: &pcb_router::RouteReport) -> BTreeSet<String> {
    report
        .per_net
        .iter()
        .filter(|(_, o)| matches!(o, Outcome::Failed { .. }))
        .map(|(n, _)| n.clone())
        .collect()
}

/// The net a violation is about, or `None` when it cannot be attributed to
/// exactly one net.
///
/// Primary source is `involved`: DRC labels pads `REF.PAD`, so resolving
/// every label through the board's pad→net map is exact. The quoted name in
/// the message is a fallback for reports whose pads are not in the map
/// (a synthetic report in a unit test, a pad renamed mid-flight).
fn violation_net(v: &pcb_drc::Violation, pad_nets: &HashMap<String, String>) -> Option<String> {
    // Exact when every involved pad is known AND they all name one net; a
    // missing pad or a second net makes the attribution ambiguous.
    let mut resolved = v.involved.iter().map(|label| pad_nets.get(label));
    if let Some(Some(first)) = resolved.next() {
        if resolved.all(|net| net == Some(first)) {
            return Some(first.clone());
        }
    }
    // Fallback: `net "NAME" is split into …`.
    let mut parts = v.message.split('"');
    parts.next()?;
    parts.next().map(str::to_string)
}

/// **The** DRC half of the feasibility gate, as a pure function of a
/// report. `true` when nothing in it disqualifies the candidate.
///
/// Every `Error` fails the gate except a `NetSplit` on a net in `excused`:
/// a net the router could not finish *necessarily* leaves its pads on
/// separate copper islands, so counting that as a DRC failure would just
/// re-reject the connectivity the caller already accepted via
/// `allow_failed`. Nothing else is ever excused — clearance, edge, body
/// and short errors are all things a shrink can *create*, and a short is
/// never a consequence of an open. Warnings never gate.
fn drc_gate_passes(
    drc: &pcb_drc::DrcReport,
    pad_nets: &HashMap<String, String>,
    excused: &BTreeSet<String>,
) -> bool {
    // Fast path: the strict, overwhelmingly common case.
    if drc.error_count == 0 {
        return true;
    }
    if excused.is_empty() {
        return false;
    }
    drc.violations.iter().all(|v| {
        v.severity != pcb_drc::Severity::Error
            || (v.kind == pcb_drc::ViolationKind::NetSplit
                && violation_net(v, pad_nets).is_some_and(|n| excused.contains(&n)))
    })
}

/// **The** feasibility gate for one candidate: connectivity budget plus
/// the DRC gate above. Pure, so the policy can be unit-tested against
/// synthetic route/DRC reports instead of only end-to-end.
fn candidate_passes(
    failed: &BTreeSet<String>,
    drc: &pcb_drc::DrcReport,
    pad_nets: &HashMap<String, String>,
    allow_failed: usize,
) -> bool {
    failed.len() <= allow_failed && drc_gate_passes(drc, pad_nets, failed)
}

/// Nets a DRC report says are split into isolated islands. Used to seed
/// the trim gate's excused set for a board whose routing was carried over
/// rather than produced here (so there is no route report to read).
fn split_nets(drc: &pcb_drc::DrcReport, pad_nets: &HashMap<String, String>) -> BTreeSet<String> {
    drc.violations
        .iter()
        .filter(|v| v.kind == pcb_drc::ViolationKind::NetSplit)
        .filter_map(|v| violation_net(v, pad_nets))
        .collect()
}

/// Geometric lower bound on the outline. Returns `(w_min, h_min,
/// area_min)` in mm / mm².
///
/// Per-dimension floor: a component always fits if the board's shorter
/// side clears the component's shorter side (it can be rotated), so the
/// floor is `max over parts of min(width, height)` plus twice the edge
/// clearance. `min_w_mm` / `min_h_mm` raise the floor further.
///
/// Area floor: `packing_factor * Σ (width · height)` over every
/// component's inflated (margin-folded) bbox — a hard minimum below
/// which no packing can fit the copper.
#[must_use]
pub fn lower_bound_outline(
    board: &Board,
    margins: &MarginMap,
    opts: &CompactOptions,
) -> (f64, f64, f64) {
    let mut max_min_side = 0.0_f64;
    let mut sum_area = 0.0_f64;
    for fp in board.footprints_in_order() {
        let Some(bb) = inflated_bounds(fp, margins) else {
            continue;
        };
        let w = bb.width().to_mm();
        let h = bb.height().to_mm();
        // The body still has to FIT on the board, elevated or not, so
        // the dimension floor always uses the inflated bbox.
        max_min_side = max_min_side.max(w.min(h));
        // …but a socketed module's plastic does not consume board
        // plane: the parts it shadows use that same area. Counting it
        // in the packing floor would forbid the very layout the
        // elevation allows, so an elevated part contributes only the
        // area its pads actually occupy.
        if margins.get(&fp.id).is_some_and(|m| m.elevated) {
            if let Some(pads) = fp.bounds() {
                sum_area += pads.width().to_mm() * pads.height().to_mm();
                continue;
            }
        }
        sum_area += w * h;
    }
    let dim_floor = max_min_side + 2.0 * opts.edge_clearance_mm;
    let w_min = dim_floor.max(opts.min_w_mm.unwrap_or(0.0));
    let h_min = dim_floor.max(opts.min_h_mm.unwrap_or(0.0));
    let area_min = opts.packing_factor * sum_area;
    (w_min, h_min, area_min)
}

/// World-frame bbox of a footprint inflated by its placement margin (if
/// any). Mirrors the placer's `fp_bounds_with_margin`.
fn inflated_bounds(fp: &pcb_core::Footprint, margins: &MarginMap) -> Option<Rect> {
    let base = fp.bounds()?;
    let Some(margin) = margins.get(&fp.id) else {
        return Some(base);
    };
    if margin.is_zero() {
        return Some(base);
    }
    let world = pcb_core::rotate_margin_trbl(margin.as_trbl_mm(), fp.rotation);
    let [t, r, b, l] = world;
    Some(Rect {
        min: Point::new(
            base.min.x - Length::from_mm(l),
            base.min.y - Length::from_mm(b),
        ),
        max: Point::new(
            base.max.x + Length::from_mm(r),
            base.max.y + Length::from_mm(t),
        ),
    })
}

/// Run board compaction. Returns the best feasible (or untouched) board
/// plus metrics. Errors only when the board has no outline to shrink.
// `drc_margins` always uses the default hasher at every call site; a
// generic `BuildHasher` param would only add noise.
#[allow(clippy::implicit_hasher)]
pub fn compact(
    base: &Board,
    schematic: &Arc<Schematic>,
    place_margins: &MarginMap,
    drc_margins: &HashMap<String, PlacementMargin>,
    fab_profile: Option<&pcb_drc::FabProfile>,
    opts: &CompactOptions,
) -> Result<CompactOutcome, String> {
    let outline = base
        .outline
        .ok_or_else(|| "compact needs a board outline; set one with `outline W H`".to_string())?;
    let base_min = outline.min;
    let w_cur = outline.width().to_mm();
    let h_cur = outline.height().to_mm();
    let old_area = w_cur * h_cur;

    let (w_min, h_min, area_min) = lower_bound_outline(base, place_margins, opts);

    // Baseline metrics, used for the "no shrink" path.
    let untouched_metrics = |checks: usize| CompactMetrics {
        old_w_mm: w_cur,
        old_h_mm: h_cur,
        old_area_mm2: old_area,
        new_w_mm: w_cur,
        new_h_mm: h_cur,
        new_area_mm2: old_area,
        area_reduction_pct: 0.0,
        trace_count: base.traces.len(),
        via_count: base.vias.len(),
        total_length_mm: 0.0,
        failed_nets: 0,
        drc_errors: 0,
        lower_bound_w_mm: w_min,
        lower_bound_h_mm: h_min,
        checks,
    };

    // Scale lower bound: both dims scale by `s`, so `s` must clear every
    // per-dimension floor and the area floor.
    let s_min = (w_min / w_cur)
        .max(h_min / h_cur)
        .max((area_min / old_area).sqrt())
        .clamp(0.0, 1.0);
    // Already at (or below) the geometric floor — nothing to gain.
    if s_min >= 1.0 - 1e-6 {
        return Ok(CompactOutcome {
            board: base.clone(),
            metrics: untouched_metrics(0),
            shrunk: false,
        });
    }

    let route_opts = RouteOptions {
        cell: Length::from_mm(0.20),
        trace_width: Length::from_mm(0.25),
        clearance: Length::from_mm(0.20),
        fine_escape: false,
        via_cost: 8,
        via_drill: Length::from_mm(0.30),
        via_diameter: Length::from_mm(0.60),
        net_overrides: HashMap::new(),
        schematic: Some(schematic.clone()),
        // Compaction probes many candidates; the organic pass is cheap
        // and DRC-neutral, so keeping it on makes every feasibility
        // check exercise the same pipeline the final `route` verb runs.
        organic: true,
        engine: Default::default(),
        organic_fillet_mm: 3.0,
        initial_net_order: None,
        heuristic_weight: 1.0,
        // Compaction probes many candidates — keep each route bounded so
        // a single bad candidate cannot burn the whole time budget. The
        // budget is a knob because a fine-pitch board needs more than the
        // 30 s default before a probe means "infeasible" rather than
        // "ran out of time".
        max_seconds: Some(opts.route_seconds),
        // Compaction wants the cheapest reliable answer per candidate, not
        // the best one; the negotiation loop is an iterative process that
        // pays off over a full route budget, not a 30 s feasibility probe.
        negotiate: false,
        on_progress: None,
    };
    let base_min_gap = opts
        .min_gap_mm
        .unwrap_or_else(|| PlaceOptions::default().min_gap_mm);

    let base_radius = base.outline_corner_radius;
    let pad_nets = pad_net_map(base);
    // Edge-mount violations the INPUT board already has (a screw
    // terminal whose pads sit inland while its wire-entry body reaches
    // the cut). Compaction may inherit these; it may not add to them.
    let edge_excused = edge_mount_violators(base);
    let edge_excused_trim = edge_excused.clone();
    let start = Instant::now();
    let mut checks = 0usize;
    let mut best: Option<(f64, f64, Feasible)> = None;

    // One feasibility check: prove a W×H outline routes + passes DRC.
    let feasible = |w_mm: f64, h_mm: f64, checks: &mut usize| -> Option<Feasible> {
        if *checks >= opts.max_checks || start.elapsed() >= opts.time_budget {
            return None;
        }
        *checks += 1;
        let seed = derive_seed(opts.seed, *checks);
        try_feasible(
            base,
            base_min,
            w_mm,
            h_mm,
            base_radius,
            seed,
            base_min_gap,
            old_area,
            opts,
            place_margins,
            &route_opts,
            drc_margins,
            fab_profile,
            schematic,
            &pad_nets,
            &edge_excused,
        )
    };

    // ── Phase 1: binary search a uniform scale factor. ──
    // Invariant: `hi` brackets a feasible-or-larger scale, `lo` a
    // (presumed) infeasible one. When a mid is feasible we record it and
    // pull `hi` down (try smaller); otherwise we push `lo` up.
    let mut lo = s_min;
    let mut hi = 1.0_f64;
    let bigger_dim = w_cur.max(h_cur);
    for _ in 0..opts.binary_steps {
        if (hi - lo) * bigger_dim < 0.5 {
            break;
        }
        let mid = 0.5 * (lo + hi);
        let (w, h) = (w_cur * mid, h_cur * mid);
        if let Some(f) = feasible(w, h, &mut checks) {
            best = Some((w, h, f));
            hi = mid;
        } else {
            lo = mid;
        }
    }

    // Greedy per-dimension shrink from `(bw, bh)`: repeatedly try
    // `W-step` / `H-step` while feasible, re-placing + re-routing each
    // candidate. Returns whether it shrank the outline at all.
    let run_greedy = |mut bw: f64,
                      mut bh: f64,
                      best: &mut Option<(f64, f64, Feasible)>,
                      checks: &mut usize|
     -> bool {
        let mut any = false;
        loop {
            if start.elapsed() >= opts.time_budget || *checks >= opts.max_checks {
                break;
            }
            let mut improved = false;
            if bw - opts.step_mm >= w_min {
                if let Some(f) = feasible(bw - opts.step_mm, bh, checks) {
                    bw -= opts.step_mm;
                    *best = Some((bw, bh, f));
                    improved = true;
                    any = true;
                }
            }
            if bh - opts.step_mm >= h_min {
                if let Some(f) = feasible(bw, bh - opts.step_mm, checks) {
                    bh -= opts.step_mm;
                    *best = Some((bw, bh, f));
                    improved = true;
                    any = true;
                }
            }
            if !improved {
                break;
            }
        }
        any
    };

    // Trim a board's floating content against the edges, gated by DRC.
    // Returns the trimmed clone (outline shrunk to hug the content) only
    // when the trim both shrinks the outline and stays DRC-clean. Rounded
    // corners can nudge a just-trimmed edge into violation, so retry with
    // increasing slack a couple of times before giving up.
    //
    // `excused` names the nets whose `NetSplit` opens the gate must ignore
    // (see `drc_gate_passes`). A trim is a rigid translation, so it cannot
    // change connectivity: excusing exactly the nets that were already
    // open before the slide can hide nothing the trim itself introduced.
    //
    // Bounded by `max_checks` only, NOT the wall-clock budget: a trim is
    // a single DRC run (no placer/router), so it must still run to hug
    // the edges even once the expensive search has burned the time budget.
    let trim_apply = |b: &Board, excused: &BTreeSet<String>, checks: &mut usize| -> Option<Board> {
        for &slack in &[0.05_f64, 0.10, 0.20] {
            if *checks >= opts.max_checks {
                break;
            }
            let mut cand = b.clone();
            // A larger slack can only hug less, so if the tightest slack
            // doesn't shrink at all there is nothing to gain.
            if !trim_to_content(&mut cand, place_margins, opts.edge_clearance_mm, slack) {
                return None;
            }
            *checks += 1;
            // A trim slides the OUTLINE, not the parts, so it can pull
            // the cut away from an edge-mounted part that was flush
            // against it — same unbuildable result as a bad placement,
            // and DRC is just as blind to it.
            if !edge_mount_violators(&cand).is_subset(&edge_excused_trim) {
                continue;
            }
            let drc = run_drc(&cand, drc_margins, fab_profile, schematic);
            if drc_gate_passes(&drc, &pad_nets, excused) {
                return Some(cand);
            }
        }
        None
    };

    // ── Phase 2: greedy per-dimension shrink. ──
    // Runs whether aspect is keep (refinement) or free (the main event),
    // starting from the best size we have. If phase 1 found nothing, seed
    // the greedy pass from the full outline so a shape that only shrinks
    // on one axis is still discovered.
    let (bw0, bh0) = best.as_ref().map_or((w_cur, h_cur), |(w, h, _)| (*w, *h));
    run_greedy(bw0, bh0, &mut best, &mut checks);

    // ── Phase 3: pull the (HPWL-centred, floating) content against the
    // edges, then let the greedy pass exploit the freed border. Alternate
    // trim → greedy until neither shrinks the outline further. ──
    let mut trimmed_last = false;
    if best.is_some() {
        while let Some((bw, bh, feas)) = best.take() {
            let excused = feas.failed.clone();
            let Some(tb) = trim_apply(&feas.board, &excused, &mut checks) else {
                // Content already hugs the edges — nothing to trim.
                best = Some((bw, bh, feas));
                break;
            };
            let to = tb.outline.expect("trim keeps an outline");
            let (tw, th) = (to.width().to_mm(), to.height().to_mm());
            let meaningful = (bw - tw) > 0.5 || (bh - th) > 0.5;
            // Rigid translation: routing is unchanged, so the trace/via
            // counts and wire length carry over verbatim.
            best = Some((tw, th, Feasible { board: tb, ..feas }));
            trimmed_last = true;
            if !meaningful {
                break;
            }
            // Content now hugs the edges; re-run the greedy shrink from
            // the trimmed size — the freed border may make new candidates
            // feasible. If it improves, loop to trim the fresh board.
            if !run_greedy(tw, th, &mut best, &mut checks) {
                break;
            }
            trimmed_last = false;
        }
        // Guarantee the returned board hugs its content even if the loop
        // exited right after a greedy improvement (e.g. on the budget).
        if !trimmed_last {
            if let Some((bw, bh, feas)) = best.take() {
                let excused = feas.failed.clone();
                best = match trim_apply(&feas.board, &excused, &mut checks) {
                    Some(tb) => {
                        let to = tb.outline.expect("trim keeps an outline");
                        Some((
                            to.width().to_mm(),
                            to.height().to_mm(),
                            Feasible { board: tb, ..feas },
                        ))
                    }
                    None => Some((bw, bh, feas)),
                };
            }
        }
    }

    match best {
        Some((w, h, f)) if w * h < old_area - 1e-6 => {
            let new_area = w * h;
            let metrics = CompactMetrics {
                old_w_mm: w_cur,
                old_h_mm: h_cur,
                old_area_mm2: old_area,
                new_w_mm: w,
                new_h_mm: h,
                new_area_mm2: new_area,
                area_reduction_pct: (old_area - new_area) / old_area * 100.0,
                trace_count: f.trace_count,
                via_count: f.via_count,
                total_length_mm: f.total_length_mm,
                failed_nets: f.failed.len(),
                drc_errors: 0,
                lower_bound_w_mm: w_min,
                lower_bound_h_mm: h_min,
                checks,
            };
            Ok(CompactOutcome {
                board: f.board,
                metrics,
                shrunk: true,
            })
        }
        _ => {
            // Even when no scale/greedy shrink was feasible, a plain trim
            // of the ORIGINAL routed board may still pull the content off
            // the borders and shrink the outline — that alone satisfies
            // "compact the edges". Routing is carried over untouched.
            //
            // There is no route report for the input board, so when the
            // caller tolerates failures the excused set is measured from
            // the board itself: whichever nets DRC already sees as split.
            // Not counted as a check — it is a measurement of the input,
            // not a candidate, and must not perturb `checks` (determinism
            // tests compare it) or the budget.
            let base_excused = if opts.allow_failed > 0 {
                split_nets(
                    &run_drc(base, drc_margins, fab_profile, schematic),
                    &pad_nets,
                )
            } else {
                BTreeSet::new()
            };
            if let Some(tb) = trim_apply(base, &base_excused, &mut checks) {
                let to = tb.outline.expect("trim keeps an outline");
                let (w, h) = (to.width().to_mm(), to.height().to_mm());
                if w * h < old_area - 1e-6 {
                    let new_area = w * h;
                    let metrics = CompactMetrics {
                        old_w_mm: w_cur,
                        old_h_mm: h_cur,
                        old_area_mm2: old_area,
                        new_w_mm: w,
                        new_h_mm: h,
                        new_area_mm2: new_area,
                        area_reduction_pct: (old_area - new_area) / old_area * 100.0,
                        trace_count: base.traces.len(),
                        via_count: base.vias.len(),
                        total_length_mm: sum_trace_length(base),
                        // Routing is the input's, so its open nets are too.
                        failed_nets: base_excused.len(),
                        drc_errors: 0,
                        lower_bound_w_mm: w_min,
                        lower_bound_h_mm: h_min,
                        checks,
                    };
                    return Ok(CompactOutcome {
                        board: tb,
                        metrics,
                        shrunk: true,
                    });
                }
            }
            Ok(CompactOutcome {
                board: base.clone(),
                metrics: untouched_metrics(checks),
                shrunk: false,
            })
        }
    }
}

/// References of every edge-mounted footprint that is NOT sitting on the
/// outline it declares. Used as a set difference, never as an absolute:
/// the rule is pad-based, and a real board can enter compaction already
/// violating it (the fecha gateway's screw terminal has its pads inland
/// and only its wire-entry body reaching the cut). Compaction must not
/// ADD to this set — that is what turns a reachable USB-C port into a
/// buried one — but it is not compaction's job to fix what came in.
fn edge_mount_violators(board: &Board) -> BTreeSet<String> {
    board
        .footprints_in_order()
        .filter(|fp| board.edge_mount_violation(fp).is_some())
        .map(|fp| fp.reference.clone())
        .collect()
}

/// Deterministically derive a per-check placer seed from the base seed
/// and the check index, so the same base seed → the same search.
fn derive_seed(base: u64, check: usize) -> u64 {
    base.wrapping_mul(0x9E37_79B9_7F4A_7C15)
        .wrapping_add(check as u64)
        .wrapping_add(1)
        .max(1)
}

/// Build one candidate board at `w_mm × h_mm`, re-place, re-route,
/// re-DRC. `Some` iff the candidate clears `candidate_passes` — with the
/// default `allow_failed = 0` that is "every net routes and DRC is
/// error-free".
#[allow(clippy::too_many_arguments)]
fn try_feasible(
    base: &Board,
    base_min: Point,
    w_mm: f64,
    h_mm: f64,
    corner_radius: Length,
    seed: u64,
    base_min_gap: f64,
    old_area: f64,
    opts: &CompactOptions,
    place_margins: &MarginMap,
    route_opts: &RouteOptions,
    drc_margins: &HashMap<String, PlacementMargin>,
    fab_profile: Option<&pcb_drc::FabProfile>,
    schematic: &Arc<Schematic>,
    pad_nets: &HashMap<String, String>,
    edge_excused: &BTreeSet<String>,
) -> Option<Feasible> {
    let mut b = base.clone();
    let new_outline = Rect::from_corners(
        base_min,
        Point::new(
            base_min.x + Length::from_mm(w_mm),
            base_min.y + Length::from_mm(h_mm),
        ),
    );
    // Clamp the corner radius to half the shorter side of the new outline.
    let cap = new_outline.width().0.min(new_outline.height().0) / 2;
    b.outline = Some(new_outline);
    b.outline_corner_radius = Length(corner_radius.0.max(0).min(cap));
    // Traces/vias are re-laid from scratch; pours stay (they are
    // net/layer policies, not geometry, and re-fill downstream).
    b.clear_routing();

    // Snap edge-mounted parts onto the new outline first, then clamp any
    // footprint poking outside back inside — otherwise the placer's hard
    // "edge parts must touch the outline" / "fit inside" constraints can
    // start from an infeasible pose and never recover.
    for id in b.footprint_order.clone() {
        if let Some(fp) = b.footprints.get(&id) {
            if fp.edge_mounted {
                if let Some(delta) = snap_to_nearest_edge(fp, new_outline) {
                    if let Some(fp) = b.footprints.get_mut(&id) {
                        fp.position = fp.position.translate(delta.0, delta.1);
                    }
                }
            }
        }
        if let Some(fp) = b.footprints.get(&id) {
            if let Some(delta) = clamp_inside(fp, new_outline) {
                if let Some(fp) = b.footprints.get_mut(&id) {
                    fp.position = fp.position.translate(delta.0, delta.1);
                }
            }
        }
    }
    // Board-level silk that would now fall outside the outline is pulled
    // back inside with a small margin.
    clamp_silk_texts(&mut b.silk_texts, new_outline);

    // Scale the soft gap preference down with the board, floored at the
    // hard solder-access gap so the soft preference never drops below the
    // gap the user needs for hand-soldering.
    let scale = (w_mm * h_mm / old_area).sqrt();
    let min_gap = (base_min_gap * scale).max(opts.solder_gap_mm);

    let place_opts = PlaceOptions {
        seed,
        max_iterations: opts.place_iters,
        min_gap_mm: min_gap,
        solder_gap_mm: opts.solder_gap_mm,
        // Compaction starts from an already-structured layout and probes
        // MANY candidate outlines — re-running the electrostatic global
        // stage on every candidate would re-flow the whole board each
        // time (slow, and it can undo the structure the previous steps
        // settled on). SA-only is the right tool for "same layout,
        // slightly smaller box".
        global_stage: false,
        // Compaction owns the outline (trim phase + full DRC re-check);
        // the placer's own edge stand-off would just waste packable
        // millimetres here.
        edge_clearance_mm: 0.0,
        ..PlaceOptions::default()
    };
    let movable: Vec<String> = b
        .footprints_in_order()
        .map(|fp| fp.reference.clone())
        .collect();
    place(&mut b, &movable, &place_opts, place_margins).ok()?;

    // Hard feasibility: the finished placement must honour the
    // solder-access floor on the MARGIN-INFLATED bodies. The SA only
    // guarantees "never worsen" — a candidate whose scaled starting
    // layout dropped below the floor can end still below it, and the
    // DRC gate alone does not model body-to-body access.
    if pcb_placer::min_pairwise_gap(&b, place_margins) < opts.solder_gap_mm - 0.02 {
        return None;
    }

    // An edge-mounted part driven inboard makes the candidate
    // unbuildable: the USB-C port / wire entry / antenna face it exists
    // for is no longer reachable. Nothing downstream notices — DRC has
    // no violation kind for it, and the SA hands back its best-seen
    // layout even when no legal one was found — so gate on it here, for
    // the same reason the solder floor is gated above.
    if !edge_mount_violators(&b).is_subset(edge_excused) {
        return None;
    }

    // An edge-mounted part that drifted inboard makes the candidate
    // unbuildable: the USB-C port / wire entry / antenna face it exists
    // for is no longer reachable, and nothing downstream would notice —
    // DRC has no violation kind for it, and the SA hands back its
    // best-seen layout even when no legal one was found. Gate on it here
    // for the same reason the solder floor is gated above.
    if b.footprints_in_order()
        .any(|fp| b.edge_mount_violation(fp).is_some())
    {
        return None;
    }

    // Every footprint just moved, so any rule area declared *around* a
    // package (a fine-pitch escape zone) must follow it — otherwise the
    // router below runs against a rect that no longer covers the QFN it
    // was declared for, and the DRC gate judges the result under different
    // rules than the router laid copper with. Must happen BEFORE routing.
    pcb_core::reanchor_rule_areas(&mut b);

    let report = pcb_router::route(&mut b, route_opts);
    let failed = failed_nets_of(&report);
    // Cheap pre-check: bail before paying for DRC when the connectivity
    // budget is already blown.
    if failed.len() > opts.allow_failed {
        return None;
    }

    let drc = run_drc(&b, drc_margins, fab_profile, schematic);
    if !candidate_passes(&failed, &drc, pad_nets, opts.allow_failed) {
        return None;
    }

    Some(Feasible {
        board: b,
        trace_count: report.trace_count,
        via_count: report.via_count,
        total_length_mm: report.total_length_mm,
        failed,
    })
}

/// Run DRC with the exact options the feasibility check uses and hand back
/// the whole report. Factored out so the trim phase gates on the same
/// rules `try_feasible` does — and so both go through `drc_gate_passes`
/// rather than each deciding what an "error" means.
fn run_drc(
    board: &Board,
    drc_margins: &HashMap<String, PlacementMargin>,
    fab_profile: Option<&pcb_drc::FabProfile>,
    schematic: &Arc<Schematic>,
) -> pcb_drc::DrcReport {
    let drc_opts = pcb_drc::DrcOptions {
        placement_margins: drc_margins.clone(),
        schematic: Some(schematic.clone()),
        fab_profile: fab_profile.cloned(),
        ..pcb_drc::DrcOptions::default()
    };
    pcb_drc::run(board, &drc_opts)
}

/// Tight world-frame bbox of everything the edge / body-off-board DRC
/// checks measure against the outline: footprint inflated bounds (the
/// body keep-out `BodyOffBoard` uses), trace segments grown by their
/// half-width, and vias grown by their radius. Board-level silk is
/// deliberately excluded — it is movable and re-clamped after the
/// translation. `None` when the board has no such content.
fn content_bbox(board: &Board, margins: &MarginMap) -> Option<Rect> {
    let mut acc: Option<Rect> = None;
    let mut add = |r: Rect| acc = Some(acc.map_or(r, |a| a.union(r)));
    for fp in board.footprints_in_order() {
        if let Some(bb) = inflated_bounds(fp, margins) {
            add(bb);
        }
    }
    for t in &board.traces {
        let half = t.width / 2;
        add(Rect::from_corners(
            Point::new(t.start.x.min(t.end.x) - half, t.start.y.min(t.end.y) - half),
            Point::new(t.start.x.max(t.end.x) + half, t.start.y.max(t.end.y) + half),
        ));
    }
    for v in &board.vias {
        add(Rect::from_center(v.position, v.diameter, v.diameter));
    }
    acc
}

/// Summed length (mm) of every trace segment. Used to report wire
/// length on the "trim only" path, where routing is carried over from
/// the input board rather than produced by the router.
fn sum_trace_length(board: &Board) -> f64 {
    board
        .traces
        .iter()
        .map(|t| {
            let dx = t.end.x.to_mm() - t.start.x.to_mm();
            let dy = t.end.y.to_mm() - t.start.y.to_mm();
            dx.hypot(dy)
        })
        .sum()
}

/// Rigidly slide all copper (footprints, traces, vias) so the tight
/// content bbox hugs the outline at `edge_clearance_mm + slack_mm` on
/// every side, then shrink the outline to match. A pure translation —
/// trace-trace and pad geometry are untouched, so NO re-route is needed.
/// Board silk is re-clamped afterwards. The slack sits just above the
/// DRC edge clearance so float rounding can't manufacture an edge
/// violation. Returns `true` iff the outline actually got smaller.
///
/// Rule areas and keepouts move with the copper, because they are
/// statements about *where the copper is*, not about the board's
/// coordinate origin: an escape zone left behind would stop covering its
/// QFN, and a keepout left behind would let a trace slide into the region
/// the user fenced off. Anchored areas are re-derived from their footprint
/// (one implementation, in `pcb_core::reanchor_rule_areas`); un-anchored
/// ones and keepout polygons take the same rigid delta as the copper.
/// Neither is clamped to the outline — an area may legally poke past it.
fn trim_to_content(
    board: &mut Board,
    margins: &MarginMap,
    edge_clearance_mm: f64,
    slack_mm: f64,
) -> bool {
    let Some(outline) = board.outline else {
        return false;
    };
    let Some(content) = content_bbox(board, margins) else {
        return false;
    };
    let pad = Length::from_mm(edge_clearance_mm + slack_mm);
    // Slide content so its min corner sits `pad` inside the outline's min
    // corner (the board's coordinate anchor stays put).
    let dx = (outline.min.x + pad) - content.min.x;
    let dy = (outline.min.y + pad) - content.min.y;
    for id in board.footprint_order.clone() {
        if let Some(fp) = board.footprints.get_mut(&id) {
            fp.position = fp.position.translate(dx, dy);
        }
    }
    for t in &mut board.traces {
        t.start = t.start.translate(dx, dy);
        t.end = t.end.translate(dx, dy);
    }
    for v in &mut board.vias {
        v.position = v.position.translate(dx, dy);
    }
    // Un-anchored rule areas: same rigid delta as the copper they govern.
    for area in &mut board.rule_areas {
        if area.anchor_ref.is_some() {
            continue;
        }
        area.rect = Rect {
            min: area.rect.min.translate(dx, dy),
            max: area.rect.max.translate(dx, dy),
        };
    }
    // Keepout polygons likewise (a keepout is a rectangle in practice, but
    // the model stores an arbitrary closed polygon).
    for k in &mut board.keepouts {
        for p in &mut k.polygon {
            *p = p.translate(dx, dy);
        }
    }
    // Anchored areas re-derive from the footprints we just moved.
    pcb_core::reanchor_rule_areas(board);
    // New outline: content size + a clearance ring on every side.
    let new_outline = Rect::from_corners(
        outline.min,
        Point::new(
            outline.min.x + content.width() + pad * 2,
            outline.min.y + content.height() + pad * 2,
        ),
    );
    board.outline = Some(new_outline);
    // Keep the corner radius, clamped to half the shorter (new) side.
    let cap = new_outline.width().0.min(new_outline.height().0) / 2;
    board.outline_corner_radius = Length(board.outline_corner_radius.0.max(0).min(cap));
    // Board-level silk that would now fall outside the outline is pulled
    // back inside with a small margin.
    clamp_silk_texts(&mut board.silk_texts, new_outline);

    let old_area = outline.width().to_mm() * outline.height().to_mm();
    let new_area = new_outline.width().to_mm() * new_outline.height().to_mm();
    new_area < old_area - 1e-9
}

/// Translation (dx, dy) that moves `fp` so its bbox touches the nearest
/// side of `outline`. `None` if the footprint has no bounds.
fn snap_to_nearest_edge(fp: &pcb_core::Footprint, outline: Rect) -> Option<(Length, Length)> {
    let b = fp.bounds()?;
    let d_left = (b.min.x.0 - outline.min.x.0).abs();
    let d_right = (outline.max.x.0 - b.max.x.0).abs();
    let d_bottom = (b.min.y.0 - outline.min.y.0).abs();
    let d_top = (outline.max.y.0 - b.max.y.0).abs();
    let nearest = d_left.min(d_right).min(d_bottom).min(d_top);
    let (mut dx, mut dy) = (Length::ZERO, Length::ZERO);
    if nearest == d_left {
        dx = outline.min.x - b.min.x;
    } else if nearest == d_right {
        dx = outline.max.x - b.max.x;
    } else if nearest == d_bottom {
        dy = outline.min.y - b.min.y;
    } else {
        dy = outline.max.y - b.max.y;
    }
    Some((dx, dy))
}

/// Translation (dx, dy) that pulls `fp`'s bbox fully inside `outline`.
/// `None` when it already fits or has no bounds.
fn clamp_inside(fp: &pcb_core::Footprint, outline: Rect) -> Option<(Length, Length)> {
    let b = fp.bounds()?;
    let mut dx = Length::ZERO;
    let mut dy = Length::ZERO;
    if b.min.x.0 < outline.min.x.0 {
        dx = outline.min.x - b.min.x;
    } else if b.max.x.0 > outline.max.x.0 {
        dx = outline.max.x - b.max.x;
    }
    if b.min.y.0 < outline.min.y.0 {
        dy = outline.min.y - b.min.y;
    } else if b.max.y.0 > outline.max.y.0 {
        dy = outline.max.y - b.max.y;
    }
    if dx.0 == 0 && dy.0 == 0 {
        None
    } else {
        Some((dx, dy))
    }
}

/// Clamp every board-level silk text anchor to sit at least `MARGIN_MM`
/// inside `outline`, so a label doesn't spill past a shrunk edge.
fn clamp_silk_texts(texts: &mut [SilkText], outline: Rect) {
    const MARGIN_MM: f64 = 1.0;
    let m = Length::from_mm(MARGIN_MM);
    let lo_x = outline.min.x + m;
    let hi_x = outline.max.x - m;
    let lo_y = outline.min.y + m;
    let hi_y = outline.max.y - m;
    for t in texts {
        // Guard against a tiny outline where the margins cross over.
        if lo_x.0 <= hi_x.0 {
            t.position.x = Length(t.position.x.0.clamp(lo_x.0, hi_x.0));
        }
        if lo_y.0 <= hi_y.0 {
            t.position.y = Length(t.position.y.0.clamp(lo_y.0, hi_y.0));
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use pcb_core::{CopperLayer, Footprint, Id, Pad, Trace, Via};

    fn pad(num: &str, off_x: f64, off_y: f64, net: Option<&str>) -> Pad {
        Pad {
            number: num.into(),
            name: String::new(),
            offset: Point::new(Length::from_mm(off_x), Length::from_mm(off_y)),
            size: (Length::from_mm(1.0), Length::from_mm(1.2)),
            layer: CopperLayer::Top,
            net: net.map(str::to_string),
            drill: None,
        }
    }

    fn footprint(reference: &str, x_mm: f64, y_mm: f64, pads: Vec<Pad>) -> Footprint {
        Footprint {
            id: Id::new(),
            reference: reference.into(),
            value: String::new(),
            library: "demo".into(),
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

    fn set_outline(board: &mut Board, w: f64, h: f64) {
        board.outline = Some(Rect::from_corners(
            Point::new(Length::ZERO, Length::ZERO),
            Point::new(Length::from_mm(w), Length::from_mm(h)),
        ));
    }

    /// Two two-pad parts on shared nets, spread across a roomy outline.
    fn two_part_board(w: f64, h: f64) -> Board {
        let mut board = Board::new();
        set_outline(&mut board, w, h);
        board.add_footprint(footprint(
            "R1",
            6.0,
            6.0,
            vec![
                pad("1", -1.0, 0.0, Some("A")),
                pad("2", 1.0, 0.0, Some("N")),
            ],
        ));
        board.add_footprint(footprint(
            "R2",
            w - 6.0,
            h - 6.0,
            vec![
                pad("1", -1.0, 0.0, Some("N")),
                pad("2", 1.0, 0.0, Some("B")),
            ],
        ));
        board
    }

    fn fast_opts() -> CompactOptions {
        CompactOptions {
            place_iters: 1500,
            binary_steps: 6,
            max_checks: 24,
            time_budget: Duration::from_secs(60),
            seed: 7,
            ..CompactOptions::default()
        }
    }

    #[test]
    fn lower_bound_tracks_geometry() {
        // Single 1×1.2 mm pad part → bbox ~1×1.2; min side ~1.0 mm, plus
        // 2 × 0.3 mm edge clearance ⇒ floor ≈ 1.6 mm on each dimension.
        let mut board = Board::new();
        set_outline(&mut board, 20.0, 20.0);
        board.add_footprint(footprint(
            "R1",
            5.0,
            5.0,
            vec![pad("1", 0.0, 0.0, Some("A"))],
        ));
        let (w, h, area) =
            lower_bound_outline(&board, &MarginMap::new(), &CompactOptions::default());
        assert!((w - 1.6).abs() < 0.05, "w_min {w}");
        assert!((h - 1.6).abs() < 0.05, "h_min {h}");
        // Area floor = packing_factor (1.3) × (1.0 × 1.2) = 1.56 mm².
        assert!((area - 1.56).abs() < 0.05, "area_min {area}");
    }

    #[test]
    fn min_w_min_h_raise_the_floor() {
        let mut board = Board::new();
        set_outline(&mut board, 20.0, 20.0);
        board.add_footprint(footprint(
            "R1",
            5.0,
            5.0,
            vec![pad("1", 0.0, 0.0, Some("A"))],
        ));
        let opts = CompactOptions {
            min_w_mm: Some(12.0),
            min_h_mm: Some(8.0),
            ..CompactOptions::default()
        };
        let (w, h, _) = lower_bound_outline(&board, &MarginMap::new(), &opts);
        assert!((w - 12.0).abs() < 1e-6);
        assert!((h - 8.0).abs() < 1e-6);
    }

    #[test]
    fn compacts_an_oversized_board() {
        // A 40×40 board holding two tiny parts should shrink a lot while
        // still routing its one shared net and passing DRC.
        let board = two_part_board(40.0, 40.0);
        let out = compact(
            &board,
            &Arc::new(Schematic::default()),
            &MarginMap::new(),
            &HashMap::new(),
            None,
            &fast_opts(),
        )
        .expect("compact ok");
        assert!(out.shrunk, "expected a shrink on a roomy board");
        assert!(
            out.metrics.new_area_mm2 < out.metrics.old_area_mm2 * 0.9,
            "area {} -> {} not measurably smaller",
            out.metrics.old_area_mm2,
            out.metrics.new_area_mm2,
        );
        assert_eq!(out.metrics.failed_nets, 0);
        assert_eq!(out.metrics.drc_errors, 0);
        // The shrunk board's outline actually matches the reported size.
        let o = out.board.outline.expect("outline");
        assert!((o.width().to_mm() - out.metrics.new_w_mm).abs() < 1e-3);
        assert!((o.height().to_mm() - out.metrics.new_h_mm).abs() < 1e-3);
        // And after the trim phase the content hugs the outline on all
        // four sides: no side may carry more than ~1 mm of dead margin
        // beyond the edge clearance (pre-trim greedy quantization can
        // leave up to `step_mm`, but the final trim tightens it).
        let c = content_bbox(&out.board, &MarginMap::new()).expect("content bbox");
        let clr = fast_opts().edge_clearance_mm;
        for gap in [
            c.min.x.to_mm() - o.min.x.to_mm(),
            o.max.x.to_mm() - c.max.x.to_mm(),
            c.min.y.to_mm() - o.min.y.to_mm(),
            o.max.y.to_mm() - c.max.y.to_mm(),
        ] {
            assert!(
                gap >= clr - 0.05,
                "content sits inside the clearance: {gap}"
            );
            assert!(gap <= clr + 1.0, "dead border {gap} beyond clearance {clr}");
        }
    }

    #[test]
    fn compaction_preserves_solder_access_gap() {
        // A roomy board still compacts, but the result must never pack two
        // component bodies closer than the hard solder-access gap (1.0 mm
        // default) — the user hand-solders and needs iron-tip room.
        let board = two_part_board(40.0, 40.0);
        let out = compact(
            &board,
            &Arc::new(Schematic::default()),
            &MarginMap::new(),
            &HashMap::new(),
            None,
            &fast_opts(),
        )
        .expect("compact ok");
        assert!(out.shrunk, "expected a shrink on a roomy board");
        let gap = pcb_placer::min_pairwise_gap(&out.board, &MarginMap::new());
        assert!(
            gap >= 1.0 - 0.02,
            "compaction packed bodies below the 1.0 mm solder gap: {gap:.3} mm",
        );
    }

    #[test]
    fn trim_translates_routing_without_rerouting() {
        // A routed board with a roomy outline: `trim_to_content` must
        // rigidly slide every trace/via by one shared delta (no re-route)
        // and shrink the outline to hug the content.
        let mut board = Board::new();
        set_outline(&mut board, 50.0, 50.0);
        board.add_footprint(footprint(
            "R1",
            20.0,
            20.0,
            vec![
                pad("1", -1.0, 0.0, Some("A")),
                pad("2", 1.0, 0.0, Some("N")),
            ],
        ));
        board.add_footprint(footprint(
            "R2",
            30.0,
            25.0,
            vec![
                pad("1", -1.0, 0.0, Some("N")),
                pad("2", 1.0, 0.0, Some("B")),
            ],
        ));
        let mk_trace = |x0, y0, x1, y1| Trace {
            id: Id::new(),
            layer: CopperLayer::Top,
            start: Point::new(Length::from_mm(x0), Length::from_mm(y0)),
            end: Point::new(Length::from_mm(x1), Length::from_mm(y1)),
            width: Length::from_mm(0.25),
            net: "N".into(),
        };
        board.traces.push(mk_trace(21.0, 20.0, 25.0, 22.0));
        board.traces.push(mk_trace(25.0, 22.0, 29.0, 25.0));
        board.vias.push(Via {
            id: Id::new(),
            position: Point::new(Length::from_mm(25.0), Length::from_mm(22.0)),
            drill: Length::from_mm(0.3),
            diameter: Length::from_mm(0.6),
            net: "N".into(),
        });

        let before: Vec<(Point, Point)> = board.traces.iter().map(|t| (t.start, t.end)).collect();
        let (n_traces, n_vias) = (board.traces.len(), board.vias.len());

        let shrank = trim_to_content(&mut board, &MarginMap::new(), 0.3, 0.1);
        assert!(shrank, "an oversized outline must shrink on trim");
        // No re-route: trace/via topology is preserved verbatim.
        assert_eq!(board.traces.len(), n_traces);
        assert_eq!(board.vias.len(), n_vias);
        // Every endpoint moved by the SAME rigid delta.
        let dx = board.traces[0].start.x.0 - before[0].0.x.0;
        let dy = board.traces[0].start.y.0 - before[0].0.y.0;
        for (t, (os, oe)) in board.traces.iter().zip(&before) {
            assert_eq!(t.start.x.0 - os.x.0, dx);
            assert_eq!(t.start.y.0 - os.y.0, dy);
            assert_eq!(t.end.x.0 - oe.x.0, dx);
            assert_eq!(t.end.y.0 - oe.y.0, dy);
        }
        // Outline hugs the content: every side is exactly clearance+slack.
        let o = board.outline.expect("outline");
        let c = content_bbox(&board, &MarginMap::new()).expect("content bbox");
        let pad = 0.4; // 0.3 clearance + 0.1 slack
        for gap in [
            c.min.x.to_mm() - o.min.x.to_mm(),
            o.max.x.to_mm() - c.max.x.to_mm(),
            c.min.y.to_mm() - o.min.y.to_mm(),
            o.max.y.to_mm() - c.max.y.to_mm(),
        ] {
            assert!((gap - pad).abs() < 1e-3, "gap {gap} != pad {pad}");
        }
    }

    /// Rule area anchored to `reference`, inflated by `margin`, declaring
    /// the board-default clearance so it cannot perturb routing (this test
    /// is about geometry bookkeeping, not about the rule itself).
    fn anchored_area(
        board: &Board,
        reference: &str,
        name: &str,
        margin: f64,
    ) -> pcb_core::RuleArea {
        let fp = board
            .footprints_in_order()
            .find(|f| f.reference == reference)
            .expect("anchor footprint");
        let mut a = pcb_core::RuleArea::around_footprint(name, fp, margin).expect("has pads");
        a.clearance_mm = Some(0.20);
        a
    }

    /// The rect an anchored area *should* have for the given footprint.
    fn expected_anchor_rect(board: &Board, reference: &str, margin: f64) -> Rect {
        let fp = board
            .footprints_in_order()
            .find(|f| f.reference == reference)
            .expect("anchor footprint");
        pcb_core::anchor_rect(fp, margin).expect("has pads")
    }

    #[test]
    fn trim_reanchors_a_rule_area_declared_around_a_part() {
        // The bug this pins: `trim_to_content` slides every footprint but
        // used to leave `rule_areas` at their old absolute rects, so a zone
        // declared around a QFN silently stopped covering it.
        let mut board = two_part_board(50.0, 50.0);
        board.set_rule_area(anchored_area(&board, "R1", "fine", 1.0));
        let before = board.rule_areas[0].rect;

        assert!(trim_to_content(&mut board, &MarginMap::new(), 0.3, 0.1));
        let area = &board.rule_areas[0];
        assert_ne!(
            area.rect, before,
            "the trim moved R1, so the zone must move"
        );
        assert_eq!(area.rect, expected_anchor_rect(&board, "R1", 1.0));
        // And it still *covers* the part it was declared for.
        let r1 = board
            .footprints_in_order()
            .find(|f| f.reference == "R1")
            .expect("R1")
            .bounds()
            .expect("bounds");
        assert!(area.rect.min.x.0 <= r1.min.x.0 && area.rect.max.x.0 >= r1.max.x.0);
        assert!(area.rect.min.y.0 <= r1.min.y.0 && area.rect.max.y.0 >= r1.max.y.0);
        // The anchor metadata survives the trim.
        assert_eq!(area.anchor_ref.as_deref(), Some("R1"));
        assert_eq!(area.anchor_margin_mm, Some(1.0));
    }

    #[test]
    fn trim_translates_unanchored_areas_and_keepouts() {
        // A plain `rule-area` rect and a keepout are statements about where
        // the copper is, so a rigid content slide must carry them along by
        // the SAME delta — otherwise a trace can end up inside a keepout
        // the user fenced off, with no DRC error to show for it.
        let mut board = two_part_board(50.0, 50.0);
        let mut plain = pcb_core::RuleArea::new(
            "moat",
            Rect::from_corners(
                Point::new(Length::from_mm(18.0), Length::from_mm(18.0)),
                Point::new(Length::from_mm(24.0), Length::from_mm(24.0)),
            ),
        );
        plain.clearance_mm = Some(0.20);
        board.set_rule_area(plain);
        board.add_keepout(pcb_core::Keepout {
            id: Id::new(),
            polygon: vec![
                Point::new(Length::from_mm(30.0), Length::from_mm(30.0)),
                Point::new(Length::from_mm(34.0), Length::from_mm(30.0)),
                Point::new(Length::from_mm(34.0), Length::from_mm(33.0)),
            ],
            layers: Vec::new(),
            nets_allowed: Vec::new(),
            label: "fence".into(),
        });
        let area_before = board.rule_areas[0].rect;
        let poly_before = board.keepouts[0].polygon.clone();
        let r1_before = board
            .footprints_in_order()
            .find(|f| f.reference == "R1")
            .expect("R1")
            .position;

        assert!(trim_to_content(&mut board, &MarginMap::new(), 0.3, 0.1));
        let r1_after = board
            .footprints_in_order()
            .find(|f| f.reference == "R1")
            .expect("R1")
            .position;
        let (dx, dy) = (r1_after.x.0 - r1_before.x.0, r1_after.y.0 - r1_before.y.0);
        assert!(dx != 0 || dy != 0, "the trim must actually move content");
        let area = board.rule_areas[0].rect;
        assert_eq!(area.min.x.0 - area_before.min.x.0, dx);
        assert_eq!(area.min.y.0 - area_before.min.y.0, dy);
        assert_eq!(area.max.x.0 - area_before.max.x.0, dx);
        for (p, before) in board.keepouts[0].polygon.iter().zip(&poly_before) {
            assert_eq!(p.x.0 - before.x.0, dx);
            assert_eq!(p.y.0 - before.y.0, dy);
        }
    }

    #[test]
    fn compaction_keeps_an_anchored_rule_area_on_its_part() {
        // End-to-end: the candidate search re-places every footprint and
        // the trim phase slides them again. Whatever the search settles on,
        // the anchored area must equal the part's FINAL bounds + margin.
        let mut board = two_part_board(40.0, 40.0);
        board.set_rule_area(anchored_area(&board, "R1", "fine", 1.0));
        let out = compact(
            &board,
            &Arc::new(Schematic::default()),
            &MarginMap::new(),
            &HashMap::new(),
            None,
            &fast_opts(),
        )
        .expect("compact ok");
        assert!(out.shrunk, "expected a shrink on a roomy board");
        let area = out
            .board
            .rule_areas
            .iter()
            .find(|a| a.name == "fine")
            .expect("area survived compaction");
        assert_eq!(area.rect, expected_anchor_rect(&out.board, "R1", 1.0));
        assert_eq!(area.clearance_mm, Some(0.20), "overrides are preserved");
    }

    // ── Pure feasibility-gate policy (no placer/router involved). ──

    fn violation(
        kind: pcb_drc::ViolationKind,
        involved: &[&str],
        message: &str,
    ) -> pcb_drc::Violation {
        pcb_drc::Violation {
            kind,
            severity: pcb_drc::Severity::Error,
            message: message.into(),
            x_mm: 0.0,
            y_mm: 0.0,
            involved: involved.iter().map(|s| (*s).to_string()).collect(),
        }
    }

    fn report(violations: Vec<pcb_drc::Violation>) -> pcb_drc::DrcReport {
        let error_count = violations
            .iter()
            .filter(|v| v.severity == pcb_drc::Severity::Error)
            .count();
        pcb_drc::DrcReport {
            violations,
            error_count,
            warning_count: 0,
        }
    }

    fn nets(list: &[&str]) -> BTreeSet<String> {
        list.iter().map(|s| (*s).to_string()).collect()
    }

    fn demo_pad_nets() -> HashMap<String, String> {
        [("R1.1", "N"), ("R2.1", "N"), ("R1.2", "A"), ("R2.2", "B")]
            .into_iter()
            .map(|(k, v)| (k.to_string(), v.to_string()))
            .collect()
    }

    #[test]
    fn gate_is_strict_by_default() {
        let pad_nets = demo_pad_nets();
        let clean = report(Vec::new());
        assert!(candidate_passes(&nets(&[]), &clean, &pad_nets, 0));
        // One failed net with allow_failed = 0 → rejected, clean DRC or not.
        assert!(!candidate_passes(&nets(&["N"]), &clean, &pad_nets, 0));
        // A DRC error with nothing excused → rejected.
        let split = report(vec![violation(
            pcb_drc::ViolationKind::NetSplit,
            &["R1.1", "R2.1"],
            "net \"N\" is split into 2 isolated copper islands: {R1.1} | {R2.1}",
        )]);
        assert!(!candidate_passes(&nets(&[]), &split, &pad_nets, 0));
    }

    #[test]
    fn gate_excuses_netsplit_only_on_nets_the_router_failed() {
        let pad_nets = demo_pad_nets();
        let split_n = violation(
            pcb_drc::ViolationKind::NetSplit,
            &["R1.1", "R2.1"],
            "net \"N\" is split into 2 isolated copper islands: {R1.1} | {R2.1}",
        );
        // Net N failed and is within budget: its open is a consequence, not
        // a new defect.
        assert!(candidate_passes(
            &nets(&["N"]),
            &report(vec![split_n.clone()]),
            &pad_nets,
            1
        ));
        // Same report, but the router said N routed fine — then the split
        // is a real defect and must still fail.
        assert!(!candidate_passes(
            &nets(&["A"]),
            &report(vec![split_n.clone()]),
            &pad_nets,
            1
        ));
        // Budget is on the COUNT of failures, not on the excusing.
        assert!(!candidate_passes(
            &nets(&["N", "A"]),
            &report(vec![split_n]),
            &pad_nets,
            1
        ));
    }

    #[test]
    fn gate_never_excuses_clearance_or_short_errors() {
        let pad_nets = demo_pad_nets();
        for kind in [
            pcb_drc::ViolationKind::TraceTraceClearance,
            pcb_drc::ViolationKind::EdgeClearance,
            pcb_drc::ViolationKind::BodyOffBoard,
            pcb_drc::ViolationKind::NetShort,
        ] {
            let r = report(vec![violation(kind, &["R1.1", "R2.1"], "net \"N\" oops")]);
            assert!(
                !candidate_passes(&nets(&["N"]), &r, &pad_nets, 4),
                "{kind:?} must never be excused by allow_failed",
            );
        }
        // Warnings never gate, even unexcused.
        let mut warn = violation(
            pcb_drc::ViolationKind::UnconnectedPad,
            &["R1.1"],
            "pad R1.1 on net N has no copper",
        );
        warn.severity = pcb_drc::Severity::Warning;
        let mut r = report(vec![warn]);
        r.error_count = 0;
        r.warning_count = 1;
        assert!(candidate_passes(&nets(&["N"]), &r, &pad_nets, 1));
    }

    #[test]
    fn violation_net_reads_pads_then_falls_back_to_the_message() {
        let pad_nets = demo_pad_nets();
        let v = violation(
            pcb_drc::ViolationKind::NetSplit,
            &["R1.1", "R2.1"],
            "net \"WRONG\" is split",
        );
        // Pad labels are authoritative when they all resolve to one net.
        assert_eq!(violation_net(&v, &pad_nets).as_deref(), Some("N"));
        // Unknown pads → the quoted name in the message.
        let v = violation(
            pcb_drc::ViolationKind::NetSplit,
            &["U9.7"],
            "net \"SPI_CLK\" is split into 2 isolated copper islands",
        );
        assert_eq!(violation_net(&v, &pad_nets).as_deref(), Some("SPI_CLK"));
        // Pads from two different nets → not attributable to one net.
        let v = violation(
            pcb_drc::ViolationKind::NetShort,
            &["R1.1", "R1.2"],
            "no quotes here",
        );
        assert_eq!(violation_net(&v, &pad_nets), None);
    }

    #[test]
    fn split_nets_collects_open_nets_from_a_report() {
        let pad_nets = demo_pad_nets();
        let r = report(vec![
            violation(
                pcb_drc::ViolationKind::NetSplit,
                &["R1.1", "R2.1"],
                "net \"N\" is split",
            ),
            violation(
                pcb_drc::ViolationKind::TraceTraceClearance,
                &["R1.2"],
                "net \"A\" too close",
            ),
        ]);
        assert_eq!(split_nets(&r, &pad_nets), nets(&["N"]));
    }

    #[test]
    fn allow_failed_and_route_seconds_leave_a_healthy_board_alone() {
        // Raising the tolerance must not *lower* the bar for a board that
        // does route: nothing failed, so the reported count stays 0 and the
        // shrink still happens under a smaller per-probe route budget.
        let board = two_part_board(40.0, 40.0);
        let opts = CompactOptions {
            allow_failed: 2,
            route_seconds: 5.0,
            ..fast_opts()
        };
        let out = compact(
            &board,
            &Arc::new(Schematic::default()),
            &MarginMap::new(),
            &HashMap::new(),
            None,
            &opts,
        )
        .expect("compact ok");
        assert!(out.shrunk, "expected a shrink on a roomy board");
        assert_eq!(out.metrics.failed_nets, 0, "nothing actually failed here");
        assert_eq!(out.metrics.drc_errors, 0);
    }

    #[test]
    fn new_knobs_default_to_the_old_behaviour() {
        // Spec-lock: the defaults must reproduce pre-feature compaction.
        let d = CompactOptions::default();
        assert_eq!(d.allow_failed, 0);
        assert!((d.route_seconds - 30.0).abs() < 1e-9);
    }

    #[test]
    fn deterministic_for_a_fixed_seed() {
        let board = two_part_board(40.0, 40.0);
        let run = || {
            compact(
                &board,
                &Arc::new(Schematic::default()),
                &MarginMap::new(),
                &HashMap::new(),
                None,
                &fast_opts(),
            )
            .unwrap()
        };
        let a = run();
        let b = run();
        assert_eq!(a.shrunk, b.shrunk);
        assert!((a.metrics.new_w_mm - b.metrics.new_w_mm).abs() < 1e-9);
        assert!((a.metrics.new_h_mm - b.metrics.new_h_mm).abs() < 1e-9);
        assert_eq!(a.metrics.checks, b.metrics.checks);
    }

    #[test]
    fn board_at_minimum_is_left_untouched() {
        // Outline already at the geometric floor: s_min ≈ 1, no shrink.
        let mut board = two_part_board(40.0, 40.0);
        let (w_min, h_min, _) =
            lower_bound_outline(&board, &MarginMap::new(), &CompactOptions::default());
        set_outline(&mut board, w_min, h_min);
        let out = compact(
            &board,
            &Arc::new(Schematic::default()),
            &MarginMap::new(),
            &HashMap::new(),
            None,
            &fast_opts(),
        )
        .expect("compact ok");
        assert!(!out.shrunk, "a board at its floor must not shrink");
        assert_eq!(out.metrics.new_area_mm2, out.metrics.old_area_mm2);
        // Untouched: same outline as we set.
        let o = out.board.outline.expect("outline");
        assert!((o.width().to_mm() - w_min).abs() < 1e-3);
    }

    #[test]
    fn edge_mounted_part_still_touches_after_compaction() {
        let mut board = Board::new();
        set_outline(&mut board, 40.0, 40.0);
        let mut j1 = footprint(
            "J1",
            2.0,
            20.0,
            vec![
                pad("1", 0.0, -1.0, Some("A")),
                pad("2", 0.0, 1.0, Some("N")),
            ],
        );
        j1.edge_mounted = true;
        board.add_footprint(j1);
        board.add_footprint(footprint(
            "R1",
            30.0,
            20.0,
            vec![
                pad("1", -1.0, 0.0, Some("N")),
                pad("2", 1.0, 0.0, Some("B")),
            ],
        ));
        let out = compact(
            &board,
            &Arc::new(Schematic::default()),
            &MarginMap::new(),
            &HashMap::new(),
            None,
            &fast_opts(),
        )
        .expect("compact ok");
        let o = out.board.outline.expect("outline");
        let j = out
            .board
            .footprints_in_order()
            .find(|f| f.reference == "J1")
            .expect("J1");
        let b = j.bounds().expect("bounds");
        let tol = 0.5; // matches EDGE_TOUCH_TOLERANCE_MM
        let touches = (b.min.x.to_mm() - o.min.x.to_mm()).abs() <= tol
            || (o.max.x.to_mm() - b.max.x.to_mm()).abs() <= tol
            || (b.min.y.to_mm() - o.min.y.to_mm()).abs() <= tol
            || (o.max.y.to_mm() - b.max.y.to_mm()).abs() <= tol;
        assert!(touches, "edge-mounted J1 no longer touches the outline");
        // Same statement, via the shared rule the feasibility gate uses.
        assert!(
            out.board.edge_mount_violation(j).is_none(),
            "compact must never accept a candidate with an edge part inboard"
        );
    }

    /// Regression: a candidate whose SA output leaves an edge-mounted
    /// part inboard must be REJECTED, not accepted. Before this gate the
    /// only checks on a candidate were the solder floor, routing and
    /// DRC — none of which model "the connector must reach the cut", so
    /// compaction happily buried a USB-C module in the middle of the
    /// board (seen on the fecha gateway: U1 ended 8.4 mm inland).
    #[test]
    fn compaction_never_accepts_an_edge_part_left_inboard() {
        let mut board = Board::new();
        set_outline(&mut board, 40.0, 40.0);
        let mut j1 = footprint(
            "J1",
            2.0,
            20.0,
            vec![
                pad("1", 0.0, -1.0, Some("A")),
                pad("2", 0.0, 1.0, Some("N")),
            ],
        );
        j1.edge_mounted = true;
        j1.edge_side = Some(pcb_core::EdgeSide::Left);
        board.add_footprint(j1);
        for (i, r) in ["R1", "R2", "R3"].iter().enumerate() {
            board.add_footprint(footprint(
                r,
                20.0 + i as f64 * 4.0,
                20.0,
                vec![
                    pad("1", -1.0, 0.0, Some("N")),
                    pad("2", 1.0, 0.0, Some("B")),
                ],
            ));
        }
        let out = compact(
            &board,
            &Arc::new(Schematic::default()),
            &MarginMap::new(),
            &HashMap::new(),
            None,
            &fast_opts(),
        )
        .expect("compact ok");
        for fp in out.board.footprints_in_order() {
            assert!(
                out.board.edge_mount_violation(fp).is_none(),
                "{} is edge-mounted but ended inboard after compaction",
                fp.reference
            );
        }
    }
}
