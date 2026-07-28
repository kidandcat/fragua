//! Bundle alignment: a header wired 1:1 to an IC bank must end up on the
//! bank's side of the IC AND in matching pin order (no crossings).

use pcb_core::{Board, CopperLayer, Footprint, Id, Length, Pad, Point, Rect};
use pcb_placer::{bundle_crossings, place, MarginMap, PlaceOptions};

fn pad(num: &str, off_x: f64, off_y: f64, net: Option<&str>) -> Pad {
    Pad {
        number: num.into(),
        name: String::new(),
        offset: Point::new(Length::from_mm(off_x), Length::from_mm(off_y)),
        size: (Length::from_mm(0.9), Length::from_mm(0.9)),
        layer: CopperLayer::Top,
        net: net.map(str::to_string),
        drill: None,
    }
}

fn footprint(reference: &str, x_mm: f64, y_mm: f64, rot: f32, pads: Vec<Pad>) -> Footprint {
    Footprint {
        id: Id::new(),
        reference: reference.into(),
        value: String::new(),
        library: "demo".into(),
        position: Point::new(Length::from_mm(x_mm), Length::from_mm(y_mm)),
        rotation: rot,
        layer: CopperLayer::Top,
        pads,
        key: String::new(),
        description: String::new(),
        edge_mounted: false,
        edge_side: None,
        silk: Vec::new(),
    }
}

/// Fixed IC in the middle of an 80 × 60 mm board with two banks of six
/// pads, one on each side, each wired 1:1 to a movable 1×6 header
/// (2.54 mm pitch). Both headers START on the WRONG side of the board and
/// in REVERSED pin order, so both the side choice and the ordering have
/// to be fixed by the placer.
fn build_board() -> Board {
    let mut board = Board::new();
    board.outline = Some(Rect::from_corners(
        Point::new(Length::from_mm(0.0), Length::from_mm(0.0)),
        Point::new(Length::from_mm(80.0), Length::from_mm(60.0)),
    ));

    let mut ic_pads = Vec::new();
    for i in 0..6 {
        let y = -3.75 + f64::from(i) * 1.5;
        ic_pads.push(pad(&format!("L{i}"), -5.0, y, Some(&format!("LB{i}"))));
        ic_pads.push(pad(&format!("R{i}"), 5.0, y, Some(&format!("RB{i}"))));
    }
    board.add_footprint(footprint("U1", 40.0, 30.0, 0.0, ic_pads));

    // Headers: pin i at y = -6.35 + i*2.54 (vertical 1×6, 2.54 pitch).
    // JL carries the LEFT bank but starts on the right of the board;
    // JR carries the RIGHT bank but starts on the left. Both are wired so
    // that pin i meets bank pad 5-i — i.e. fully reversed.
    let header = |reference: &str, x: f64, prefix: &str| -> Footprint {
        let pads = (0..6)
            .map(|i| {
                pad(
                    &format!("{}", i + 1),
                    0.0,
                    -6.35 + f64::from(i) * 2.54,
                    Some(&format!("{prefix}{}", 5 - i)),
                )
            })
            .collect();
        footprint(reference, x, 30.0, 0.0, pads)
    };
    board.add_footprint(header("JL", 70.0, "LB"));
    board.add_footprint(header("JR", 10.0, "RB"));
    board
}

fn centre_x(board: &Board, reference: &str) -> f64 {
    let fp = board
        .footprints_in_order()
        .find(|f| f.reference == reference)
        .unwrap();
    let b = fp.bounds().unwrap();
    f64::midpoint(b.min.x.to_mm(), b.max.x.to_mm())
}

#[test]
fn headers_land_on_their_own_side_in_matching_order() {
    let mut board = build_board();
    let start_crossings = bundle_crossings(&board);
    // Sanity: the deliberately reversed start really is criss-crossed —
    // 15 inversions per 6-wire bundle, two bundles.
    assert_eq!(start_crossings, 30, "start layout should be fully crossed");

    let opts = PlaceOptions {
        seed: 3,
        ..PlaceOptions::default()
    };
    let report = place(
        &mut board,
        &["JL".to_string(), "JR".to_string()],
        &opts,
        &MarginMap::new(),
    )
    .expect("place");

    let ic = centre_x(&board, "U1");
    assert!(
        centre_x(&board, "JL") < ic,
        "JL wires the LEFT bank, so it belongs left of U1 (JL x={:.1}, U1 x={ic:.1})",
        centre_x(&board, "JL")
    );
    assert!(
        centre_x(&board, "JR") > ic,
        "JR wires the RIGHT bank, so it belongs right of U1 (JR x={:.1}, U1 x={ic:.1})",
        centre_x(&board, "JR")
    );

    let end_crossings = bundle_crossings(&board);
    assert_eq!(end_crossings, report.final_crossings);
    assert!(
        end_crossings == 0 || end_crossings < start_crossings,
        "bundle crossings must improve: {start_crossings} → {end_crossings}"
    );
}

#[test]
fn crossing_counts_in_the_report_are_reproducible() {
    let opts = PlaceOptions {
        seed: 3,
        ..PlaceOptions::default()
    };
    let refs = ["JL".to_string(), "JR".to_string()];
    let mut a = build_board();
    let ra = place(&mut a, &refs, &opts, &MarginMap::new()).expect("place");
    let mut b = build_board();
    let rb = place(&mut b, &refs, &opts, &MarginMap::new()).expect("place");
    assert_eq!(ra.initial_crossings, rb.initial_crossings);
    assert_eq!(ra.final_crossings, rb.final_crossings);
    assert_eq!(ra.accepted, rb.accepted);
    assert!((ra.final_hpwl_mm - rb.final_hpwl_mm).abs() < 1e-9);
    assert!((ra.final_congestion - rb.final_congestion).abs() < 1e-9);
}

#[test]
fn crossing_counter_is_rotation_invariant() {
    // Counting inversions on the axis BETWEEN the two parts means the
    // number cannot depend on how the pair as a whole is oriented: rotate
    // the header 180° about the IC and the ribbon order is unchanged.
    let mut board = build_board();
    let before = bundle_crossings(&board);
    for fp in board.footprints.values_mut() {
        // Rotate the whole board 90° about the origin: x' = -y, y' = x.
        let x = fp.position.x.to_mm();
        let y = fp.position.y.to_mm();
        fp.position = Point::new(Length::from_mm(-y), Length::from_mm(x));
        fp.rotation = (fp.rotation + 90.0).rem_euclid(360.0);
    }
    assert_eq!(before, bundle_crossings(&board));
}
