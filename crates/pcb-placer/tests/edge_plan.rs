//! Edge planning: from a deliberately swapped start, the planner must
//! send each edge-mounted header to the edge its wiring is on.

use pcb_core::{Board, CopperLayer, EdgeSide, Footprint, Id, Length, Pad, Point, Rect};
use pcb_placer::{place, plan_edge_sides, MarginMap, PlaceOptions};

fn pad(num: &str, off_x: f64, off_y: f64, net: Option<&str>) -> Pad {
    Pad {
        number: num.into(),
        name: String::new(),
        offset: Point::new(Length::from_mm(off_x), Length::from_mm(off_y)),
        size: (Length::from_mm(1.0), Length::from_mm(1.0)),
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

/// 80 × 50 mm board with two FIXED 4-pad parts, one hugging the left edge
/// and one the right, and two movable edge-mounted 1×4 headers wired to
/// them. The headers start on the WRONG edges (JL, wired to the left-hand
/// part, sits on the right edge and vice versa), so a planner that just
/// snapped to the nearest edge would keep them both wrong.
fn build_board() -> Board {
    let mut board = Board::new();
    board.outline = Some(Rect::from_corners(
        Point::new(Length::from_mm(0.0), Length::from_mm(0.0)),
        Point::new(Length::from_mm(80.0), Length::from_mm(50.0)),
    ));

    let bank = |reference: &str, x: f64, prefix: &str| -> Footprint {
        let pads = (0..4)
            .map(|i| {
                pad(
                    &format!("{}", i + 1),
                    0.0,
                    -3.0 + f64::from(i) * 2.0,
                    Some(&format!("{prefix}{i}")),
                )
            })
            .collect();
        footprint(reference, x, 25.0, pads)
    };
    board.add_footprint(bank("U1", 12.0, "L"));
    board.add_footprint(bank("U2", 68.0, "R"));

    // Headers, 1×4 at 2.54 mm pitch, long axis along x at rotation 0 so
    // the planner has to rotate them to sit along a vertical edge.
    let header = |reference: &str, x: f64, prefix: &str| -> Footprint {
        let pads = (0..4)
            .map(|i| {
                pad(
                    &format!("{}", i + 1),
                    -3.81 + f64::from(i) * 2.54,
                    0.0,
                    Some(&format!("{prefix}{i}")),
                )
            })
            .collect();
        let mut fp = footprint(reference, x, 25.0, pads);
        fp.edge_mounted = true;
        fp
    };
    // Swapped: JL (left-bank wiring) starts on the right edge.
    board.add_footprint(header("JL", 74.0, "L"));
    board.add_footprint(header("JR", 6.0, "R"));
    board
}

fn refs() -> Vec<String> {
    vec!["JL".to_string(), "JR".to_string()]
}

#[test]
fn planner_swaps_the_headers_onto_the_right_edges() {
    let mut board = build_board();
    let opts = PlaceOptions::default();
    let report = plan_edge_sides(&mut board, &refs(), &opts, &MarginMap::new()).expect("edge plan");

    assert!(report.skipped.is_empty(), "skipped: {:?}", report.skipped);
    assert_eq!(report.placed.len(), 2);
    let side_of = |r: &str| {
        report
            .placed
            .iter()
            .find(|p| p.reference == r)
            .map(|p| p.side)
            .unwrap()
    };
    assert_eq!(side_of("JL"), EdgeSide::Left, "plan: {:?}", report.placed);
    assert_eq!(side_of("JR"), EdgeSide::Right, "plan: {:?}", report.placed);
    // The plan is an improvement on the swapped start, and it is applied
    // to the board (not just reported).
    assert!(report.final_cost < report.initial_cost);
    for p in &report.placed {
        let fp = board
            .footprints_in_order()
            .find(|f| f.reference == p.reference)
            .unwrap();
        assert_eq!(fp.position, p.position);
        assert!(
            board.edge_mount_violation(fp).is_none(),
            "{} must touch the outline: {:?}",
            p.reference,
            board.edge_mount_violation(fp)
        );
    }
}

#[test]
fn edge_plan_is_deterministic() {
    let opts = PlaceOptions::default();
    let run = || {
        let mut board = build_board();
        let r = plan_edge_sides(&mut board, &refs(), &opts, &MarginMap::new()).expect("edge plan");
        r.placed
            .iter()
            .map(|p| {
                (
                    p.reference.clone(),
                    p.side,
                    p.position.x.0,
                    p.position.y.0,
                    p.rotation,
                )
            })
            .collect::<Vec<_>>()
    };
    assert_eq!(run(), run());
}

#[test]
fn place_runs_the_edge_planner_before_the_global_stage() {
    // Same board through the full placer: the pass runs inside `place()`,
    // so the headers still end up on the correct edges (SA only refines
    // the along-edge position afterwards).
    let mut board = build_board();
    let opts = PlaceOptions {
        seed: 9,
        ..PlaceOptions::default()
    };
    place(&mut board, &refs(), &opts, &MarginMap::new()).expect("place");
    let centre_x = |r: &str| {
        let fp = board
            .footprints_in_order()
            .find(|f| f.reference == r)
            .unwrap();
        let b = fp.bounds().unwrap();
        f64::midpoint(b.min.x.to_mm(), b.max.x.to_mm())
    };
    assert!(
        centre_x("JL") < 40.0,
        "JL should be on the left half, x={:.1}",
        centre_x("JL")
    );
    assert!(
        centre_x("JR") > 40.0,
        "JR should be on the right half, x={:.1}",
        centre_x("JR")
    );
}

#[test]
fn non_edge_mounted_refs_are_reported_not_planned() {
    let mut board = build_board();
    let opts = PlaceOptions::default();
    let report = plan_edge_sides(
        &mut board,
        &["U1".to_string(), "NOPE".to_string()],
        &opts,
        &MarginMap::new(),
    )
    .expect("edge plan");
    assert!(report.placed.is_empty());
    assert_eq!(report.skipped.len(), 2);
    assert!(report
        .skipped
        .iter()
        .any(|s| s.starts_with("U1: not edge-mounted")));
    assert!(report
        .skipped
        .iter()
        .any(|s| s.starts_with("NOPE: not on the board")));
}
