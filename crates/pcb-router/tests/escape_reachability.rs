//! The router must PROVE a pad unreachable instead of spending the budget
//! discovering it, and it must say so in a way an agent can act on.
//!
//! From the v9 stress pass: a search that cannot reach its target explores
//! the grid to its A* pop cap, and in the clean-board rip-up pass each such
//! failure then buys a cascade of up to four blockers ripped, rerouted and
//! restored, two levels deep. On the RP2040 board that was 44 s of a 180 s
//! budget spent on nets that were never going to route. Flooding the bare
//! copper settles the same question in microseconds — and, unlike a
//! timeout, it distinguishes "needs more seconds" from "needs a different
//! board".

use std::time::Instant;

use pcb_core::{Board, CopperLayer, Footprint, Id, Length, Pad, Point, Rect};
use pcb_router::{route, Outcome, RouteOptions, RouteReport};

fn pad(num: &str, off_x: f64, off_y: f64, w: f64, h: f64, net: Option<&str>) -> Pad {
    Pad {
        number: num.into(),
        name: String::new(),
        offset: Point::new(Length::from_mm(off_x), Length::from_mm(off_y)),
        size: (Length::from_mm(w), Length::from_mm(h)),
        layer: CopperLayer::Top,
        net: net.map(str::to_string),
        drill: None,
    }
}

/// A through-hole pad: copper on EVERY layer, which is what makes a wall a
/// wall. An SMD ring would leave the opposite layer wide open.
fn th_pad(num: &str, off_x: f64, off_y: f64, w: f64, h: f64, net: &str) -> Pad {
    Pad {
        drill: Some(Length::from_mm(0.3)),
        ..pad(num, off_x, off_y, w, h, Some(net))
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

fn opts(secs: f64) -> RouteOptions {
    RouteOptions {
        cell: Length::from_mm(0.20),
        trace_width: Length::from_mm(0.25),
        clearance: Length::from_mm(0.20),
        via_drill: Length::from_mm(0.30),
        via_diameter: Length::from_mm(0.60),
        max_seconds: Some(secs),
        organic: false,
        ..RouteOptions::default()
    }
}

/// Two 2-pad nets on an otherwise empty board. `SEALED` has one pad inside
/// a closed ring of grounded through-hole copper whose 0.35 mm gaps are
/// under the 0.53 mm a 0.25 mm trace needs here, so no path to it exists on
/// either layer at any budget. `OPEN` is the control: same distance, clear
/// board, must route.
fn sealed_pad_board() -> Board {
    let mut board = Board::new();
    board.outline = Some(Rect::from_corners(
        Point::new(Length::from_mm(0.0), Length::from_mm(0.0)),
        Point::new(Length::from_mm(30.0), Length::from_mm(30.0)),
    ));

    // The sealed net.
    board.add_footprint(footprint(
        "U1",
        14.0,
        15.0,
        vec![pad("1", 0.0, 0.0, 1.0, 1.0, Some("SEALED"))],
    ));
    board.add_footprint(footprint(
        "R1",
        25.0,
        15.0,
        vec![pad("1", 0.0, 0.0, 1.0, 1.0, Some("SEALED"))],
    ));

    // The ring: four grounded through-hole slabs, 0.35 mm off the pad on
    // every side and overlapping at the corners so the box is closed.
    board.add_footprint(footprint(
        "GND1",
        14.0,
        15.0,
        vec![
            th_pad("1", -1.45, 0.0, 0.6, 3.4, "GND"),
            th_pad("2", 1.45, 0.0, 0.6, 3.4, "GND"),
            th_pad("3", 0.0, -1.45, 3.4, 0.6, "GND"),
            th_pad("4", 0.0, 1.45, 3.4, 0.6, "GND"),
        ],
    ));

    // The control net, well clear of the ring.
    board.add_footprint(footprint(
        "U2",
        14.0,
        25.0,
        vec![pad("1", 0.0, 0.0, 1.0, 1.0, Some("OPEN"))],
    ));
    board.add_footprint(footprint(
        "R2",
        25.0,
        25.0,
        vec![pad("1", 0.0, 0.0, 1.0, 1.0, Some("OPEN"))],
    ));
    board
}

fn failed_nets(r: &RouteReport) -> Vec<String> {
    r.per_net
        .iter()
        .filter_map(|(n, o)| matches!(o, Outcome::Failed { .. }).then(|| n.clone()))
        .collect()
}

/// A pad the router gives up on must be NAMED as geometry. That is the
/// difference between "give it more seconds" and "change the board", and
/// it is the one thing a `max_seconds` number can never say.
#[test]
fn a_sealed_pad_is_reported_as_geometry_not_budget() {
    let mut board = sealed_pad_board();
    let report = route(&mut board, &opts(60.0));

    assert_eq!(
        report.entombed_pads.len(),
        1,
        "expected exactly the sealed pad to be named: {:?} (hints {:?})",
        report.entombed_pads,
        report.hints
    );
    assert!(
        report.entombed_pads[0].starts_with("U1.1"),
        "the wrong pad was named: {:?}",
        report.entombed_pads
    );
    assert_eq!(
        failed_nets(&report),
        vec!["SEALED".to_string()],
        "the control net must still route"
    );
    // The verdict leads the hints: an agent must not have to read past
    // per-net advice to learn the board is the problem.
    let first = report.hints.first().map(String::as_str).unwrap_or("");
    assert!(
        first.starts_with("entombed:"),
        "geometry verdict is not the first hint: {first:?}"
    );
    assert!(
        first.contains("Geometry, not budget"),
        "the hint does not say which kind of failure this is: {first:?}"
    );
}

/// The compute claim, measured: a provable geometry failure costs a flood,
/// not a search. The board here has one unroutable net and one trivial one,
/// so anything but a near-instant fixpoint means the driver is still paying
/// pop caps and rip-up cascades to rediscover the ring.
#[test]
fn a_provable_geometry_failure_does_not_burn_the_budget() {
    let mut board = sealed_pad_board();
    let started = Instant::now();
    let report = route(&mut board, &opts(60.0));
    let elapsed = started.elapsed().as_secs_f64();

    assert!(
        !report.budget_hit,
        "a provable geometry failure must reach the fixpoint, not the deadline \
         ({elapsed:.1}s of 60 s)"
    );
    assert!(
        elapsed < 10.0,
        "one sealed pad should be settled in a flood, not a search: {elapsed:.1}s"
    );
}

/// Determinism survives the new levers: the plan legalisation, the pocket
/// floods and the blame ranking are all order-stable, so two runs of the
/// same board produce the same copper — down to the trace geometry.
///
/// Run back-to-back on a board that CONVERGES: a budget-truncated result is
/// a statement about the machine's load, not about the router, and
/// comparing two of those proves nothing.
#[test]
fn reachability_driven_routing_is_deterministic() {
    let mut a = sealed_pad_board();
    let ra = route(&mut a, &opts(60.0));
    let mut b = sealed_pad_board();
    let rb = route(&mut b, &opts(60.0));
    assert!(
        !ra.budget_hit && !rb.budget_hit,
        "budget-truncated runs cannot test determinism"
    );

    assert_eq!(
        ra.board_trace_count, rb.board_trace_count,
        "trace count differs between identical runs"
    );
    assert_eq!(
        ra.board_via_count, rb.board_via_count,
        "via count differs between identical runs"
    );
    assert_eq!(
        failed_nets(&ra),
        failed_nets(&rb),
        "different nets failed between identical runs"
    );
    assert_eq!(
        ra.entombed_pads, rb.entombed_pads,
        "entombment verdict differs between identical runs"
    );
    let geom = |bd: &Board| -> Vec<(i64, i64, i64, i64, String)> {
        let mut v: Vec<(i64, i64, i64, i64, String)> = bd
            .traces
            .iter()
            .map(|t| (t.start.x.0, t.start.y.0, t.end.x.0, t.end.y.0, t.net.clone()))
            .collect();
        v.sort();
        v
    };
    assert_eq!(
        geom(&a),
        geom(&b),
        "trace geometry differs between identical runs"
    );
}
