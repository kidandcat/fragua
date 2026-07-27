//! Extra copper layers must never make routing worse (issue O8).
//!
//! The 4-layer probe on the RP2040 stress board used to collapse from
//! 21/39 connected nets to 1/39 and overrun its wall-clock budget by 34 s:
//! the fine-grid escape pass took over from the proven VIP/dogbone fanout
//! as soon as the stackup reached three layers, spent the whole escape
//! budget on a QFN row it cannot fan, and left the coarse router with
//! almost no escaped pads. This test pins the property that broke: on the
//! same board, with the same budget, a 4-layer stackup must connect AT
//! LEAST as many nets as a 2-layer one, and both must respect the budget.

use std::time::Instant;

use pcb_core::{Board, CopperLayer, Footprint, Id, LayerStackup, Length, Pad, Point, Rect};
use pcb_router::{route, Outcome, RouteOptions};

/// Budget per route. Deliberately generous for this small board: both
/// stackups finish their passes well inside it, so the comparison is about
/// routing quality, not about who got more wall clock.
const BUDGET_SECONDS: f64 = 20.0;

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

fn footprint(reference: &str, x_mm: f64, y_mm: f64, pads: Vec<Pad>) -> Footprint {
    Footprint {
        id: Id::new(),
        reference: reference.into(),
        value: String::new(),
        library: "test".into(),
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

/// A dense 0.4 mm-pitch QFN (pads far too small for a via-in-pad, so the
/// escape path is exercised) with one 2-pin landing part per pin around it.
fn dense_qfn_board() -> Board {
    let per_side = 4usize;
    let pitch = 0.40;
    let (pad_len, pad_w) = (0.50, 0.20);
    let mut board = Board::new();
    board.outline = Some(Rect::from_corners(
        Point::new(Length::from_mm(0.0), Length::from_mm(0.0)),
        Point::new(Length::from_mm(30.0), Length::from_mm(30.0)),
    ));
    let ring = 3.5 - pad_len / 2.0;
    let span = (per_side as f64 - 1.0) * pitch;
    let mut pads = Vec::new();
    let mut n = 0usize;
    for side in 0..4 {
        for i in 0..per_side {
            let along = -span / 2.0 + i as f64 * pitch;
            let net = format!("N{n}");
            n += 1;
            pads.push(match side {
                0 => pad(&n.to_string(), -ring, along, pad_len, pad_w, Some(&net)),
                1 => pad(&n.to_string(), ring, along, pad_len, pad_w, Some(&net)),
                2 => pad(&n.to_string(), along, -ring, pad_w, pad_len, Some(&net)),
                _ => pad(&n.to_string(), along, ring, pad_w, pad_len, Some(&net)),
            });
        }
    }
    pads.push(pad("EP", 0.0, 0.0, 1.6, 1.6, Some("GND")));
    board.add_footprint(footprint("U1", 15.0, 15.0, pads));

    let total = per_side * 4;
    for i in 0..total {
        let angle = std::f64::consts::TAU * i as f64 / total as f64;
        let r = 10.0;
        board.add_footprint(footprint(
            &format!("R{i}"),
            15.0 + r * angle.cos(),
            15.0 + r * angle.sin(),
            vec![
                pad("1", -0.8, 0.0, 0.9, 0.9, Some(&format!("N{i}"))),
                pad("2", 0.8, 0.0, 0.9, 0.9, Some("GND")),
            ],
        ));
    }
    board
}

/// Route the fixture on `layers` copper layers with the script layer's
/// defaults. Returns (failed nets, routable nets, elapsed seconds).
fn route_with_layers(layers: u8) -> (usize, usize, f64) {
    let mut board = dense_qfn_board();
    if layers > 2 {
        board.stackup = LayerStackup::fr4(layers);
    }
    assert_eq!(board.stackup.layer_count(), layers);
    let opts = RouteOptions {
        cell: Length::from_mm(0.20),
        trace_width: Length::from_mm(0.25),
        clearance: Length::from_mm(0.20),
        max_seconds: Some(BUDGET_SECONDS),
        ..RouteOptions::default()
    };
    let t0 = Instant::now();
    let report = route(&mut board, &opts);
    let elapsed = t0.elapsed().as_secs_f64();
    let failed = report
        .per_net
        .iter()
        .filter(|(_, o)| matches!(o, Outcome::Failed { .. }))
        .count();
    println!(
        "layers={layers} failed={failed}/{} elapsed={elapsed:.1}s budget_hit={} passes={}",
        report.routable_net_count, report.budget_hit, report.iterations
    );
    (failed, report.routable_net_count, elapsed)
}

#[test]
fn four_layers_route_at_least_as_well_as_two_and_stay_in_budget() {
    let (failed_2l, routable_2l, elapsed_2l) = route_with_layers(2);
    let (failed_4l, routable_4l, elapsed_4l) = route_with_layers(4);

    assert_eq!(
        routable_2l, routable_4l,
        "the stackup must not change which nets are routable"
    );
    assert!(
        failed_4l <= failed_2l,
        "4-layer routing REGRESSED: {failed_4l} failed net(s) vs {failed_2l} on 2 layers \
         (of {routable_2l} routable) — extra layers may only add freedom"
    );
    // `max_seconds` is a soft budget, but "soft" has to mean seconds, not
    // minutes: the 4-layer path used to run 214 s on a 180 s budget.
    for (layers, elapsed) in [(2u8, elapsed_2l), (4, elapsed_4l)] {
        assert!(
            elapsed <= BUDGET_SECONDS + 15.0,
            "{layers}-layer route took {elapsed:.1}s for a {BUDGET_SECONDS:.0}s budget"
        );
    }
}
