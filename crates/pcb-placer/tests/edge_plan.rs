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

/// Which outline side a footprint is mounted on. A long part ending near a
/// corner can be within tolerance of TWO edges, so prefer the declaration
/// (`edge_side` names the LOCAL side against the cut) and fall back to the
/// nearest touching edge for parts that declare nothing.
fn touching_side(board: &Board, reference: &str) -> Option<EdgeSide> {
    let fp = board
        .footprints_in_order()
        .find(|f| f.reference == reference)?;
    if let Some(local) = fp.edge_side {
        return Some(local.world_side(fp.rotation));
    }
    let b = fp.bounds()?;
    let o = board.outline?;
    let tol = 500_000; // 0.5 mm in nm, same tolerance the edge check uses
    let d = [
        ((b.min.x.0 - o.min.x.0).abs(), EdgeSide::Left),
        ((o.max.x.0 - b.max.x.0).abs(), EdgeSide::Right),
        ((b.min.y.0 - o.min.y.0).abs(), EdgeSide::Bottom),
        ((o.max.y.0 - b.max.y.0).abs(), EdgeSide::Top),
    ];
    d.iter()
        .filter(|(dist, _)| *dist <= tol)
        .min_by_key(|(dist, _)| *dist)
        .map(|(_, side)| *side)
}

/// A 1×4 header, long axis along x at rotation 0, edge-mounted.
fn edge_header(reference: &str, x: f64, y: f64, prefix: &str) -> Footprint {
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
    let mut fp = footprint(reference, x, y, pads);
    fp.edge_mounted = true;
    fp
}

#[test]
fn blocked_along_seed_scans_the_rest_of_the_edge() {
    // 60 × 60 mm. FIXED bank U1 sits just inside the LEFT edge at mid
    // height, so the along-seed for its header is y ≈ 30 on that edge —
    // and a FIXED obstacle occupies exactly that stretch of the edge.
    // Only an along-scan can keep the header on the side its wiring is on.
    let mut board = Board::new();
    board.outline = Some(Rect::from_corners(
        Point::new(Length::from_mm(0.0), Length::from_mm(0.0)),
        Point::new(Length::from_mm(60.0), Length::from_mm(60.0)),
    ));
    let bank_pads = (0..4)
        .map(|i| {
            pad(
                &format!("{}", i + 1),
                0.0,
                -3.0 + f64::from(i) * 2.0,
                Some(&format!("L{i}")),
            )
        })
        .collect();
    board.add_footprint(footprint("U1", 12.0, 30.0, bank_pads));
    // Obstacle: 6 × 12 mm of FIXED copper hugging the left edge, y 24…36.
    board.add_footprint(footprint(
        "OBS",
        3.0,
        30.0,
        vec![pad("1", -2.5, -5.5, None), pad("2", 2.5, 5.5, None)],
    ));
    board.add_footprint(edge_header("JL", 30.0, 30.0, "L"));

    let opts = PlaceOptions::default();
    let report = plan_edge_sides(&mut board, &["JL".to_string()], &opts, &MarginMap::new())
        .expect("edge plan");

    assert_eq!(report.placed.len(), 1, "skipped: {:?}", report.skipped);
    let p = &report.placed[0];
    assert_eq!(
        p.side,
        EdgeSide::Left,
        "the seed is blocked, not the edge — JL still belongs left (plan: {p:?})"
    );
    assert!(
        (p.along_mm - 30.0).abs() > 5.0,
        "JL must have scanned away from the blocked seed, along={:.2}",
        p.along_mm
    );
    assert_eq!(touching_side(&board, "JL"), Some(EdgeSide::Left));
    let jl = board
        .footprints_in_order()
        .find(|f| f.reference == "JL")
        .unwrap();
    assert!(board.edge_mount_violation(jl).is_none());
    // And the scan honoured the hard clearance against the fixed obstacle.
    assert!(
        pcb_placer::min_pairwise_gap(&board, &MarginMap::new()) >= 1.0 - 1e-6,
        "the scanned position must still clear OBS"
    );
}

/// Campaign-shaped board: 36 × 30 mm, one FIXED QFN in the middle, four
/// edge-mounted headers each wired to one bank of it, and a scatter of
/// movable interior passives parked all along the outline (what a previous
/// layout leaves behind). The passives are about to be re-placed, so they
/// must not veto the headers' sides.
fn build_crowded_board() -> Board {
    let mut board = Board::new();
    board.outline = Some(Rect::from_corners(
        Point::new(Length::from_mm(0.0), Length::from_mm(0.0)),
        Point::new(Length::from_mm(36.0), Length::from_mm(30.0)),
    ));
    // FIXED U1: four banks of three pads, one per side of a 10 × 10 body.
    let mut ic_pads = Vec::new();
    for i in 0..3 {
        let off = -2.5 + f64::from(i) * 2.5;
        ic_pads.push(pad(&format!("L{i}"), -5.0, off, Some(&format!("LB{i}"))));
        ic_pads.push(pad(&format!("R{i}"), 5.0, off, Some(&format!("RB{i}"))));
        ic_pads.push(pad(&format!("B{i}"), off, -5.0, Some(&format!("BB{i}"))));
        ic_pads.push(pad(&format!("T{i}"), off, 5.0, Some(&format!("TB{i}"))));
    }
    ic_pads.push(pad("G", 0.0, 0.0, Some("GND")));
    board.add_footprint(footprint("U1", 18.0, 15.0, ic_pads));

    // Headers, 1×3, all four starting bunched near the board centre-right
    // — the nearest-edge fallback would send most of them to the wrong
    // side, so only real planning gets this right.
    let header = |reference: &str, x: f64, y: f64, prefix: &str| -> Footprint {
        let pads = (0..3)
            .map(|i| {
                pad(
                    &format!("{}", i + 1),
                    -2.54 + f64::from(i) * 2.54,
                    0.0,
                    Some(&format!("{prefix}{i}")),
                )
            })
            .collect();
        let mut fp = footprint(reference, x, y, pads);
        fp.edge_mounted = true;
        fp
    };
    board.add_footprint(header("J1", 30.0, 4.0, "LB"));
    board.add_footprint(header("J2", 30.0, 26.0, "RB"));
    board.add_footprint(header("J3", 6.0, 26.0, "BB"));
    board.add_footprint(header("J4", 26.0, 15.0, "TB"));

    // Movable interior passives parked hard against every edge.
    let spots = [
        (2.0, 6.0),
        (2.0, 15.0),
        (2.0, 24.0),
        (34.0, 6.0),
        (34.0, 15.0),
        (34.0, 24.0),
        (10.0, 2.0),
        (18.0, 2.0),
        (26.0, 2.0),
        (10.0, 28.0),
        (18.0, 28.0),
        (26.0, 28.0),
    ];
    for (i, (x, y)) in spots.iter().enumerate() {
        board.add_footprint(footprint(
            &format!("C{}", i + 1),
            *x,
            *y,
            vec![
                pad("1", -0.8, 0.0, Some("GND")),
                pad("2", 0.8, 0.0, Some("GND")),
            ],
        ));
    }
    board
}

#[test]
fn crowded_board_still_plans_every_edge_part() {
    let mut board = build_crowded_board();
    let mut movable: Vec<String> = vec![
        "J1".to_string(),
        "J2".to_string(),
        "J3".to_string(),
        "J4".to_string(),
    ];
    movable.extend((1..=12).map(|i| format!("C{i}")));

    let opts = PlaceOptions {
        seed: 4,
        ..PlaceOptions::default()
    };
    place(&mut board, &movable, &opts, &MarginMap::new()).expect("place");

    // Every edge part must be legally ON an edge — the campaign failure
    // was J4 parked mid-board, which the SA can never repair.
    for r in ["J1", "J2", "J3", "J4"] {
        let fp = board
            .footprints_in_order()
            .find(|f| f.reference == r)
            .unwrap();
        assert!(
            board.edge_mount_violation(fp).is_none(),
            "{r} must sit on an outline edge: {:?}",
            board.edge_mount_violation(fp)
        );
    }
    // …and on the edge its bank is on, not wherever it happened to spawn.
    assert_eq!(touching_side(&board, "J1"), Some(EdgeSide::Left));
    assert_eq!(touching_side(&board, "J2"), Some(EdgeSide::Right));
    assert_eq!(touching_side(&board, "J3"), Some(EdgeSide::Bottom));
    assert_eq!(touching_side(&board, "J4"), Some(EdgeSide::Top));
}

#[test]
fn crowded_board_plan_is_deterministic() {
    let opts = PlaceOptions {
        seed: 4,
        ..PlaceOptions::default()
    };
    let mut movable: Vec<String> = vec![
        "J1".to_string(),
        "J2".to_string(),
        "J3".to_string(),
        "J4".to_string(),
    ];
    movable.extend((1..=12).map(|i| format!("C{i}")));
    let run = || {
        let mut board = build_crowded_board();
        let r = place(&mut board, &movable, &opts, &MarginMap::new()).expect("place");
        let mut poses: Vec<(String, i64, i64, f32)> = board
            .footprints_in_order()
            .map(|f| {
                (
                    f.reference.clone(),
                    f.position.x.0,
                    f.position.y.0,
                    f.rotation,
                )
            })
            .collect();
        poses.sort_by(|a, b| a.0.cmp(&b.0));
        (poses, r.final_crossings, r.accepted)
    };
    assert_eq!(run(), run());
}

/// Pad-row extents of a footprint: `(along x, along y)` in mm.
fn extents(board: &Board, reference: &str) -> (f64, f64) {
    let fp = board
        .footprints_in_order()
        .find(|f| f.reference == reference)
        .expect("footprint");
    let b = fp.bounds().expect("bounds");
    (
        b.max.x.to_mm() - b.min.x.to_mm(),
        b.max.y.to_mm() - b.min.y.to_mm(),
    )
}

/// Extent perpendicular to the edge the part is sitting on, mm. This is the
/// number that exposes an end-on header: a 25 mm pad row planned
/// perpendicular reaches 25 mm into the board.
fn perpendicular_extent(board: &Board, reference: &str) -> f64 {
    let (w, h) = extents(board, reference);
    match touching_side(board, reference).expect("part must touch an edge") {
        EdgeSide::Left | EdgeSide::Right => w,
        EdgeSide::Top | EdgeSide::Bottom => h,
    }
}

/// A 1×10 2.54 mm header exactly like the campaign board's `header_1x10`:
/// pad row authored along LOCAL X (±11.43 mm) and the library declaring
/// `edge_side = left`. Read as a plug face, that declaration forces the row
/// end-on into the board; the planner must ignore it for rotation purposes.
fn header_1x10(reference: &str, x: f64, y: f64, prefix: &str) -> Footprint {
    let pads = (0..10)
        .map(|i| {
            pad(
                &format!("{}", i + 1),
                -11.43 + f64::from(i) * 2.54,
                0.0,
                Some(&format!("{prefix}{i}")),
            )
        })
        .collect();
    let mut fp = footprint(reference, x, y, pads);
    fp.edge_mounted = true;
    fp.edge_side = Some(EdgeSide::Left);
    fp
}

#[test]
fn declared_edge_side_never_forces_a_header_end_on() {
    // One 1×10 header per test case, wired to a fixed bank placed so the
    // cheapest side is the one under test. Whatever side it lands on, the
    // pad row must lie ALONG that side.
    for (case, bank_x, bank_y, want) in [
        ("left", 8.0, 25.0, EdgeSide::Left),
        ("bottom", 25.0, 8.0, EdgeSide::Bottom),
    ] {
        let mut board = Board::new();
        board.outline = Some(Rect::from_corners(
            Point::new(Length::from_mm(0.0), Length::from_mm(0.0)),
            Point::new(Length::from_mm(50.0), Length::from_mm(50.0)),
        ));
        let bank_pads = (0..10)
            .map(|i| {
                let off = -11.43 + f64::from(i) * 2.54;
                // Bank pads run along the same axis as the target edge, so
                // wirelength alone already prefers a parallel header.
                let (ox, oy) = if want == EdgeSide::Left {
                    (0.0, off)
                } else {
                    (off, 0.0)
                };
                pad(&format!("{}", i + 1), ox, oy, Some(&format!("H{i}")))
            })
            .collect();
        board.add_footprint(footprint("U1", bank_x, bank_y, bank_pads));
        board.add_footprint(header_1x10("J1", 25.0, 25.0, "H"));

        let opts = PlaceOptions::default();
        let report = plan_edge_sides(&mut board, &["J1".to_string()], &opts, &MarginMap::new())
            .expect("edge plan");
        assert_eq!(
            report.placed.len(),
            1,
            "{case}: skipped {:?}",
            report.skipped
        );
        assert_eq!(report.placed[0].side, want, "{case}: wrong side");
        assert_eq!(touching_side(&board, "J1"), Some(want), "{case}");
        let perp = perpendicular_extent(&board, "J1");
        assert!(
            perp < 5.0,
            "{case}: the 25 mm pad row must lie ALONG the {} edge, but it reaches {perp:.2} mm into the board",
            want.name()
        );
        // The pose the planner committed must satisfy the rule the SA and
        // the DRC enforce — including the re-declared local side.
        let j1 = board
            .footprints_in_order()
            .find(|f| f.reference == "J1")
            .unwrap();
        assert!(
            board.edge_mount_violation(j1).is_none(),
            "{case}: {:?}",
            board.edge_mount_violation(j1)
        );
        assert_eq!(
            report.placed[0].edge_side, j1.edge_side,
            "{case}: the report must expose the declaration it wrote"
        );
    }
}

#[test]
fn crowded_board_headers_lie_parallel_to_their_edge() {
    // Same crowded campaign-shaped board, but with the two long 1×10
    // headers from the real design (declared `left`, row along local x).
    let mut board = build_crowded_board();
    // Swap J3/J4 for 1×10s wired to the bottom / top banks.
    for r in ["J3", "J4"] {
        let id = board
            .footprints_in_order()
            .find(|f| f.reference == r)
            .unwrap()
            .id;
        board.footprints.remove(&id);
    }
    let mut j3 = header_1x10("J3", 18.0, 8.0, "BB");
    j3.pads.truncate(3);
    let mut j4 = header_1x10("J4", 18.0, 22.0, "TB");
    j4.pads.truncate(3);
    // Keep the full 23 mm span: re-add the row ends as unconnected pads so
    // the body is still long, like the real part.
    for (fp, prefix) in [(&mut j3, "BB"), (&mut j4, "TB")] {
        for i in 3..10 {
            fp.pads.push(pad(
                &format!("{}", i + 1),
                -11.43 + f64::from(i) * 2.54,
                0.0,
                None,
            ));
        }
        let _ = prefix;
    }
    board.add_footprint(j3);
    board.add_footprint(j4);

    let mut movable: Vec<String> = vec![
        "J1".to_string(),
        "J2".to_string(),
        "J3".to_string(),
        "J4".to_string(),
    ];
    movable.extend((1..=12).map(|i| format!("C{i}")));
    let opts = PlaceOptions {
        seed: 4,
        ..PlaceOptions::default()
    };
    place(&mut board, &movable, &opts, &MarginMap::new()).expect("place");

    for r in ["J1", "J2", "J3", "J4"] {
        let fp = board
            .footprints_in_order()
            .find(|f| f.reference == r)
            .unwrap();
        assert!(
            board.edge_mount_violation(fp).is_none(),
            "{r}: {:?}",
            board.edge_mount_violation(fp)
        );
        let perp = perpendicular_extent(&board, r);
        assert!(
            perp < 5.0,
            "{r} must lie along its edge, reaches {perp:.2} mm into the board"
        );
    }
}
