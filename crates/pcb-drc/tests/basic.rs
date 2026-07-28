//! Smoke tests for the DRC checks.

use std::collections::HashMap;

use pcb_core::{
    Board, CopperLayer, Footprint, Id, Length, Pad, PlacementMargin, Point, Rect, Trace,
};
use pcb_drc::{run, DrcOptions, Severity, ViolationKind};

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

fn fp(reference: &str, x_mm: f64, y_mm: f64, pads: Vec<Pad>) -> Footprint {
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

#[test]
fn pad_pad_clearance_violation() {
    let mut board = Board::new();
    board.outline = Some(Rect::from_corners(
        Point::new(Length::from_mm(0.0), Length::from_mm(0.0)),
        Point::new(Length::from_mm(50.0), Length::from_mm(20.0)),
    ));
    // Two pads, 0.05 mm apart at the edges → way under 0.2 mm clearance.
    board.add_footprint(fp("R1", 10.0, 10.0, vec![pad("1", 0.0, 0.0, Some("A"))]));
    board.add_footprint(fp("R2", 11.05, 10.0, vec![pad("1", 0.0, 0.0, Some("B"))]));
    let report = run(&board, &DrcOptions::default());
    assert!(report
        .violations
        .iter()
        .any(|v| v.kind == ViolationKind::PadPadClearance));
}

#[test]
fn unconnected_pad_is_flagged() {
    let mut board = Board::new();
    board.outline = Some(Rect::from_corners(
        Point::new(Length::from_mm(0.0), Length::from_mm(0.0)),
        Point::new(Length::from_mm(50.0), Length::from_mm(20.0)),
    ));
    board.add_footprint(fp("R1", 10.0, 10.0, vec![pad("1", 0.0, 0.0, Some("VCC"))]));
    let report = run(&board, &DrcOptions::default());
    assert!(report
        .violations
        .iter()
        .any(|v| v.kind == ViolationKind::UnconnectedPad));
}

#[test]
fn trace_touching_pad_marks_pad_as_connected() {
    let mut board = Board::new();
    board.outline = Some(Rect::from_corners(
        Point::new(Length::from_mm(0.0), Length::from_mm(0.0)),
        Point::new(Length::from_mm(50.0), Length::from_mm(20.0)),
    ));
    board.add_footprint(fp("R1", 10.0, 10.0, vec![pad("1", 0.0, 0.0, Some("VCC"))]));
    board.add_trace(Trace {
        id: Id::new(),
        layer: CopperLayer::Top,
        start: Point::new(Length::from_mm(10.0), Length::from_mm(10.0)),
        end: Point::new(Length::from_mm(20.0), Length::from_mm(10.0)),
        width: Length::from_mm(0.25),
        net: "VCC".into(),
    });
    let report = run(&board, &DrcOptions::default());
    assert!(!report
        .violations
        .iter()
        .any(|v| v.kind == ViolationKind::UnconnectedPad));
}

#[test]
fn routing_inefficient_fires_when_actual_far_exceeds_hpwl() {
    let mut board = Board::new();
    board.outline = Some(Rect::from_corners(
        Point::new(Length::from_mm(0.0), Length::from_mm(0.0)),
        Point::new(Length::from_mm(50.0), Length::from_mm(50.0)),
    ));
    // Two pads on net "S" 10 mm apart on the X axis. HPWL = 10 mm.
    board.add_footprint(fp("R1", 5.0, 25.0, vec![pad("1", 0.0, 0.0, Some("S"))]));
    board.add_footprint(fp("R2", 15.0, 25.0, vec![pad("1", 0.0, 0.0, Some("S"))]));
    // Snake the trace so the actual length is ~30 mm — 3× HPWL, well
    // above the default 1.5× threshold.
    let seg = |x1, y1, x2, y2| Trace {
        id: Id::new(),
        layer: CopperLayer::Top,
        start: Point::new(Length::from_mm(x1), Length::from_mm(y1)),
        end: Point::new(Length::from_mm(x2), Length::from_mm(y2)),
        width: Length::from_mm(0.25),
        net: "S".into(),
    };
    board.add_trace(seg(5.0, 25.0, 5.0, 35.0));
    board.add_trace(seg(5.0, 35.0, 15.0, 35.0));
    board.add_trace(seg(15.0, 35.0, 15.0, 25.0));
    let report = run(&board, &DrcOptions::default());
    assert!(report
        .violations
        .iter()
        .any(|v| v.kind == ViolationKind::RoutingInefficient));
}

#[test]
fn routing_inefficient_silent_when_close_to_hpwl() {
    let mut board = Board::new();
    board.outline = Some(Rect::from_corners(
        Point::new(Length::from_mm(0.0), Length::from_mm(0.0)),
        Point::new(Length::from_mm(50.0), Length::from_mm(50.0)),
    ));
    board.add_footprint(fp("R1", 5.0, 25.0, vec![pad("1", 0.0, 0.0, Some("S"))]));
    board.add_footprint(fp("R2", 15.0, 25.0, vec![pad("1", 0.0, 0.0, Some("S"))]));
    // Direct trace; length ≈ HPWL.
    board.add_trace(Trace {
        id: Id::new(),
        layer: CopperLayer::Top,
        start: Point::new(Length::from_mm(5.0), Length::from_mm(25.0)),
        end: Point::new(Length::from_mm(15.0), Length::from_mm(25.0)),
        width: Length::from_mm(0.25),
        net: "S".into(),
    });
    let report = run(&board, &DrcOptions::default());
    assert!(!report
        .violations
        .iter()
        .any(|v| v.kind == ViolationKind::RoutingInefficient));
}

#[test]
fn body_off_board_is_error_even_for_edge_mounted() {
    let mut board = Board::new();
    board.outline = Some(Rect::from_corners(
        Point::new(Length::from_mm(0.0), Length::from_mm(0.0)),
        Point::new(Length::from_mm(20.0), Length::from_mm(20.0)),
    ));
    // Edge-mounted connector flush against the right edge; the pads
    // fit on the board, but the placement margin pushes the body 3 mm
    // past the right outline.
    let mut connector = fp("J1", 18.75, 10.0, vec![pad("1", 0.0, 0.0, Some("D+"))]);
    connector.key = "usb_c".into();
    connector.edge_mounted = true;
    board.add_footprint(connector);

    let mut margins = HashMap::new();
    margins.insert(
        "usb_c".to_string(),
        PlacementMargin {
            top_mm: 0.0,
            right_mm: 3.0,
            bottom_mm: 0.0,
            left_mm: 0.0,
            elevated: false,
        },
    );
    let opts = DrcOptions {
        placement_margins: margins,
        ..DrcOptions::default()
    };
    let report = run(&board, &opts);
    let off_board: Vec<_> = report
        .violations
        .iter()
        .filter(|v| v.kind == ViolationKind::BodyOffBoard)
        .collect();
    assert_eq!(
        off_board.len(),
        1,
        "expected exactly one BodyOffBoard violation, got: {:#?}",
        report.violations
    );
    assert_eq!(
        off_board[0].severity,
        Severity::Error,
        "BodyOffBoard must be a hard error, not a warning"
    );
}

#[test]
fn edge_clearance_violation() {
    let mut board = Board::new();
    board.outline = Some(Rect::from_corners(
        Point::new(Length::from_mm(0.0), Length::from_mm(0.0)),
        Point::new(Length::from_mm(20.0), Length::from_mm(20.0)),
    ));
    // Pad sitting flush on the left edge.
    board.add_footprint(fp("R1", 0.5, 10.0, vec![pad("1", 0.0, 0.0, None)]));
    let report = run(&board, &DrcOptions::default());
    assert!(report
        .violations
        .iter()
        .any(|v| v.kind == ViolationKind::EdgeClearance));
}

#[test]
fn keepout_visible_in_drc_report() {
    use pcb_core::Trace;
    let mut board = Board::new();
    board.outline = Some(Rect::from_corners(
        Point::new(Length::from_mm(0.0), Length::from_mm(0.0)),
        Point::new(Length::from_mm(40.0), Length::from_mm(20.0)),
    ));
    // Keepout in the centre.
    board.keepouts.push(pcb_core::Keepout {
        id: pcb_core::Id::new(),
        polygon: vec![
            Point::new(Length::from_mm(10.0), Length::from_mm(5.0)),
            Point::new(Length::from_mm(30.0), Length::from_mm(5.0)),
            Point::new(Length::from_mm(30.0), Length::from_mm(15.0)),
            Point::new(Length::from_mm(10.0), Length::from_mm(15.0)),
        ],
        layers: vec![],
        nets_allowed: vec![],
        label: "test".into(),
    });
    // A trace running right through the keepout.
    board.traces.push(Trace {
        id: pcb_core::Id::new(),
        layer: pcb_core::CopperLayer::Top,
        start: Point::new(Length::from_mm(5.0), Length::from_mm(10.0)),
        end: Point::new(Length::from_mm(35.0), Length::from_mm(10.0)),
        width: Length::from_mm(0.25),
        net: "FOO".into(),
    });
    let report = run(&board, &DrcOptions::default());
    let kp_violations: Vec<_> = report
        .violations
        .iter()
        .filter(|v| v.kind == ViolationKind::KeepoutViolation)
        .collect();
    assert!(
        !kp_violations.is_empty(),
        "expected at least one KeepoutViolation, got: {:#?}",
        report.violations,
    );
}

// ── Rule areas ────────────────────────────────────────────────────────
//
// The area is an ABSOLUTE clearance override at the point where the gap
// is measured: the same two pads are a violation outside it and legal
// inside it. Everything else about DRC stays put.

fn two_close_pads(x_mm: f64) -> Board {
    let mut board = Board::new();
    board.outline = Some(Rect::from_corners(
        Point::new(Length::from_mm(0.0), Length::from_mm(0.0)),
        Point::new(Length::from_mm(50.0), Length::from_mm(20.0)),
    ));
    // Pads are 1.0 mm wide, so their edges sit 0.15 mm apart — under the
    // 0.2 mm default, over a 0.13 mm area rule.
    board.add_footprint(fp("R1", x_mm, 10.0, vec![pad("1", 0.0, 0.0, Some("A"))]));
    board.add_footprint(fp(
        "R2",
        x_mm + 1.15,
        10.0,
        vec![pad("1", 0.0, 0.0, Some("B"))],
    ));
    board
}

fn clearance_errors(board: &Board) -> usize {
    run(board, &DrcOptions::default())
        .violations
        .iter()
        .filter(|v| v.kind == ViolationKind::PadPadClearance)
        .count()
}

#[test]
fn rule_area_relaxes_clearance_only_inside_it() {
    let board = two_close_pads(10.0);
    assert_eq!(clearance_errors(&board), 1, "0.15 mm < 0.2 mm default");

    // Area over the pads → legal.
    let mut with_area = board.clone();
    let mut area = pcb_core::RuleArea::new(
        "fine",
        Rect::from_corners(
            Point::new(Length::from_mm(5.0), Length::from_mm(5.0)),
            Point::new(Length::from_mm(15.0), Length::from_mm(15.0)),
        ),
    );
    area.clearance_mm = Some(0.13);
    with_area.set_rule_area(area.clone());
    assert_eq!(clearance_errors(&with_area), 0, "0.15 mm ≥ 0.13 mm in area");

    // Same area, pads moved out of it → violation again. Membership is
    // positional, not per-item.
    let mut elsewhere = two_close_pads(30.0);
    elsewhere.set_rule_area(area.clone());
    assert_eq!(clearance_errors(&elsewhere), 1);

    // An area can tighten as well as relax.
    let mut tight = board.clone();
    let mut hv = area;
    hv.name = "hv".into();
    hv.clearance_mm = Some(0.5);
    tight.set_rule_area(hv);
    assert_eq!(clearance_errors(&tight), 1);
}

#[test]
fn rule_area_below_fab_limit_warns_but_does_not_error() {
    let mut board = two_close_pads(10.0);
    let mut area = pcb_core::RuleArea::new(
        "fine",
        Rect::from_corners(
            Point::new(Length::from_mm(5.0), Length::from_mm(5.0)),
            Point::new(Length::from_mm(15.0), Length::from_mm(15.0)),
        ),
    );
    area.clearance_mm = Some(0.10); // below JLCPCB's 0.127 mm
    area.via_diameter_mm = Some(0.45); // exactly at the limit — no warning
    board.set_rule_area(area);
    board.fab_rules = pcb_core::FabRules::preset("jlcpcb-2l");

    let report = run(&board, &DrcOptions::default());
    let warns: Vec<&pcb_drc::Violation> = report
        .violations
        .iter()
        .filter(|v| v.kind == ViolationKind::RuleBelowFabLimit)
        .collect();
    assert_eq!(warns.len(), 1, "{:?}", report.violations);
    assert_eq!(warns[0].severity, Severity::Warning);
    assert!(
        warns[0].message.contains("clearance"),
        "{}",
        warns[0].message
    );
    // The board's own preset also drives the fab minimum checks even
    // without an explicitly adopted in-memory profile.
    assert!(report
        .violations
        .iter()
        .all(|v| v.kind != ViolationKind::PadPadClearance));
}

// ─── Elevated (header-socketed) bodies ────────────────────────────────
//
// A 0.96" OLED or an LTE modem sits on a 2.54 mm socket ~8 mm above the
// copper, so its body legitimately shadows the passives underneath. The
// board these tests model is the fecha gateway v3, which is built and
// working with its modem body over an MCU and four passives.

/// Two footprints 4 mm apart carrying `wide` / `narrow` margins that
/// make their bodies overlap by ~6 mm. `elevated` says which keys are
/// socketed on headers.
fn overlapping_bodies(elevated: &[&str]) -> (Board, DrcOptions) {
    let mut board = Board::new();
    board.outline = Some(Rect::from_corners(
        Point::new(Length::from_mm(0.0), Length::from_mm(0.0)),
        Point::new(Length::from_mm(60.0), Length::from_mm(60.0)),
    ));
    let mut module = fp("U1", 30.0, 30.0, vec![pad("1", 0.0, 0.0, Some("A"))]);
    module.key = "module".into();
    board.add_footprint(module);
    let mut chip = fp("R1", 34.0, 30.0, vec![pad("1", 0.0, 0.0, Some("B"))]);
    chip.key = "chip".into();
    board.add_footprint(chip);

    let margin = |w: f64, key: &str| PlacementMargin {
        top_mm: w,
        right_mm: w,
        bottom_mm: w,
        left_mm: w,
        elevated: elevated.contains(&key),
    };
    let mut margins = HashMap::new();
    margins.insert("module".to_string(), margin(8.0, "module"));
    margins.insert("chip".to_string(), margin(1.0, "chip"));
    let opts = DrcOptions {
        placement_margins: margins,
        ..DrcOptions::default()
    };
    (board, opts)
}

fn body_overlaps(board: &Board, opts: &DrcOptions) -> Vec<pcb_drc::Violation> {
    run(board, opts)
        .violations
        .into_iter()
        .filter(|v| v.kind == ViolationKind::BodyOverlap)
        .collect()
}

#[test]
fn body_overlap_between_two_flat_parts_is_an_error() {
    let (board, opts) = overlapping_bodies(&[]);
    let v = body_overlaps(&board, &opts);
    assert_eq!(v.len(), 1, "expected one BodyOverlap, got {v:#?}");
    assert_eq!(
        v[0].severity,
        Severity::Error,
        "two bodies in the board plane physically collide"
    );
}

#[test]
fn elevated_body_over_a_flat_body_is_only_a_warning() {
    let (board, opts) = overlapping_bodies(&["module"]);
    let v = body_overlaps(&board, &opts);
    assert_eq!(v.len(), 1, "the overlap is still reported, got {v:#?}");
    assert_eq!(
        v[0].severity,
        Severity::Warning,
        "a socketed module clears the part underneath it: {:#?}",
        v[0]
    );
    assert!(
        v[0].message.contains("socketed"),
        "the message should explain why it is legal: {}",
        v[0].message
    );
}

#[test]
fn two_elevated_bodies_overlapping_is_still_an_error() {
    let (board, opts) = overlapping_bodies(&["module", "chip"]);
    let v = body_overlaps(&board, &opts);
    assert_eq!(v.len(), 1, "expected one BodyOverlap, got {v:#?}");
    assert_eq!(
        v[0].severity,
        Severity::Error,
        "two modules on headers sit at the same height — real collision"
    );
}

#[test]
fn body_off_board_is_unaffected_by_elevated() {
    // Same geometry as `body_off_board_is_error_even_for_edge_mounted`,
    // with the part socketed on headers: floating above the copper does
    // not conjure board area under the overhang.
    for elevated in [false, true] {
        let mut board = Board::new();
        board.outline = Some(Rect::from_corners(
            Point::new(Length::from_mm(0.0), Length::from_mm(0.0)),
            Point::new(Length::from_mm(20.0), Length::from_mm(20.0)),
        ));
        let mut module = fp("U1", 10.0, 10.0, vec![pad("1", 0.0, 0.0, Some("D+"))]);
        module.key = "oled".into();
        board.add_footprint(module);

        let mut margins = HashMap::new();
        margins.insert(
            "oled".to_string(),
            PlacementMargin {
                top_mm: 0.0,
                right_mm: 12.0,
                bottom_mm: 0.0,
                left_mm: 0.0,
                elevated,
            },
        );
        let opts = DrcOptions {
            placement_margins: margins,
            ..DrcOptions::default()
        };
        let off_board: Vec<_> = run(&board, &opts)
            .violations
            .into_iter()
            .filter(|v| v.kind == ViolationKind::BodyOffBoard)
            .collect();
        assert_eq!(
            off_board.len(),
            1,
            "elevated={elevated}: body hanging past the cut must still be flagged"
        );
        assert_eq!(
            off_board[0].severity,
            Severity::Error,
            "elevated={elevated}: BodyOffBoard stays a hard error"
        );
    }
}
