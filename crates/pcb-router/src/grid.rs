//! Uniform-cell occupancy grid used by the Theta* router.
//!
//! N parallel layers (top / bottom plus optional inner layers — set at
//! `Grid::new` time, defaults to 2 to preserve the pre-Phase-4 router
//! contract). Each cell on each layer is one of:
//! - `Free`        — routable.
//! - `Obstacle`    — never enter (foreign pad, board edge).
//! - `NetPad(u32)` — entrance point for the named net; obstacle for
//!   every other net.
//! - `Trace(u32)`  — already routed by another net; obstacle for
//!   everyone else, free for the same net (allows
//!   multi-segment polylines on a star route).
//!
//! Bresenham line rasterisation is shared between `line_of_sight`,
//! `cost_along`, and `stamp_trace` so any-angle segments behave
//! consistently across visibility, cost accumulation, and obstacle
//! stamping.

use pcb_core::{Board, CopperLayer, Footprint, Keepout, Layer, Length, Point, Rect};

/// Pad pitch (mm) at or below which a package counts as "fine pitch":
/// QFN/QFP/BGA territory, where every pin is modelled and the package
/// carries no un-modelled copper under its body. 0.5 mm is the usual
/// industry line between hand-solderable and fine-pitch.
pub(crate) const FINE_PITCH_MM: f64 = 0.5;

/// Smallest centre-to-centre distance (mm) between any two pads of a
/// footprint. `None` for footprints with fewer than two pads.
pub(crate) fn min_pad_pitch_mm(fp: &Footprint) -> Option<f64> {
    let centres: Vec<(f64, f64)> = fp
        .pads
        .iter()
        .map(|p| {
            let c = fp.pad_world_center(p);
            (c.x.to_mm(), c.y.to_mm())
        })
        .collect();
    let mut best = f64::INFINITY;
    for i in 0..centres.len() {
        for j in (i + 1)..centres.len() {
            let d = ((centres[i].0 - centres[j].0).powi(2) + (centres[i].1 - centres[j].1).powi(2))
                .sqrt();
            if d < best {
                best = d;
            }
        }
    }
    best.is_finite().then_some(best)
}

/// Per-cell extra cost layered on top of the grid for negotiated
/// congestion. A* adds `at(p)` to the step cost when entering `p`, so
/// raising the bias on a corridor pushes the next pass's nets to detour
/// around it. Lives across rip-up-and-reroute iterations and accumulates;
/// the grid itself is rebuilt each pass.
#[derive(Debug, Clone)]
pub struct CostMap {
    cols: i32,
    rows: i32,
    layer_count: u8,
    /// Layer-major: index = layer * cols * rows + r * cols + c.
    extra: Vec<u32>,
}

impl CostMap {
    /// Bias for the cell at `p`. Returns 0 for out-of-bounds points so
    /// callers don't need a separate bounds check.
    pub fn at(&self, p: GridPoint) -> u32 {
        if p.col < 0
            || p.row < 0
            || p.col >= self.cols
            || p.row >= self.rows
            || p.layer >= self.layer_count
        {
            return 0;
        }
        let idx = (p.layer as usize) * (self.cols * self.rows) as usize
            + (p.row * self.cols + p.col) as usize;
        self.extra[idx]
    }

    /// Bump the inclusive rectangle on ONE layer only. Negotiated-congestion
    /// history is per-layer by nature: a corridor that stays over-subscribed
    /// on the bottom layer says nothing about the top, and bumping both
    /// would push nets off the free layer as well.
    pub fn bump_box_on(
        &mut self,
        layer: u8,
        c0: i32,
        r0: i32,
        c1: i32,
        r1: i32,
        amount: u32,
        max: u32,
    ) {
        if layer >= self.layer_count {
            return;
        }
        let c0 = c0.max(0);
        let r0 = r0.max(0);
        let c1 = c1.min(self.cols - 1);
        let r1 = r1.min(self.rows - 1);
        if c1 < c0 || r1 < r0 {
            return;
        }
        let stride = (self.cols * self.rows) as usize;
        for r in r0..=r1 {
            let row_base = layer as usize * stride + (r * self.cols) as usize;
            for c in c0..=c1 {
                let i = row_base + c as usize;
                self.extra[i] = (self.extra[i] + amount).min(max);
            }
        }
    }

    /// Bump every cell inside the inclusive rectangle `[c0..=c1, r0..=r1]`
    /// on every layer by `amount`, capped at `max`. Out-of-range columns
    /// and rows are silently clipped.
    pub fn bump_box(&mut self, c0: i32, r0: i32, c1: i32, r1: i32, amount: u32, max: u32) {
        let c0 = c0.max(0);
        let r0 = r0.max(0);
        let c1 = c1.min(self.cols - 1);
        let r1 = r1.min(self.rows - 1);
        if c1 < c0 || r1 < r0 {
            return;
        }
        let stride = (self.cols * self.rows) as usize;
        for layer in 0..self.layer_count as usize {
            for r in r0..=r1 {
                let row_base = layer * stride + (r * self.cols) as usize;
                for c in c0..=c1 {
                    let i = row_base + c as usize;
                    self.extra[i] = (self.extra[i] + amount).min(max);
                }
            }
        }
    }
}

/// Sentinel net id stamped on pads that carry no net (NC pads, mounting
/// holes, etc.). It is never a real net id (those are `0..order.len()`),
/// so the per-trace clearance disk treats this copper as foreign to every
/// net — a trace keeps clearance to a no-net pad — while `walkable` never
/// lets any net enter it (it is `n == target_net` for nobody). This is
/// what replaces the old "no-net pad = Obstacle" stamp, which blocked
/// entry but demanded no clearance.
pub(crate) const FOREIGN_NET: u32 = u32::MAX;

/// True if `c` is copper belonging to a net other than `target` — the
/// only thing the per-trace clearance disk treats as a clearance demand.
/// `Obstacle` is deliberately NOT foreign: component bodies and keepouts
/// block entry (via `walkable`) but impose no edge-to-edge clearance, so
/// traces may still run flush to a body edge exactly as before.
#[inline]
pub(crate) fn is_foreign(c: Cell, target: u32) -> bool {
    matches!(c, Cell::NetPad(n) | Cell::DrilledPad(n) | Cell::Trace(n) if n != target)
}

/// True if `target` may occupy `c`: a free cell or copper of its own net.
#[inline]
pub(crate) fn walkable(c: Cell, target: u32) -> bool {
    match c {
        Cell::Free => true,
        Cell::NetPad(n) | Cell::DrilledPad(n) | Cell::Trace(n) => n == target,
        Cell::Obstacle => false,
    }
}

/// Negotiation-mode variant of [`walkable`]: foreign TRACE/VIA copper is
/// enterable (the two nets "share" the cell and pay for it), while foreign
/// PADS, bodies and keepouts stay hard. Pads cannot negotiate — they are
/// fixed placement geometry, so a shared pad cell could never be legalised.
#[inline]
pub(crate) fn walkable_soft(c: Cell, target: u32) -> bool {
    match c {
        Cell::Free | Cell::Trace(_) => true,
        Cell::NetPad(n) | Cell::DrilledPad(n) => n == target,
        Cell::Obstacle => false,
    }
}

/// All `(dc, dr)` cell offsets inside the Euclidean disk of radius `r`
/// cells (`dc² + dr² ≤ r²`). Precomputed once per search and reused for
/// every clearance test, so the radius² comparison isn't redone per
/// expansion.
pub(crate) fn disk_offsets(r: i32) -> Vec<(i32, i32)> {
    let r = r.max(0);
    let r2 = r * r;
    let mut v = Vec::new();
    for dr in -r..=r {
        for dc in -r..=r {
            if dc * dc + dr * dr <= r2 {
                v.push((dc, dr));
            }
        }
    }
    v
}

/// Same disk, but carrying each offset's squared cell distance and
/// sorted by it. Lets a clearance scan stop as soon as it passes the
/// largest radius any net could demand at that spot.
pub(crate) fn disk_offsets_sorted(r: i32) -> Vec<(i32, i32, i32)> {
    let r = r.max(0);
    let r2 = r * r;
    let mut v: Vec<(i32, i32, i32)> = Vec::new();
    for dr in -r..=r {
        for dc in -r..=r {
            let d2 = dc * dc + dr * dr;
            if d2 <= r2 {
                v.push((dc, dr, d2));
            }
        }
    }
    v.sort_by_key(|(_, _, d2)| *d2);
    v
}

/// Per-cell map of "which clearance-bearing rule area owns this spot",
/// built once per routing pass. `0` = none; `k` = the k-th clearance
/// area (1-based) in the router's area list.
///
/// Rule areas are resolved POSITIONALLY (see `pcb_core::rules`), so the
/// search needs the answer per candidate cell, not per net. Storing one
/// byte per cell keeps that lookup O(1) and the memory a few hundred KB
/// on a board-sized grid, instead of re-testing every rect per
/// expansion.
#[derive(Debug, Clone)]
pub struct AreaField {
    cols: i32,
    rows: i32,
    layer_count: u8,
    idx: Vec<u8>,
}

impl AreaField {
    /// Empty field (no areas anywhere) sized like `grid`.
    pub fn new(cols: i32, rows: i32, layer_count: u8) -> Self {
        Self {
            cols,
            rows,
            layer_count,
            idx: vec![0; (cols.max(0) * rows.max(0)) as usize * layer_count.max(1) as usize],
        }
    }

    /// Mark cells of the inclusive box on `layers` (empty = all) with
    /// `area_idx`, keeping the HIGHEST index already present only when
    /// `wins` says the incoming one loses. The router stamps areas in
    /// resolution order (weakest first), so a plain overwrite is enough.
    pub fn stamp_box(&mut self, c0: i32, r0: i32, c1: i32, r1: i32, layers: &[u8], area_idx: u8) {
        let c0 = c0.max(0);
        let r0 = r0.max(0);
        let c1 = c1.min(self.cols - 1);
        let r1 = r1.min(self.rows - 1);
        if c1 < c0 || r1 < r0 {
            return;
        }
        let stride = (self.cols * self.rows) as usize;
        for layer in 0..self.layer_count {
            if !layers.is_empty() && !layers.contains(&layer) {
                continue;
            }
            for r in r0..=r1 {
                let base = layer as usize * stride + (r * self.cols) as usize;
                for c in c0..=c1 {
                    self.idx[base + c as usize] = area_idx;
                }
            }
        }
    }

    #[inline]
    pub fn at(&self, p: GridPoint) -> u8 {
        if p.col < 0
            || p.row < 0
            || p.col >= self.cols
            || p.row >= self.rows
            || p.layer >= self.layer_count
        {
            return 0;
        }
        let i = p.layer as usize * (self.cols * self.rows) as usize
            + (p.row * self.cols + p.col) as usize;
        self.idx[i]
    }
}

/// Everything one search needs to decide "does this candidate cell keep
/// legal clearance", honouring BOTH halves of the rule:
///
/// - **pairwise**: the required gap is the strictest of the two nets
///   involved, so the radius depends on WHICH foreign net is found —
///   `per_net_r2`, indexed by net id.
/// - **positional**: a rule area containing the candidate cell overrides
///   outright — `area_r2`, indexed through [`AreaField`].
///
/// Radii are squared cell counts so the scan needs no square roots.
#[derive(Debug, Clone)]
pub struct ClearanceModel<'a> {
    /// Offsets sorted by ascending squared distance, up to the largest
    /// radius this model can demand anywhere.
    disk: Vec<(i32, i32, i32)>,
    /// Required r² against foreign net id `i` (outside any area).
    per_net_r2: Vec<i32>,
    /// Required r² against copper with no table entry: the `FOREIGN_NET`
    /// sentinel (no-net pads) and any id past the table.
    other_r2: i32,
    /// Largest of the above — the scan bound outside any area.
    pair_r2: i32,
    /// Required r² inside clearance area `k` (1-based; `[0]` unused).
    area_r2: Vec<i32>,
    field: Option<&'a AreaField>,
}

impl<'a> ClearanceModel<'a> {
    pub fn new(
        per_net_r2: Vec<i32>,
        other_r2: i32,
        area_r2: Vec<i32>,
        field: Option<&'a AreaField>,
    ) -> Self {
        let pair_r2 = per_net_r2.iter().copied().fold(other_r2, i32::max);
        let max_r2 = area_r2.iter().copied().fold(pair_r2, i32::max);
        // r² → r, rounded up so the disk always covers the radius.
        let r = (f64::from(max_r2).sqrt().ceil()) as i32;
        Self {
            disk: disk_offsets_sorted(r),
            per_net_r2,
            other_r2,
            pair_r2,
            area_r2,
            field,
        }
    }

    /// A model that demands the same radius from every net and knows no
    /// areas — the historical single-radius behaviour, used by callers
    /// with no rule context (fine-grid escape, tests).
    pub fn uniform(radius_cells: i32) -> Self {
        let r = radius_cells.max(0);
        Self::new(Vec::new(), r * r, Vec::new(), None)
    }

    /// `(scan bound r², fixed requirement r² or -1)` at `p`. Inside a
    /// clearance area the requirement is absolute — the same for every
    /// foreign net — which is also the scan bound.
    #[inline]
    fn bound_and_fixed(&self, p: GridPoint) -> (i32, i32) {
        if let Some(f) = self.field {
            let k = f.at(p) as usize;
            if k > 0 {
                if let Some(&r2) = self.area_r2.get(k) {
                    return (r2, r2);
                }
            }
        }
        (self.pair_r2, -1)
    }

    #[inline]
    fn need2_of(&self, c: Cell) -> i32 {
        let id = match c {
            Cell::NetPad(n) | Cell::DrilledPad(n) | Cell::Trace(n) => n,
            _ => return self.other_r2,
        };
        if id == FOREIGN_NET {
            return self.other_r2;
        }
        self.per_net_r2
            .get(id as usize)
            .copied()
            .unwrap_or(self.other_r2)
    }

    /// Largest radius (cells) this model can demand — used to size
    /// corridor scans that still want a single number.
    pub fn max_radius_cells(&self) -> i32 {
        let max_r2 = self.area_r2.iter().copied().fold(self.pair_r2, i32::max);
        (f64::from(max_r2).sqrt().ceil()) as i32
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Cell {
    Free,
    Obstacle,
    /// A pad belonging to net id `u32`. Used by the search both as a
    /// source/destination and as an obstacle for foreign nets.
    NetPad(u32),
    /// A through-hole pad cell. Behaves like `NetPad` for traces, but
    /// the via-safe check rejects vias landing inside it (the existing
    /// PTH drill already connects both layers — a router via on top
    /// would collide with the fab drill).
    DrilledPad(u32),
    /// A previously-laid trace cell belonging to net `u32`.
    Trace(u32),
}

#[derive(Debug, Clone)]
pub struct Grid {
    pub origin_nm: (i64, i64),
    pub cell_nm: i64,
    pub cols: i32,
    pub rows: i32,
    /// Number of copper layers the grid was sized for. Always ≥ 2;
    /// defaults to 2 via `Grid::new` for pre-Phase-4 callers.
    pub layer_count: u8,
    /// `layer_count` layers, row-major — index = layer * cols * rows + r * cols + c.
    cells: Vec<Cell>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub struct GridPoint {
    pub layer: u8, // 0 = top, layer_count - 1 = bottom
    pub col: i32,
    pub row: i32,
}

impl GridPoint {
    /// Convert back to the model `Layer`. Identity mapping.
    pub fn copper_layer(self) -> CopperLayer {
        Layer { index: self.layer }
    }
}

impl Grid {
    /// Build a 2-layer grid (legacy default). Use `with_layers` for an
    /// N-layer routing surface.
    #[allow(dead_code)]
    pub fn new(region: Rect, cell: Length) -> Self {
        Self::with_layers(region, cell, 2)
    }

    /// Build a grid covering the routing region with `layer_count`
    /// copper layers. Caller chooses cell pitch — common choice is
    /// 0.25 mm so that 0.2 mm traces with 0.2 mm clearance comfortably
    /// fit per cell. `layer_count` is clamped to `[2, 8]` and aligned
    /// to an even number (manufacturing constraint).
    pub fn with_layers(region: Rect, cell: Length, layer_count: u8) -> Self {
        let layer_count = layer_count.clamp(2, 8);
        let layer_count = if layer_count.is_multiple_of(2) {
            layer_count
        } else {
            layer_count + 1
        };
        let cell_nm = cell.0.max(1);
        let w_nm = region.width().0;
        let h_nm = region.height().0;
        let cols = (w_nm / cell_nm) as i32 + 1;
        let rows = (h_nm / cell_nm) as i32 + 1;
        Self {
            origin_nm: (region.min.x.0, region.min.y.0),
            cell_nm,
            cols,
            rows,
            layer_count,
            cells: vec![Cell::Free; (cols * rows * i32::from(layer_count)) as usize],
        }
    }

    fn idx(&self, p: GridPoint) -> usize {
        let layer_off = p.layer as usize * (self.cols * self.rows) as usize;
        layer_off + (p.row * self.cols + p.col) as usize
    }

    pub fn in_bounds(&self, p: GridPoint) -> bool {
        p.col >= 0
            && p.col < self.cols
            && p.row >= 0
            && p.row < self.rows
            && p.layer < self.layer_count
    }

    pub fn get(&self, p: GridPoint) -> Cell {
        if !self.in_bounds(p) {
            return Cell::Obstacle;
        }
        self.cells[self.idx(p)]
    }

    pub fn set(&mut self, p: GridPoint, cell: Cell) {
        if self.in_bounds(p) {
            let idx = self.idx(p);
            self.cells[idx] = cell;
        }
    }

    /// Snap a board-coord point to the nearest grid cell on the given layer.
    /// Column/row of a board point, layer-agnostic. Used to rasterise
    /// rectangular regions (rule areas) onto the grid.
    pub fn cell_of(&self, p: Point) -> (i32, i32) {
        let dx = p.x.0 - self.origin_nm.0;
        let dy = p.y.0 - self.origin_nm.1;
        (
            (dx + self.cell_nm / 2) as i32 / self.cell_nm as i32,
            (dy + self.cell_nm / 2) as i32 / self.cell_nm as i32,
        )
    }

    pub fn snap(&self, p: Point, layer: CopperLayer) -> GridPoint {
        let dx = p.x.0 - self.origin_nm.0;
        let dy = p.y.0 - self.origin_nm.1;
        // Multi-layer mapping: `CopperLayer::Bottom` (index 1) maps to
        // the BOTTOM of the actual stackup, not literal index 1. The
        // router/grid mostly works in terms of `Layer { index }` now;
        // this helper is for legacy 2-layer call sites.
        let raw_idx = layer.index;
        let actual = if raw_idx == 1 && self.layer_count > 2 {
            self.layer_count - 1
        } else {
            raw_idx
        };
        GridPoint {
            layer: actual.min(self.layer_count - 1),
            col: (dx + self.cell_nm / 2) as i32 / self.cell_nm as i32,
            row: (dy + self.cell_nm / 2) as i32 / self.cell_nm as i32,
        }
    }

    /// Cell range `(min, max)` (layer 0, only `col`/`row` meaningful)
    /// that fully COVERS the board-coord rectangle `[lo, hi]`: the low
    /// corner is floored and the high corner ceiled to cell boundaries.
    /// Unlike `snap` (nearest), this never under-covers the rectangle, so
    /// a bare pad stamp can't leave a sliver of true copper outside the
    /// stamped cells.
    pub(crate) fn cover_rect_cells(&self, lo: Point, hi: Point) -> (GridPoint, GridPoint) {
        let cell = self.cell_nm.max(1) as f64;
        let fdiv = |num: i64| -> i32 { (num as f64 / cell).floor() as i32 };
        let cdiv = |num: i64| -> i32 { (num as f64 / cell).ceil() as i32 };
        let lo = GridPoint {
            layer: 0,
            col: fdiv(lo.x.0 - self.origin_nm.0),
            row: fdiv(lo.y.0 - self.origin_nm.1),
        };
        let hi = GridPoint {
            layer: 0,
            col: cdiv(hi.x.0 - self.origin_nm.0),
            row: cdiv(hi.y.0 - self.origin_nm.1),
        };
        (lo, hi)
    }

    /// Convert a grid point back to a board-coord `Point`.
    pub fn unsnap(&self, p: GridPoint) -> Point {
        Point::new(
            Length(self.origin_nm.0 + i64::from(p.col) * self.cell_nm),
            Length(self.origin_nm.1 + i64::from(p.row) * self.cell_nm),
        )
    }

    /// Stamp each footprint's full body bbox as `Obstacle` on its
    /// copper layer. Prevents foreign-net traces from running
    /// underneath component bodies on the same side — some packages
    /// (ESP32-S3-Zero etc.) carry un-modelled pads or thermal slugs
    /// under the body, and a trace there is a shorting risk even when
    /// no pad is declared. Call this BEFORE `stamp_pads` so the pad
    /// cells overwrite the obstacle inside the bbox. For TH footprints
    /// (any pad with `drill`) the body is stamped on both layers
    /// because the package physically straddles both.
    /// `clearance` is the same pad clearance `stamp_pads` uses: body
    /// cells within `pad + clearance + ESCAPE_MOAT_MM` of one of the
    /// footprint's OWN pads are left free. Without the moat, a part
    /// whose pads do not fill its pad-hull entombs them: an 0.96" OLED
    /// is a pin row plus four mounting holes at the far corners, so the
    /// hull is the whole module and the pins end up sealed inside a
    /// 23x27 mm obstacle slab with no exit ("no path to pad DS1.x").
    /// The moat only frees THIS footprint's stamp - other parts' body
    /// stamps and `stamp_pads` (which re-covers every pad plus
    /// clearance, mounting holes included) still apply.
    pub fn stamp_bodies(&mut self, board: &Board, clearance: Length) {
        /// Escape corridor width around a pad, mm.
        const ESCAPE_MOAT_MM: f64 = 1.0;
        let moat = clearance + Length::from_mm(ESCAPE_MOAT_MM);
        let bottom = self.layer_count - 1;
        for fp in board.footprints_in_order() {
            let Some(bounds) = fp.bounds() else { continue };
            let is_th = fp.pads.iter().any(|p| p.drill.is_some());
            // Fine-pitch SMD packages (0.5 mm pitch and below: QFN, QFP,
            // BGA) are fully modelled — every pin AND the thermal pad is a
            // declared pad, so `stamp_pads` already blocks all the copper
            // that exists. The extra body slab only buys us a keep-out for
            // un-modelled copper, which these parts do not have, while it
            // does close the under-package escape channels the dogbone
            // fanout depends on. Coarse-pitch parts (modules with hidden
            // slugs, castellated boards) keep the conservative stamp.
            if !is_th && min_pad_pitch_mm(fp).is_some_and(|p| p < FINE_PITCH_MM) {
                continue;
            }
            // Pad rects inflated by the moat, world frame, nm.
            let moats: Vec<(i64, i64, i64, i64)> = fp
                .pads
                .iter()
                .map(|pad| {
                    let c = fp.pad_world_center(pad);
                    let (pw, ph) = fp.pad_world_size(pad);
                    (
                        (c.x - pw / 2 - moat).0,
                        (c.y - ph / 2 - moat).0,
                        (c.x + pw / 2 + moat).0,
                        (c.y + ph / 2 + moat).0,
                    )
                })
                .collect();
            // Map the model layer to a grid-layer index. Pre-Phase-4
            // boards only use Top/Bottom; on a 4-layer grid Bottom
            // still means "the very bottom".
            let primary: u8 = if fp.layer.is_top() {
                0
            } else if fp.layer.index == 1 {
                bottom
            } else {
                fp.layer.index.min(bottom)
            };
            // TH pads punch every copper layer they straddle. Since
            // we currently only model through-hole vias top↔bottom,
            // stamp the outer two for TH parts.
            let layers: Vec<u8> = if is_th {
                vec![0, bottom]
            } else {
                vec![primary]
            };
            let cmin = self.snap(bounds.min, fp.layer);
            let cmax = self.snap(bounds.max, fp.layer);
            for &layer in &layers {
                for r in cmin.row..=cmax.row {
                    for c in cmin.col..=cmax.col {
                        let gp = GridPoint {
                            layer,
                            col: c,
                            row: r,
                        };
                        if !self.in_bounds(gp) {
                            continue;
                        }
                        let centre = self.unsnap(gp);
                        if moats.iter().any(|&(x0, y0, x1, y1)| {
                            centre.x.0 >= x0
                                && centre.x.0 <= x1
                                && centre.y.0 >= y0
                                && centre.y.0 <= y1
                        }) {
                            continue; // escape moat around an own pad
                        }
                        if matches!(self.get(gp), Cell::Free) {
                            self.set(gp, Cell::Obstacle);
                        }
                    }
                }
            }
        }
    }

    /// Stamp obstacles for every pad: the pad rectangle expanded by
    /// `clearance` is marked `Obstacle` for foreign nets and `NetPad`
    /// for its own net (so the search can enter it).
    pub fn stamp_pads(
        &mut self,
        board: &Board,
        net_id_of: &dyn Fn(&str) -> Option<u32>,
        clearance: Length,
    ) {
        let bottom = self.layer_count - 1;
        for fp in board.footprints_in_order() {
            for pad in &fp.pads {
                let is_th = pad.drill.is_some();
                let primary_layer: u8 = if pad.layer.is_top() {
                    0
                } else if pad.layer.index == 1 {
                    bottom
                } else {
                    pad.layer.index.min(bottom)
                };
                let center = fp.pad_world_center(pad);
                let (pw, ph) = fp.pad_world_size(pad);
                let half_w = pw / 2 + clearance;
                let half_h = ph / 2 + clearance;
                let min = center.translate(-half_w, -half_h);
                let max = center.translate(half_w, half_h);
                // Round the stamped rect OUTWARD (floor the min corner,
                // ceil the max corner) so the stamped copper always fully
                // covers the true pad rectangle. Snapping to the NEAREST
                // cell could shave up to half a cell off each edge, which —
                // now that pads are stamped bare and clearance is enforced
                // by the search disk against these cells — silently let a
                // trace sit up to half a cell too close to the real pad
                // edge (the sub-cell TracePadClearance / NetShort the bare
                // stamp would otherwise produce on fine-pitch connectors).
                let (cmin, cmax) = self.cover_rect_cells(min, max);
                let net = pad.net.as_deref().and_then(net_id_of);
                let cell_for_net = match (net, is_th) {
                    (Some(id), true) => Cell::DrilledPad(id),
                    (Some(id), false) => Cell::NetPad(id),
                    // No-net pad: stamp the sentinel so the clearance disk
                    // still keeps traces off it, while nobody may enter it.
                    (None, _) => Cell::NetPad(FOREIGN_NET),
                };
                // TH pads punch EVERY copper layer — their drilled barrel
                // is real copper on all of them, so stamp the copper region
                // on every layer (not just the outer two). Inner-layer
                // routing must keep clearance to a through-hole pad exactly
                // like the outer layers do, or it grazes the barrel and the
                // DRC flags it. (On a 2-layer board `0..layer_count` is just
                // `[0, bottom]`, so this is unchanged there.)
                let layers: Vec<u8> = if is_th {
                    (0..self.layer_count).collect()
                } else {
                    vec![primary_layer]
                };
                for &layer in &layers {
                    for r in cmin.row..=cmax.row {
                        for c in cmin.col..=cmax.col {
                            let gp = GridPoint {
                                layer,
                                col: c,
                                row: r,
                            };
                            if !self.in_bounds(gp) {
                                continue;
                            }
                            self.set(gp, cell_for_net);
                        }
                    }
                }
            }
        }
    }

    /// Stamp every cell whose centre falls OUTSIDE `keep` as `Obstacle`,
    /// on every layer. The grid may be larger than the area copper is
    /// allowed to occupy (it has to cover pads that sit between the
    /// outline inset and Edge.Cuts — see `router::compute_region`), and
    /// this is what keeps the router from treating that margin as routable
    /// space. Call it BEFORE `stamp_pads`, so the pads themselves — fixed
    /// placement, not the router's to move — stay reachable.
    pub fn stamp_outside(&mut self, keep: Rect) {
        for r in 0..self.rows {
            for c in 0..self.cols {
                let p = self.unsnap(GridPoint {
                    layer: 0,
                    col: c,
                    row: r,
                });
                if p.x >= keep.min.x && p.x <= keep.max.x && p.y >= keep.min.y && p.y <= keep.max.y
                {
                    continue;
                }
                for layer in 0..self.layer_count {
                    self.set(
                        GridPoint {
                            layer,
                            col: c,
                            row: r,
                        },
                        Cell::Obstacle,
                    );
                }
            }
        }
    }

    /// Rasterise every keepout polygon into `Obstacle` cells on the
    /// applicable layers. Only the "blocks all nets" case is honoured
    /// in this iteration (every `keepout.nets_allowed` is treated as
    /// empty) — the grid's `Cell` enum doesn't yet carry a per-cell
    /// allow-net bitmap. Per-net allow lists stay in the model so a
    /// future grid extension can pick them up without a schema change.
    pub fn stamp_keepouts(&mut self, board: &Board) {
        for kp in &board.keepouts {
            if kp.polygon.len() < 3 {
                continue;
            }
            let bottom = self.layer_count - 1;
            let layers: Vec<u8> = if kp.layers.is_empty() {
                (0..self.layer_count).collect()
            } else {
                kp.layers
                    .iter()
                    .map(|l| {
                        if l.is_top() {
                            0
                        } else if l.index == 1 {
                            bottom
                        } else {
                            l.index.min(bottom)
                        }
                    })
                    .collect()
            };
            // Bounding box of the polygon in grid cells.
            let mut min_c = i32::MAX;
            let mut min_r = i32::MAX;
            let mut max_c = i32::MIN;
            let mut max_r = i32::MIN;
            for p in &kp.polygon {
                let gp = self.snap(*p, CopperLayer::Top);
                min_c = min_c.min(gp.col);
                min_r = min_r.min(gp.row);
                max_c = max_c.max(gp.col);
                max_r = max_r.max(gp.row);
            }
            min_c = min_c.max(0);
            min_r = min_r.max(0);
            max_c = max_c.min(self.cols - 1);
            max_r = max_r.min(self.rows - 1);
            // Standard point-in-polygon scanline test on each cell.
            for r in min_r..=max_r {
                for c in min_c..=max_c {
                    // Use cell-centre coords in mm for the test.
                    let p = self.unsnap(GridPoint {
                        layer: 0,
                        col: c,
                        row: r,
                    });
                    let px = p.x.to_mm();
                    let py = p.y.to_mm();
                    if !point_in_polygon(&kp.polygon, px, py) {
                        continue;
                    }
                    for &layer in &layers {
                        let gp = GridPoint {
                            layer,
                            col: c,
                            row: r,
                        };
                        if matches!(self.get(gp), Cell::Free) {
                            self.set(gp, Cell::Obstacle);
                        }
                    }
                }
            }
        }
    }

    /// Stamp a fanout via's landing as `DrilledPad(net)` — a disk of
    /// radius `copper` cells centred on the via, on EVERY copper layer.
    /// Stamps only the via barrel's footprint, NOT the whole SMD pad rect:
    /// on the inner layers the SMD pad does not physically exist, only the
    /// via copper does. Stamping just the via keeps the inner layers'
    /// approach lanes open between neighbouring fine-pitch pins, which a
    /// full-rect stamp would wall off. Overwrites any cell (the via barrel
    /// shorts whatever shares its column/row).
    pub fn stamp_drilled_disk(&mut self, center: Point, copper: i32, net: u32) {
        let gp = self.snap(center, CopperLayer::Top);
        let copper = copper.max(0);
        let r2 = copper * copper;
        for layer in 0..self.layer_count {
            for dr in -copper..=copper {
                for dc in -copper..=copper {
                    if dc * dc + dr * dr > r2 {
                        continue;
                    }
                    let p = GridPoint {
                        layer,
                        col: gp.col + dc,
                        row: gp.row + dr,
                    };
                    if self.in_bounds(p) {
                        self.set(p, Cell::DrilledPad(net));
                    }
                }
            }
        }
    }

    /// The grid cells a straight segment `a..b` (same layer) rasterises
    /// to, via the shared Bresenham line. Used by the router to track a
    /// net's already-laid trace cells incrementally — so the multi-source
    /// search can seed from them without rescanning the whole grid.
    pub fn line_cells(&self, a: GridPoint, b: GridPoint) -> Vec<GridPoint> {
        debug_assert_eq!(a.layer, b.layer);
        let layer = a.layer;
        bresenham(a.col, a.row, b.col, b.row)
            .into_iter()
            .map(|(c, r)| GridPoint {
                layer,
                col: c,
                row: r,
            })
            .collect()
    }

    /// Distinct net ids of `Trace` copper lying in the clearance corridor of
    /// the straight segment `a..b` (the Bresenham line dilated by the disk of
    /// radius `clr_cells`), scanned on EVERY layer, restricted to nets for
    /// which `accept(net)` is true. Returned ascending (deterministic). Only
    /// `Trace` cells are reported — pads/drilled pads are fixed placement and
    /// cannot be ripped, so they are deliberately excluded. Used by the
    /// router to pick rip-up candidates that actually block a failed corridor.
    pub fn corridor_trace_nets(
        &self,
        a: GridPoint,
        b: GridPoint,
        clr_cells: i32,
        accept: impl Fn(u32) -> bool,
    ) -> Vec<u32> {
        let disk = disk_offsets(clr_cells);
        let mut found: std::collections::BTreeSet<u32> = std::collections::BTreeSet::new();
        for layer in 0..self.layer_count {
            for (c, r) in bresenham(a.col, a.row, b.col, b.row) {
                for &(dc, dr) in &disk {
                    let gp = GridPoint {
                        layer,
                        col: c + dc,
                        row: r + dr,
                    };
                    if let Cell::Trace(n) = self.get(gp) {
                        if n != FOREIGN_NET && accept(n) {
                            found.insert(n);
                        }
                    }
                }
            }
        }
        found.into_iter().collect()
    }

    /// Stamp the bare copper of a trace `a..b`: each Bresenham cell, plus a
    /// disk of radius `copper` cells around it, becomes `Trace(net)` — the
    /// trace's own half-width and nothing more. No clearance halo is
    /// stamped; edge-to-edge clearance is enforced at search time by the
    /// per-trace clearance disk. Works for any-angle segments via Bresenham.
    pub fn stamp_trace(&mut self, a: GridPoint, b: GridPoint, net: u32, copper: i32) {
        debug_assert_eq!(a.layer, b.layer);
        let layer = a.layer;
        for (c, r) in bresenham(a.col, a.row, b.col, b.row) {
            self.stamp_cell_copper(layer, c, r, net, copper);
        }
    }

    /// Exact inverse of `stamp_trace`: for the same Bresenham cells + copper
    /// disk, free ONLY cells whose current value is `Trace(net)`. Pads,
    /// foreign copper, and obstacles are never touched, so the grid stays
    /// byte-consistent with the board copper that remains. Re-rasterises the
    /// trace's own geometry, never the whole grid — O(trace length), not O(board).
    pub fn unstamp_trace(&mut self, a: GridPoint, b: GridPoint, net: u32, copper: i32) {
        debug_assert_eq!(a.layer, b.layer);
        let layer = a.layer;
        for (c, r) in bresenham(a.col, a.row, b.col, b.row) {
            self.unstamp_cell_copper(layer, c, r, net, copper);
        }
    }

    /// True if no foreign copper sits inside the precomputed clearance
    /// `disk` centred on `p` (same layer). The disk is the set of cell
    /// offsets within the searching net's clearance radius; any foreign
    /// `NetPad`/`DrilledPad`/`Trace` inside it rejects the move. Bodies
    /// (`Obstacle`) are ignored here — they only block entry.
    pub(crate) fn clearance_ok(
        &self,
        p: GridPoint,
        target: u32,
        model: &ClearanceModel<'_>,
    ) -> bool {
        self.clearance_scan(p, target, model, false)
    }

    fn clearance_scan(
        &self,
        p: GridPoint,
        target: u32,
        model: &ClearanceModel<'_>,
        block_obstacles: bool,
    ) -> bool {
        let (bound2, fixed) = model.bound_and_fixed(p);
        for &(dc, dr, d2) in &model.disk {
            // The disk is sorted by distance, so once we are past the
            // widest radius anything could demand here, we are done.
            if d2 > bound2 {
                break;
            }
            let gp = GridPoint {
                layer: p.layer,
                col: p.col + dc,
                row: p.row + dr,
            };
            let c = self.get(gp);
            if block_obstacles && matches!(c, Cell::Obstacle) {
                return false;
            }
            if !is_foreign(c, target) {
                continue;
            }
            let need2 = if fixed >= 0 { fixed } else { model.need2_of(c) };
            if d2 <= need2 {
                return false;
            }
        }
        true
    }

    /// Negotiated-congestion variant of [`Grid::clearance_ok`].
    ///
    /// **This is where PathFinder meets copper clearance.** In classic FPGA
    /// PathFinder a resource is a wire: two nets either share it or they do
    /// not. On a PCB the resource a net really consumes is its copper *plus
    /// the clearance halo around it*, and that halo is pairwise (the
    /// strictest of the two nets' rules, or a rule area's override). We keep
    /// the asymmetric formulation the exact-geometry check uses — copper is
    /// stamped BARE and each net scans its own `clearance + half-width` disk
    /// — and simply turn the verdict from a veto into a price:
    ///
    /// - foreign **trace/via** copper inside the disk → shareable: return
    ///   how many distinct foreign nets are involved so the caller can
    ///   charge the present-congestion price. Both nets see the same
    ///   overlap from their own side (the requirement `w_a/2 + clr + w_b/2`
    ///   is symmetric), so negotiation is symmetric too.
    /// - foreign **pad** copper inside the disk → still a hard no. A pad
    ///   cannot be ripped up and rerouted, so a halo violation against one
    ///   could never be negotiated away; letting the search "buy" it would
    ///   only produce solutions that can never be legalised.
    ///
    /// Because the price is computed from the very same disk test that
    /// decides legality, "zero conflicts everywhere" is exactly "DRC-legal
    /// copper" — the convergence test needs no separate geometry pass.
    ///
    /// `None` = hard-blocked. `Some(0)` = a clean cell. `Some(k>0)` = the
    /// cell is shared and costs `k` units of present-congestion price.
    ///
    /// The price is deliberately capped at the FIRST sharing incident found
    /// (`k` is 0 or 1) unless obstacles must still be scanned for: counting
    /// every distinct foreign net would force a full disk sweep on every
    /// expansion, where the hard test bails on the first violation. That
    /// difference is the whole per-expansion cost of the search, and a
    /// finer-grained price bought nothing measurable.
    pub(crate) fn soft_clearance(
        &self,
        p: GridPoint,
        target: u32,
        model: &ClearanceModel<'_>,
        block_obstacles: bool,
    ) -> Option<u32> {
        let (bound2, fixed) = model.bound_and_fixed(p);
        let mut shared = 0u32;
        for &(dc, dr, d2) in &model.disk {
            if d2 > bound2 {
                break;
            }
            let gp = GridPoint {
                layer: p.layer,
                col: p.col + dc,
                row: p.row + dr,
            };
            let c = self.get(gp);
            if block_obstacles && matches!(c, Cell::Obstacle) {
                return None;
            }
            if shared > 0 && !block_obstacles {
                // Nothing left to learn on this cell.
                break;
            }
            if !is_foreign(c, target) {
                continue;
            }
            let need2 = if fixed >= 0 { fixed } else { model.need2_of(c) };
            if d2 > need2 {
                continue;
            }
            match c {
                Cell::Trace(_) => shared = 1,
                // Foreign pad copper (including the `FOREIGN_NET` sentinel
                // and fanout via landings): not negotiable.
                _ => return None,
            }
        }
        Some(shared)
    }

    /// Soft variant of [`Grid::line_of_sight`]: the Theta* any-angle
    /// shortcut in negotiation mode. `None` when any cell on the segment is
    /// hard-blocked; otherwise the total number of (cell, foreign net)
    /// sharing incidents along the segment, which the caller prices.
    pub(crate) fn los_conflicts(
        &self,
        a: GridPoint,
        b: GridPoint,
        target_net: u32,
        model: &ClearanceModel<'_>,
    ) -> Option<u32> {
        debug_assert_eq!(a.layer, b.layer);
        let layer = a.layer;
        let mut total = 0u32;
        for (c, r) in bresenham(a.col, a.row, b.col, b.row) {
            let p = GridPoint {
                layer,
                col: c,
                row: r,
            };
            if !walkable_soft(self.get(p), target_net) {
                return None;
            }
            total = total.saturating_add(self.soft_clearance(p, target_net, model, false)?);
        }
        Some(total)
    }

    /// Soft variant of [`Grid::via_clearance_ok`]: the barrel exists on
    /// every layer, so its sharing price is the sum over layers. Obstacles
    /// still block (the annular ring has to sit somewhere real).
    pub(crate) fn via_soft_clearance(
        &self,
        p: GridPoint,
        target: u32,
        model: &ClearanceModel<'_>,
    ) -> Option<u32> {
        let mut total = 0u32;
        for layer in 0..self.layer_count {
            total = total.saturating_add(self.soft_clearance(
                GridPoint { layer, ..p },
                target,
                model,
                true,
            )?);
        }
        Some(total)
    }

    /// Via variant of [`Grid::clearance_ok`]: a via barrel exists on
    /// every layer, so its clearance must hold on every layer — and
    /// unlike a trace, a via may not be crowded by an `Obstacle` either
    /// (its pad ring is real copper that has to sit somewhere legal,
    /// including inside the routing region: out-of-bounds reads as
    /// `Obstacle`).
    pub(crate) fn via_clearance_ok(
        &self,
        p: GridPoint,
        target: u32,
        model: &ClearanceModel<'_>,
    ) -> bool {
        (0..self.layer_count)
            .all(|layer| self.clearance_scan(GridPoint { layer, ..p }, target, model, true))
    }

    /// True if every Bresenham cell of the straight segment `a..b`
    /// (inclusive) is enterable by `target_net` AND keeps the per-trace
    /// clearance `disk` clear of foreign copper. This is the LOS test the
    /// Theta* any-angle shortcut uses: the centre cell must be walkable
    /// (catches `Obstacle`, which the disk ignores) and its clearance disk
    /// must hold no foreign copper. Requires `a.layer == b.layer`.
    pub(crate) fn line_of_sight(
        &self,
        a: GridPoint,
        b: GridPoint,
        target_net: u32,
        model: &ClearanceModel<'_>,
    ) -> bool {
        debug_assert_eq!(a.layer, b.layer);
        let layer = a.layer;
        for (c, r) in bresenham(a.col, a.row, b.col, b.row) {
            let p = GridPoint {
                layer,
                col: c,
                row: r,
            };
            if !walkable(self.get(p), target_net) {
                return false;
            }
            if !self.clearance_ok(p, target_net, model) {
                return false;
            }
        }
        true
    }

    /// Sum of `cost_map.at(p)` for each cell on the Bresenham line from
    /// `a` (exclusive) to `b` (inclusive). Used by Theta* to charge
    /// per-cell congestion bias along an any-angle straight segment.
    /// Method on `Grid` to keep the line-rasterisation helpers
    /// co-located, even though it doesn't read the grid itself.
    #[allow(clippy::unused_self)]
    pub fn cost_along(&self, cost_map: &CostMap, a: GridPoint, b: GridPoint) -> u32 {
        debug_assert_eq!(a.layer, b.layer);
        let layer = a.layer;
        let mut sum: u32 = 0;
        let mut first = true;
        for (c, r) in bresenham(a.col, a.row, b.col, b.row) {
            if first {
                first = false;
                continue;
            }
            let gp = GridPoint {
                layer,
                col: c,
                row: r,
            };
            sum = sum.saturating_add(cost_map.at(gp));
        }
        sum
    }

    /// Stamp a via's bare copper disk (radius `copper` cells) on every
    /// copper layer — the via barrel shorts all layers, so its copper
    /// blocks later nets' clearance disks on every layer. No clearance
    /// halo: separation to the via is enforced at search time like any
    /// other copper.
    pub fn stamp_via(&mut self, p: GridPoint, net: u32, copper: i32) {
        for layer in 0..self.layer_count {
            self.stamp_cell_copper(layer, p.col, p.row, net, copper);
        }
    }

    /// Inverse of `stamp_via`: free this net's via-barrel copper (cells equal
    /// to `Trace(net)`) on every layer. Via copper is stamped as `Trace(net)`
    /// (see `stamp_via`), so the same free-if-`Trace(net)` rule applies.
    pub fn unstamp_via(&mut self, p: GridPoint, net: u32, copper: i32) {
        for layer in 0..self.layer_count {
            self.unstamp_cell_copper(layer, p.col, p.row, net, copper);
        }
    }

    /// Allocate a same-shape `CostMap` for negotiated-congestion routing.
    /// Identical layer/col/row dims as this grid; all biases start at 0.
    pub fn new_cost_map(&self) -> CostMap {
        CostMap {
            cols: self.cols,
            rows: self.rows,
            layer_count: self.layer_count,
            extra: vec![0; (self.cols * self.rows * i32::from(self.layer_count)) as usize],
        }
    }

    /// Stamp the bare copper of a single trace/via cell: the centre cell
    /// plus a Euclidean disk of radius `copper` cells become `Trace(net)`.
    /// Only `Free` cells are overwritten — pads and foreign copper are
    /// never clobbered. This represents the feature's own copper
    /// half-width; all edge-to-edge clearance is enforced at search time.
    fn stamp_cell_copper(&mut self, layer: u8, c: i32, r: i32, net: u32, copper: i32) {
        let copper = copper.max(0);
        let r2 = copper * copper;
        for dr in -copper..=copper {
            for dc in -copper..=copper {
                if dc * dc + dr * dr > r2 {
                    continue;
                }
                let gp = GridPoint {
                    layer,
                    col: c + dc,
                    row: r + dr,
                };
                if matches!(self.get(gp), Cell::Free) {
                    self.set(gp, Cell::Trace(net));
                }
            }
        }
    }

    /// Inverse of `stamp_cell_copper`: over the same copper disk, set to
    /// `Free` only cells currently equal to `Trace(net)`. Mirror of the
    /// free-only stamp, so unstamping a net leaves every other net's copper
    /// and all pads exactly as they were.
    fn unstamp_cell_copper(&mut self, layer: u8, c: i32, r: i32, net: u32, copper: i32) {
        let copper = copper.max(0);
        let r2 = copper * copper;
        for dr in -copper..=copper {
            for dc in -copper..=copper {
                if dc * dc + dr * dr > r2 {
                    continue;
                }
                let gp = GridPoint {
                    layer,
                    col: c + dc,
                    row: r + dr,
                };
                if matches!(self.get(gp), Cell::Trace(n) if n == net) {
                    self.set(gp, Cell::Free);
                }
            }
        }
    }
}

/// Ray-cast point-in-polygon test. The polygon is treated as a
/// closed loop (last → first edge implicit). Stable in degenerate
/// cases (point exactly on an edge): the boundary may resolve either
/// way, which is fine for keepout rasterisation — a one-cell error
/// at the boundary is well within router resolution.
fn point_in_polygon(poly: &[pcb_core::Point], x: f64, y: f64) -> bool {
    let mut inside = false;
    let n = poly.len();
    if n < 3 {
        return false;
    }
    let mut j = n - 1;
    for i in 0..n {
        let pix = poly[i].x.to_mm();
        let piy = poly[i].y.to_mm();
        let pjx = poly[j].x.to_mm();
        let pjy = poly[j].y.to_mm();
        if (piy > y) != (pjy > y) {
            let t = (pjy - piy).abs();
            if t > 1e-12 {
                let x_intersect = pix + (y - piy) * (pjx - pix) / (pjy - piy);
                if x < x_intersect {
                    inside = !inside;
                }
            }
        }
        j = i;
    }
    inside
}

/// Use `Keepout` parameter so the symbol is alive when callers only
/// reach the impl via `Grid::stamp_keepouts`. Pure type-system hint —
/// never invoked.
#[allow(dead_code)]
fn _keepout_type_anchor(_k: &Keepout) {}

/// Integer Bresenham line from (c0,r0) to (c1,r1) inclusive on both
/// endpoints. Used by `line_of_sight`, `cost_along`, and `stamp_trace`
/// so visibility checks, congestion bias, and obstacle stamping all
/// agree on which cells a straight any-angle segment touches.
fn bresenham(c0: i32, r0: i32, c1: i32, r1: i32) -> Vec<(i32, i32)> {
    let dc = (c1 - c0).abs();
    let dr = (r1 - r0).abs();
    let sc: i32 = if c0 < c1 { 1 } else { -1 };
    let sr: i32 = if r0 < r1 { 1 } else { -1 };
    let mut err = dc - dr;
    let mut c = c0;
    let mut r = r0;
    let mut out = Vec::with_capacity((dc.max(dr) + 1) as usize);
    loop {
        out.push((c, r));
        if c == c1 && r == r1 {
            break;
        }
        let e2 = 2 * err;
        if e2 > -dr {
            err -= dr;
            c += sc;
        }
        if e2 < dc {
            err += dc;
            r += sr;
        }
    }
    out
}
