//! Decoupling ring: every movable 2-pad cap must end up AT its own
//! power pin, not at the centroid of all of them.

use pcb_core::{Board, CopperLayer, Footprint, Id, Length, Pad, Point, Rect};
use pcb_placer::{place, MarginMap, PlaceOptions};

fn pad(num: &str, off_x: f64, off_y: f64, net: Option<&str>) -> Pad {
    Pad {
        number: num.into(),
        name: String::new(),
        offset: Point::new(Length::from_mm(off_x), Length::from_mm(off_y)),
        size: (Length::from_mm(0.8), Length::from_mm(0.8)),
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

/// ~16 × 16 mm IC, fixed at the middle of a 80 × 80 mm board. Two rails,
/// four pins each, spread over the four sides (two pins per side), plus
/// two GND pads so the "prefer the non-GND net" rule is exercised.
/// Eight movable 0603 caps: C1-C4 on `+3V3`, C5-C8 on `+1V1`.
///
/// Pin pitch along each side is 8 mm so neighbouring caps still clear the
/// 2.0 mm hand-solder pad-AABB floor when both seat on the ring.
fn build_board() -> Board {
    let mut board = Board::new();
    board.outline = Some(Rect::from_corners(
        Point::new(Length::from_mm(0.0), Length::from_mm(0.0)),
        Point::new(Length::from_mm(80.0), Length::from_mm(80.0)),
    ));

    // Rail pins: one +3V3 and one +1V1 per side, at ±8 mm (the body edge)
    // and offset 4 mm along that side (8 mm pin pitch).
    let ic = footprint(
        "U1",
        40.0,
        40.0,
        vec![
            pad("1", 8.0, 4.0, Some("+3V3")),
            pad("2", 8.0, -4.0, Some("+1V1")),
            pad("3", -8.0, 4.0, Some("+3V3")),
            pad("4", -8.0, -4.0, Some("+1V1")),
            pad("5", 4.0, 8.0, Some("+3V3")),
            pad("6", -4.0, 8.0, Some("+1V1")),
            pad("7", 4.0, -8.0, Some("+3V3")),
            pad("8", -4.0, -8.0, Some("+1V1")),
            pad("9", 0.0, 8.0, Some("GND")),
            pad("10", 0.0, -8.0, Some("GND")),
        ],
    );
    board.add_footprint(ic);

    // Caps start bunched in a corner so the pass has real work to do.
    // 3.5 mm pitch keeps them past the 2.0 mm pad-AABB floor while still
    // packed far from the IC.
    for i in 0..8 {
        let rail = if i < 4 { "+3V3" } else { "+1V1" };
        let x = 6.0 + f64::from(i % 4) * 3.5;
        let y = 6.0 + f64::from(i / 4) * 3.5;
        board.add_footprint(footprint(
            &format!("C{}", i + 1),
            x,
            y,
            vec![
                pad("1", -0.8, 0.0, Some(rail)),
                pad("2", 0.8, 0.0, Some("GND")),
            ],
        ));
    }
    board
}

fn cap_refs() -> Vec<String> {
    (1..=8).map(|i| format!("C{i}")).collect()
}

/// For each cap: the IC pad on its rail nearest to the cap's own rail pad,
/// and that distance in mm.
fn nearest_anchor(board: &Board) -> Vec<(String, String, f64)> {
    let ic = board
        .footprints_in_order()
        .find(|f| f.reference == "U1")
        .unwrap();
    let mut out = Vec::new();
    for cap in board.footprints_in_order().filter(|f| f.reference != "U1") {
        let rail_pad = cap
            .pads
            .iter()
            .find(|p| p.net.as_deref() != Some("GND"))
            .unwrap();
        let rail = rail_pad.net.clone().unwrap();
        let c = cap.pad_world_center(rail_pad);
        let mut best: Option<(String, f64)> = None;
        for p in ic.pads.iter().filter(|p| p.net.as_deref() == Some(&rail)) {
            let a = ic.pad_world_center(p);
            let d = (a.x.to_mm() - c.x.to_mm()).hypot(a.y.to_mm() - c.y.to_mm());
            if best.as_ref().is_none_or(|(_, bd)| d < *bd) {
                best = Some((p.number.clone(), d));
            }
        }
        let (num, d) = best.unwrap();
        out.push((cap.reference.clone(), num, d));
    }
    out
}

#[test]
fn each_cap_lands_on_its_own_power_pin() {
    let mut board = build_board();
    let opts = PlaceOptions {
        seed: 5,
        ..PlaceOptions::default()
    };
    place(&mut board, &cap_refs(), &opts, &MarginMap::new()).expect("place");

    let anchors = nearest_anchor(&board);
    for (cap, pin, d) in &anchors {
        // Ring seats sit just outside the IC body at the hard solder floor
        // (~2 mm body gap + pad extents). 4.5 mm covers that plus a rank
        // of outward walk under the 2.0 mm floor without accepting a
        // "still in the corner" failure (~30 mm).
        assert!(
            *d <= 4.5,
            "{cap} should sit at a power pin (nearest is U1.{pin} at {d:.2} mm)"
        );
    }
    // The whole point of the pass: eight caps, eight DIFFERENT pins.
    let mut pins: Vec<&String> = anchors.iter().map(|(_, p, _)| p).collect();
    pins.sort();
    let distinct = {
        let mut p = pins.clone();
        p.dedup();
        p.len()
    };
    assert_eq!(
        distinct,
        anchors.len(),
        "each cap must ring a distinct pin, got {pins:?}"
    );
}

#[test]
fn decoupling_ring_is_deterministic() {
    let opts = PlaceOptions {
        seed: 5,
        ..PlaceOptions::default()
    };
    let poses = |board: &Board| -> Vec<(String, i64, i64, f32)> {
        let mut v: Vec<(String, i64, i64, f32)> = board
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
        v.sort_by(|a, b| a.0.cmp(&b.0));
        v
    };

    let mut a = build_board();
    place(&mut a, &cap_refs(), &opts, &MarginMap::new()).expect("place");
    let mut b = build_board();
    let report = place(&mut b, &cap_refs(), &opts, &MarginMap::new()).expect("place");

    assert_eq!(poses(&a), poses(&b), "same seed must give the same poses");
    // And the report numbers must match too (they are what agents read).
    let report_a = {
        let mut a2 = build_board();
        place(&mut a2, &cap_refs(), &opts, &MarginMap::new()).expect("place")
    };
    assert_eq!(report.final_crossings, report_a.final_crossings);
    assert!((report.final_hpwl_mm - report_a.final_hpwl_mm).abs() < 1e-9);
    assert!((report.final_congestion - report_a.final_congestion).abs() < 1e-9);
}
