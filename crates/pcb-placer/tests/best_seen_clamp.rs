//! `place` must never hand back a layout worse than the one it was given.
//!
//! The v8 stress pass measured the opposite: re-placing an already-good
//! 44 × 34 mm layout returned HPWL +78 mm, congestion +606 cells and one
//! extra bundle crossing. Only the SA stage tracked a best-so-far; the
//! edge planner, the electrostatic global solve and the decoupling ring
//! each passed on whatever they produced. `place` now clamps to the best
//! legal layout any stage saw, including the caller's own.

use pcb_core::{Board, CopperLayer, Footprint, Id, Length, Pad, Point, Rect};
use pcb_placer::{bundle_crossings, place, Checkpoint, MarginMap, PlaceOptions};

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

/// An IC in the middle of a 60 × 40 mm board with two 1:1 wired headers
/// and a handful of decoupling passives — enough structure that HPWL,
/// congestion and bundle crossings are all non-trivial, and enough slack
/// that the first `place` call has real work to do.
fn build_board() -> Board {
    let mut board = Board::new();
    board.outline = Some(Rect::from_corners(
        Point::new(Length::from_mm(0.0), Length::from_mm(0.0)),
        Point::new(Length::from_mm(60.0), Length::from_mm(40.0)),
    ));

    let mut ic_pads = Vec::new();
    for i in 0..6 {
        let y = -3.75 + f64::from(i) * 1.5;
        ic_pads.push(pad(&format!("L{i}"), -5.0, y, Some(&format!("LB{i}"))));
        ic_pads.push(pad(&format!("R{i}"), 5.0, y, Some(&format!("RB{i}"))));
    }
    ic_pads.push(pad("VDD", -5.0, 5.5, Some("+3V3")));
    ic_pads.push(pad("GND", 5.0, 5.5, Some("GND")));
    board.add_footprint(footprint("U1", 30.0, 20.0, ic_pads));

    let header = |reference: &str, x: f64, y: f64, prefix: &str| -> Footprint {
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
        footprint(reference, x, y, pads)
    };
    board.add_footprint(header("JL", 52.0, 20.0, "LB"));
    board.add_footprint(header("JR", 8.0, 20.0, "RB"));

    // Decoupling passives, deliberately parked in a corner.
    for (i, reference) in ["C1", "C2", "C3", "C4"].iter().enumerate() {
        board.add_footprint(footprint(
            reference,
            4.0 + f64::from(i as u32) * 3.0,
            36.0,
            vec![
                pad("1", -0.8, 0.0, Some("+3V3")),
                pad("2", 0.8, 0.0, Some("GND")),
            ],
        ));
    }
    board
}

fn movable(board: &Board) -> Vec<String> {
    board
        .footprints_in_order()
        .filter(|fp| fp.reference != "U1")
        .map(|fp| fp.reference.clone())
        .collect()
}

/// Feed the placer a layout it produced itself: the second run must not
/// come back worse on ANY of the three headline metrics.
#[test]
fn replacing_an_optimized_layout_never_regresses() {
    let opts = PlaceOptions {
        seed: 11,
        ..PlaceOptions::default()
    };
    let margins = MarginMap::new();

    // Round 1 — from the scattered start, so round 2 gets a genuinely
    // pre-optimized layout rather than a lucky one.
    let mut board = build_board();
    let refs = movable(&board);
    let first = place(&mut board, &refs, &opts, &margins).expect("first place");
    assert!(
        first.final_hpwl_mm <= first.initial_hpwl_mm,
        "round 1 should not make the scattered board worse: {:.1} → {:.1} mm",
        first.initial_hpwl_mm,
        first.final_hpwl_mm,
    );

    // Round 2 — the regression under test. A different seed so the SA
    // explores a different trajectory and really can wander off.
    let opts2 = PlaceOptions { seed: 29, ..opts };
    let second = place(&mut board, &refs, &opts2, &margins).expect("second place");

    assert!(
        second.final_hpwl_mm <= second.initial_hpwl_mm + 1e-6,
        "HPWL regressed on a pre-optimized layout: {:.3} → {:.3} mm",
        second.initial_hpwl_mm,
        second.final_hpwl_mm,
    );
    assert!(
        second.final_congestion <= second.initial_congestion + 1e-6,
        "congestion regressed on a pre-optimized layout: {:.0} → {:.0}",
        second.initial_congestion,
        second.final_congestion,
    );
    assert!(
        second.final_crossings <= second.initial_crossings,
        "bundle crossings regressed on a pre-optimized layout: {} → {}",
        second.initial_crossings,
        second.final_crossings,
    );
    // And the board itself agrees with the report.
    assert_eq!(
        bundle_crossings(&board),
        second.final_crossings,
        "report crossings must describe the board that was returned"
    );
}

/// The clamp must be able to fire: a layout the placer cannot improve on
/// comes back byte-identical, reported as `Checkpoint::Entry`.
#[test]
fn an_unimprovable_layout_comes_back_untouched() {
    let opts = PlaceOptions {
        seed: 5,
        ..PlaceOptions::default()
    };
    let margins = MarginMap::new();
    let mut board = build_board();
    let refs = movable(&board);
    place(&mut board, &refs, &opts, &margins).expect("warm-up place");

    let before: Vec<(String, f64, f64, f32)> = board
        .footprints_in_order()
        .map(|fp| {
            (
                fp.reference.clone(),
                fp.position.x.to_mm(),
                fp.position.y.to_mm(),
                fp.rotation,
            )
        })
        .collect();

    // Re-run at a tiny iteration count so the SA cannot realistically
    // beat the converged layout — the clamp is the only thing standing
    // between the caller and a half-annealed board.
    let stingy = PlaceOptions {
        seed: 77,
        max_iterations: 40,
        ..opts
    };
    let report = place(&mut board, &refs, &stingy, &margins).expect("stingy place");
    assert_eq!(
        report.kept,
        Checkpoint::Entry,
        "40 SA moves should not beat a converged layout; kept {:?}",
        report.kept
    );

    let after: Vec<(String, f64, f64, f32)> = board
        .footprints_in_order()
        .map(|fp| {
            (
                fp.reference.clone(),
                fp.position.x.to_mm(),
                fp.position.y.to_mm(),
                fp.rotation,
            )
        })
        .collect();
    assert_eq!(before, after, "the input layout must survive unmodified");
    assert!(
        report.moved.is_empty(),
        "nothing moved, so the report must say so: {:?}",
        report.moved
    );
}
