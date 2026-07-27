//! Escape autopsy — a test-only diagnostic, compiled out of the library.
//!
//! Answers, for a given board, the question the negotiation autopsy only
//! states: *why* is a net "blocked even when allowed to share every foreign
//! trace"? It rebuilds exactly the grid a routing pass sees (same escape
//! plan, same net order, same rule context) and then floods, from every
//! terminal of the net, the set of cells the search could legally stand on.
//! Terminals in different flood components can never be joined; the frontier
//! of the isolated component names the copper that walls it in.
//!
//! It answers three questions the route report can only assert:
//!
//! * `region …` / `OUTSIDE REGION` — pads the routing grid does not cover.
//! * `DIAG_FP=U1` — the per-pad escape classification (cluster? surface
//!   escape? via-in-pad? how many dogbone sites?) for one footprint.
//! * `DIAG_NETS=A,B` — per terminal, the flood component it lives in and,
//!   when that component is a pocket, the copper walling it in.
//!
//! ```text
//! DIAG_FP=U1 cargo test --release -p pcb-router diag::escape_autopsy \
//!     -- --ignored --nocapture
//! ```

use std::collections::{BTreeMap, BTreeSet, HashMap, VecDeque};

use pcb_core::{Board, Length, Point, Schematic};

use crate::grid::{self, Cell, Grid, GridPoint};
use crate::router::{build_pass_grid, compute_region, NetPadInfo, RouteOptions, RuleCtx};

fn collect_nets(board: &Board) -> BTreeMap<String, Vec<NetPadInfo>> {
    let mut nets: BTreeMap<String, Vec<NetPadInfo>> = BTreeMap::new();
    for fp in board.footprints_in_order() {
        for pad in &fp.pads {
            if let Some(net) = &pad.net {
                nets.entry(net.clone()).or_default().push(NetPadInfo {
                    center: fp.pad_world_center(pad),
                    layer: pad.layer,
                    pad_ref: format!("{}.{}", fp.reference, pad.number),
                });
            }
        }
    }
    nets
}

/// Flood the cells `net_id` may legally occupy, starting at `start`.
/// Mirrors the A* move rules (8-neighbour planar moves gated by the trace
/// clearance disk, via moves gated by the via disk and the no-drilled-pad
/// rule) without any cost model — reachability only.
fn flood(
    grid: &Grid,
    start: GridPoint,
    net_id: u32,
    clr: &grid::ClearanceModel<'_>,
    via: &grid::ClearanceModel<'_>,
    limit: usize,
) -> BTreeSet<(u8, i32, i32)> {
    let mut seen: BTreeSet<(u8, i32, i32)> = BTreeSet::new();
    let mut q: VecDeque<GridPoint> = VecDeque::new();
    seen.insert((start.layer, start.col, start.row));
    q.push_back(start);
    while let Some(p) = q.pop_front() {
        if seen.len() > limit {
            break;
        }
        let mut cands: Vec<(GridPoint, bool)> = Vec::new();
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
                let mut ok = true;
                for l in 0..grid.layer_count {
                    if matches!(
                        grid.get(GridPoint {
                            layer: l,
                            col: n.col,
                            row: n.row
                        }),
                        Cell::DrilledPad(_)
                    ) {
                        ok = false;
                    }
                }
                if !ok || !grid.via_clearance_ok(n, net_id, via) {
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
    seen
}

/// What copper sits nearest to `p`, by kind and net, within `r` cells.
fn blame(grid: &Grid, p: GridPoint, net_id: u32, r: i32, order: &[String]) -> Vec<String> {
    let mut hits: Vec<(i32, String)> = Vec::new();
    for dc in -r..=r {
        for dr in -r..=r {
            let d2 = dc * dc + dr * dr;
            if d2 > r * r {
                continue;
            }
            let q = GridPoint {
                layer: p.layer,
                col: p.col + dc,
                row: p.row + dr,
            };
            if !grid.in_bounds(q) {
                continue;
            }
            let name = |i: u32| -> String {
                order
                    .get(i as usize)
                    .cloned()
                    .unwrap_or_else(|| format!("#{i}"))
            };
            let s = match grid.get(q) {
                Cell::DrilledPad(i) if i != net_id => format!("via/{}", name(i)),
                Cell::NetPad(i) if i != net_id => format!("pad/{}", name(i)),
                Cell::Trace(i) if i != net_id => format!("trace/{}", name(i)),
                Cell::Obstacle => "body".to_string(),
                _ => continue,
            };
            hits.push((d2, s));
        }
    }
    hits.sort();
    let mut out: Vec<String> = Vec::new();
    for (d2, s) in hits {
        if out.len() >= 6 {
            break;
        }
        let label = format!("{s}@{:.2}", f64::from(d2).sqrt());
        if !out.iter().any(|o| o.split('@').next() == Some(&s)) {
            out.push(label);
        }
    }
    out
}

#[test]
#[ignore]
fn escape_autopsy() {
    let path = concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/../../stress/rp2040-minimal.fragua"
    );
    let bytes = std::fs::read(path).expect("stress board");
    let v: serde_json::Value = serde_json::from_slice(&bytes).expect("parse");
    let mut board: Board = serde_json::from_value(v["board"].clone()).expect("board");
    let sch: Schematic = serde_json::from_value(v["schematic"].clone()).expect("schematic");
    board.clear_routing();
    let opts = RouteOptions {
        cell: Length::from_mm(0.20),
        trace_width: Length::from_mm(0.25),
        clearance: Length::from_mm(0.20),
        via_cost: 8,
        via_drill: Length::from_mm(0.30),
        via_diameter: Length::from_mm(0.60),
        schematic: Some(std::sync::Arc::new(sch)),
        organic: true,
        max_seconds: Some(180.0),
        ..RouteOptions::default()
    };
    let plan = crate::escape::plan_escapes(&board, &opts);
    let (fanout, stubs) = (plan.fanout, plan.stubs);
    println!(
        "escape: {} via(s), {} escaped pad(s), {} stranded",
        fanout.vias.len(),
        fanout.through_pads.len(),
        fanout.stranded_pads.len()
    );
    let nets = collect_nets(&board);
    let mut order: Vec<String> = nets.keys().cloned().collect();
    order.sort_by_key(|n| nets.get(n).map_or(0, Vec::len));
    if !fanout.through_pads.is_empty() {
        let fanned: BTreeSet<String> = nets
            .iter()
            .filter(|(_, pads)| {
                pads.iter()
                    .any(|p| fanout.through_pads.contains(&p.pad_ref))
            })
            .map(|(n, _)| n.clone())
            .collect();
        let (mut hard, easy): (Vec<String>, Vec<String>) =
            order.into_iter().partition(|n| fanned.contains(n));
        hard.extend(easy);
        order = hard;
    }
    let grid = build_pass_grid(&board, &opts, &order, &fanout, &stubs);
    let _region = compute_region(&board, &opts);
    let areas = board.rule_areas.clone();
    let rules = RuleCtx::new(&areas, opts.schematic.as_deref(), &opts, &order, &grid);
    let ids: HashMap<&str, u32> = order
        .iter()
        .enumerate()
        .map(|(i, n)| (n.as_str(), i as u32))
        .collect();

    // Pads the routing region does not even contain: unroutable by
    // construction, and the failure gets blamed on the far end.
    let region = compute_region(&board, &opts);
    println!(
        "region ({:.2},{:.2})..({:.2},{:.2})",
        region.min.x.to_mm(),
        region.min.y.to_mm(),
        region.max.x.to_mm(),
        region.max.y.to_mm()
    );
    for (net, pads) in &nets {
        for p in pads {
            let (x, y) = (p.center.x, p.center.y);
            if x < region.min.x || x > region.max.x || y < region.min.y || y > region.max.y {
                println!(
                    "OUTSIDE REGION: {} {} at ({:.2},{:.2})",
                    net,
                    p.pad_ref,
                    x.to_mm(),
                    y.to_mm()
                );
            }
        }
    }

    // Per-pad escape classification for one footprint: exactly the tests
    // `fanout::plan_footprint_inner` runs, printed.
    if let Ok(reference) = std::env::var("DIAG_FP") {
        let rects = crate::fanout::pad_rects(&board);
        let foreign: Vec<&crate::fanout::PadRect> = rects.iter().collect();
        let resolver = pcb_core::RuleResolver::new(
            &board.rule_areas,
            pcb_core::RuleDefaults {
                clearance: opts.clearance,
                trace_width: opts.trace_width,
                via_diameter: opts.via_diameter,
                via_drill: opts.via_drill,
            },
        )
        .with_schematic(opts.schematic.as_deref());
        let fab = board.fab_rules.as_ref();
        for fp in board
            .footprints_in_order()
            .filter(|f| f.reference == reference)
        {
            let (mut sx, mut sy, mut n) = (0.0, 0.0, 0.0);
            for pad in &fp.pads {
                let c = fp.pad_world_center(pad);
                sx += c.x.to_mm();
                sy += c.y.to_mm();
                n += 1.0;
            }
            let (fx, fy) = (sx / n, sy / n);
            println!("-- {reference} centroid ({fx:.2},{fy:.2})");
            for pad in &fp.pads {
                let Some(net) = pad.net.as_deref() else {
                    continue;
                };
                let c = fp.pad_world_center(pad);
                let (w, h) = fp.pad_world_size(pad);
                let (cx, cy) = (c.x.to_mm(), c.y.to_mm());
                let (hw, hh) = (w.to_mm() / 2.0, h.to_mm() / 2.0);
                let rules = crate::fanout::PadRules::at_pad(&resolver, net, c, pad.layer, fab);
                let via_r = rules.via_diameter / 2.0;
                let cluster = crate::fanout::in_fine_cluster(cx, cy, net, &foreign);
                let surf = crate::fanout::can_escape_surface(
                    cx,
                    cy,
                    hw,
                    hh,
                    net,
                    &foreign,
                    opts.trace_width.to_mm(),
                    &rules,
                    opts.cell.to_mm(),
                    board.outline,
                );
                let (dx, dy) =
                    crate::fanout::escape_axis(cx, cy, hw, hh, fx, fy, via_r, board.outline);
                let cands = crate::fanout::dogbone_via_candidates(
                    cx, cy, hw, hh, dx, dy, 0, net, via_r, &rules, &foreign, &board,
                );
                println!(
                    "   {}.{:<4} net={:<10} c=({cx:6.2},{cy:6.2}) half=({hw:.2},{hh:.2}) via_r={via_r:.2} \
                     cluster={cluster} surf_escape={surf} vip_fits={} inward=({dx},{dy}) cands={}",
                    fp.reference,
                    pad.number,
                    net,
                    via_r <= hw.min(hh) + 1e-9,
                    cands.len()
                );
            }
        }
    }

    let interest: Vec<String> = std::env::var("DIAG_NETS")
        .map(|s| s.split(',').map(str::to_string).collect())
        .unwrap_or_else(|_| {
            "HDRB1,SWCLK,SWDIO,RUN,+1V1,+3V3,CC2,HDRB4,USB_DM,USB_DP,VBUS,GPIO1,GPIO3,GPIO7"
                .split(',')
                .map(str::to_string)
                .collect()
        });

    for net in &interest {
        let Some(pads) = nets.get(net) else {
            println!("== {net}: not on the board");
            continue;
        };
        let id = ids[net.as_str()];
        let clr = rules.trace_model(net, opts.trace_width, opts.cell);
        let via = rules.via_model(net, opts.cell);
        println!("== {net} ({} pad(s))", pads.len());
        // Component id per terminal.
        let mut comps: Vec<BTreeSet<(u8, i32, i32)>> = Vec::new();
        let mut owner: Vec<usize> = Vec::new();
        for p in pads {
            let rp = fanout
                .via_positions
                .get(&p.pad_ref)
                .copied()
                .unwrap_or(p.center);
            let gp = grid.snap(rp, p.layer);
            if let Some(k) = comps
                .iter()
                .position(|c| c.contains(&(gp.layer, gp.col, gp.row)))
            {
                owner.push(k);
                continue;
            }
            let c = flood(&grid, gp, id, &clr, &via, 400_000);
            owner.push(comps.len());
            comps.push(c);
        }
        for (i, p) in pads.iter().enumerate() {
            let rp = fanout
                .via_positions
                .get(&p.pad_ref)
                .copied()
                .unwrap_or(p.center);
            let gp = grid.snap(rp, p.layer);
            let size = comps[owner[i]].len();
            let fan = if fanout.via_positions.contains_key(&p.pad_ref) {
                "fanout"
            } else {
                "pad   "
            };
            println!(
                "   {fan} {:<10} at ({:6.2},{:6.2}) comp#{} size={}",
                p.pad_ref,
                rp.x.to_mm(),
                rp.y.to_mm(),
                owner[i],
                size
            );
            if size <= 3 {
                // Degenerate: name what stops the very first step.
                println!(
                    "        cell={:?} in_bounds={}",
                    grid.get(gp),
                    grid.in_bounds(gp)
                );
                for (dc, dr) in [(1, 0), (-1, 0), (0, 1), (0, -1)] {
                    let q = GridPoint {
                        layer: gp.layer,
                        col: gp.col + dc,
                        row: gp.row + dr,
                    };
                    println!(
                        "        step({dc},{dr}) in={} cell={:?} walk={} clr={}",
                        grid.in_bounds(q),
                        grid.get(q),
                        grid::walkable(grid.get(q), id),
                        grid.clearance_ok(q, id, &clr)
                    );
                }
            }
            // For a small (entombed) component, name the wall.
            if size < 4000 {
                let mut wall: BTreeMap<String, usize> = BTreeMap::new();
                for &(l, c, r) in &comps[owner[i]] {
                    let p = GridPoint {
                        layer: l,
                        col: c,
                        row: r,
                    };
                    for b in blame(&grid, p, id, 4, &order) {
                        let key = b.split('@').next().unwrap_or("?").to_string();
                        *wall.entry(key).or_insert(0) += 1;
                    }
                }
                let mut v: Vec<(usize, String)> = wall.into_iter().map(|(k, n)| (n, k)).collect();
                v.sort_by(|a, b| b.0.cmp(&a.0));
                let top: Vec<String> = v.iter().take(8).map(|(n, k)| format!("{k}x{n}")).collect();
                println!("        wall: {}", top.join(" "));
                // Extent of the component in mm.
                let (mut x0, mut y0, mut x1, mut y1) = (f64::MAX, f64::MAX, f64::MIN, f64::MIN);
                for &(l, c, r) in &comps[owner[i]] {
                    let q = grid.unsnap(GridPoint {
                        layer: l,
                        col: c,
                        row: r,
                    });
                    x0 = x0.min(q.x.to_mm());
                    y0 = y0.min(q.y.to_mm());
                    x1 = x1.max(q.x.to_mm());
                    y1 = y1.max(q.y.to_mm());
                }
                println!("        extent: ({x0:.2},{y0:.2})..({x1:.2},{y1:.2})");
            }
            let _ = gp;
        }
    }
    let _ = Point::new(Length::from_mm(0.0), Length::from_mm(0.0));
}
