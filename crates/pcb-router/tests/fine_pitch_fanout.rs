//! Fine-pitch QFN regression tests.
//!
//! These pin down the three properties the RP2040 stress campaign needs:
//! a 0.4 mm-pitch pad that cannot host a via-in-pad still gets a DOGBONE
//! escape (barrel outside the pad, fed by a copper stub), the wall-clock
//! budget is actually honoured, and a full QFN-56 board routes to real
//! copper inside the budget instead of hanging.

use std::time::Instant;

use pcb_core::{Board, CopperLayer, Footprint, Id, Length, Pad, Point, Rect};
use pcb_router::{route, Outcome, RouteOptions};

/// The script layer's defaults — `RouteOptions::default()` uses a 0.40 mm
/// clearance, which no fine-pitch board ever passes through. Every stress
/// measurement goes through these numbers, so the tests do too.
fn script_opts(max_seconds: f64) -> RouteOptions {
    RouteOptions {
        cell: Length::from_mm(0.20),
        trace_width: Length::from_mm(0.25),
        clearance: Length::from_mm(0.20),
        max_seconds: Some(max_seconds),
        ..RouteOptions::default()
    }
}

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

/// A QFN with `per_side` pins on each of its four sides at `pitch`, pads
/// `pad_len` long and `pad_w` wide, plus a shrunken thermal pad. Every pin
/// gets its own net so nothing can be trivially merged. Nets are named
/// `N<i>`; a matching landing pad for each net is dropped in a ring of
/// coarse 2-pin parts around the package so the router has somewhere to go.
fn qfn_board(per_side: usize, pitch: f64, pad_len: f64, pad_w: f64) -> Board {
    let mut board = Board::new();
    board.outline = Some(Rect::from_corners(
        Point::new(Length::from_mm(0.0), Length::from_mm(0.0)),
        Point::new(Length::from_mm(40.0), Length::from_mm(40.0)),
    ));
    let half_body = 3.5;
    // Pad centre sits half a pad-length inside the body edge.
    let ring = half_body - pad_len / 2.0;
    let span = (per_side as f64 - 1.0) * pitch;
    let mut pads = Vec::new();
    let mut n = 0usize;
    for side in 0..4 {
        for i in 0..per_side {
            let along = -span / 2.0 + i as f64 * pitch;
            let net = format!("N{n}");
            n += 1;
            let p = match side {
                0 => pad(&format!("{}", n), -ring, along, pad_len, pad_w, Some(&net)),
                1 => pad(&format!("{}", n), ring, along, pad_len, pad_w, Some(&net)),
                2 => pad(&format!("{}", n), along, -ring, pad_w, pad_len, Some(&net)),
                _ => pad(&format!("{}", n), along, ring, pad_w, pad_len, Some(&net)),
            };
            pads.push(p);
        }
    }
    pads.push(pad("EP", 0.0, 0.0, 1.6, 1.6, Some("GND")));
    board.add_footprint(footprint("U1", 20.0, 20.0, pads));

    // Coarse landing parts around the package, one 2-pin pad pair per net.
    let total = per_side * 4;
    for i in 0..total {
        let angle = std::f64::consts::TAU * i as f64 / total as f64;
        let r = 13.0;
        let x = 20.0 + r * angle.cos();
        let y = 20.0 + r * angle.sin();
        board.add_footprint(footprint(
            &format!("R{i}"),
            x,
            y,
            vec![
                pad("1", -0.8, 0.0, 0.9, 0.9, Some(&format!("N{i}"))),
                pad("2", 0.8, 0.0, 0.9, 0.9, Some("GND")),
            ],
        ));
    }
    board
}

#[test]
fn tiny_pad_gets_a_dogbone_via_outside_its_copper() {
    // 0.4 mm pitch, 0.20 mm-wide pads: far too small for the 0.30 mm
    // fanout barrel, so a via-in-pad is impossible and the escape must be
    // a dogbone sitting OUTSIDE the pad.
    let pad_w = 0.20;
    let pad_len = 0.50;
    let mut board = qfn_board(6, 0.40, pad_len, pad_w);
    let opts = script_opts(30.0);
    let report = route(&mut board, &opts);

    assert!(
        report.dogbone_via_count > 0,
        "no dogbone escapes on a 0.4 mm-pitch QFN: {report:?}"
    );
    assert_eq!(
        report.escape_stub_count, report.dogbone_via_count,
        "every dogbone must be fed by a pad->via copper stub: {report:?}"
    );

    // Each dogbone barrel must sit clear of the pad copper it serves (that
    // is what makes it a dogbone and not a via-in-pad), and the pad must
    // still be reachable: a stub trace of the same net has to touch it.
    let u1 = board
        .footprints_in_order()
        .find(|f| f.reference == "U1")
        .expect("U1");
    let via_r = 0.15;
    let mut checked = 0;
    for p in &u1.pads {
        let Some(net) = p.net.as_deref() else {
            continue;
        };
        if net == "GND" {
            continue;
        }
        let c = u1.pad_world_center(p);
        let (w, h) = u1.pad_world_size(p);
        let (cx, cy) = (c.x.to_mm(), c.y.to_mm());
        let (hw, hh) = (w.to_mm() / 2.0, h.to_mm() / 2.0);
        let Some(v) = board.vias.iter().find(|v| v.net == net) else {
            continue;
        };
        let (vx, vy) = (v.position.x.to_mm(), v.position.y.to_mm());
        let outside = (vx - cx).abs() > hw + via_r - 1e-6 || (vy - cy).abs() > hh + via_r - 1e-6;
        assert!(
            outside,
            "via for {net} at ({vx:.3},{vy:.3}) overlaps its {:.2}x{:.2} mm pad at ({cx:.3},{cy:.3})",
            2.0 * hw,
            2.0 * hh
        );
        assert!(
            board
                .traces
                .iter()
                .any(|t| t.net == net && t.layer == p.layer),
            "no copper on {net} to reach its dogbone via"
        );
        checked += 1;
    }
    assert!(checked > 0, "no fine-pitch pad was escaped at all");
}

#[test]
fn a_tiny_budget_returns_quickly_with_timeout_failures() {
    // A board the router cannot finish in 10 ms: it must give up almost
    // immediately and report the leftovers as timeouts, never hang.
    let mut board = qfn_board(14, 0.40, 0.50, 0.20);
    let opts = script_opts(0.01);

    let t0 = Instant::now();
    let report = route(&mut board, &opts);
    let elapsed = t0.elapsed();

    assert!(
        elapsed.as_secs_f64() < 20.0,
        "max_seconds=0.01 took {elapsed:?}"
    );
    assert!(report.budget_hit, "budget_hit not set: {report:?}");
    assert!(
        report.elapsed_seconds > 0.0 && report.elapsed_seconds < 20.0,
        "reported elapsed {:.2}s out of range",
        report.elapsed_seconds
    );
    let timed_out = report
        .per_net
        .iter()
        .filter(|(_, o)| matches!(o, Outcome::Failed { reason } if reason.contains("timeout")))
        .count();
    assert!(
        timed_out > 0,
        "no net was marked as a timeout failure: {report:?}"
    );
}

#[test]
fn qfn56_routes_real_copper_inside_the_budget() {
    // The stress board's package: 56 pins at 0.4 mm pitch on a 2-layer
    // stackup. Full connectivity is a known open problem (2-layer 0.4 mm
    // escape is geometrically brutal), but the router must lay real
    // escape copper and come back on time.
    let mut board = qfn_board(14, 0.40, 0.50, 0.20);
    let opts = script_opts(60.0);

    let t0 = Instant::now();
    let report = route(&mut board, &opts);
    let elapsed = t0.elapsed().as_secs_f64();

    assert!(
        elapsed < 120.0,
        "route took {elapsed:.1}s with a 60 s budget"
    );
    assert!(
        board.vias.len() >= 10,
        "only {} via(s) on the board: {report:?}",
        board.vias.len()
    );
    // The report must agree with the board — that is the whole point of
    // the board-truthful totals.
    assert_eq!(report.board_via_count, board.vias.len());
    assert_eq!(report.board_trace_count, board.traces.len());
    assert!(
        report.fanout_via_count >= 10,
        "fanout produced only {} via(s)",
        report.fanout_via_count
    );
}
