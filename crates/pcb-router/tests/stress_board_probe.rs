//! Manual probe: route the RP2040 stress board (`stress/rp2040-minimal.fragua`)
//! in-process, on any stackup, with the script layer's route defaults and a
//! progress log stamped with wall-clock offsets. This is how the O8
//! (multi-layer regression) numbers in `stress/AGENT-HANDOFF.md` are taken —
//! it reproduces what the HTTP `route` verb does without a server, an
//! autosave, or a client timeout.
//!
//! `#[ignore]`d: it costs a full route budget, so CI never runs it.
//!
//! ```text
//! O8_LAYERS=4 O8_SECS=180 cargo test --release -p pcb-router \
//!     --test stress_board_probe -- --ignored --nocapture
//! ```
//!
//! `O8_LAYERS` (default 2) appends inner signal layers exactly like the
//! script's `layer add In1.Cu signal`; `O8_SECS` (default 90) is the budget.
//! Connectivity is reported as failed/routable — the same count the script's
//! `N/39 net(s) fully connected` headline is derived from.

use std::sync::Arc;
use std::time::Instant;

use pcb_core::{Board, Dielectric, LayerKind, LayerSpec, Length, Schematic};
use pcb_router::{route, Outcome, RouteOptions};

struct ProjectFile {
    board: Board,
    schematic: Schematic,
}

fn opts(sch: &Schematic, secs: f64) -> RouteOptions {
    let started = Instant::now();
    RouteOptions {
        cell: Length::from_mm(0.20),
        trace_width: Length::from_mm(0.25),
        clearance: Length::from_mm(0.20),
        via_cost: 8,
        via_drill: Length::from_mm(0.30),
        via_diameter: Length::from_mm(0.60),
        schematic: Some(Arc::new(sch.clone())),
        // `O8_ORGANIC=0` isolates the smoothing pass: the same route with
        // and without it is how an organic-only DRC regression is caught.
        organic: std::env::var("O8_ORGANIC").as_deref() != Ok("0"),
        organic_fillet_mm: 3.0,
        max_seconds: Some(secs),
        // `O8_NEG=1` turns on PathFinder negotiation, whose corridor
        // autopsy prints "blocked even when allowed to share" — the direct
        // measure of escape-slot progress (see AGENT-HANDOFF §6.5).
        negotiate: std::env::var("O8_NEG").is_ok(),
        on_progress: Some(Arc::new(move |m: &str| {
            println!("[{:7.2}s] {m}", started.elapsed().as_secs_f64());
        })),
        ..RouteOptions::default()
    }
}

#[test]
#[ignore]
fn stress_board_probe() {
    // `O8_BOARD` points the probe at any other project file (e.g. a
    // placement variant under `stress/`) without cloning the harness.
    let path = std::env::var("O8_BOARD").unwrap_or_else(|_| {
        concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/../../stress/rp2040-minimal.fragua"
        )
        .to_string()
    });
    let bytes = std::fs::read(&path).expect("stress board");
    let v: serde_json::Value = serde_json::from_slice(&bytes).expect("parse");
    let pf = ProjectFile {
        board: serde_json::from_value(v["board"].clone()).expect("board"),
        schematic: serde_json::from_value(v["schematic"].clone()).expect("schematic"),
    };
    let mut board = pf.board;
    board.clear_routing(); // the script recipe always runs `clear-route` first
    let layers: u8 = std::env::var("O8_LAYERS")
        .ok()
        .and_then(|v| v.parse().ok())
        .unwrap_or(2);
    let secs: f64 = std::env::var("O8_SECS")
        .ok()
        .and_then(|v| v.parse().ok())
        .unwrap_or(90.0);
    while board.stackup.layer_count() < layers {
        let n = board.stackup.layer_count();
        board.stackup.push_layer(
            LayerSpec {
                name: format!("In{n}.Cu"),
                kind: LayerKind::Signal,
                copper_thickness_mm: 0.035,
            },
            Dielectric {
                thickness_mm: 0.5,
                er: 4.5,
            },
        );
    }
    println!(
        "layers={} — routing with a {secs}s budget",
        board.stackup.layer_count()
    );
    let o = opts(&pf.schematic, secs);
    let t = Instant::now();
    let rep = route(&mut board, &o);
    let failed: Vec<&str> = rep
        .per_net
        .iter()
        .filter_map(|(n, out)| matches!(out, Outcome::Failed { .. }).then_some(n.as_str()))
        .collect();
    println!(
        "RESULT layers={} elapsed={:.1}s passes={} traces={} vias={} search_traces={} failed={}/{} budget_hit={}",
        board.stackup.layer_count(),
        t.elapsed().as_secs_f64(),
        rep.iterations,
        rep.board_trace_count,
        rep.board_via_count,
        rep.trace_count,
        failed.len(),
        rep.routable_net_count,
        rep.budget_hit
    );
    println!("FAILED: {}", failed.join(", "));
    // Width histogram: the fanout narrows a stub deliberately to fit a
    // fine-pitch escape, so a post-pass that loses those widths shows up
    // here as the narrow buckets collapsing into the class width.
    let mut widths: std::collections::BTreeMap<i64, usize> = std::collections::BTreeMap::new();
    for t in &board.traces {
        *widths.entry(t.width.0).or_default() += 1;
    }
    let hist: Vec<String> = widths
        .iter()
        .map(|(w, n)| format!("{:.3}mm×{n}", Length(*w).to_mm()))
        .collect();
    println!("TRACE WIDTHS {}", hist.join(" "));
    // The acceptance metric the script reports as "N/39 net(s) fully
    // connected": a net counts when DRC finds no unconnected pad on it.
    // Same source of truth (`pcb_drc::run`), so the probe and the server
    // cannot drift apart.
    let drc = pcb_drc::run(&board, &pcb_drc::DrcOptions::default());
    let mut bad_nets: std::collections::BTreeSet<String> = std::collections::BTreeSet::new();
    for v in &drc.violations {
        if v.kind == pcb_drc::ViolationKind::UnconnectedPad {
            if let Some(rp) = v.involved.first() {
                if let Some((r, p)) = rp.split_once('.') {
                    if let Some(fp) = board.footprints_in_order().find(|f| f.reference == r) {
                        if let Some(pad) = fp.pads.iter().find(|q| q.number == p) {
                            if let Some(n) = pad.net.as_deref() {
                                bad_nets.insert(n.to_string());
                            }
                        }
                    }
                }
            }
        }
    }
    let total_nets = pf.schematic.nets.len();
    let errors = drc
        .violations
        .iter()
        .filter(|v| v.severity == pcb_drc::Severity::Error)
        .count();
    let clearance_errors = drc
        .violations
        .iter()
        .filter(|v| {
            v.severity == pcb_drc::Severity::Error
                && matches!(
                    v.kind,
                    pcb_drc::ViolationKind::PadPadClearance
                        | pcb_drc::ViolationKind::TraceTraceClearance
                        | pcb_drc::ViolationKind::TracePadClearance
                        | pcb_drc::ViolationKind::EdgeClearance
                )
        })
        .count();
    for v in drc
        .violations
        .iter()
        .filter(|v| v.severity == pcb_drc::Severity::Error)
    {
        println!("DRC {:?} {:?} — {}", v.kind, v.involved, v.message);
    }
    println!(
        "CONNECTIVITY {}/{} fully connected — DRC {} error(s) ({} clearance), {} warning(s)",
        total_nets.saturating_sub(bad_nets.len()),
        total_nets,
        errors,
        clearance_errors,
        drc.violations
            .iter()
            .filter(|v| v.severity == pcb_drc::Severity::Warning)
            .count()
    );
    // Every hint: the `escape:`/`entombed:` verdicts classify the
    // survivors as geometry-bound or budget-bound, which is the number
    // the campaign is actually chasing.
    for h in &rep.hints {
        println!("HINT {h}");
    }
}
