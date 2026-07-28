//! Driver that ties the grid and A* together.

use std::collections::{BTreeMap, HashMap, HashSet};
use std::sync::Arc;
use std::time::{Duration, Instant};

use pcb_core::{
    Board, CopperLayer, Length, Point, Rect, RuleArea, RuleDefaults, RuleResolver, Schematic,
    Trace, Via,
};

use crate::astar::{search, Limits};
use crate::grid::{AreaField, ClearanceModel, CostMap, Grid, GridPoint};

/// Which search engine lays the copper.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub enum RouteEngine {
    /// Theta* over a uniform cell grid (the battle-tested default).
    #[default]
    Grid,
    /// Topological engine: homotopy classes over a Delaunay dual graph,
    /// rubber-band realisation, exact-geometry validation. 2-layer
    /// boards only for now — other stackups fall back to Grid.
    Topo,
}

#[derive(Clone)]
pub struct RouteOptions {
    /// Cell pitch on the routing grid. 0.25 mm is the default sweet
    /// spot for SMD-only boards: fine enough for 0.5 mm pin pitch,
    /// coarse enough for grids of ~250 × 250 cells per layer to stay
    /// fast.
    pub cell: Length,
    /// Default trace width laid down by the router. Per-net entries in
    /// `net_overrides` win when set.
    pub trace_width: Length,
    /// The board's DEFAULT copper-to-copper clearance — the floor every
    /// pair of nets is held to.
    ///
    /// The actual gap enforced between two pieces of copper is resolved
    /// per pair and per location by `pcb_core::RuleResolver`: the
    /// strictest of this default and either net's class, unless a
    /// `RuleArea` covering the point overrides it outright. Obstacles are
    /// stamped BARE (their own copper extent, no clearance halo) and the
    /// gap is enforced inside the search, so a fine-pitch escape zone is
    /// genuinely routable at its own rule while the rest of the board
    /// keeps this one. (Historically the grid inflated everything by the
    /// max clearance in play and each search only checked its OWN net's
    /// clearance — a finer class could never route finer, and a finer net
    /// could hug a stricter neighbour.)
    pub clearance: Length,
    /// Cost of punching a via, expressed as a multiplier on the
    /// per-cell base step. Higher = router prefers single-layer
    /// detours. Internally scaled to the search's fixed-point
    /// Euclidean cost domain.
    pub via_cost: u32,
    /// Via geometry produced when the path flips layers.
    pub via_drill: Length,
    pub via_diameter: Length,
    /// Per-net rule overrides keyed by net name. Built by the caller
    /// from the schematic's `NetClass` definitions; the router stays
    /// schematic-agnostic and just consults this map.
    ///
    /// **Deprecated** in favour of `schematic` — kept for one release
    /// so existing callers (router-tune, tests) compile unchanged.
    #[doc(hidden)]
    pub net_overrides: HashMap<String, NetOverride>,
    /// Optional schematic reference. When set, the router consults
    /// `schematic.resolved_for_net(net)` for per-net trace width and
    /// clearance — superseding `net_overrides`. The arc is cheap to
    /// clone and keeps the router lock-free with respect to the
    /// schematic.
    pub schematic: Option<Arc<Schematic>>,
    /// Copper search engine. `Topo` is the rubber-band topological
    /// engine (see `topo.rs`); non-2-layer stackups fall back to Grid.
    pub engine: RouteEngine,
    /// Run the organic post-pass (string-pulling + arc fillets) on the
    /// routed copper. DRC-neutral by construction — every rewrite is
    /// clearance-checked before being accepted. See `organic.rs`.
    pub organic: bool,
    /// Largest fillet radius the organic pass attempts, mm.
    pub organic_fillet_mm: f64,
    /// If `Some`, use this exact net order as the first-pass ordering instead
    /// of the built-in "fewest pads first" heuristic. Net names not present
    /// in the board are silently dropped; nets in the board but missing from
    /// the override are appended at the end in default order. The rip-up-and-
    /// reroute loop is unaffected — it still reorders on subsequent passes.
    pub initial_net_order: Option<Vec<String>>,
    /// Greedy-search weight `W` applied to the A* heuristic (`f = g + W·h`).
    /// `1.0` = admissible/optimal A* (default — byte-identical to the
    /// historical router). Values in `1.25..=1.5` collapse the near-tied-f
    /// frontier on long, board-spanning nets 5–30× at a few-percent
    /// path-length cost. The weighting is **size-gated inside the search**:
    /// short connections (every tight-detour test, fanout/diff-pair
    /// end-cap) stay at `W=1.0` and provably optimal, so only the long
    /// searches — exactly where the frontier explosion lives — pay the
    /// small detour for the big speed win. Orthogonal to clearance
    /// stamping, so it never changes the DRC/CLEAN outcome, only latency.
    pub heuristic_weight: f64,
    /// Soft wall-clock budget for the whole `route()` call. When elapsed,
    /// the driver keeps the best report so far, marks remaining nets as
    /// failed with a timeout reason, and returns. `None` = no budget
    /// (legacy behaviour). Agents should pass e.g. `60` so a stuck fine-
    /// pitch board cannot hang the HTTP session for 10+ minutes.
    pub max_seconds: Option<f64>,
    /// Opt in to the localized fine-grid escape (`escape.rs`) on stackups
    /// with three or more copper layers.
    ///
    /// OFF by default. The pass was built for a fine-pitch connector row
    /// on a roomy multi-layer board, but measured against the VIP/dogbone
    /// fanout on the RP2040 stress board it bought one extra escaped pad
    /// for seconds of the route budget and ended up one net WORSE — the
    /// 4-layer regression of issue O8. Until it demonstrably pays for
    /// itself, callers who want it have to ask for it.
    pub fine_escape: bool,
    /// Run the PathFinder-style **negotiated congestion** driver
    /// (`negotiate.rs`) before the historical rip-up-and-reroute loop.
    ///
    /// OFF by default — see the measurement below. Negotiation lets every
    /// net take its shortest path in the first iteration and then makes
    /// contended corridors progressively more expensive until the nets with
    /// an alternative detour, which is the textbook fix for "the fat net
    /// routed first owns the corridor forever". When it converges (no net
    /// shares copper with any other) its solution is legal and complete and
    /// the hard passes are skipped; when it does not, the RR&R loop still
    /// runs on the remaining budget and the better board wins.
    ///
    /// **Why it is not the default.** On the RP2040 stress board it is a
    /// congestion *diagnosis* rather than a cure: 12–13 of the 39 nets
    /// cannot be routed even when every foreign trace is treated as
    /// shareable, because the escape lanes at a 0.4 mm QFN are walled by
    /// copper no negotiation can move (neighbouring pads and the fanout's
    /// own via landings). Negotiation therefore cannot converge there, and
    /// the budget it spends costs the hard passes ~2 nets of connectivity.
    /// It is kept opt-in (`route negotiate=true`) because it is the right
    /// algorithm for boards whose wall really is inter-net contention, and
    /// because its corridor autopsy (reported in `hints`) is what names the
    /// geometry that has to change instead.
    ///
    /// Ignored for `engine=topo`, and automatically declined on boards that
    /// declare a differential pair (the follow-the-partner geometry lives in
    /// the classic per-net path — see `try_diff_pair_follow`).
    pub negotiate: bool,
    /// Optional progress callback — one short human-readable line per
    /// RR pass / net batch. Wired by the script API into the activity
    /// log so the UI and agents see routing progress without polling.
    #[allow(clippy::type_complexity)]
    pub on_progress: Option<Arc<dyn Fn(&str) + Send + Sync>>,
}

impl std::fmt::Debug for RouteOptions {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("RouteOptions")
            .field("cell", &self.cell)
            .field("trace_width", &self.trace_width)
            .field("clearance", &self.clearance)
            .field("via_cost", &self.via_cost)
            .field("via_drill", &self.via_drill)
            .field("via_diameter", &self.via_diameter)
            .field("engine", &self.engine)
            .field("organic", &self.organic)
            .field("organic_fillet_mm", &self.organic_fillet_mm)
            .field("heuristic_weight", &self.heuristic_weight)
            .field("max_seconds", &self.max_seconds)
            .field("fine_escape", &self.fine_escape)
            .field("negotiate", &self.negotiate)
            .field(
                "on_progress",
                &self.on_progress.as_ref().map(|_| "<callback>"),
            )
            .finish()
    }
}

/// Per-net rule overrides — fields default to "use the global
/// `RouteOptions` value" when `None`.
#[derive(Debug, Clone, Default)]
pub struct NetOverride {
    pub trace_width: Option<Length>,
    pub clearance: Option<Length>,
}

impl Default for RouteOptions {
    fn default() -> Self {
        Self {
            cell: Length::from_mm(0.25),
            trace_width: Length::from_mm(0.25),
            // 0.4 mm gives a 2-cell halo around traces and pads on a
            // 0.25 mm grid: even at the closest legal spacing, two
            // foreign-net traces have at least one empty cell of gap
            // between them, so they never appear visually pegged.
            clearance: Length::from_mm(0.40),
            via_cost: 8,
            via_drill: Length::from_mm(0.3),
            via_diameter: Length::from_mm(0.6),
            net_overrides: HashMap::new(),
            schematic: None,
            engine: RouteEngine::Grid,
            organic: true,
            organic_fillet_mm: 3.0,
            initial_net_order: None,
            heuristic_weight: 1.0,
            // 90 s is enough for typical boards and keeps agent HTTP
            // sessions from hanging. Heavy boards can raise via
            // `route max_seconds=N`.
            max_seconds: Some(90.0),
            fine_escape: false,
            negotiate: false,
            on_progress: None,
        }
    }
}

pub(crate) fn progress(opts: &RouteOptions, msg: impl AsRef<str>) {
    if let Some(cb) = opts.on_progress.as_ref() {
        cb(msg.as_ref());
    }
}

fn deadline_of(opts: &RouteOptions) -> Option<Instant> {
    opts.max_seconds
        .filter(|s| *s > 0.0 && s.is_finite())
        .map(|s| Instant::now() + Duration::from_secs_f64(s))
}

pub(crate) fn timed_out(deadline: Option<Instant>) -> bool {
    deadline.is_some_and(|d| Instant::now() >= d)
}

/// Minimum trace width (mm) the router lays on a *power* net when the
/// net has no explicit class/override width. Power distribution carries
/// real current and wants low impedance, so a backbone thinner than this
/// is almost never what the designer intends. A net that explicitly sets
/// a narrower width via a class still wins — this is only a floor for the
/// "router picked the default" case.
const POWER_MIN_TRACE_WIDTH_MM: f64 = 0.50;

/// Classify a net as power/ground by name. Keyed on the conventional
/// rail names so the router can widen them automatically without the
/// designer having to declare a `class power` for every board. Matches
/// the exact rail or a name that starts with it (e.g. `+3V3`, `3V3_MCU`,
/// `VBUS_IN`, `VCC_IO`).
pub fn is_power_net(net: &str) -> bool {
    let u = net.to_ascii_uppercase();
    const RAILS: &[&str] = &[
        "GND", "VBUS", "+3V3", "3V3", "+5V", "5V", "+1V", "1V", "VCC", "VDD", "VDDA", "VIN",
        "VSYS", "VBAT", "PWR", "+12V", "12V",
    ];
    RAILS.iter().any(|p| u == *p || u.starts_with(p))
}

/// Helper: resolve `(trace_width, clearance)` for `net` honouring
/// `opts.schematic` first, then `opts.net_overrides`, then the global
/// defaults on `opts`. Centralises the precedence so the grid stamp,
/// per-net layout, and `compute_region` stay in sync.
///
/// Power nets get a width *floor* (`POWER_MIN_TRACE_WIDTH_MM`) applied
/// last: a power rail that resolved to the bare global default is widened
/// to the floor, while a net that explicitly asked for a specific width
/// (via a class or override) keeps it.
pub(crate) fn effective_net_rules(opts: &RouteOptions, net: &str) -> (Length, Length) {
    // `explicit` tracks whether the width came from a real class/override
    // (respect it) or fell through to the global default (floor it).
    let (mut w, c, explicit_width) = {
        if let Some(sch) = opts.schematic.as_ref() {
            // Only consult the schematic when the net actually has a class —
            // otherwise we'd shadow the override map below for every net.
            if sch.class_for_net(net).is_some() {
                let res = sch.resolved_for_net(
                    net,
                    opts.trace_width,
                    opts.clearance,
                    opts.via_diameter,
                    opts.via_drill,
                );
                // A class whose width differs from the default is an
                // explicit choice; one that equals the default is just
                // inheriting it.
                let explicit = res.trace_width.0 != opts.trace_width.0;
                (res.trace_width, res.clearance, explicit)
            } else {
                resolve_from_overrides(opts, net)
            }
        } else {
            resolve_from_overrides(opts, net)
        }
    };

    if !explicit_width && is_power_net(net) {
        let floor = Length::from_mm(POWER_MIN_TRACE_WIDTH_MM);
        if floor.0 > w.0 {
            w = floor;
        }
    }
    (w, c)
}

/// Quantization guard (in cells) folded into every search-time clearance
/// radius. A Theta* any-angle segment can pass up to ~0.5 cell off its
/// Bresenham raster, and a bare pad/trace cell represents copper up to
/// ~0.5 cell from the cell point, so the true edge-to-edge distance can
/// fall up to ~1 cell short of the disk-measured distance. Adding one
/// cell to the clearance radius absorbs that, keeping the DRC's true-
/// geometry clearance honest at a coarse grid. (Costs ~1 cell of extra
/// separation — at cell 0.20 that's 0.20 mm — traded for zero collisions.)
///
/// It is a *distance*, not a cell count to round to: see `need_r2`.
const CLEARANCE_GUARD_CELLS: f64 = 1.0;

/// The rule context of one routing pass: the shared resolver plus the
/// rasterised rule-area field the search consults per cell.
///
/// This is what makes per-net clearance HONEST. The old model inflated
/// nothing and checked only the searching net's own clearance, so a net
/// could hug a stricter neighbour (under-enforcement), and a rule area
/// could not be honoured at all. Here every search gets a
/// [`ClearanceModel`] whose radius depends on **which** foreign net is
/// found (strictest of the two classes) and on **where** the candidate
/// cell is (a rule area overrides outright).
pub(crate) struct RuleCtx<'a> {
    resolver: RuleResolver<'a>,
    /// Per-cell area index, `None` when the board declares no
    /// clearance-bearing area (the overwhelmingly common case — then
    /// every model is a plain single-radius disk, as before).
    field: Option<AreaField>,
    /// Clearance of area `k` (1-based, `[0]` unused), matching `field`.
    area_clearance: Vec<Length>,
    /// Net name by net id, so a model can resolve the pair rule.
    order: &'a [String],
}

impl<'a> RuleCtx<'a> {
    /// Build the context for a pass. `areas`/`schematic` are borrowed
    /// from locals in the caller so the resolver never fights the
    /// `&mut Board` the pass needs.
    pub(crate) fn new(
        areas: &'a [RuleArea],
        schematic: Option<&'a Schematic>,
        opts: &RouteOptions,
        order: &'a [String],
        grid: &Grid,
    ) -> Self {
        let resolver = RuleResolver::new(
            areas,
            RuleDefaults {
                clearance: opts.clearance,
                trace_width: opts.trace_width,
                via_diameter: opts.via_diameter,
                via_drill: opts.via_drill,
            },
        )
        .with_schematic(schematic);

        // Rasterise the clearance-bearing areas, weakest-priority first
        // so a later stamp (higher priority / smaller area) overwrites.
        let mut clearance_areas: Vec<&RuleArea> =
            areas.iter().filter(|a| a.clearance_mm.is_some()).collect();
        clearance_areas.sort_by_key(|a| (a.priority, -a.area_nm2()));
        let (field, area_clearance) = if clearance_areas.is_empty() {
            (None, Vec::new())
        } else {
            let mut f = AreaField::new(grid.cols, grid.rows, grid.layer_count);
            let mut clr = vec![opts.clearance]; // index 0 = "no area"
            for (i, a) in clearance_areas.iter().enumerate() {
                let idx = u8::try_from(i + 1).unwrap_or(u8::MAX);
                let (c0, r0) = grid.cell_of(a.rect.min);
                let (c1, r1) = grid.cell_of(a.rect.max);
                let layers: Vec<u8> = a.layers.iter().map(|l| l.index).collect();
                f.stamp_box(c0, r0, c1, r1, &layers, idx);
                clr.push(Length::from_mm(a.clearance_mm.unwrap_or(0.0)));
            }
            (Some(f), clr)
        };

        Self {
            resolver,
            field,
            area_clearance,
            order,
        }
    }

    /// Clearance radius in cells for `clearance` against copper whose
    /// own half-width is already baked into its stamp: the searching
    /// net's half-width plus the required gap, plus the quantization
    /// guard.
    /// Required **squared** cell distance to foreign copper.
    ///
    /// The search accepts a candidate cell when `d2 > need_r2`, so this
    /// returns the largest squared distance that is still too close.
    ///
    /// Deriving it from the real distance matters more than it looks. The
    /// old form rounded the required distance UP to a whole cell, added a
    /// whole guard cell, and squared that — three roundings that compound.
    /// At the stress board's fine rule (0.12 mm clearance, 0.25 mm trace,
    /// 0.20 mm cell) the true requirement is 0.245 mm and the guard adds
    /// 0.20 mm, so 0.445 mm is honest; the old form demanded **0.632 mm**,
    /// 42 % more, and it demanded it of every escape lane on a 0.4 mm-pitch
    /// QFN — where 0.19 mm is the difference between a lane and a wall.
    /// The physical guard is unchanged: only the rounding is gone.
    fn need_r2(clearance: Length, half_width: Length, cell: Length, guard_cells: f64) -> i32 {
        let cell_nm = cell.0.max(1) as f64;
        let r = (clearance.0 + half_width.0) as f64 / cell_nm + guard_cells;
        // Accept iff `d2 >= r²`; the test rejects on `d2 <= need`, so the
        // largest rejected value is `ceil(r²) - 1`. Floored at 0 so a
        // zero-clearance model still rejects copper in the same cell.
        let need = (r * r).ceil() as i64 - 1;
        i32::try_from(need.max(0)).unwrap_or(i32::MAX)
    }

    fn model(&self, net: &str, half_width: Length, cell: Length, guard: f64) -> ClearanceModel<'_> {
        let per_net: Vec<i32> = self
            .order
            .iter()
            .map(|other| {
                let c = self.resolver.pair_clearance(Some(net), Some(other));
                Self::need_r2(c, half_width, cell, guard)
            })
            .collect();
        // Copper with no net (NC pads, mounting holes) has no class, so
        // it demands the searching net's own rule.
        let other = Self::need_r2(
            self.resolver.pair_clearance(Some(net), None),
            half_width,
            cell,
            guard,
        );
        let area_r2: Vec<i32> = self
            .area_clearance
            .iter()
            .map(|c| Self::need_r2(*c, half_width, cell, guard))
            .collect();
        ClearanceModel::new(per_net, other, area_r2, self.field.as_ref())
    }

    /// Model for a trace of `width` on `net`.
    pub(crate) fn trace_model(&self, net: &str, width: Length, cell: Length) -> ClearanceModel<'_> {
        self.model(net, Length(width.0 / 2), cell, CLEARANCE_GUARD_CELLS)
    }

    /// Model for a via barrel of `net`. Same rule, with the via's copper
    /// radius in place of the trace's half-width and no quantization
    /// guard beyond what `radius_cells` already adds.
    pub(crate) fn via_model(&self, net: &str, cell: Length) -> ClearanceModel<'_> {
        // No quantization guard: a via is a disk stamped at its own
        // centre cell, not an any-angle segment that can drift half a
        // cell off its raster — this matches the historical
        // `via_safe_radius` exactly.
        self.model(
            net,
            Length(self.resolver.defaults().via_diameter.0 / 2),
            cell,
            0.0,
        )
    }
}

/// Ceil-divide `num_nm` by `cell_nm` on the underlying nm integers,
/// returning a cell count. Used to size per-net clearance / copper /
/// via radii so the discrete grid never undersells a distance (always
/// rounds the radius up). Callers apply `.max(1)` (clearance / via-safe)
/// or `.max(0)` (copper) as appropriate.
pub(crate) fn ceil_cells(num_nm: i64, cell_nm: i64) -> i32 {
    let raw = (num_nm + cell_nm.max(1) - 1) / cell_nm.max(1);
    i32::try_from(raw).unwrap_or(1)
}

/// `(trace_width, clearance, explicit_width)` from the per-net override
/// map, falling back to the global defaults.
fn resolve_from_overrides(opts: &RouteOptions, net: &str) -> (Length, Length, bool) {
    let ov = opts.net_overrides.get(net);
    let w = ov.and_then(|o| o.trace_width);
    let explicit = w.is_some();
    let c = ov.and_then(|o| o.clearance).unwrap_or(opts.clearance);
    (w.unwrap_or(opts.trace_width), c, explicit)
}

#[derive(Debug, Clone)]
pub enum Outcome {
    Ok {
        trace_segments: usize,
        vias: usize,
        /// Sum of straight-segment lengths laid down for this net, mm.
        length_mm: f64,
        /// Sum of Manhattan distances from the chosen hub pad to every
        /// other pad in the net, mm. This is the lower bound a perfect
        /// orthogonal star tree could hit. `length_mm / lower_bound_mm`
        /// is the "detour ratio" — 1.0 = optimal, > 1.5 = the router
        /// (or the placement) made the net work harder than it should.
        lower_bound_mm: f64,
    },
    Failed {
        reason: String,
    },
}

#[derive(Debug, Clone, Default)]
pub struct RouteReport {
    pub per_net: Vec<(String, Outcome)>,
    /// Trace segments the coarse per-net search laid. This is a SEARCH
    /// statistic: it excludes the fanout stubs, and it predates the
    /// organic pass (which re-splits polylines). Use
    /// `board_trace_count` for "what is actually on the board".
    pub trace_count: usize,
    /// Vias the coarse per-net search laid — excludes fanout/dogbone and
    /// pour-stitching vias. See `board_via_count`.
    pub via_count: usize,
    /// Traces on the FINAL board once every pass has committed: coarse
    /// routing + escape/dogbone stubs + organic re-segmentation. This is
    /// the number a `view`/`status` of the saved project will agree with.
    pub board_trace_count: usize,
    /// Vias on the FINAL board: coarse routing + fanout/dogbone +
    /// pour stitching.
    pub board_via_count: usize,
    /// Vias contributed by the fanout/escape pre-pass.
    pub fanout_via_count: usize,
    /// Of those, how many are dogbones (barrel outside the pad, fed by a
    /// copper stub) rather than true via-in-pad.
    pub dogbone_via_count: usize,
    /// Pre-laid escape/dogbone stub segments committed to the board.
    pub escape_stub_count: usize,
    /// Pads the escape pass gave a barrel site to (via-in-pad or dogbone).
    /// With `stranded_pads`, this is the escape-stage metric that predicts
    /// what the search can possibly finish: a net whose pad never got a
    /// slot cannot be routed no matter how long the search runs.
    pub escaped_pad_count: usize,
    /// Pads that NEEDED an escape and got none — no barrel site clears
    /// their neighbours, or none that does can be reached by legal copper.
    /// Geometrically stranded; reported before routing rather than
    /// discovered as a mysterious failed net afterwards.
    pub stranded_pads: Vec<String>,
    /// Nets the router actually attempted (pour-only nets are skipped by
    /// design and are not counted here).
    pub routable_net_count: usize,
    /// Wall-clock seconds the whole `route()` call took.
    pub elapsed_seconds: f64,
    /// True when the `max_seconds` budget was exhausted, so the result is
    /// "best so far" rather than "best the router can do".
    pub budget_hit: bool,
    /// Sum of `length_mm` over every successfully-routed net.
    pub total_length_mm: f64,
    /// Sum of `lower_bound_mm` over the same set.
    pub total_lower_bound_mm: f64,
    /// How many full rip-up-and-reroute passes the driver performed
    /// before settling on this report — the REAL round count, not a
    /// quota: the loop runs until it reaches a fixpoint or the budget
    /// truncates it. 1 = a single pass was enough (or the first round
    /// already hit the fixpoint).
    pub iterations: usize,
    /// Plain-text suggestions for the agent: which footprints to move
    /// to fix the still-failing nets. Generated post-hoc from the best
    /// report — empty when every net routed.
    pub hints: Vec<String>,
    /// What the organic post-pass did; `None` when disabled or nothing
    /// was routed.
    pub organic: Option<crate::organic::OrganicReport>,
    /// Pads proved unreachable on the winning escape plan's BARE copper —
    /// see `crate::reach`. These are geometry failures, not budget ones:
    /// no net order and no wall clock can route them, only a different
    /// escape plan, placement or stackup. Formatted `U1.52 (QSPI_SD2)`.
    pub entombed_pads: Vec<String>,
}

/// A net is "bad" — and pulled to the front of the next iteration's
/// order — if its detour ratio exceeds this threshold or it failed
/// outright. The lower bound is HPWL (Manhattan), but Theta* lays
/// Euclidean traces that can be shorter than Manhattan even on a
/// detour; so the ratio is looser than the number suggests. 2.2 catches
/// real failures without flagging healthy diagonal runs.
const BAD_DETOUR_RATIO: f64 = 2.2;

/// Negotiated congestion: per-cell bias added to the corridor around a
/// failed net's pads on the next iteration. Compared to a base step
/// cost of 1 per cell, 4 makes the corridor 5× more expensive — strong
/// enough to push easy nets to detour, weak enough that the bad net
/// itself (which routes first under RR&R) still uses the corridor.
const CONGESTION_BUMP_FAILED: u32 = 4;
/// Lighter bump for a net that succeeded but took a long detour: the
/// "blame" is fuzzier so the bias is too.
const CONGESTION_BUMP_INEFFICIENT: u32 = 2;
/// Cells around a bad net's pad bbox to mark as congested. ~1.5 mm at
/// the default 0.25 mm cell pitch — about a trace width plus clearance,
/// so other nets see the whole "corridor" as expensive, not just the
/// pads themselves.
const CONGESTION_RADIUS_CELLS: i32 = 6;
/// Hard cap on accumulated bias per cell. Beyond this the bias would
/// dominate the heuristic and A* would refuse the cell even when it's
/// the only path; keep it bounded.
const CONGESTION_MAX: u32 = 32;

/// Rip-up: max distinct blocker nets tried per failing net. Tiny by
/// design — bounds the per-pass extra work and guarantees termination.
const RIPUP_MAX_BLOCKERS: usize = 4;
/// Rip-up recursion cap: how many levels of "rip a blocker, and if the
/// blocker can't reroute, let IT rip one of its own blockers" we allow.
/// Depth 2 resolves 3-way mutual conflicts (e.g. CC2 needs VBUS_OUT moved,
/// and VBUS_OUT in turn needs a third net moved) without unbounded churn.
/// Termination is guaranteed independently by the pass-wide `taboo` set
/// (each successful rip tabooes a net; taboo only grows), so this is just a
/// work cap.
const RIPUP_MAX_DEPTH: usize = 2;
/// Extra cells added to the clearance radius when scanning a failed net's
/// corridor for rip-up blockers. The failing spoke is a single straight
/// segment; the net that actually boxes it can sit a little off that line
/// (especially for multi-pad nets), so we dilate the scan beyond the bare
/// trace clearance to surface the real obstruction.
const RIPUP_CORRIDOR_WIDEN: i32 = 4;

/// Route every net found in the board's pad assignments. Mutates
/// `board` in place: existing routing is cleared, new routing is laid.
///
/// The driver runs rip-up-and-reroute ROUNDS (one pass + one fire of the
/// rip-and-reassign lever) until a fixpoint or the wall-clock budget.
/// After each pass, any net that failed or whose detour ratio exceeds
/// `BAD_DETOUR_RATIO` is pulled to the front of the order for the next
/// pass — those bad nets get pristine corridors before the easy nets
/// claim the obvious paths. The "best" report (fewest failures, then
/// shortest total wire) wins; if no iteration improves on the first,
/// the first wins and the board is laid back to its first-pass state.
///
/// Honours `RouteOptions::max_seconds`: when the budget is exhausted the
/// best-so-far board is committed and remaining unrouted nets are reported
/// as failed with a timeout reason.
/// Safety net on the round loop. The loop's real exit is the FIXPOINT
/// test (no new net routed and no barrel moved) or the wall clock; this
/// only bounds a pathological board where neither ever trips.
const MAX_RR_ROUNDS: usize = 64;
/// How many passes a net must fail before its barrel counts as stuck. Two
/// means the net survived a reorder and a congestion bump and still could
/// not reach its own barrel — at that point the barrel, not the routing
/// order, is what is wrong. (Measured at one as well on the RP2040 stress
/// board: it fires a pass earlier and lands on the same board, so the
/// stricter reading of "keeps failing" is the one kept.)
const REASSIGN_MIN_STREAK: usize = 2;
/// Most pads moved in one rip-and-reassign round. A whole package side at
/// once would change the geometry so much that the next pass measures a
/// different board rather than the effect of the move.
const MAX_REASSIGN_PADS: usize = 8;

/// Rounds the pre-routing escape-plan legalisation may spend proving and
/// repairing entombed pads. Each round is a flood plus one re-assignment
/// — milliseconds — so the cap is a safety net against a plan that
/// oscillates, not a budget.
const MAX_PLAN_LEGALISE_ROUNDS: usize = 64;

/// Barrels one legalisation round may move. Larger than the in-loop
/// lever's quota (`MAX_REASSIGN_PADS`): here a round is cheap, and a
/// pocket is usually walled by several barrels at once.
const MAX_PLAN_LEGALISE_PADS: usize = 16;

/// Share of the wall-clock budget the legalisation may spend before the
/// first net is routed. It normally converges in well under this.
const PLAN_LEGALISE_FRACTION: f64 = 0.15;

/// Fraction of `max_seconds` reserved for the tail: stitching, the
/// organic smoothing pass and length matching. Measured on the RP2040
/// stress board, where the tail costs well under a tenth of a 180 s
/// budget.
const TAIL_RESERVE_FRACTION: f64 = 0.04;

pub fn route(board: &mut Board, opts: &RouteOptions) -> RouteReport {
    let started = Instant::now();
    let deadline = deadline_of(opts);
    // Two nested deadlines. The RR&R rounds now run until they reach a
    // fixpoint, so left alone they would spend the entire budget and the
    // tail (stitching, organic smoothing, length matching) would be
    // skipped outright, which is exactly what happened on the first v8
    // measurement.
    // Reserving the tail's slice up front keeps `max_seconds` honoured to
    // the second while guaranteeing the board that comes back is a
    // FINISHED board.
    let budget = opts.max_seconds.unwrap_or(0.0).max(0.0);
    let tail_deadline =
        deadline.map(|d| d - Duration::from_secs_f64(budget * TAIL_RESERVE_FRACTION));
    let rr_deadline = tail_deadline;
    progress(
        opts,
        format!(
            "route: start (max_seconds={})",
            opts.max_seconds
                .map(|s| format!("{s:.0}"))
                .unwrap_or_else(|| "unlimited".into())
        ),
    );
    if opts.engine == RouteEngine::Topo && board.stackup.layer_count() <= 2 {
        return route_topo(board, opts);
    }
    let nets = collect_nets(board);
    // Fanout / escape pre-pass: get fine-pitch pads off the congested
    // surface so the coarse router can reach them. The localized fine-grid
    // escape (short surface stub → fanned-out breakout via) is the standard
    // path; it internally falls back to a plain via-in-pad fanout for parts
    // that don't need a fine escape (and for coarse cell pitches where the
    // breakout spread can't fit). Runs on 2+ layer stackups (bottom is the
    // escape layer on classic 2-layer boards).
    progress(
        opts,
        format!(
            "route: planning escapes ({} net(s), stackup {} layer(s))",
            nets.len(),
            board.stackup.layer_count()
        ),
    );
    let plan = crate::escape::plan_escapes(board, opts);
    let (mut fanout, escape_stubs) = (plan.fanout, plan.stubs);
    progress(
        opts,
        format!(
            "route: escape ready — {} via(s), {} stub segment(s), {} pad(s) escaped, {} stranded{}",
            fanout.vias.len(),
            fanout.stubs.len() + escape_stubs.len(),
            fanout.through_pads.len(),
            fanout.stranded_pads.len(),
            if fanout.stranded_pads.is_empty() {
                String::new()
            } else {
                format!(" ({})", fanout.stranded_pads.join(", "))
            }
        ),
    );
    if nets.is_empty() {
        board.clear_routing();
        return RouteReport {
            fanout_via_count: fanout.vias.len(),
            dogbone_via_count: fanout.dogbone_pads.len(),
            escaped_pad_count: fanout.through_pads.len(),
            stranded_pads: fanout.stranded_pads.clone(),
            elapsed_seconds: started.elapsed().as_secs_f64(),
            ..RouteReport::default()
        };
    }

    // First-pass order: caller override (e.g. the GA tuner) wins;
    // otherwise easy nets (fewest pads) first. Same heuristic as before
    // when no override is supplied — gets the unconstrained nets to lay
    // copper before the hairy ones contend for space.
    let mut order: Vec<String> = if let Some(custom) = opts.initial_net_order.as_ref() {
        let valid: HashSet<&str> = nets.keys().map(String::as_str).collect();
        let mut seen: HashSet<String> = HashSet::new();
        let mut out: Vec<String> = Vec::with_capacity(nets.len());
        for n in custom {
            if valid.contains(n.as_str()) && !seen.contains(n.as_str()) {
                seen.insert(n.clone());
                out.push(n.clone());
            }
        }
        let mut leftover: Vec<String> = nets
            .keys()
            .filter(|n| !seen.contains(n.as_str()))
            .cloned()
            .collect();
        leftover.sort_by_key(|n| nets.get(n).map_or(0, Vec::len));
        out.extend(leftover);
        out
    } else {
        let mut o: Vec<String> = nets.keys().cloned().collect();
        o.sort_by_key(|n| nets.get(n).map_or(0, Vec::len));
        o
    };

    // Fine-pitch escape nets first. A net with a fanned-out pad has to
    // thread the congested escape channel of a fine-pitch part (USB-C row,
    // QFN edge); if the easy 2-pin nets route first they claim the channel
    // lanes and box the escapes out. Pull every net that owns a fanned pad
    // to the front (preserving relative order) so the hard escapes get a
    // clear channel before the easy nets fill in around them.
    if !fanout.through_pads.is_empty() {
        let fanned_nets: HashSet<String> = nets
            .iter()
            .filter(|(_, pads)| {
                pads.iter()
                    .any(|p| fanout.through_pads.contains(&p.pad_ref))
            })
            .map(|(n, _)| n.clone())
            .collect();
        if !fanned_nets.is_empty() {
            let (mut hard, easy): (Vec<String>, Vec<String>) =
                order.into_iter().partition(|n| fanned_nets.contains(n));
            hard.extend(easy);
            order = hard;
        }
    }

    // Diff-pair adjacency: any net whose class declares `diff_pair_with = X`
    // must route immediately AFTER X so the "follow" mode can read X's
    // already-laid geometry. Stable in the rest of the ordering.
    order = reorder_for_diff_pairs(order, opts);

    // ---- Escape-plan legalisation by reachability -------------------
    //
    // The slot assignment prices tightness between barrels, but pricing is
    // not proof: on the compact stress board it still commits plans where
    // 15 pads sit in closed pockets — walled by their neighbours' pads and
    // barrels — and no search on such a plan can ever finish those nets.
    // Until now that was only discovered by the RR&R loop, one 30 s
    // rip-up pass at a time, and repaired by a lever that fires once per
    // round.
    //
    // Prove it up front instead. A flood on the bare copper names every
    // entombed pad and, crucially, the barrels forming each pocket; those
    // barrels go back to the slot assignment with their current sites
    // banned. It costs milliseconds per round against a pass's tens of
    // seconds, so the plan can be iterated to a fixpoint before a single
    // net is routed. Best-seen by entombed count, so a round that makes
    // the plan worse is discarded rather than routed.
    let all_nets: Vec<String> = order.clone();
    if !fanout.through_pads.is_empty() {
        let legalise_deadline = deadline.map(|d| {
            let budget = opts.max_seconds.unwrap_or(0.0).max(0.0);
            d - Duration::from_secs_f64(budget * (1.0 - PLAN_LEGALISE_FRACTION))
        });
        let mut verdict =
            analyse_reach(board, opts, &order, &nets, &fanout, &escape_stubs, &all_nets);
        let initial_entombed = verdict.entombed.len();
        let mut best_plan = (verdict.entombed.len(), fanout.clone());
        let mut rounds = 0usize;
        while !verdict.entombed.is_empty() && rounds < MAX_PLAN_LEGALISE_ROUNDS {
            if timed_out(legalise_deadline) {
                break;
            }
            // The pocket's own barrel first (it may have a better site of
            // its own), then the barrels that form the wall, most-blame
            // first. Both are ranked deterministically.
            // Both repairs at once: a pad WITH a barrel in a pocket wants
            // a different site, and a pad with NO barrel that the flood
            // proves sealed in wants one — the planner's exact-millimetre
            // surface probe is finer than the grid the router searches, so
            // "escapes on the surface" is sometimes not true at 0.20 mm.
            let mut movable: Vec<String> = verdict.entombed.keys().cloned().collect();
            movable.extend(
                verdict
                    .blamed_barrels
                    .iter()
                    .filter(|p| fanout.through_pads.contains(*p))
                    .cloned(),
            );
            let mut seen: HashSet<String> = HashSet::new();
            movable.retain(|p| seen.insert(p.clone()));
            movable.truncate(MAX_PLAN_LEGALISE_PADS);
            if movable.is_empty() {
                break;
            }
            if crate::escape::reassign_escapes(board, opts, &mut fanout, &escape_stubs, &movable)
                == 0
            {
                break;
            }
            rounds += 1;
            verdict = analyse_reach(board, opts, &order, &nets, &fanout, &escape_stubs, &all_nets);
            if verdict.entombed.len() < best_plan.0 {
                best_plan = (verdict.entombed.len(), fanout.clone());
            }
        }
        if best_plan.0 < verdict.entombed.len() {
            fanout = best_plan.1;
        }
        if rounds > 0 {
            progress(
                opts,
                format!(
                    "route: escape plan legalised in {rounds} round(s) — entombed pads {} → {}",
                    initial_entombed, best_plan.0
                ),
            );
        } else if initial_entombed > 0 {
            progress(
                opts,
                format!("route: {initial_entombed} pad(s) entombed and no barrel can move"),
            );
        }
    }

    // Cost map shared across iterations: starts at 0, accumulates bias
    // around the corridors of failed/inefficient nets so the next pass
    // detours easy nets out of those corridors. Built from a one-shot
    // grid only for its dims; the actual obstacle grid is built fresh
    // per pass inside `route_pass`.
    let region = compute_region(board, opts);
    let layer_count = board.stackup.layer_count();
    let mut cost_map = Grid::with_layers(region, opts.cell, layer_count).new_cost_map();

    // Rip-and-reassign bookkeeping (see the lever inside the loop).
    let mut failing_streak: HashMap<String, usize> = HashMap::new();
    // Reachability verdict for the CURRENT escape plan (see `crate::reach`).
    // Recomputed whenever the rip-and-reassign lever moves a barrel, since
    // the barrels are half of what walls a pocket in.
    let mut reach = crate::reach::Reach::default();
    let mut reach_stale = true;
    // Every escape plan the lever has produced, as its sorted barrel
    // sites. The lever itself only bans the site a barrel is being moved
    // OFF (banning its whole history costs real nets — measured), so two
    // barrels can trade pockets forever; recognising a plan we have
    // already routed is what turns "no barrel moved" into a real
    // fixpoint. Deterministic and clock-free, like the rest of the test.
    let mut seen_plans: std::collections::BTreeSet<Vec<(String, i64, i64)>> =
        std::collections::BTreeSet::new();
    let mut best: Option<(Board, RouteReport)> = None;
    // The escape plan `best` was routed against. The rip-and-reassign
    // lever below MOVES barrels between passes, and a barrel is fixed
    // copper the winning board's traces already run to — so the plan has
    // to travel with the board it produced, or the committed board would
    // carry barrels its own traces never met.
    let mut best_fanout = fanout.clone();
    let mut last_order: Option<Vec<String>> = None;
    let mut iterations_run = 0;
    // Two different clocks. `hit_deadline` = the REAL budget is gone, so
    // the tail (post passes) must be skipped. `truncated` = the search
    // stopped because of the clock rather than because it converged —
    // that is what the caller's `budget_hit` means.
    let mut hit_deadline = false;
    let mut truncated = false;
    // Extra report lines the negotiation driver contributes (the corridor
    // autopsy). Empty for the classic loop.
    let mut extra_hints: Vec<String> = Vec::new();

    // PathFinder-style negotiated congestion runs FIRST when enabled: it
    // lets every net take its shortest path, then prices the corridors they
    // fight over. If it converges, its solution is legal and complete and we
    // are done. If it does not, it still hands the rip-up-and-reroute loop
    // below still runs on the remaining budget and the driver keeps whichever
    // board is better — so opting in can only cost wall-clock, never
    // connectivity relative to what the hard passes find in the time they
    // are left. Everything downstream (fanout copper, stitching, organic
    // pass, reporting) is shared.
    let negotiating = opts.negotiate && !declares_diff_pairs(opts, &order);
    let mut converged = false;
    if negotiating {
        progress(
            opts,
            format!(
                "route: negotiated congestion (PathFinder) on {} net(s)",
                order.len()
            ),
        );
        let neg = crate::negotiate::negotiate_route(
            board,
            &nets,
            &order,
            opts,
            &fanout,
            &escape_stubs,
            deadline,
        );
        iterations_run = neg.iterations;
        extra_hints = neg.autopsy;
        progress(
            opts,
            format!(
                "route: negotiation {} after {} iteration(s) — {} failed net(s)",
                if neg.converged {
                    "converged (no shared copper)"
                } else {
                    "did not converge"
                },
                neg.iterations,
                count_failed(&neg.report),
            ),
        );
        hit_deadline = timed_out(deadline);
        truncated |= hit_deadline;
        converged = neg.converged;
        best = Some((neg.board, neg.report));
        // Negotiation runs before the first pass, so the plan it routed
        // against is still the untouched one — snapshot it anyway, so the
        // invariant "best_fanout is the plan that produced best" holds at
        // every assignment site rather than by accident of ordering.
        best_fanout = fanout.clone();
    }

    // The pristine order the RR loop starts from — easy-first, or (after a
    // negotiation) contention-first. The loop below mutates `order` (failed
    // nets to the front); the dedicated rip-up pass wants this baseline
    // substrate back.
    let original_order = order.clone();

    // Dedicated clean-board rip-up pass. The round loop is rip-up-free, so
    // its `best` is exactly the historical baseline. If failures remain, run
    // ONE pass on the PRISTINE substrate — original easy-first order, zero
    // cost bias (no reorder/congestion churn) — but with rip-up enabled. On
    // that substrate only the genuinely congested nets fail, so targeted
    // rip-up resolves them surgically; doing it there (rather than inside a
    // reordered/bumped pass) avoids the global degradation greedy rip-up
    // causes on a churned board. Kept only when strictly better, so it can
    // never regress below baseline.
    //
    // It runs once per ROUND, so every escape plan the rip-and-reassign
    // lever produces is judged by both strategies (see the call site).
    let mut clean_pass_done = false;
    macro_rules! run_clean_pass {
        () => {{
            progress(opts, "route: clean-board rip-up pass");
            let mut work = board.clone();
            work.clear_routing();
            let clean_cost = Grid::with_layers(region, opts.cell, layer_count).new_cost_map();
            let report = route_pass(
                &mut work,
                &nets,
                &original_order,
                opts,
                &clean_cost,
                &fanout,
                &escape_stubs,
                true,
                tail_deadline,
                &reach,
            );
            iterations_run += 1;
            clean_pass_done = true;
            let take_it = match &best {
                None => true,
                Some((_, prev)) => report_is_better(&report, prev),
            };
            if take_it {
                best = Some((work, report));
                best_fanout = fanout.clone();
            }
        }};
    }

    // Rounds, not a fixed pass count. A round is one RR&R pass plus one
    // fire of the rip-and-reassign lever, and the loop runs while the wall
    // clock allows AND the last round made progress. It stops on a
    // FIXPOINT — a round that routed no new net and moved no barrel — so a
    // converged board is the same board whatever the budget was; the
    // budget only truncates. `MAX_RR_ROUNDS` is a safety net, not the
    // normal exit (progress is bounded: failures only fall, and the
    // barrel-site history only shrinks the lever's candidate set).
    let mut moved_last_round = 0usize;
    let mut round = 0usize;
    for _ in 1..=MAX_RR_ROUNDS {
        if converged {
            break;
        }
        if timed_out(rr_deadline) {
            truncated = true;
            hit_deadline = timed_out(deadline);
            progress(
                opts,
                "route: max_seconds budget exhausted — keeping best so far",
            );
            break;
        }
        // Stop early if reordering produced nothing new AND the geometry
        // did not move either — that pass would be byte-identical.
        if last_order.as_ref() == Some(&order) && moved_last_round == 0 {
            break;
        }
        last_order = Some(order.clone());
        round += 1;
        iterations_run += 1;
        let failed_before = best.as_ref().map_or(usize::MAX, |(_, r)| count_failed(r));
        progress(
            opts,
            format!("route: pass {iterations_run} ({} net(s))", order.len()),
        );

        let mut work = board.clone();
        work.clear_routing();
        // The reorder/congestion loop is rip-up-free: it stays fast and
        // byte-identical to the historical baseline, so `best` is always
        // seeded at (and never drops below) the baseline result. Rip-up runs
        // once after the loop, on the pristine substrate (see below).
        let report = route_pass(
            &mut work,
            &nets,
            &order,
            opts,
            &cost_map,
            &fanout,
            &escape_stubs,
            false,
            rr_deadline,
            &reach,
        );

        let take_it = match &best {
            None => true,
            Some((_, prev)) => report_is_better(&report, prev),
        };
        if take_it {
            best = Some((work, report.clone()));
            best_fanout = fanout.clone();
        }
        if timed_out(rr_deadline) {
            truncated = true;
            hit_deadline = timed_out(deadline);
            break;
        }
        // Identify bad nets for next iteration. Failed nets always go
        // to the front; inefficient ones follow. Everything else keeps
        // its relative position so we don't rotate the easy nets too.
        let mut failed: Vec<String> = Vec::new();
        let mut inefficient: Vec<String> = Vec::new();
        for (name, outcome) in &report.per_net {
            match outcome {
                Outcome::Failed { .. } => failed.push(name.clone()),
                Outcome::Ok {
                    length_mm,
                    lower_bound_mm,
                    ..
                } if *lower_bound_mm > 0.0 && length_mm / lower_bound_mm > BAD_DETOUR_RATIO => {
                    inefficient.push(name.clone());
                }
                _ => {}
            }
        }
        if failed.is_empty() && inefficient.is_empty() {
            break;
        }

        // Reachability autopsy on the failures. Cheap (bounded pocket
        // floods on the pads that actually failed) and worth a lot: from
        // here on the passes stop paying a pop cap plus a rip-up cascade
        // for spokes no search can finish, and the lever below learns
        // which barrels are doing the walling instead of inferring it
        // from failure streaks.
        if reach_stale && !failed.is_empty() {
            reach = analyse_reach(board, opts, &order, &nets, &fanout, &escape_stubs, &failed);
            reach_stale = false;
            if !reach.entombed.is_empty() {
                progress(
                    opts,
                    format!(
                        "route: {} pad(s) entombed on this escape plan ({})",
                        reach.entombed.len(),
                        reach
                            .entombed
                            .iter()
                            .map(|(p, n)| format!("{p}/{n}"))
                            .collect::<Vec<_>>()
                            .join(", ")
                    ),
                );
            }
        }

        // Both strategies, every round. The clean-board rip-up pass is
        // judged against the escape plan the lever has built SO FAR, and
        // on the stress board that is what decides it: the same pass
        // scores 23 nets on the plan after two barrel moves and 17 on the
        // pristine one. Running it once — before the lever has moved
        // anything, or after it has moved everything — is a coin flip on
        // which plan it happens to see; running it per round tries them
        // all and keeps the best.
        if count_failed(&report) > 0 && !timed_out(rr_deadline) {
            run_clean_pass!();
            if timed_out(rr_deadline) {
                truncated = true;
                hit_deadline = timed_out(deadline);
                break;
            }
        }


        // Rip-and-reassign: a net that failed TWICE at a pad whose barrel
        // we placed is not going to be saved by another reroute. The
        // barrel is stamped on every layer and never ripped, so if it
        // landed in a pocket the neighbouring barrels wall off, every
        // remaining pass re-runs into the same wall. Move it instead —
        // re-ask the escape-slot assignment for those pads with the sites
        // they have already used excluded (`escape::reassign_escapes`), and
        // let the next pass route against the new geometry. It fires on
        // EVERY round now: the history ban makes each fire cost the lever a
        // candidate, so "moved 0" is a statement about the geometry, not
        // about a round quota that happened to run out.
        for name in &failed {
            *failing_streak.entry(name.clone()).or_insert(0) += 1;
        }
        let mut moved = 0usize;
        if !timed_out(deadline) {
            let mut movable: Vec<String> = Vec::new();
            // Blame first. A pad sealed in a pocket is not freed by moving
            // its OWN barrel — that barrel is inside the pocket, and every
            // alternative site on that side is what the pocket is made of.
            // What frees it is moving the barrels that FORM the wall, and
            // the flood names them (`reach::blamed_barrels`, most-blame
            // first). They need no failure streak: the pocket is proof.
            for pad_ref in &reach.blamed_barrels {
                if fanout.through_pads.contains(pad_ref) {
                    movable.push(pad_ref.clone());
                }
            }
            for name in &failed {
                if failing_streak.get(name).copied().unwrap_or(0) < REASSIGN_MIN_STREAK {
                    continue;
                }
                for p in nets.get(name).map_or(&[][..], Vec::as_slice) {
                    if fanout.through_pads.contains(&p.pad_ref) {
                        movable.push(p.pad_ref.clone());
                    }
                }
            }
            // Keep the blame ranking: dedup without sorting the head away,
            // then cap. Deterministic — `blamed_barrels` is ranked by blame
            // then pad ref, and `failed` follows the report order.
            let mut seen: HashSet<String> = HashSet::new();
            movable.retain(|p| seen.insert(p.clone()));
            movable.truncate(MAX_REASSIGN_PADS);
            if !movable.is_empty() {
                moved = crate::escape::reassign_escapes(
                    board,
                    opts,
                    &mut fanout,
                    &escape_stubs,
                    &movable,
                );
                if moved > 0 {
                    let mut key: Vec<(String, i64, i64)> = fanout
                        .via_positions
                        .iter()
                        .map(|(p, v)| (p.clone(), v.x.0, v.y.0))
                        .collect();
                    key.sort();
                    if !seen_plans.insert(key) {
                        // A plan we have already routed: the lever is
                        // cycling, so this round moved nothing new.
                        moved = 0;
                    }
                }
                if moved > 0 {
                    // The barrels are half of what walls a pocket in, so
                    // the verdict above describes geometry that no longer
                    // exists. Re-derive it NOW rather than at the end of
                    // the next round: the next round's two passes are
                    // exactly what it is for, and dropping it would leave
                    // them paying the full pop-cap-plus-rip-up price all
                    // over again (measured: 44 s of a 180 s budget).
                    reach = analyse_reach(
                        board,
                        opts,
                        &order,
                        &nets,
                        &fanout,
                        &escape_stubs,
                        &failed,
                    );
                    reach_stale = false;
                }
                progress(
                    opts,
                    format!(
                        "route: rip-and-reassign — {moved} of {} stuck escape barrel(s) moved",
                        movable.len()
                    ),
                );
            }
        }
        moved_last_round = moved;

        // FIXPOINT. The round routed no new net and could not move a
        // barrel, so the geometry the next round would search is the
        // geometry this one already failed on. Stopping here (rather than
        // on the clock) is what makes a converged result budget-independent.
        let failed_after = best.as_ref().map_or(usize::MAX, |(_, r)| count_failed(r));
        if failed_after >= failed_before && moved == 0 {
            progress(
                opts,
                format!(
                    "route: fixpoint after {iterations_run} pass(es) — no new net routed and no barrel moved"
                ),
            );
            break;
        }

        // Negotiated congestion: bump the corridor around each bad
        // net's pads so easy nets in the NEXT pass detour around it
        // and leave the bad net a clear shot. Bias scales with
        // iteration index — if a net survives its first bump, the
        // next iteration applies a stronger one (capped at
        // `CONGESTION_MAX`) until A* finds a way through.
        let snap_grid = Grid::with_layers(region, opts.cell, layer_count);
        // Rounds, not passes: the clean-board rip-up pass also bumps
        // `iterations_run` (it IS a pass the report must own up to), but
        // it runs on a pristine cost map and must not double-step the
        // congestion ramp the biased passes climb.
        let bump_factor = round as u32; // 1, 2, 3...
        for name in &failed {
            bump_corridor(
                &snap_grid,
                &mut cost_map,
                nets.get(name).map_or(&[], Vec::as_slice),
                CONGESTION_BUMP_FAILED * bump_factor,
            );
        }
        for name in &inefficient {
            bump_corridor(
                &snap_grid,
                &mut cost_map,
                nets.get(name).map_or(&[], Vec::as_slice),
                CONGESTION_BUMP_INEFFICIENT * bump_factor,
            );
        }

        let bad: std::collections::HashSet<String> =
            failed.iter().chain(inefficient.iter()).cloned().collect();
        let rest: Vec<String> = order
            .iter()
            .filter(|n| !bad.contains(*n))
            .cloned()
            .collect();
        order = failed.into_iter().chain(inefficient).chain(rest).collect();
    }

    if !converged
        && !clean_pass_done
        && !hit_deadline
        && best.as_ref().is_none_or(|(_, r)| count_failed(r) > 0)
    {
        if timed_out(tail_deadline) {
            truncated = true;
            hit_deadline = timed_out(deadline);
        } else {
            run_clean_pass!();
        }
    }

    // NOT here: negotiated congestion as a closer. With the escape plan
    // proved reachable, the survivors of the RR&R fixpoint are contention
    // by elimination — exactly what PathFinder exists to arbitrate, and the
    // v6 verdict ("negotiation loses because the wall is geometry") no
    // longer applies for its original reason. Measured on the compact
    // stress board anyway: handed the legalised plan and the ~95 s the
    // converged loop had left, negotiation reaches 11 failed nets against
    // the classic loop's 9. So the verdict survives for a new reason —
    // a global reroute priced by congestion is worse at these survivors
    // than targeted rip-up is — and the code stays out of the driver.

    let fanout = best_fanout;

    // A budget small enough that the escape/fanout pre-pass alone exhausts
    // it leaves `best` empty — the RR loop never got to run a single pass.
    // Synthesise an all-timed-out report rather than panicking: the caller
    // asked for a hard wall-clock bound and is entitled to an answer, and
    // the fanout copper below is still worth committing.
    let (best_work, mut best_report) = best.unwrap_or_else(|| {
        let mut work = board.clone();
        work.clear_routing();
        let per_net = order
            .iter()
            .map(|n| {
                (
                    n.clone(),
                    Outcome::Failed {
                        reason: "timeout (route max_seconds budget) before the first pass".into(),
                    },
                )
            })
            .collect();
        (
            work,
            RouteReport {
                per_net,
                ..RouteReport::default()
            },
        )
    });
    best_report.iterations = iterations_run;
    best_report.hints = generate_hints(&best_report, &nets);
    best_report.hints.extend(extra_hints);
    if !fanout.stranded_pads.is_empty() {
        // The escape-stage truth: these pads have no legal barrel site at
        // all, so their nets are unroutable by construction. Say so up
        // front — no amount of `max_seconds` changes it, only geometry
        // (pitch, rule area, via size, exposed-pad shrink) does.
        best_report.hints.insert(
            0,
            format!(
                "escape: {} pad(s) stranded — no legal escape slot at the resolved rules: {}",
                fanout.stranded_pads.len(),
                fanout.stranded_pads.join(", ")
            ),
        );
    }
    // Final classification of what is left, against the escape plan that
    // actually won. This is the answer to "is this net short of time or
    // short of room?", and it is the one thing a `max_seconds` number can
    // never tell an agent. Cheap: bounded pocket floods on the failures.
    {
        let still_failing: Vec<String> = best_report
            .per_net
            .iter()
            .filter_map(|(n, o)| matches!(o, Outcome::Failed { .. }).then(|| n.clone()))
            .collect();
        if !still_failing.is_empty() {
            let verdict = analyse_reach(
                board,
                opts,
                &original_order,
                &nets,
                &fanout,
                &escape_stubs,
                &still_failing,
            );
            best_report.entombed_pads = verdict
                .entombed
                .iter()
                .map(|(pad, net)| format!("{pad} ({net})"))
                .collect();
            if !best_report.entombed_pads.is_empty() {
                let entombed_nets = verdict.entombed_nets();
                let budget_bound: Vec<&str> = still_failing
                    .iter()
                    .map(String::as_str)
                    .filter(|n| !entombed_nets.contains(n))
                    .collect();
                // Second only to the escape verdict: a pad with no barrel
                // at all is a stronger statement than a pad whose barrel
                // landed in a pocket, and it is the one the agent should
                // read first.
                let at = usize::from(
                    best_report
                        .hints
                        .first()
                        .is_some_and(|h| h.starts_with("escape:")),
                );
                best_report.hints.insert(
                    at,
                    format!(
                        "entombed: {} pad(s) have NO legal path on this escape plan's bare copper \
                         — {}. Geometry, not budget: only a different escape plan, placement, \
                         rule area or stackup moves them. The other {} failed net(s) are \
                         budget-bound{}",
                        best_report.entombed_pads.len(),
                        best_report.entombed_pads.join(", "),
                        budget_bound.len(),
                        if budget_bound.is_empty() {
                            String::new()
                        } else {
                            format!(" ({})", budget_bound.join(", "))
                        }
                    ),
                );
            }
        }
    }
    // Stamp the winning routing onto the caller's board.
    board.clear_routing();
    for trace in best_work.traces {
        board.add_trace(trace);
    }
    for via in best_work.vias {
        board.add_via(via);
    }
    // Fanout vias are fixed geometry, not part of the rip-up/reroute
    // search, so they're added once to the final board.
    for via in &fanout.vias {
        board.add_via(via.clone());
    }
    // Escape stubs are fixed copper too — and they are committed from the
    // SAME plan snapshot as the barrels above (`best_fanout`), so the pad →
    // barrel copper on the board is always the copper the winning pass was
    // routed against.
    for stub in fanout.stubs.iter().chain(escape_stubs.iter()) {
        board.add_trace(stub.clone());
    }
    // Organic post-pass can also be expensive — skip it when the budget
    // is already exhausted so agents get a timely reply.
    if !hit_deadline && !timed_out(deadline) {
        let t = Instant::now();
        post_passes(board, opts, &mut best_report);
        progress(
            opts,
            format!("route: post passes in {:.1}s", t.elapsed().as_secs_f64()),
        );
    } else {
        // Still do cheap stitching so power pours stay connected.
        crate::stitching::add_stitching_vias(board, opts);
        hit_deadline = true;
        truncated = true;
    }
    // Truthful totals: everything above (coarse routing, fanout vias,
    // escape stubs, organic re-segmentation, pour stitching) has now been
    // committed, so read the counts back off the board rather than
    // trusting the coarse search's own tally.
    best_report.board_trace_count = board.traces.len();
    best_report.board_via_count = board.vias.len();
    best_report.fanout_via_count = fanout.vias.len();
    best_report.dogbone_via_count = fanout.dogbone_pads.len();
    best_report.escape_stub_count = fanout.stubs.len() + escape_stubs.len();
    best_report.escaped_pad_count = fanout.through_pads.len();
    best_report.stranded_pads = fanout.stranded_pads.clone();
    best_report.routable_net_count = best_report.per_net.len();
    best_report.elapsed_seconds = started.elapsed().as_secs_f64();
    best_report.budget_hit = truncated || hit_deadline;
    progress(
        opts,
        format!(
            "route: done in {:.1}s — {} traces, {} vias on board, {} failed net(s){}",
            best_report.elapsed_seconds,
            best_report.board_trace_count,
            best_report.board_via_count,
            count_failed(&best_report),
            if best_report.budget_hit {
                " (budget hit)"
            } else {
                ""
            }
        ),
    );
    best_report
}

/// Shared tail for both engines: stitching vias, the organic smoothing
/// pass (BEFORE length matching, so the meander tuner works on — and
/// re-tunes — the final geometry), then length matching.
fn post_passes(board: &mut Board, opts: &RouteOptions, report: &mut RouteReport) {
    crate::stitching::add_stitching_vias(board, opts);
    if opts.organic {
        let org_opts = crate::organic::OrganicOptions {
            max_fillet_radius_mm: opts.organic_fillet_mm,
            ..crate::organic::OrganicOptions::default()
        };
        let org = crate::organic::organic_pass(board, &org_opts, opts, effective_net_rules);
        if org.chains > 0 {
            // Refresh per-net and total lengths from the smoothed copper.
            let mut net_len: HashMap<&str, f64> = HashMap::new();
            for t in &board.traces {
                let dx = t.start.x.to_mm() - t.end.x.to_mm();
                let dy = t.start.y.to_mm() - t.end.y.to_mm();
                *net_len.entry(t.net.as_str()).or_default() += (dx * dx + dy * dy).sqrt();
            }
            let mut total = 0.0;
            for (name, outcome) in &mut report.per_net {
                if let Outcome::Ok { length_mm, .. } = outcome {
                    if let Some(l) = net_len.get(name.as_str()) {
                        *length_mm = *l;
                    }
                    total += *length_mm;
                }
            }
            report.total_length_mm = total;
            report.organic = Some(org);
        }
    }
    if let Some(sch) = opts.schematic.as_ref() {
        let _ = crate::length_match::length_match_pass(board, sch.as_ref());
    }
}

/// Drive the topological engine and shape its outcomes into the same
/// report the grid driver produces (hints included).
fn route_topo(board: &mut Board, opts: &RouteOptions) -> RouteReport {
    let started = Instant::now();
    let nets = collect_nets(board);
    let results = crate::topo::route_all(board, opts);
    let mut per_net: Vec<(String, Outcome)> = Vec::new();
    let mut total_length_mm = 0.0;
    let mut total_lower_bound_mm = 0.0;
    let mut trace_count = 0usize;
    let mut via_count = 0usize;
    for r in results {
        let outcome = if r.ok {
            total_length_mm += r.length_mm;
            total_lower_bound_mm += r.lower_bound_mm;
            trace_count += r.trace_segments;
            via_count += r.vias;
            Outcome::Ok {
                trace_segments: r.trace_segments,
                vias: r.vias,
                length_mm: r.length_mm,
                lower_bound_mm: r.lower_bound_mm,
            }
        } else {
            Outcome::Failed { reason: r.reason }
        };
        per_net.push((r.net, outcome));
    }
    // Stable order for the report (route_all orders by difficulty).
    per_net.sort_by(|a, b| a.0.cmp(&b.0));
    let mut report = RouteReport {
        per_net,
        trace_count,
        via_count,
        total_length_mm,
        total_lower_bound_mm,
        iterations: 1,
        ..RouteReport::default()
    };
    report.hints = generate_hints(&report, &nets);
    post_passes(board, opts, &mut report);
    report.board_trace_count = board.traces.len();
    report.board_via_count = board.vias.len();
    report.routable_net_count = report.per_net.len();
    report.elapsed_seconds = started.elapsed().as_secs_f64();
    report
}

/// Look at the report and emit human-readable suggestions for the
/// agent. For every net that is still failing or whose detour ratio is
/// pathological (>2× HPWL), pick the *outlier* pad — the one whose
/// removal would shrink the bbox the most — and suggest moving its
/// footprint closer to the rest of the net. This is heuristic but
/// usually right: the failing/detoured nets are the ones with one or
/// two pads geographically far from the cluster, and moving those is
/// the lowest-effort fix.
pub(crate) fn generate_hints(
    report: &RouteReport,
    nets: &BTreeMap<String, Vec<NetPadInfo>>,
) -> Vec<String> {
    let mut hints = Vec::new();
    for (net_name, outcome) in &report.per_net {
        let troubled = match outcome {
            Outcome::Failed { .. } => true,
            Outcome::Ok {
                length_mm,
                lower_bound_mm,
                ..
            } if *lower_bound_mm > 0.0 && length_mm / lower_bound_mm > 2.0 => true,
            _ => false,
        };
        if !troubled {
            continue;
        }
        let Some(pads) = nets.get(net_name) else {
            continue;
        };
        if pads.len() < 2 {
            continue;
        }
        // Outlier = pad with max sum-Manhattan distance to all other
        // pads on the net. A central cluster has tight pairwise sums;
        // the outlier sticks out and dominates the bbox.
        let outlier = pads.iter().max_by_key(|p| {
            pads.iter()
                .map(|q| {
                    (p.center.x.0 - q.center.x.0).unsigned_abs()
                        + (p.center.y.0 - q.center.y.0).unsigned_abs()
                })
                .sum::<u64>()
        });
        if let Some(o) = outlier {
            // Reference is "REF.PIN"; the footprint reference is the
            // bit before the dot. Tell the agent that piece.
            let fp_ref = o
                .pad_ref
                .split_once('.')
                .map_or(o.pad_ref.as_str(), |(r, _)| r);
            let kind = if matches!(outcome, Outcome::Failed { .. }) {
                "failed"
            } else {
                "detoured"
            };
            hints.push(format!(
                "net {net_name} {kind}: {fp_ref} (pad at {:.1},{:.1} mm) is the outlier — moving it closer to the rest of the net usually frees the corridor",
                o.center.x.to_mm(),
                o.center.y.to_mm(),
            ));
        }
    }
    hints
}

/// Heuristic: fewer failed nets > shorter total wire > fewer vias.
pub(crate) fn report_is_better(a: &RouteReport, b: &RouteReport) -> bool {
    let fails_a = count_failed(a);
    let fails_b = count_failed(b);
    if fails_a != fails_b {
        return fails_a < fails_b;
    }
    if (a.total_length_mm - b.total_length_mm).abs() > 1e-6 {
        return a.total_length_mm < b.total_length_mm;
    }
    a.via_count < b.via_count
}

pub(crate) fn count_failed(r: &RouteReport) -> usize {
    r.per_net
        .iter()
        .filter(|(_, o)| matches!(o, Outcome::Failed { .. }))
        .count()
}

/// The area copper is allowed to occupy: the outline inset so the centre
/// of the widest copper feature (a via) still satisfies the DRC's edge
/// clearance (default 0.3 mm). Everything the grid holds outside this
/// rectangle is stamped as an obstacle (see [`build_pass_grid`]) — only
/// pad copper, which is placement and not the router's to move, is
/// reachable there.
fn copper_region(board: &Board, opts: &RouteOptions) -> Rect {
    let edge_clearance = Length::from_mm(0.3);
    // Widest copper feature across the *effective* trace widths (max
    // of default and any class override) and the via diameter. Used
    // to inset the routing region so even the fattest power trace
    // sits clear of the board edge.
    let mut widest = opts.trace_width.0.max(opts.via_diameter.0);
    for o in opts.net_overrides.values() {
        if let Some(w) = o.trace_width {
            widest = widest.max(w.0);
        }
    }
    if let Some(sch) = opts.schematic.as_ref() {
        for class in sch.net_classes.values() {
            if let Some(w_mm) = class.trace_width_mm {
                widest = widest.max(Length::from_mm(w_mm).0);
            }
        }
    }
    let half_widest = Length(widest / 2);
    let mut outline_inset = edge_clearance + half_widest;
    // Rounded outline cuts inward at each corner by `r × (1 − 1/√2)`
    // (~0.293 r). The straight sides of the rounded outline still
    // line up with the rect, so only the corners would otherwise
    // intrude into the router's region. Easiest fix: shrink the
    // entire region by that worst-case extra inset — costs a few mm
    // of routable area near the sides, but guarantees no copper
    // crosses the rounded edge.
    if board.outline_corner_radius.0 > 0 {
        let corner_extra_nm = (board.outline_corner_radius.0 as f64 * 0.293).ceil() as i64;
        outline_inset = outline_inset + Length(corner_extra_nm);
    }
    match board.outline {
        Some(r) => Rect::from_corners(
            Point::new(r.min.x + outline_inset, r.min.y + outline_inset),
            Point::new(r.max.x - outline_inset, r.max.y - outline_inset),
        ),
        None => match board.content_bounds() {
            Some(r) => r.expand(Length::from_mm(5.0)),
            None => Rect::from_corners(
                Point::new(Length::from_mm(0.0), Length::from_mm(0.0)),
                Point::new(Length::from_mm(50.0), Length::from_mm(50.0)),
            ),
        },
    }
}

/// The grid's extent. This is [`copper_region`] **grown until it contains
/// every pad**, because a pad the grid does not cover is a pad no search
/// can ever land on — the net is lost before the first expansion, and the
/// failure gets blamed on whichever pad the search was aiming from.
///
/// That is not hypothetical: an edge-mounted header sits with its pads a
/// few tenths of a mm inside Edge.Cuts, i.e. OUTSIDE the inset copper
/// region, and `Grid::snap` truncates toward zero — so the same pad
/// geometry was silently routable on the low side of the board and
/// unroutable on the high side. On the RP2040 stress board that alone
/// cost three nets (`HDRB1`, `SWCLK`, `SWDIO` at J4/J2).
///
/// Growth is in WHOLE CELLS so the cell lattice is bit-for-bit unchanged
/// (only its index origin moves): a board whose pads all sit inside the
/// copper region routes exactly as before, cell for cell.
pub(crate) fn compute_region(board: &Board, opts: &RouteOptions) -> Rect {
    let base = copper_region(board, opts);
    let cell = Length(opts.cell.0.max(1));
    // One cell of slack past the outermost pad centre, so the cell a pad
    // snaps to always has neighbours on the pad's own side to be entered
    // from.
    let grow = |need: Length| -> Length {
        if need.0 <= 0 {
            Length(0)
        } else {
            Length((ceil_cells(need.0, cell.0) as i64 + 1) * cell.0)
        }
    };
    let (mut lo_x, mut lo_y, mut hi_x, mut hi_y) = (Length(0), Length(0), Length(0), Length(0));
    for fp in board.footprints_in_order() {
        for pad in &fp.pads {
            let c = fp.pad_world_center(pad);
            lo_x = Length(lo_x.0.max(grow(base.min.x - c.x).0));
            lo_y = Length(lo_y.0.max(grow(base.min.y - c.y).0));
            hi_x = Length(hi_x.0.max(grow(c.x - base.max.x).0));
            hi_y = Length(hi_y.0.max(grow(c.y - base.max.y).0));
        }
    }
    Rect::from_corners(
        Point::new(base.min.x - lo_x, base.min.y - lo_y),
        Point::new(base.max.x + hi_x, base.max.y + hi_y),
    )
}

/// Via copper radius in cells — the barrel's own half-diameter, stamped
/// bare on every layer. Independent of trace width, so it is one number
/// per routing pass.
pub(crate) fn via_copper_cells(opts: &RouteOptions) -> i32 {
    ceil_cells(opts.via_diameter.0 / 2, opts.cell.0).max(0)
}

/// Nets already carrying a copper pour on some layer. The pour IS the
/// electrical connection, so the router skips them by design.
pub(crate) fn pour_nets(board: &Board) -> HashSet<String> {
    board.pours.iter().map(|p| p.net.clone()).collect()
}

/// The obstacle grid every routing pass starts from: bodies, keepouts,
/// pads, fanout landings and pre-laid escape stubs — everything that is
/// FIXED for the whole route, and nothing that a net laid. Shared by the
/// classic rip-up-and-reroute pass and the negotiation loop, which both
/// need the identical substrate (and the negotiation loop rebuilds it
/// from scratch every iteration).
///
/// `order` doubles as the net-id table: a net's id is its index here.
pub(crate) fn build_pass_grid(
    board: &Board,
    opts: &RouteOptions,
    order: &[String],
    fanout: &crate::fanout::FanoutPlan,
    escape_stubs: &[Trace],
) -> Grid {
    let net_id_of: HashMap<&str, u32> = order
        .iter()
        .enumerate()
        .map(|(i, n)| (n.as_str(), i as u32))
        .collect();
    let net_id_lookup = |n: &str| net_id_of.get(n).copied();

    let region = compute_region(board, opts);
    let mut grid = Grid::with_layers(region, opts.cell, board.stackup.layer_count());
    // Layered stamping, broad-to-narrow: bodies block the area each
    // footprint occupies, keepouts block any user-marked region, pads
    // overwrite the cells they actually own so they stay reachable.
    grid.stamp_bodies(board, opts.clearance);
    grid.stamp_keepouts(board);
    // The grid can extend past the area copper may occupy (it must cover
    // pads sitting between the outline inset and Edge.Cuts). Everything out
    // there is an obstacle; only the pad stamps below reopen it, so a net
    // can land on such a pad without the router treating the board margin
    // as free routing space.
    grid.stamp_outside(copper_region(board, opts));
    // Stamp pads BARE (no clearance inflation): a pad cell holds its true
    // copper extent only. Edge-to-edge clearance to a pad is enforced at
    // search time by each net's own clearance disk — exact at any grid
    // pitch and never over-inflating thin signals (which is what used to
    // box fine-pitch pins in). No-net pads stamp the FOREIGN_NET sentinel
    // (see `stamp_pads`) so they still demand clearance.
    grid.stamp_pads(board, &net_id_lookup, Length(0));
    // Fanned-out pads become through-hole landing zones at their VIA: the
    // via-in-pad ties every layer together, so stamp a DrilledPad disk
    // (the via barrel's footprint) on all layers at the via position. The
    // router can then reach it from an inner layer where there's room,
    // instead of being trapped on the congested surface between fine-pitch
    // neighbours. Crucially we stamp only the via disk, NOT the whole SMD
    // pad rect on every layer: on the inner layers the SMD pad does not
    // exist (only the barrel does), so walling off the full rect there
    // would block the very approach lanes the inner-layer escape needs.
    if !fanout.through_pads.is_empty() {
        // Copper radius of a fanout barrel, in cells, floored at one so
        // the landing is always at least a single reachable DrilledPad
        // cell. The barrel is the smallest via legal where it sits (fab
        // floor / rule-area override), so read the planned via rather
        // than assuming the historical 0.30 mm one.
        let fanout_via_radius = fanout
            .vias
            .iter()
            .map(|v| v.diameter.0 / 2)
            .max()
            .unwrap_or(Length::from_mm(0.15).0);
        let fanout_via_copper_cells = ceil_cells(fanout_via_radius, opts.cell.0).max(1);
        for fp in board.footprints_in_order() {
            for pad in &fp.pads {
                let key = format!("{}.{}", fp.reference, pad.number);
                if !fanout.through_pads.contains(&key) {
                    continue;
                }
                let Some(id) = pad.net.as_deref().and_then(net_id_lookup) else {
                    continue;
                };
                let via_pos = fanout
                    .via_positions
                    .get(&key)
                    .copied()
                    .unwrap_or_else(|| fp.pad_world_center(pad));
                grid.stamp_drilled_disk(via_pos, fanout_via_copper_cells, id);
            }
        }
    }
    // Escape stubs — the dogbone pad → barrel copper carried in the plan,
    // plus any pre-laid fine-escape stubs. Stamped as their net's bare
    // trace copper so foreign nets keep clearance and the escaped net can
    // branch off them. They connect each fine-pitch pad to its breakout
    // via, which is the DrilledPad landing stamped above. The plan's own
    // stubs are read from `fanout` (never from a parallel list), so a pass
    // always sees the stubs that belong to the barrels it is routing to.
    for stub in fanout.stubs.iter().chain(escape_stubs.iter()) {
        if let Some(id) = net_id_lookup(&stub.net) {
            let a = grid.snap(stub.start, stub.layer);
            let b = grid.snap(stub.end, stub.layer);
            let copper = ceil_cells(stub.width.0 / 2, opts.cell.0).max(0);
            grid.stamp_trace(a, b, id, copper);
        }
    }
    grid
}

/// One full routing pass: lay every net (in `order`) onto a freshly
/// cleared `board` and return the per-net outcomes. The board's
/// routing must already be cleared by the caller.
///
/// Reachability verdict for the current escape plan, computed on the BARE
/// board — the copper no pass can move. See `crate::reach` for why that
/// makes the verdict hold for every pass, and what the driver does with it.
fn analyse_reach(
    board: &Board,
    opts: &RouteOptions,
    order: &[String],
    nets: &BTreeMap<String, Vec<NetPadInfo>>,
    fanout: &crate::fanout::FanoutPlan,
    escape_stubs: &[Trace],
    failed: &[String],
) -> crate::reach::Reach {
    let mut bare = board.clone();
    bare.clear_routing();
    let grid = build_pass_grid(&bare, opts, order, fanout, escape_stubs);
    let areas: Vec<RuleArea> = bare.rule_areas.clone();
    let schematic = opts.schematic.clone();
    let rules = RuleCtx::new(&areas, schematic.as_deref(), opts, order, &grid);
    crate::reach::analyse(&grid, &rules, opts, order, nets, fanout, failed)
}

/// When `deadline` is hit mid-pass, remaining nets are recorded as
/// failed (`timeout`) and the partial copper already laid is kept.
#[allow(clippy::too_many_arguments)]
fn route_pass(
    board: &mut Board,
    nets: &BTreeMap<String, Vec<NetPadInfo>>,
    order: &[String],
    opts: &RouteOptions,
    cost_map: &CostMap,
    fanout: &crate::fanout::FanoutPlan,
    escape_stubs: &[Trace],
    allow_ripup: bool,
    deadline: Option<Instant>,
    reach: &crate::reach::Reach,
) -> RouteReport {
    // Every A* in this pass inherits the pass deadline, so a single
    // pathological search can no longer run past the caller's budget.
    let limits = Limits {
        deadline,
        ..Limits::default()
    };
    let net_id_of: HashMap<String, u32> = order
        .iter()
        .enumerate()
        .map(|(i, n)| (n.clone(), i as u32))
        .collect();

    let mut grid = build_pass_grid(board, opts, order, fanout, escape_stubs);

    // Rule context for the pass. The areas are cloned (a handful of
    // rects) so the resolver doesn't borrow the board this pass mutates;
    // the schematic is already behind an Arc.
    let areas: Vec<RuleArea> = board.rule_areas.clone();
    let schematic = opts.schematic.clone();
    let rules = RuleCtx::new(&areas, schematic.as_deref(), opts, order, &grid);

    // Via copper radius (in cells): the via's own half-diameter, stamped
    // bare on every layer. Independent of trace width, so computed once.
    let via_copper_cells = via_copper_cells(opts);

    // Nets that already have a copper pour on at least one layer
    // skip the router entirely — the pour itself is the electrical
    // connection, so adding traces is redundant copper that just
    // clutters the board.
    let pour_nets: std::collections::HashSet<String> = pour_nets(board);

    // Per-net outcomes, folded into the report totals after the pass.
    // Storing outcomes (instead of incrementally summing) makes rip-up's
    // re-accounting of a rerouted blocker trivial and order-independent.
    let mut outcomes: BTreeMap<String, NetRoute> = BTreeMap::new();
    let mut routed_ids: HashSet<u32> = HashSet::new();
    let mut taboo: HashSet<u32> = HashSet::new();

    let mut nets_done = 0usize;
    let total_nets = order.len();
    for net_name in order {
        if timed_out(deadline) {
            // Mark every remaining net (including this one) as timed out
            // so the report still has a complete per-net list.
            for rest in order.iter().skip(nets_done) {
                if outcomes.contains_key(rest) {
                    continue;
                }
                outcomes.insert(
                    rest.clone(),
                    NetRoute::Failed {
                        reason: "timeout (route max_seconds budget)".into(),
                        corridor: None,
                    },
                );
            }
            progress(
                opts,
                format!("route: timeout after {nets_done}/{total_nets} net(s) this pass"),
            );
            break;
        }
        let Some(pad_points) = nets.get(net_name) else {
            nets_done += 1;
            continue;
        };
        let net_id = net_id_of[net_name];

        // Snapshot before attempting so a rip-up retry starts from a clean
        // corridor (this net's partial copper discarded). Only paid when
        // rip-up is enabled (iterations >= 2); iteration 1 is byte-identical
        // to the historical baseline.
        let snap = if allow_ripup {
            Some(Snapshot::take(board, &grid))
        } else {
            None
        };
        let mut nr = route_one_net(
            board,
            &mut grid,
            net_name,
            net_id,
            pad_points,
            opts,
            &rules,
            cost_map,
            fanout,
            via_copper_cells,
            &pour_nets,
            limits,
            reach,
        );
        if allow_ripup {
            if let NetRoute::Failed {
                corridor: Some((seed_g, spoke_g, clr)),
                ..
            } = &nr
            {
                let (seed_g, spoke_g, clr) = (*seed_g, *spoke_g, *clr);
                if let Some(s) = snap.as_ref() {
                    Snapshot::restore(board, &mut grid, s);
                }
                nr = try_ripup_route(
                    board,
                    &mut grid,
                    net_name,
                    net_id,
                    pad_points,
                    (seed_g, spoke_g, clr),
                    nets,
                    &net_id_of,
                    opts,
                    &rules,
                    cost_map,
                    fanout,
                    via_copper_cells,
                    &pour_nets,
                    &routed_ids,
                    &mut taboo,
                    &mut outcomes,
                    0,
                    limits,
                    reach,
                );
            }
        }
        routed_ids.insert(net_id);
        outcomes.insert(net_name.clone(), nr);
        nets_done += 1;
        // Progress every few nets (or last) so agents see forward motion
        // without drowning the activity log.
        if nets_done == total_nets || nets_done % 5 == 0 {
            let ok_so_far = outcomes
                .values()
                .filter(|o| matches!(o, NetRoute::Ok { .. }))
                .count();
            progress(
                opts,
                format!("route: {nets_done}/{total_nets} net(s), {ok_so_far} ok this pass"),
            );
        }
    }

    // Fold outcomes into report totals, emitting `per_net` in `order`
    // sequence so the report stays deterministic.
    let mut per_net = Vec::with_capacity(order.len());
    let mut total_traces = 0usize;
    let mut total_vias = 0usize;
    let mut total_length_mm = 0.0_f64;
    let mut total_lower_bound_mm = 0.0_f64;
    for net_name in order {
        let Some(nr) = outcomes.get(net_name) else {
            continue;
        };
        let outcome = match nr {
            NetRoute::Ok {
                trace_segments,
                vias,
                length_mm,
                lower_bound_mm,
            } => {
                total_traces += *trace_segments;
                total_vias += *vias;
                total_length_mm += *length_mm;
                total_lower_bound_mm += *lower_bound_mm;
                Outcome::Ok {
                    trace_segments: *trace_segments,
                    vias: *vias,
                    length_mm: *length_mm,
                    lower_bound_mm: *lower_bound_mm,
                }
            }
            NetRoute::Failed { reason, .. } => Outcome::Failed {
                reason: reason.clone(),
            },
        };
        per_net.push((net_name.clone(), outcome));
    }

    RouteReport {
        per_net,
        trace_count: total_traces,
        via_count: total_vias,
        total_length_mm,
        total_lower_bound_mm,
        iterations: 0,
        ..RouteReport::default()
    }
}

/// Outcome of routing a single net into `grid`/`board`.
#[derive(Clone)]
pub(crate) enum NetRoute {
    Ok {
        trace_segments: usize,
        vias: usize,
        length_mm: f64,
        lower_bound_mm: f64,
    },
    /// Failed at a spoke. `corridor` = (seed cell, failing spoke cell,
    /// clr_cells used) so a rip-up retry can scan that exact corridor.
    /// Partial copper laid before the failure REMAINS on board/grid
    /// (unchanged from today's behaviour); the caller rolls back via its
    /// own snapshot when it wants the failure undone.
    Failed {
        reason: String,
        corridor: Option<(GridPoint, GridPoint, i32)>,
    },
}

/// Route a single net into `grid`/`board`, laying copper exactly as the
/// historical per-net loop body did. Returns `Ok` with the net's tallies,
/// or `Failed` carrying the failing spoke's corridor so the caller can
/// pick rip-up blockers. Never accumulates pass totals, never nests rip-up.
#[allow(clippy::too_many_arguments)]
pub(crate) fn route_one_net(
    board: &mut Board,
    grid: &mut Grid,
    net_name: &str,
    net_id: u32,
    pad_points: &[NetPadInfo],
    opts: &RouteOptions,
    rules: &RuleCtx<'_>,
    cost_map: &CostMap,
    fanout: &crate::fanout::FanoutPlan,
    via_copper_cells: i32,
    pour_nets: &std::collections::HashSet<String>,
    limits: Limits,
    reach: &crate::reach::Reach,
) -> NetRoute {
    if pour_nets.contains(net_name) {
        return NetRoute::Ok {
            trace_segments: 0,
            vias: 0,
            length_mm: 0.0,
            lower_bound_mm: 0.0,
        };
    }
    if pad_points.len() < 2 {
        return NetRoute::Ok {
            trace_segments: 0,
            vias: 0,
            length_mm: 0.0,
            lower_bound_mm: 0.0,
        };
    }

    // Differential-pair follow attempt. If this net's class names
    // a partner that already has traces, try to lay parallel
    // geometry first. On success we skip the normal Theta* loop
    // for this net; on failure we log and fall through.
    let (net_trace_width_early, _) = effective_net_rules(opts, net_name);
    if let Some(partner) = diff_pair_partner(opts, net_name) {
        let partner_traces: Vec<Trace> = board
            .traces
            .iter()
            .filter(|t| t.net == partner)
            .cloned()
            .collect();
        if !partner_traces.is_empty() {
            let gap_mm = opts
                .schematic
                .as_ref()
                .and_then(|s| s.class_for(net_name).diff_gap_mm)
                .unwrap_or(0.2);
            match try_diff_pair_follow(
                board,
                grid,
                pad_points,
                &partner_traces,
                net_name,
                net_id,
                net_trace_width_early,
                gap_mm,
                opts,
                rules,
                via_copper_cells,
                cost_map,
                limits,
            ) {
                Ok((segs, vias, length_mm)) => {
                    return NetRoute::Ok {
                        trace_segments: segs,
                        vias,
                        length_mm,
                        lower_bound_mm: hpwl_mm(pad_points),
                    };
                }
                Err(reason) => {
                    eprintln!("diff_pair.fallback: net={net_name} reason={reason}");
                }
            }
        }
    }

    // Lower bound = HPWL: half-perimeter of the pad bounding box,
    // mm. The minimum wire length any tree connecting these pads
    // can use, regardless of topology. Same metric the DRC reports
    // so router and DRC agree on what "optimal" means.
    let net_lower_bound_mm = hpwl_mm(pad_points);

    // Pick the geographically central pad as the seed: minimum sum
    // of Manhattan distances to every other pad. With multi-source
    // A* the hub is no longer mandatory — any same-net cell is a
    // search source — but the spoke ordering "closest to seed
    // first" still helps build a tight Prim-style tree.
    // Prefer a NON-fanned-out pad as the seed when one exists: the
    // seed anchors the trunk, and a wide trunk emanating from a
    // fine-pitch fanout pad would short its neighbours. Among the
    // eligible pads pick the geographically central one.
    // Entombed pads are excluded first: seeding the tree at a pad no other
    // pad can reach makes every spoke fail, turning one geometry failure
    // into a whole-net one. (`reach` is empty until the first pass has
    // failures to analyse, so pass 1 is unchanged.)
    let eligible: Vec<usize> = {
        let reachable: Vec<usize> = (0..pad_points.len())
            .filter(|&i| !reach.is_entombed(&pad_points[i].pad_ref))
            .collect();
        let pool = if reachable.is_empty() {
            (0..pad_points.len()).collect::<Vec<usize>>()
        } else {
            reachable
        };
        let non_fanout: Vec<usize> = pool
            .iter()
            .copied()
            .filter(|&i| !fanout.through_pads.contains(&pad_points[i].pad_ref))
            .collect();
        if non_fanout.is_empty() {
            pool
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
                    let q = q.center;
                    (p.x.0 - q.x.0).unsigned_abs() + (p.y.0 - q.y.0).unsigned_abs()
                })
                .sum::<u64>()
        })
        .unwrap_or(&0);
    let seed = pad_points[seed_idx].clone();
    // For a fanned-out pad, aim at the via-in-pad, not the pad centre:
    // the via (possibly slid along the pad) is the only point where the
    // inner-layer copper exists, so it is where the search must land.
    let route_point = |p: &NetPadInfo| -> Point {
        fanout
            .via_positions
            .get(&p.pad_ref)
            .copied()
            .unwrap_or(p.center)
    };
    let seed_grid = grid.snap(route_point(&seed), seed.layer);
    let seed_is_fanout = fanout.through_pads.contains(&seed.pad_ref);

    // Resolve this net's trace width and clearance: schematic class
    // first, then per-net override, then the global default.
    let (net_trace_width, _net_clearance) = effective_net_rules(opts, net_name);
    // Via clearance model for this net: a via's copper extends
    // `via_diameter/2` and must keep the required clearance to foreign
    // copper — whose own half-width is already baked into its bare
    // stamp, so no extra term is needed here. The required clearance is
    // now resolved per foreign net and per cell (rule areas), which is
    // what `via_model` encodes.
    let via_model = rules.via_model(net_name, opts.cell);

    let mut net_segments = 0usize;
    let mut net_vias = 0usize;
    let mut net_length_mm = 0.0_f64;
    // The net's already-laid trace cells, accumulated as each spoke
    // is routed. Seeds the multi-source search (Prim/Steiner growth)
    // without rescanning the whole grid — the key to fine-grid speed.
    let mut net_trace_cells: Vec<GridPoint> = Vec::new();
    // Spokes ordered by distance to seed (closest first). After
    // each spoke is laid, multi-source A* will pick whichever
    // existing same-net cell is closest to the next spoke — so the
    // tree grows Prim-style, not star.
    let mut spokes_sorted: Vec<NetPadInfo> = pad_points
        .iter()
        .enumerate()
        .filter(|(i, _)| *i != seed_idx)
        .map(|(_, p)| p.clone())
        .collect();
    // Entombed spokes go LAST. The loop abandons the net at its first
    // failure, so an unreachable pad sitting in the middle of the order
    // would throw away the copper its reachable siblings could still have
    // laid — copper that keeps the rest of the net one island instead of
    // several, and that the DRC counts.
    spokes_sorted.sort_by_key(|q| {
        (
            reach.is_entombed(&q.pad_ref),
            (seed.center.x.0 - q.center.x.0).unsigned_abs()
                + (seed.center.y.0 - q.center.y.0).unsigned_abs(),
        )
    });
    for spoke in spokes_sorted {
        // Provably unreachable on this escape plan's bare copper: the
        // search would explore the whole grid to its pop cap and then buy a
        // rip-up cascade, all to rediscover a fact the flood already
        // established. Fail it here, with no corridor — rip-up can only
        // move *traces*, and a pocket walled by pads and barrels has none
        // to give.
        if reach.is_entombed(&spoke.pad_ref) {
            return NetRoute::Failed {
                reason: format!(
                    "pad {} at ({:.2}, {:.2}) mm is entombed — no legal path exists to it on \
                     this escape plan, at any budget",
                    spoke.pad_ref,
                    spoke.center.x.to_mm(),
                    spoke.center.y.to_mm(),
                ),
                corridor: None,
            };
        }
        let spoke_grid = grid.snap(route_point(&spoke), spoke.layer);
        // Neck a spoke down to the default (signal) width when either
        // end is a fanned-out fine-pitch pad. A 0.5 mm power trace
        // can't physically enter a 0.30 mm connector pin without
        // shorting the 0.5 mm-pitch neighbour, so the entry necks —
        // exactly what a hand layout does. The trunk between regular
        // pads keeps the full power width.
        let spoke_is_fanout = fanout.through_pads.contains(&spoke.pad_ref);
        let spoke_width = if seed_is_fanout || spoke_is_fanout {
            Length(net_trace_width.0.min(opts.trace_width.0))
        } else {
            net_trace_width
        };
        // Per-trace clearance model + copper radius, from this spoke's
        // (possibly necked) width. The model drives the search-time
        // clearance test; `copper_cells` the bare-copper stamp.
        let clr_model = rules.trace_model(net_name, spoke_width, opts.cell);
        let clr_cells = clr_model.max_radius_cells();
        let copper_cells = ceil_cells(spoke_width.0 / 2, opts.cell.0).max(0);
        let Some(result) = search(
            grid,
            seed_grid,
            net_id,
            opts.via_cost,
            spoke_grid,
            Some(&via_model),
            &clr_model,
            cost_map,
            &net_trace_cells,
            opts.heuristic_weight,
            limits,
            None,
        ) else {
            return NetRoute::Failed {
                reason: format!(
                    "no path to pad {} at ({:.2}, {:.2}) mm",
                    spoke.pad_ref,
                    spoke.center.x.to_mm(),
                    spoke.center.y.to_mm(),
                ),
                corridor: Some((seed_grid, spoke_grid, clr_cells)),
            };
        };
        let (segs, vias, length_mm) = lay_path(
            board,
            grid,
            &result.path,
            net_name,
            net_id,
            opts,
            copper_cells,
            via_copper_cells,
            spoke_width,
            Some(route_point(&spoke)),
        );
        net_segments += segs;
        net_vias += vias;
        net_length_mm += length_mm;
        // Record this spoke's path cells as future search sources so
        // the next spoke branches off the nearest point of the tree.
        for w in result.path.windows(2) {
            if w[0].layer == w[1].layer {
                net_trace_cells.extend(grid.line_cells(w[0], w[1]));
            }
        }
        // The spoke's own pad cell joins the tree too.
        net_trace_cells.push(spoke_grid);
    }
    NetRoute::Ok {
        trace_segments: net_segments,
        vias: net_vias,
        length_mm: net_length_mm,
        lower_bound_mm: net_lower_bound_mm,
    }
}

/// Byte-exact snapshot of the routing-mutable state, used to make a
/// rip-up attempt transactional: any failure path restores this clone, so
/// the grid never desyncs from the committed board copper (INV-A) and a
/// failed speculation costs nothing. Only `grid`, `board.traces` and
/// `board.vias` change during a pass, so the rest of the board is not cloned.
struct Snapshot {
    grid: Grid,
    traces: Vec<Trace>,
    vias: Vec<Via>,
}

impl Snapshot {
    fn take(board: &Board, grid: &Grid) -> Self {
        Snapshot {
            grid: grid.clone(),
            traces: board.traces.clone(),
            vias: board.vias.clone(),
        }
    }
    fn restore(board: &mut Board, grid: &mut Grid, s: &Snapshot) {
        grid.clone_from(&s.grid);
        board.traces.clone_from(&s.traces);
        board.vias.clone_from(&s.vias);
    }
}

/// Remove every trace and via of `net_name` from both the board and the
/// grid. Grid removal uses the exact-inverse unstamp (free only
/// `Trace(net_id)` cells), so INV-A holds. O(net copper), not O(board).
fn rip_net(
    board: &mut Board,
    grid: &mut Grid,
    net_name: &str,
    net_id: u32,
    opts: &RouteOptions,
    via_copper_cells: i32,
) {
    for t in board.traces.iter().filter(|t| t.net == net_name) {
        let a = grid.snap(t.start, t.layer);
        let b = grid.snap(t.end, t.layer);
        let copper = ceil_cells(t.width.0 / 2, opts.cell.0).max(0);
        grid.unstamp_trace(a, b, net_id, copper);
    }
    for v in board.vias.iter().filter(|v| v.net == net_name) {
        let p = grid.snap(v.position, CopperLayer::Top);
        grid.unstamp_via(p, net_id, via_copper_cells);
    }
    board.traces.retain(|t| t.net != net_name);
    board.vias.retain(|v| v.net != net_name);
}

/// Last-resort, transactional rip-up-and-reroute for a failed net `A`.
/// Scans A's failing corridor for already-routed blocker nets (deterministic,
/// ascending net id) and rips them CUMULATIVELY — one at a time, retrying A
/// after each — because a net is often boxed by two or three adjacent traces
/// at once. As soon as A routes, every ripped blocker is rerouted into the
/// updated grid; a blocker that can't fit on its own gets one depth-limited
/// rip-up of its OWN (so chained conflicts resolve). If the whole rip set
/// reroutes, it is committed (blockers tabooed, accounting updated). Any
/// failure path restores a byte-exact `Snapshot`, so the grid never desyncs
/// from board copper (INV-A) and a dead-end attempt costs nothing. If no rip
/// set lets A route, A is laid once more to reproduce the baseline
/// partial-copper + `Failed`.
#[allow(clippy::too_many_arguments)]
fn try_ripup_route(
    board: &mut Board,
    grid: &mut Grid,
    a_name: &str,
    a_id: u32,
    a_pads: &[NetPadInfo],
    corridor: (GridPoint, GridPoint, i32),
    nets: &BTreeMap<String, Vec<NetPadInfo>>,
    net_id_of: &HashMap<String, u32>,
    opts: &RouteOptions,
    rules: &RuleCtx<'_>,
    cost_map: &CostMap,
    fanout: &crate::fanout::FanoutPlan,
    via_copper_cells: i32,
    pour_nets: &std::collections::HashSet<String>,
    routed_ids: &HashSet<u32>,
    taboo: &mut HashSet<u32>,
    outcomes: &mut BTreeMap<String, NetRoute>,
    depth: usize,
    limits: Limits,
    reach: &crate::reach::Reach,
) -> NetRoute {
    let (seed_g, spoke_g, clr) = corridor;
    let id_to_name: HashMap<u32, &String> = net_id_of.iter().map(|(n, i)| (*i, n)).collect();

    // Candidate blockers: routed `Trace` nets in A's failing corridor, not
    // tabooed, excluding A itself. Ascending net id ⇒ deterministic order.
    let candidates: Vec<u32> =
        grid.corridor_trace_nets(seed_g, spoke_g, clr + RIPUP_CORRIDOR_WIDEN, |n| {
            n != a_id && routed_ids.contains(&n) && !taboo.contains(&n)
        });

    // Pristine pre-rip state (A absent — the caller rolled its partial copper
    // back; every other net as routed). Restored on any give-up so the whole
    // attempt is transactional (INV-A: grid never desyncs from board copper).
    let snap0 = Snapshot::take(board, grid);
    // The bookkeeping (taboo + outcomes) is part of the transaction: inner
    // blocker re-routes mutate them as they go, so a give-up must roll them
    // back together with the copper or they describe geometry no longer on
    // the board (stale wire tallies, spurious taboos).
    let taboo0 = taboo.clone();
    let outcomes0 = outcomes.clone();

    // Cumulative rip set. A single blocker often isn't enough (a net can be
    // boxed by two adjacent traces at once), so we rip blockers one at a time
    // and retry A after each; once A routes we reroute the WHOLE rip set.
    let mut ripped: Vec<(u32, &String)> = Vec::new();

    for b_id in candidates.into_iter().take(RIPUP_MAX_BLOCKERS) {
        let Some(&b_name) = id_to_name.get(&b_id) else {
            continue;
        };
        if nets.get(b_name).is_none() {
            continue;
        }
        rip_net(board, grid, b_name, b_id, opts, via_copper_cells);
        ripped.push((b_id, b_name));

        // Route A through the freed corridor.
        let a_res = route_one_net(
            board,
            grid,
            a_name,
            a_id,
            a_pads,
            opts,
            rules,
            cost_map,
            fanout,
            via_copper_cells,
            pour_nets,
            limits,
            reach,
        );
        let NetRoute::Ok { .. } = a_res else {
            // A still boxed: keep this blocker ripped and add the next.
            continue;
        };

        // A routes with the current rip set. Reroute every ripped blocker;
        // each may itself need a depth-limited rip-up to fit back.
        let mut all_ok = true;
        for &(rid, rname) in &ripped {
            let Some(r_pads) = nets.get(rname) else {
                continue;
            };
            let snap_r = Snapshot::take(board, grid);
            let mut r_res = route_one_net(
                board,
                grid,
                rname,
                rid,
                r_pads,
                opts,
                rules,
                cost_map,
                fanout,
                via_copper_cells,
                pour_nets,
                limits,
                reach,
            );
            if let NetRoute::Failed {
                corridor: Some((rs, rsp, rc)),
                ..
            } = &r_res
            {
                if depth + 1 < RIPUP_MAX_DEPTH {
                    let rcc = (*rs, *rsp, *rc);
                    // Discard the blocker's partial copper so its own rip-up
                    // starts from a clean corridor.
                    Snapshot::restore(board, grid, &snap_r);
                    r_res = try_ripup_route(
                        board,
                        grid,
                        rname,
                        rid,
                        r_pads,
                        rcc,
                        nets,
                        net_id_of,
                        opts,
                        rules,
                        cost_map,
                        fanout,
                        via_copper_cells,
                        pour_nets,
                        routed_ids,
                        taboo,
                        outcomes,
                        depth + 1,
                        limits,
                        reach,
                    );
                }
            }
            if let NetRoute::Ok { .. } = r_res {
                taboo.insert(rid);
                outcomes.insert((*rname).clone(), r_res);
            } else {
                all_ok = false;
                break;
            }
        }
        if all_ok {
            return a_res;
        }
        // A routed but some blocker couldn't be replaced: abandon the whole
        // attempt and restore the pristine pre-rip state (copper + bookkeeping).
        Snapshot::restore(board, grid, &snap0);
        *taboo = taboo0.clone();
        *outcomes = outcomes0.clone();
        break;
    }

    // No rip set let A route: restore the pristine state and reproduce
    // baseline behaviour (A's partial copper + Failed), identical to the
    // non-rip path.
    Snapshot::restore(board, grid, &snap0);
    *taboo = taboo0;
    *outcomes = outcomes0;
    route_one_net(
        board,
        grid,
        a_name,
        a_id,
        a_pads,
        opts,
        rules,
        cost_map,
        fanout,
        via_copper_cells,
        pour_nets,
        limits,
        reach,
    )
}

/// HPWL (half-perimeter wire length) of the net's pad bounding box, in
/// mm. The minimum wire any tree connecting these pads can use; matches
/// the DRC's `RoutingInefficient` lower bound so the two layers report
/// the same "optimum we're measuring against".
pub(crate) fn hpwl_mm(pads: &[NetPadInfo]) -> f64 {
    if pads.len() < 2 {
        return 0.0;
    }
    let mut min_x = f64::INFINITY;
    let mut min_y = f64::INFINITY;
    let mut max_x = f64::NEG_INFINITY;
    let mut max_y = f64::NEG_INFINITY;
    for pad in pads {
        let x = pad.center.x.to_mm();
        let y = pad.center.y.to_mm();
        min_x = min_x.min(x);
        min_y = min_y.min(y);
        max_x = max_x.max(x);
        max_y = max_y.max(y);
    }
    (max_x - min_x) + (max_y - min_y)
}

/// Snap every pad of a bad net to the grid, take the bbox, expand by
/// `CONGESTION_RADIUS_CELLS`, and bump the cost map there. We bump the
/// whole bbox (not just the pad cells) so the corridor that any star
/// route from these pads would naturally take becomes expensive — easy
/// nets routed in the next pass detour around it, leaving a clear lane
/// when the bad net itself runs (it's now first in `order`).
fn bump_corridor(snap_grid: &Grid, cost_map: &mut CostMap, pads: &[NetPadInfo], amount: u32) {
    if pads.is_empty() {
        return;
    }
    let mut min_c = i32::MAX;
    let mut min_r = i32::MAX;
    let mut max_c = i32::MIN;
    let mut max_r = i32::MIN;
    for pad in pads {
        let gp = snap_grid.snap(pad.center, pad.layer);
        min_c = min_c.min(gp.col);
        min_r = min_r.min(gp.row);
        max_c = max_c.max(gp.col);
        max_r = max_r.max(gp.row);
    }
    cost_map.bump_box(
        min_c - CONGESTION_RADIUS_CELLS,
        min_r - CONGESTION_RADIUS_CELLS,
        max_c + CONGESTION_RADIUS_CELLS,
        max_r + CONGESTION_RADIUS_CELLS,
        amount,
        CONGESTION_MAX,
    );
}

/// Collapse the path's grid cells into trace segments + via flips and
/// add them to the board. Stamps the new traces onto the grid so
/// subsequent nets honour them as obstacles. Returns
/// `(segments, vias, length_mm)` where `length_mm` is the sum of all
/// straight segments laid (vias themselves contribute zero length).
#[allow(clippy::too_many_arguments)]
pub(crate) fn lay_path(
    board: &mut Board,
    grid: &mut Grid,
    path: &[GridPoint],
    net: &str,
    net_id: u32,
    opts: &RouteOptions,
    copper_cells: i32,
    via_copper_cells: i32,
    trace_width: Length,
    target_world: Option<Point>,
) -> (usize, usize, f64) {
    if path.len() < 2 {
        return (0, 0, 0.0);
    }
    let mut segments = 0;
    let mut vias = 0;
    let mut length_mm = 0.0_f64;
    let mut seg_start_idx = 0;
    for i in 1..path.len() {
        let prev = path[i - 1];
        let cur = path[i];
        if cur.layer != prev.layer {
            if seg_start_idx < i - 1 {
                length_mm += emit_trace(
                    board,
                    grid,
                    &path[seg_start_idx..i],
                    net,
                    net_id,
                    opts,
                    copper_cells,
                    trace_width,
                    None,
                );
                segments += 1;
            }
            board.add_via(Via {
                id: pcb_core::Id::new(),
                position: grid.unsnap(prev),
                drill: opts.via_drill,
                diameter: opts.via_diameter,
                net: net.to_string(),
            });
            grid.stamp_via(prev, net_id, via_copper_cells);
            vias += 1;
            seg_start_idx = i;
        }
    }
    if seg_start_idx < path.len() - 1 {
        length_mm += emit_trace(
            board,
            grid,
            &path[seg_start_idx..],
            net,
            net_id,
            opts,
            copper_cells,
            trace_width,
            target_world,
        );
        segments += 1;
    }
    (segments, vias, length_mm)
}

/// Emit one straight segment per consecutive same-layer pair in `path`.
/// Theta* hands us explicit corners (any-angle), so each window is
/// already a straight LOS run — no need to detect direction changes.
/// Returns total Euclidean length in mm. If `target_world` is `Some`,
/// the LAST segment's end is overridden with that exact world point
/// (typically the spoke pad's true centre, not the snapped grid cell).
/// Cleans up the visual gap a grid-rounded endpoint leaves between
/// trace and pad copper.
#[allow(clippy::too_many_arguments)]
fn emit_trace(
    board: &mut Board,
    grid: &mut Grid,
    path: &[GridPoint],
    net: &str,
    net_id: u32,
    _opts: &RouteOptions,
    copper_cells: i32,
    trace_width: Length,
    target_world: Option<Point>,
) -> f64 {
    if path.len() < 2 {
        return 0.0;
    }
    let layer = path[0].copper_layer();
    let mut total_mm = 0.0_f64;
    let last_idx = path.len() - 2;
    for (i, w) in path.windows(2).enumerate() {
        let s = w[0];
        let e = w[1];
        if s == e {
            continue;
        }
        let start = grid.unsnap(s);
        let end = if i == last_idx {
            target_world.unwrap_or_else(|| grid.unsnap(e))
        } else {
            grid.unsnap(e)
        };
        let dx = start.x.to_mm() - end.x.to_mm();
        let dy = start.y.to_mm() - end.y.to_mm();
        let len_mm = (dx * dx + dy * dy).sqrt();
        let trace = Trace {
            id: pcb_core::Id::new(),
            layer,
            start,
            end,
            width: trace_width,
            net: net.to_string(),
        };
        grid.stamp_trace(s, e, net_id, copper_cells);
        board.add_trace(trace);
        total_mm += len_mm;
    }
    total_mm
}

/// One pad to be routed: its world-coord centre, copper layer, and
/// the human-friendly reference (e.g. "U3.2") so failures can name
/// the offender instead of dumping nm coordinates.
#[derive(Debug, Clone)]
pub struct NetPadInfo {
    pub center: Point,
    pub layer: CopperLayer,
    pub pad_ref: String,
}

/// Board-coord corridor check for diff-pair follow. We reject a
/// proposed B segment when it intersects a foreign-net trace, a pad of
/// any other net, or a keepout polygon — but ALLOW running close to
/// the partner trace (which is the whole point of diff-pair routing).
fn check_diff_corridor_clear(
    board: &Board,
    t: &Trace,
    self_net: &str,
    partner_net: &str,
    clearance_mm: f64,
) -> Result<(), String> {
    let half_w = t.width.to_mm() / 2.0;
    // Foreign-net traces — except the partner, which we deliberately
    // want to run alongside. Require full edge-to-edge clearance: the
    // diff-pair offset is only loosened toward the PARTNER, never toward
    // unrelated copper (otherwise the parallel run hugs foreign traces
    // and the DRC flags TraceTraceClearance).
    for other in &board.traces {
        if other.net == self_net || other.net == partner_net {
            continue;
        }
        if other.layer != t.layer {
            continue;
        }
        let half_other = other.width.to_mm() / 2.0;
        let min_dist = half_w + half_other + clearance_mm;
        let d = segment_to_segment_distance_mm(t, other);
        if d < min_dist {
            return Err(format!("crosses foreign net `{}`", other.net));
        }
    }
    // Keepouts.
    for kp in &board.keepouts {
        if kp.polygon.len() < 3 {
            continue;
        }
        // Sample a few points along the segment.
        for k in 0..=10 {
            let f = f64::from(k) / 10.0;
            let x = t.start.x.to_mm() + f * (t.end.x.to_mm() - t.start.x.to_mm());
            let y = t.start.y.to_mm() + f * (t.end.y.to_mm() - t.start.y.to_mm());
            if simple_point_in_polygon(&kp.polygon, x, y) {
                return Err("enters keepout".into());
            }
        }
    }
    // Foreign pads — but NOT the partner's: a diff pair runs deliberately
    // close to its partner, including the partner's breakout pads at the
    // connector. Only unrelated copper demands full clearance.
    for fp in board.footprints_in_order() {
        for pad in &fp.pads {
            if pad.net.as_deref() == Some(self_net) || pad.net.as_deref() == Some(partner_net) {
                continue;
            }
            if pad.layer != t.layer {
                continue;
            }
            let c = fp.pad_world_center(pad);
            let (pw, ph) = fp.pad_world_size(pad);
            // Distance from pad center to segment.
            let d = point_to_segment_mm(
                c.x.to_mm(),
                c.y.to_mm(),
                t.start.x.to_mm(),
                t.start.y.to_mm(),
                t.end.x.to_mm(),
                t.end.y.to_mm(),
            );
            let pad_r = pw.to_mm().max(ph.to_mm()) / 2.0;
            if d < pad_r + half_w + clearance_mm {
                return Err(format!(
                    "crosses pad of net `{}`",
                    pad.net.as_deref().unwrap_or("(no-net)")
                ));
            }
        }
    }
    Ok(())
}

fn point_to_segment_mm(px: f64, py: f64, ax: f64, ay: f64, bx: f64, by: f64) -> f64 {
    let dx = bx - ax;
    let dy = by - ay;
    let len2 = dx * dx + dy * dy;
    if len2 < 1e-12 {
        let ex = px - ax;
        let ey = py - ay;
        return (ex * ex + ey * ey).sqrt();
    }
    let t = ((px - ax) * dx + (py - ay) * dy) / len2;
    let t = t.clamp(0.0, 1.0);
    let cx = ax + t * dx;
    let cy = ay + t * dy;
    let ex = px - cx;
    let ey = py - cy;
    (ex * ex + ey * ey).sqrt()
}

fn segment_to_segment_distance_mm(a: &Trace, b: &Trace) -> f64 {
    // Approximate: minimum of the four endpoint→segment distances.
    let ax1 = a.start.x.to_mm();
    let ay1 = a.start.y.to_mm();
    let ax2 = a.end.x.to_mm();
    let ay2 = a.end.y.to_mm();
    let bx1 = b.start.x.to_mm();
    let by1 = b.start.y.to_mm();
    let bx2 = b.end.x.to_mm();
    let by2 = b.end.y.to_mm();
    let mut d = point_to_segment_mm(ax1, ay1, bx1, by1, bx2, by2);
    d = d.min(point_to_segment_mm(ax2, ay2, bx1, by1, bx2, by2));
    d = d.min(point_to_segment_mm(bx1, by1, ax1, ay1, ax2, ay2));
    d = d.min(point_to_segment_mm(bx2, by2, ax1, ay1, ax2, ay2));
    d
}

fn simple_point_in_polygon(poly: &[Point], x: f64, y: f64) -> bool {
    let n = poly.len();
    if n < 3 {
        return false;
    }
    let mut inside = false;
    let mut j = n - 1;
    for i in 0..n {
        let pix = poly[i].x.to_mm();
        let piy = poly[i].y.to_mm();
        let pjx = poly[j].x.to_mm();
        let pjy = poly[j].y.to_mm();
        if (piy > y) != (pjy > y) {
            let denom = pjy - piy;
            if denom.abs() > 1e-12 {
                let xi = pix + (y - piy) * (pjx - pix) / denom;
                if x < xi {
                    inside = !inside;
                }
            }
        }
        j = i;
    }
    inside
}

/// True when any net on the board declares a differential-pair partner.
/// The follow-the-partner geometry (`try_diff_pair_follow`) is part of the
/// classic per-net path and has no negotiated equivalent, so such boards
/// keep the historical driver.
fn declares_diff_pairs(opts: &RouteOptions, order: &[String]) -> bool {
    order.iter().any(|n| diff_pair_partner(opts, n).is_some())
}

/// Resolve the diff-pair partner net for `net_name` from the schematic.
/// Returns the partner only when the schematic declares one and it is a
/// different net.
fn diff_pair_partner(opts: &RouteOptions, net_name: &str) -> Option<String> {
    let sch = opts.schematic.as_ref()?;
    let partner = sch.class_for(net_name).diff_pair_with.as_ref()?.clone();
    if partner == net_name {
        return None;
    }
    Some(partner)
}

/// Attempt to lay net `B`'s traces as parallel offsets of partner A's
/// existing traces, then short stub paths to B's pads. Returns
/// `(segments, vias, length_mm)` on success or an error string on
/// failure (caller falls back to plain Theta*).
#[allow(clippy::too_many_arguments)]
fn try_diff_pair_follow(
    board: &mut Board,
    grid: &mut crate::grid::Grid,
    b_pads: &[NetPadInfo],
    partner_traces: &[Trace],
    net_b: &str,
    net_id_b: u32,
    width_b: Length,
    gap_mm: f64,
    opts: &RouteOptions,
    rules: &RuleCtx<'_>,
    via_copper_cells: i32,
    cost_map: &crate::grid::CostMap,
    limits: Limits,
) -> Result<(usize, usize, f64), String> {
    use crate::astar::search;
    if b_pads.len() < 2 {
        return Err("less than 2 pads on follower".into());
    }
    // Per-trace clearance model / copper radius for net B, from its own
    // width and clearance — mirrors the spoke loop in `route_pass`.
    let (_, net_b_clearance) = effective_net_rules(opts, net_b);
    let clr_model_b = rules.trace_model(net_b, width_b, opts.cell);
    let copper_cells_b = ceil_cells(width_b.0 / 2, opts.cell.0).max(0);
    let via_model_b = rules.via_model(net_b, opts.cell);
    // Pick a layer that the partner actually uses (same layer for both).
    let layer = partner_traces[0].layer;
    if !partner_traces.iter().all(|t| t.layer == layer) {
        return Err("partner uses multiple layers".into());
    }
    // Width of partner traces (assume all the same — first one wins).
    let width_a_mm = partner_traces[0].width.to_mm();
    let width_b_mm = width_b.to_mm();
    let offset_mm = width_a_mm / 2.0 + gap_mm + width_b_mm / 2.0;

    // Choose offset side: pick the side that puts the parallel run
    // closer to B's pad cluster centroid.
    let centroid_x_mm: f64 =
        b_pads.iter().map(|p| p.center.x.to_mm()).sum::<f64>() / b_pads.len() as f64;
    let centroid_y_mm: f64 =
        b_pads.iter().map(|p| p.center.y.to_mm()).sum::<f64>() / b_pads.len() as f64;

    let mut emitted: Vec<Trace> = Vec::with_capacity(partner_traces.len());
    let mut total_len_mm = 0.0_f64;
    for t in partner_traces {
        let sx = t.start.x.to_mm();
        let sy = t.start.y.to_mm();
        let ex = t.end.x.to_mm();
        let ey = t.end.y.to_mm();
        let dx = ex - sx;
        let dy = ey - sy;
        let len = (dx * dx + dy * dy).sqrt();
        if len < 1e-9 {
            continue;
        }
        let nx = -dy / len;
        let ny = dx / len;
        // Side selection per segment: pick the side whose midpoint is
        // closer to B's centroid.
        let mid_x = f64::midpoint(sx, ex);
        let mid_y = f64::midpoint(sy, ey);
        let pos_d = (mid_x + offset_mm * nx - centroid_x_mm).powi(2)
            + (mid_y + offset_mm * ny - centroid_y_mm).powi(2);
        let neg_d = (mid_x - offset_mm * nx - centroid_x_mm).powi(2)
            + (mid_y - offset_mm * ny - centroid_y_mm).powi(2);
        let sign = if pos_d <= neg_d { 1.0 } else { -1.0 };
        let ox = sign * offset_mm * nx;
        let oy = sign * offset_mm * ny;
        let p_start = Point::new(Length::from_mm(sx + ox), Length::from_mm(sy + oy));
        let p_end = Point::new(Length::from_mm(ex + ox), Length::from_mm(ey + oy));
        emitted.push(Trace {
            id: pcb_core::Id::new(),
            layer,
            start: p_start,
            end: p_end,
            width: width_b,
            net: net_b.to_string(),
        });
        total_len_mm += len;
    }
    if emitted.is_empty() {
        return Err("no usable partner segments".into());
    }

    // Check the parallel corridor in board coords (not via the grid):
    // the grid's halo around the partner trace would falsely reject
    // the very-close diff-pair offset because gap < default clearance
    // by design. We check foreign-net traces (excluding the partner),
    // foreign pads, and keepouts directly.
    let partner_net_str = diff_pair_partner(opts, net_b).unwrap_or_default();
    let clearance_mm = net_b_clearance.to_mm();
    for t in &emitted {
        check_diff_corridor_clear(board, t, net_b, &partner_net_str, clearance_mm)?;
    }
    // Commit traces and stamp them on the grid.
    let mut segments = 0usize;
    for t in &emitted {
        let a = grid.snap(t.start, t.layer);
        let b = grid.snap(t.end, t.layer);
        grid.stamp_trace(a, b, net_id_b, copper_cells_b);
        board.add_trace(t.clone());
        segments += 1;
    }

    // Now do short Theta* end-cap searches from the closest emitted
    // endpoint to each pad of B. We attempt to land each pad onto the
    // existing parallel net. Multi-source over Trace(net_id_b) covers
    // that for free.
    let mut vias = 0usize;
    let mut total_segs = segments;
    for pad in b_pads {
        let spoke_grid = grid.snap(pad.center, pad.layer);
        // If pad already lands on a same-net trace, skip.
        if matches!(grid.get(spoke_grid), crate::grid::Cell::Trace(n) if n == net_id_b) {
            continue;
        }
        // If the pad is very close to one of the emitted parallel
        // endpoints (within a couple of grid cells), emit a direct
        // stub trace instead of running A* — A* sometimes refuses to
        // start from cells already stamped as `NetPad(self)` because
        // the pad cell is the search start AND target.
        let mut nearest: Option<(Point, f64)> = None;
        for t in &emitted {
            for ep in [t.start, t.end] {
                let dx = ep.x.to_mm() - pad.center.x.to_mm();
                let dy = ep.y.to_mm() - pad.center.y.to_mm();
                let d = (dx * dx + dy * dy).sqrt();
                if nearest.is_none_or(|(_, nd)| d < nd) {
                    nearest = Some((ep, d));
                }
            }
        }
        if let Some((closest, d)) = nearest {
            // Two grid cells worth of stub is the threshold for a
            // direct connection — A* would just emit the same line.
            if d <= 2.0 * grid.cell_nm as f64 / 1_000_000.0 {
                let stub = Trace {
                    id: pcb_core::Id::new(),
                    layer,
                    start: closest,
                    end: pad.center,
                    width: width_b,
                    net: net_b.to_string(),
                };
                let a = grid.snap(stub.start, stub.layer);
                let b = grid.snap(stub.end, stub.layer);
                grid.stamp_trace(a, b, net_id_b, copper_cells_b);
                board.add_trace(stub);
                total_segs += 1;
                total_len_mm += d;
                continue;
            }
        }
        // Synthesise a "seed" — pick the closest emitted endpoint as
        // the start (multi-source A* will also see the Trace cells).
        let mut best_seed = grid.snap(emitted[0].start, layer);
        let mut best_d = u64::MAX;
        for t in &emitted {
            for ep in [t.start, t.end] {
                let gp = grid.snap(ep, layer);
                let dc = u64::from((gp.col - spoke_grid.col).unsigned_abs());
                let dr = u64::from((gp.row - spoke_grid.row).unsigned_abs());
                let d = dc + dr;
                if d < best_d {
                    best_d = d;
                    best_seed = gp;
                }
            }
        }
        // Multi-source set = every cell of net_b's already-emitted
        // traces, so the search branches off the partner-parallel run
        // just as it did when it rescanned the grid for Trace cells.
        let mut db_sources: Vec<GridPoint> = Vec::new();
        for t in &emitted {
            let a = grid.snap(t.start, t.layer);
            let b = grid.snap(t.end, t.layer);
            if a.layer == b.layer {
                db_sources.extend(grid.line_cells(a, b));
            }
        }
        let Some(result) = search(
            grid,
            best_seed,
            net_id_b,
            opts.via_cost,
            spoke_grid,
            Some(&via_model_b),
            &clr_model_b,
            cost_map,
            &db_sources,
            opts.heuristic_weight,
            limits,
            None,
        ) else {
            return Err(format!("no end-cap to pad {}", pad.pad_ref));
        };
        let (segs, vs, len) = lay_path(
            board,
            grid,
            &result.path,
            net_b,
            net_id_b,
            opts,
            copper_cells_b,
            via_copper_cells,
            width_b,
            Some(pad.center),
        );
        total_segs += segs;
        vias += vs;
        total_len_mm += len;
    }
    Ok((total_segs, vias, total_len_mm))
}

/// Reorder so any net whose class declares `diff_pair_with = X` is
/// scheduled IMMEDIATELY after X. The partner has to be in the board's
/// net set too — otherwise we can't follow what isn't there.
/// Preserves the relative order of every other net.
fn reorder_for_diff_pairs(order: Vec<String>, opts: &RouteOptions) -> Vec<String> {
    let Some(sch) = opts.schematic.as_ref() else {
        return order;
    };
    let present: HashSet<&str> = order.iter().map(String::as_str).collect();
    // For each net, the partner it depends on (if any).
    let mut depends_on: HashMap<String, String> = HashMap::new();
    for n in &order {
        if let Some(p) = sch.class_for(n).diff_pair_with.as_ref() {
            if p != n && present.contains(p.as_str()) {
                depends_on.insert(n.clone(), p.clone());
            }
        }
    }
    if depends_on.is_empty() {
        return order;
    }
    // Pair declarations are symmetric (A pair=B and B pair=A). To avoid
    // both halves being treated as followers, break ties by picking
    // whichever appears FIRST in `order` as the leader, then the other
    // becomes the follower. Net names that aren't part of any cycle
    // keep their leader/follower roles as declared.
    let mut leader_of_follower: HashMap<String, String> = HashMap::new();
    let order_idx: HashMap<&str, usize> = order
        .iter()
        .enumerate()
        .map(|(i, n)| (n.as_str(), i))
        .collect();
    for (b, a) in &depends_on {
        // If `a` also depends on `b`, this is a symmetric pair → the
        // earlier one wins as leader.
        if depends_on.get(a).is_some_and(|x| x == b) {
            let bi = order_idx.get(b.as_str()).copied().unwrap_or(usize::MAX);
            let ai = order_idx.get(a.as_str()).copied().unwrap_or(usize::MAX);
            if ai < bi {
                // a leads, b follows.
                leader_of_follower.insert(b.clone(), a.clone());
            }
            // else: b is earlier or equal → b leads; skip recording this
            // (the partner direction `a depends on b` will handle b's
            // follower role from its own loop iteration).
        } else {
            // Asymmetric — `b` depends on `a`, `a` doesn't depend on
            // `b`. `b` is the follower.
            leader_of_follower.insert(b.clone(), a.clone());
        }
    }
    if leader_of_follower.is_empty() {
        return order;
    }
    let followers: HashSet<&str> = leader_of_follower.keys().map(String::as_str).collect();
    let mut followers_of: HashMap<String, Vec<String>> = HashMap::new();
    for (b, a) in &leader_of_follower {
        followers_of.entry(a.clone()).or_default().push(b.clone());
    }
    let mut out: Vec<String> = Vec::with_capacity(order.len());
    for n in &order {
        if followers.contains(n.as_str()) {
            continue;
        }
        out.push(n.clone());
        if let Some(fs) = followers_of.get(n) {
            let mut sorted: Vec<&String> = fs.iter().collect();
            sorted.sort_by_key(|f| order.iter().position(|x| x == *f).unwrap_or(usize::MAX));
            for f in sorted {
                out.push(f.clone());
            }
        }
    }
    out
}

/// For every footprint pad with a net assignment, record the pad's
/// world-coord center (rotation-aware), copper layer, and "Ref.Pin"
/// label under that net's name.
fn collect_nets(board: &Board) -> BTreeMap<String, Vec<NetPadInfo>> {
    let mut nets: BTreeMap<String, Vec<NetPadInfo>> = BTreeMap::new();
    for fp in board.footprints_in_order() {
        for pad in &fp.pads {
            if let Some(net) = &pad.net {
                let center = fp.pad_world_center(pad);
                nets.entry(net.clone()).or_default().push(NetPadInfo {
                    center,
                    layer: pad.layer,
                    pad_ref: format!("{}.{}", fp.reference, pad.number),
                });
            }
        }
    }
    nets
}
