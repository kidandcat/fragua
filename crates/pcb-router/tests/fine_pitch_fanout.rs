//! Fine-pitch QFN regression tests.
//!
//! These pin down the properties the RP2040 stress campaign needs:
//! a 0.4 mm-pitch pad that cannot host a via-in-pad still gets a DOGBONE
//! escape (barrel outside the pad, fed by a copper stub), the wall-clock
//! budget is actually honoured, a full QFN-56 board routes to real
//! copper inside the budget instead of hanging, the organic post-pass
//! never trades a rule area's clearance for a smoother line, and the
//! driver stops on a fixpoint rather than on the clock.

use std::time::Instant;

use pcb_core::{Board, CopperLayer, Footprint, Id, Length, Pad, Point, Rect, RuleArea};
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

/// Clearance errors on ROUTED copper. Pad-vs-pad is deliberately out of
/// scope: that is the fixture's own placement, which no router pass can
/// change — these tests are about what the routing writes.
fn trace_clearance_errors(board: &Board) -> Vec<String> {
    let drc = pcb_drc::run(board, &pcb_drc::DrcOptions::default());
    drc.violations
        .iter()
        .filter(|v| v.severity == pcb_drc::Severity::Error)
        .filter(|v| {
            matches!(
                v.kind,
                pcb_drc::ViolationKind::TraceTraceClearance
                    | pcb_drc::ViolationKind::TracePadClearance
            )
        })
        .map(|v| format!("{:?}: {}", v.kind, v.message))
        .collect()
}

/// End-to-end guard for the v8 organic bug: on a fine-pitch package
/// inside a tight rule area, turning the smoothing pass ON must not cost
/// a single clearance error. The mechanism itself is pinned by the unit
/// test `organic::tests::smoothing_never_widens_a_chain_into_a_violation`
/// (the escape stubs the fanout deliberately narrows used to be rewritten
/// at the net's class width, un-checked); this one is the receipt that
/// the whole pipeline agrees with DRC on a real fine-pitch board.
#[test]
fn organic_smoothing_keeps_the_rule_area_clearance() {
    let mut board = qfn_board(8, 0.40, 0.50, 0.20);
    // Same shape as the stress board's `fine` area: a tight clearance and
    // a small via, declared around the fine-pitch package.
    let u1 = board
        .footprints_in_order()
        .find(|f| f.reference == "U1")
        .expect("U1")
        .clone();
    let mut area = RuleArea::around_footprint("fine", &u1, 1.5).expect("area");
    area.clearance_mm = Some(0.12);
    area.via_drill_mm = Some(0.20);
    area.via_diameter_mm = Some(0.45);
    board.rule_areas.push(area);

    let opts = RouteOptions {
        organic: true,
        ..script_opts(60.0)
    };
    let report = route(&mut board, &opts);
    assert!(
        report.escape_stub_count > 0,
        "no fine-pitch escape stubs — the repro needs narrowed stubs: {report:?}"
    );
    assert!(
        report.organic.is_some(),
        "organic pass did not run — the assertion below would be vacuous: {report:?}"
    );
    let errors = trace_clearance_errors(&board);
    assert!(
        errors.is_empty(),
        "organic pass produced {} clearance error(s):\n{}",
        errors.len(),
        errors.join("\n")
    );
}

/// The driver stops when it has nothing left to try, not when the clock
/// runs out.
///
/// The board carries one net whose two pads sit on opposite sides of a
/// solid through-hole wall: no router will ever connect it, on any
/// stackup, with any budget. Given a budget far larger than the work the
/// driver must come back early with `budget_hit` clear, and it must
/// return the SAME board for a small budget and a huge one — a fixpoint
/// is budget-independent by construction, the wall clock only truncates.
#[test]
fn driver_stops_on_fixpoint_not_on_the_clock() {
    fn walled_board() -> Board {
        let mut board = Board::new();
        board.outline = Some(Rect::from_corners(
            Point::new(Length::from_mm(0.0), Length::from_mm(0.0)),
            Point::new(Length::from_mm(20.0), Length::from_mm(20.0)),
        ));
        // The wall: through-hole copper on every layer, 0.70 mm pads on a
        // 0.75 mm pitch, so the 0.05 mm gaps cannot take a trace at any
        // clearance the board declares. It overhangs the outline at both
        // ends so nothing sneaks around it either.
        let mut wall = Vec::new();
        let mut y = -0.5;
        let mut n = 0;
        while y <= 20.5 {
            n += 1;
            let mut p = pad(&format!("{n}"), 0.0, y, 0.70, 0.70, Some("WALL"));
            p.drill = Some(Length::from_mm(0.3));
            wall.push(p);
            y += 0.75;
        }
        board.add_footprint(footprint("W1", 10.0, 0.0, wall));
        board.add_footprint(footprint(
            "J1",
            3.0,
            10.0,
            vec![pad("1", 0.0, 0.0, 0.9, 0.9, Some("BLOCKED"))],
        ));
        board.add_footprint(footprint(
            "J2",
            17.0,
            10.0,
            vec![pad("1", 0.0, 0.0, 0.9, 0.9, Some("BLOCKED"))],
        ));
        // A couple of nets that route trivially, so the pass has work
        // to do and "no NEW net routed" is a real statement.
        for (i, y) in [3.0f64, 17.0].into_iter().enumerate() {
            board.add_footprint(footprint(
                &format!("R{i}"),
                3.0,
                y,
                vec![pad("1", 0.0, 0.0, 0.9, 0.9, Some(&format!("EASY{i}")))],
            ));
            board.add_footprint(footprint(
                &format!("R{i}b"),
                6.0,
                y,
                vec![pad("1", 0.0, 0.0, 0.9, 0.9, Some(&format!("EASY{i}")))],
            ));
        }
        board
    }

    let budget = 600.0;
    let mut board = walled_board();
    let t0 = Instant::now();
    let report = route(&mut board, &script_opts(budget));
    let elapsed = t0.elapsed().as_secs_f64();

    let failed: Vec<&str> = report
        .per_net
        .iter()
        .filter_map(|(n, o)| matches!(o, Outcome::Failed { .. }).then_some(n.as_str()))
        .collect();
    assert_eq!(
        failed,
        ["BLOCKED"],
        "the repro needs exactly the walled net to fail: {report:?}"
    );
    assert!(
        !report.budget_hit,
        "driver ran out the {budget}s clock instead of converging ({elapsed:.1}s, {} pass(es))",
        report.iterations
    );
    assert!(
        elapsed < 60.0,
        "converged only after {elapsed:.1}s of a {budget}s budget"
    );
    assert!(report.iterations >= 1, "no pass reported: {report:?}");

    // Budget-independence: the fixpoint, not the clock, decided the
    // answer, so a 30× smaller budget must produce the same one.
    let mut small = walled_board();
    let small_report = route(&mut small, &script_opts(budget / 30.0));
    assert!(!small_report.budget_hit, "small budget was truncated");
    assert_eq!(
        report.iterations, small_report.iterations,
        "round count depends on the budget"
    );
    assert_eq!(
        report.board_trace_count, small_report.board_trace_count,
        "board depends on the budget"
    );
}
