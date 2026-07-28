//! A module socketed on pin headers floats above the copper, so its
//! body may sit over low parts. The placer's body-to-body measures must
//! agree: elevated-over-flat is free, elevated-over-elevated and
//! flat-over-flat are not.

use pcb_core::{Board, CopperLayer, Footprint, Id, Length, Pad, PlacementMargin, Point, Rect};
use pcb_placer::{min_pairwise_gap, MarginMap};

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

fn footprint(reference: &str, x_mm: f64, y_mm: f64) -> Footprint {
    Footprint {
        id: Id::new(),
        reference: reference.into(),
        value: String::new(),
        library: "demo".into(),
        position: Point::new(Length::from_mm(x_mm), Length::from_mm(y_mm)),
        rotation: 0.0,
        layer: CopperLayer::Top,
        pads: vec![pad("1", 0.0, 0.0, Some("S"))],
        key: String::new(),
        description: String::new(),
        edge_mounted: false,
        edge_side: None,
        silk: Vec::new(),
    }
}

fn square_margin(mm: f64, elevated: bool) -> PlacementMargin {
    PlacementMargin {
        top_mm: mm,
        right_mm: mm,
        bottom_mm: mm,
        left_mm: mm,
        elevated,
    }
}

/// A 60×60 board with a big "module" body at the centre and a small
/// "chip" 4 mm to its right — deep inside the module's 8 mm keep-out.
fn overlapping_pair(module_elevated: bool, chip_elevated: bool) -> (Board, MarginMap) {
    let mut board = Board::new();
    board.outline = Some(Rect::from_corners(
        Point::new(Length::from_mm(0.0), Length::from_mm(0.0)),
        Point::new(Length::from_mm(60.0), Length::from_mm(60.0)),
    ));
    let module = board.add_footprint(footprint("U1", 30.0, 30.0));
    let chip = board.add_footprint(footprint("R1", 34.0, 30.0));
    let mut margins = MarginMap::new();
    margins.insert(module, square_margin(8.0, module_elevated));
    margins.insert(chip, square_margin(1.0, chip_elevated));
    (board, margins)
}

#[test]
fn flat_bodies_overlapping_report_a_negative_pairwise_gap() {
    let (board, margins) = overlapping_pair(false, false);
    assert!(
        min_pairwise_gap(&board, &margins) < 0.0,
        "two bodies in the board plane must read as overlapping"
    );
}

#[test]
fn elevated_module_over_a_flat_part_leaves_the_pairwise_gap_clear() {
    let (board, margins) = overlapping_pair(true, false);
    assert_eq!(
        min_pairwise_gap(&board, &margins),
        f64::INFINITY,
        "a socketed module and the part under it never compete for the plane"
    );
}

#[test]
fn two_elevated_modules_still_collide() {
    let (board, margins) = overlapping_pair(true, true);
    assert!(
        min_pairwise_gap(&board, &margins) < 0.0,
        "two modules on headers sit at the same height — still a collision"
    );
}

#[test]
fn placement_apis_accept_a_flat_part_under_an_elevated_module() {
    // Same geometry, driven through `Board::first_body_overlapper` —
    // the check every placement API (place / move / rotate /
    // edge-place) funnels through.
    let (board, _) = overlapping_pair(true, false);
    let module_id = board
        .footprints_in_order()
        .find(|f| f.reference == "U1")
        .expect("module")
        .id;
    let chip = board
        .footprints_in_order()
        .find(|f| f.reference == "R1")
        .expect("chip")
        .clone();

    let elevated_module = |fp: &Footprint| {
        if fp.reference == "U1" {
            square_margin(8.0, true)
        } else {
            square_margin(1.0, false)
        }
    };
    assert!(
        board
            .first_body_overlapper(&chip, Some(chip.id), &elevated_module)
            .is_none(),
        "a flat part under a socketed module must not be rejected"
    );

    // …and the module itself, probed against the flat part.
    let module = board.footprints[&module_id].clone();
    assert!(
        board
            .first_body_overlapper(&module, Some(module_id), &elevated_module)
            .is_none(),
        "the socketed module must not be rejected for shadowing a flat part"
    );

    // Both elevated → rejected again.
    let both_elevated = |_: &Footprint| square_margin(8.0, true);
    assert_eq!(
        board
            .first_body_overlapper(&module, Some(module_id), &both_elevated)
            .as_deref(),
        Some("R1"),
        "two elevated bodies must still be rejected"
    );
}

#[test]
fn elevated_body_off_the_board_is_still_rejected() {
    let mut board = Board::new();
    board.outline = Some(Rect::from_corners(
        Point::new(Length::from_mm(0.0), Length::from_mm(0.0)),
        Point::new(Length::from_mm(20.0), Length::from_mm(20.0)),
    ));
    board.add_footprint(footprint("U1", 10.0, 10.0));
    let probe = board.footprints_in_order().next().expect("fp").clone();
    for elevated in [false, true] {
        assert!(
            board
                .body_outline_violation(&probe, square_margin(12.0, elevated))
                .is_some(),
            "elevated={elevated}: a body hanging past the cut is always rejected"
        );
    }
}
