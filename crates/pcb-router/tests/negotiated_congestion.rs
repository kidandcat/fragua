//! PathFinder-style negotiated congestion (`route negotiate=true`).
//!
//! The board below is the smallest honest statement of the problem the
//! negotiation exists for: **the order nets are routed in decides who gets
//! the good corridor, and the greedy driver has no way to reconsider.**
//!
//! ```text
//!   y=29.4 ┌─────────────────────────────────────────────┐
//!          │            the long way round               │
//!   y=24   │- - - - - - - -▓▓- - - - - - - - - - - - - - │
//!          │               ▓▓                            │
//!   y=19   │  CHEAP(10,19) ▓▓            CHEAP(50,19)    │
//!   y=15.4 │               ▓▓                            │
//!          │               ]]   <- gap G, one trace wide │
//!   y=14.6 │               ▓▓                            │
//!   y=5    │  EXPENSIVE    ▓▓            EXPENSIVE       │
//!   y=0    └───────────────▓▓────────────────────────────┘
//!                        x=29..31
//! ```
//!
//! Both nets' shortest path goes through **G**, which fits exactly one
//! trace (vias are priced out of the picture, so there is no sneaking
//! underneath on the other layer). Their alternatives are worth very
//! different amounts:
//!
//! - `CHEAP` sits close to the top opening — going round costs it ~2 mm.
//! - `EXPENSIVE` sits at the bottom — going round costs it ~12 mm.
//!
//! Route `CHEAP` first and it takes G (it is its shortest path too), and
//! `EXPENSIVE` pays the 12 mm detour forever: nothing failed, so the classic
//! driver's rip-up-and-reroute never even reconsiders. Negotiation gets the
//! assignment right from EITHER order, because both nets take G in the first
//! iteration, the sharing makes G expensive, and the net that moves is the
//! one whose alternative is cheap.

use pcb_core::{Board, CopperLayer, Footprint, Id, Keepout, Length, Pad, Point, Rect};
use pcb_router::{route, Outcome, RouteOptions};

/// x of the divider's centre — the cut every net has to cross.
const CUT_X: f64 = 30.0;
/// Crossings below this y went through the narrow gap; above it, round the
/// top. (The gap is at y≈15, the top opening above y=24.)
const GAP_MAX_Y: f64 = 22.0;

fn pad(num: &str, net: &str) -> Pad {
    Pad {
        number: num.into(),
        name: String::new(),
        offset: Point::new(Length::from_mm(0.0), Length::from_mm(0.0)),
        size: (Length::from_mm(0.8), Length::from_mm(0.8)),
        layer: CopperLayer::Top,
        net: Some(net.into()),
        drill: None,
    }
}

fn part(reference: &str, x_mm: f64, y_mm: f64, net: &str) -> Footprint {
    Footprint {
        id: Id::new(),
        reference: reference.into(),
        value: String::new(),
        library: "test".into(),
        position: Point::new(Length::from_mm(x_mm), Length::from_mm(y_mm)),
        rotation: 0.0,
        layer: CopperLayer::Top,
        pads: vec![pad("1", net)],
        key: String::new(),
        description: String::new(),
        edge_mounted: false,
        edge_side: None,
        silk: Vec::new(),
    }
}

/// Rectangular keepout on every layer — a wall the router cannot go
/// through, under, or around.
fn wall(board: &mut Board, x0: f64, y0: f64, x1: f64, y1: f64, label: &str) {
    board.add_keepout(Keepout {
        id: Id::new(),
        polygon: vec![
            Point::new(Length::from_mm(x0), Length::from_mm(y0)),
            Point::new(Length::from_mm(x1), Length::from_mm(y0)),
            Point::new(Length::from_mm(x1), Length::from_mm(y1)),
            Point::new(Length::from_mm(x0), Length::from_mm(y1)),
        ],
        layers: vec![],
        nets_allowed: vec![],
        label: label.into(),
    });
}

fn congested_board() -> Board {
    let mut board = Board::new();
    board.outline = Some(Rect::from_corners(
        Point::new(Length::from_mm(0.0), Length::from_mm(0.0)),
        Point::new(Length::from_mm(60.0), Length::from_mm(30.0)),
    ));
    // Divider with one trace-wide gap at y 14.6..15.4; open above y = 24.
    wall(&mut board, 29.0, 0.0, 31.0, 14.6, "divider_lower");
    wall(&mut board, 29.0, 15.4, 31.0, 24.0, "divider_upper");

    board.add_footprint(part("E1", 10.0, 5.0, "EXPENSIVE"));
    board.add_footprint(part("E2", 50.0, 5.0, "EXPENSIVE"));
    board.add_footprint(part("C1", 10.0, 19.0, "CHEAP"));
    board.add_footprint(part("C2", 50.0, 19.0, "CHEAP"));
    board
}

fn opts(order: &[&str], negotiate: bool) -> RouteOptions {
    RouteOptions {
        cell: Length::from_mm(0.25),
        trace_width: Length::from_mm(0.25),
        clearance: Length::from_mm(0.4),
        max_seconds: Some(30.0),
        // Vias are made prohibitively expensive on purpose: without this the
        // gap is not a capacity-1 resource at all — a net simply dives to the
        // bottom layer, crosses underneath the other trace, and comes back
        // up, which costs two vias and no negotiation.
        via_cost: 200,
        negotiate,
        initial_net_order: Some(order.iter().map(|s| (*s).to_string()).collect()),
        // Keep the smoothing pass out of the way: this test is about which
        // corridor each net gets, not about the shape of the copper.
        organic: false,
        ..RouteOptions::default()
    }
}

fn failed(report: &pcb_router::RouteReport) -> Vec<&str> {
    report
        .per_net
        .iter()
        .filter_map(|(n, o)| matches!(o, Outcome::Failed { .. }).then_some(n.as_str()))
        .collect()
}

/// True when `net` crosses the divider through the narrow gap rather than
/// round the top.
fn takes_the_gap(board: &Board, net: &str) -> bool {
    board.traces.iter().filter(|t| t.net == net).any(|t| {
        let (x0, y0) = (t.start.x.to_mm(), t.start.y.to_mm());
        let (x1, y1) = (t.end.x.to_mm(), t.end.y.to_mm());
        if (x0 - CUT_X).signum() == (x1 - CUT_X).signum() {
            return false;
        }
        // Linear interpolation of the crossing height.
        let f = (CUT_X - x0) / (x1 - x0);
        y0 + f * (y1 - y0) < GAP_MAX_Y
    })
}

#[test]
fn negotiation_gives_the_corridor_to_the_net_that_needs_it() {
    for order in [["CHEAP", "EXPENSIVE"], ["EXPENSIVE", "CHEAP"]] {
        let mut board = congested_board();
        let report = route(&mut board, &opts(&order, true));
        assert!(
            failed(&report).is_empty(),
            "negotiated route failed with order {order:?}: {:?}",
            failed(&report)
        );
        assert!(
            takes_the_gap(&board, "EXPENSIVE"),
            "order {order:?}: EXPENSIVE (12 mm detour) did not get the gap"
        );
        assert!(
            !takes_the_gap(&board, "CHEAP"),
            "order {order:?}: CHEAP (2 mm detour) kept the gap it should have yielded"
        );
    }
}

#[test]
fn the_classic_driver_lets_the_routing_order_decide() {
    // Same board, classic rip-up-and-reroute. Nothing FAILS here — the loser
    // just takes a long detour — so the reorder heuristic never fires and
    // whoever routed first keeps the gap. This is the behaviour negotiation
    // replaces; if it ever stops holding, the board above stopped
    // exercising the problem.
    let mut board = congested_board();
    let report = route(&mut board, &opts(&["CHEAP", "EXPENSIVE"], false));
    assert!(
        failed(&report).is_empty(),
        "classic route failed unexpectedly: {:?}",
        failed(&report)
    );
    assert!(
        takes_the_gap(&board, "CHEAP") && !takes_the_gap(&board, "EXPENSIVE"),
        "expected the greedy order to hand the gap to CHEAP (cheap={}, expensive={})",
        takes_the_gap(&board, "CHEAP"),
        takes_the_gap(&board, "EXPENSIVE"),
    );
}

/// PathFinder is deterministic only if everything feeding it is: the net
/// order, the iteration order over routes, the conflict scan. A `HashMap`
/// iteration order leaking into any of those would show up here as two
/// different boards from identical input.
#[test]
fn routing_is_reproducible() {
    let geometry = |negotiate: bool| -> Vec<String> {
        let mut board = congested_board();
        // A few more nets so the run has something to order and negotiate.
        board.add_footprint(part("X1", 8.0, 12.0, "EXTRA1"));
        board.add_footprint(part("X2", 52.0, 12.0, "EXTRA1"));
        board.add_footprint(part("Y1", 8.0, 27.0, "EXTRA2"));
        board.add_footprint(part("Y2", 52.0, 27.0, "EXTRA2"));
        let report = route(&mut board, &opts(&["CHEAP", "EXPENSIVE"], negotiate));
        let mut out: Vec<String> = board
            .traces
            .iter()
            .map(|t| {
                format!(
                    "{} L{} {:.4},{:.4} -> {:.4},{:.4} w{:.4}",
                    t.net,
                    t.layer.index,
                    t.start.x.to_mm(),
                    t.start.y.to_mm(),
                    t.end.x.to_mm(),
                    t.end.y.to_mm(),
                    t.width.to_mm(),
                )
            })
            .collect();
        for v in &board.vias {
            out.push(format!(
                "via {} {:.4},{:.4}",
                v.net,
                v.position.x.to_mm(),
                v.position.y.to_mm()
            ));
        }
        for (n, o) in &report.per_net {
            out.push(format!(
                "{n}={}",
                match o {
                    Outcome::Ok { .. } => "ok",
                    Outcome::Failed { .. } => "failed",
                }
            ));
        }
        out
    };

    for negotiate in [true, false] {
        let a = geometry(negotiate);
        let b = geometry(negotiate);
        assert_eq!(
            a, b,
            "routing is not reproducible with negotiate={negotiate}"
        );
        assert!(!a.is_empty(), "no copper at all with negotiate={negotiate}");
    }
}
