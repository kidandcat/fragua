//! Smoke tests for the topological engine: it must route simple boards
//! completely, only emit clearance-clean copper, and stay deterministic.

use pcb_core::{Board, CopperLayer, Footprint, Id, Length, Pad, Point, Rect};
use pcb_router::{route, RouteEngine, RouteOptions};

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

fn crossing_board() -> Board {
    // Two connections that CROSS: A-A' and B-B' on swapped diagonals.
    // Single-layer routing cannot solve both without a via — exercises
    // the multilayer homotopy search.
    let mut board = Board::new();
    board.outline = Some(Rect::from_corners(
        Point::new(Length::from_mm(0.0), Length::from_mm(0.0)),
        Point::new(Length::from_mm(30.0), Length::from_mm(30.0)),
    ));
    board.add_footprint(footprint(
        "A1",
        5.0,
        5.0,
        vec![pad("1", 0.0, 0.0, Some("A"))],
    ));
    board.add_footprint(footprint(
        "A2",
        25.0,
        25.0,
        vec![pad("1", 0.0, 0.0, Some("A"))],
    ));
    board.add_footprint(footprint(
        "B1",
        25.0,
        5.0,
        vec![pad("1", 0.0, 0.0, Some("B"))],
    ));
    board.add_footprint(footprint(
        "B2",
        5.0,
        25.0,
        vec![pad("1", 0.0, 0.0, Some("B"))],
    ));
    // A blocker wall in the middle forces real detours.
    board.add_footprint(footprint(
        "W1",
        15.0,
        15.0,
        vec![pad("1", 0.0, 0.0, Some("W")), pad("2", 0.0, 3.0, Some("W"))],
    ));
    board
}

#[test]
fn topo_engine_routes_crossing_nets() {
    let mut board = crossing_board();
    let opts = RouteOptions {
        engine: RouteEngine::Topo,
        organic: false, // inspect the raw engine output
        ..RouteOptions::default()
    };
    let report = route(&mut board, &opts);
    let failed: Vec<&str> = report
        .per_net
        .iter()
        .filter(|(_, o)| matches!(o, pcb_router::Outcome::Failed { .. }))
        .map(|(n, _)| n.as_str())
        .collect();
    assert!(failed.is_empty(), "topo engine failed nets: {failed:?}");
    assert!(!board.traces.is_empty(), "no copper emitted");
    // Every trace endpoint stays inside the outline.
    let o = board.outline.unwrap();
    for t in &board.traces {
        for p in [t.start, t.end] {
            assert!(
                p.x.0 >= o.min.x.0
                    && p.x.0 <= o.max.x.0
                    && p.y.0 >= o.min.y.0
                    && p.y.0 <= o.max.y.0,
                "trace point left the outline"
            );
        }
    }
}

#[test]
fn topo_engine_is_deterministic() {
    let opts = RouteOptions {
        engine: RouteEngine::Topo,
        ..RouteOptions::default()
    };
    let run = |mut b: Board| {
        let r = route(&mut b, &opts);
        let mut sig: Vec<(i64, i64, i64, i64)> = b
            .traces
            .iter()
            .map(|t| (t.start.x.0, t.start.y.0, t.end.x.0, t.end.y.0))
            .collect();
        sig.sort_unstable();
        (r.total_length_mm, sig)
    };
    // Same INPUT geometry both times (fresh Ids are fine — nothing may
    // depend on them).
    let (l1, s1) = run(crossing_board());
    let (l2, s2) = run(crossing_board());
    assert_eq!(s1, s2, "copper differs between identical runs");
    assert!((l1 - l2).abs() < 1e-9);
}
