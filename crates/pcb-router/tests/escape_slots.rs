//! A pad with nowhere to escape to must be REPORTED, not silently dropped.
//!
//! The escape stage is the only place that knows a pin is geometrically
//! doomed: no barrel site clears its neighbours, or none that does can be
//! reached by legal copper. If that fact never leaves the stage, the net
//! comes back as an ordinary failed net and an agent spends the rest of its
//! budget raising `max_seconds` against a wall. So `route()` puts the
//! strand at the TOP of its hints, before anything the search has to say.
//!
//! ```text
//!            SHIELD (0.15 mm off the pad row)  -> no outward barrel
//!        ┌───────────────────────────────────┐
//!        │ ▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓ │
//!        │ ▓▓ ┌───────────────────────┐  ▓▓ │
//!        │ ▓▓ │ ▪ ▪   EP (unshrunk)   │  ▓▓ │  -> no inward barrel
//!        │ ▓▓ └───────────────────────┘  ▓▓ │
//!        │ ▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓ │
//!        └───────────────────────────────────┘
//! ```
//!
//! The 0.4 mm-pitch pins are 0.20 mm wide, so a 0.30 mm barrel cannot sit
//! in the pad either: every pin needs a dogbone and no dogbone site exists.

use pcb_core::{Board, CopperLayer, Footprint, Id, Length, Pad, Point, Rect};
use pcb_router::{route, Outcome, RouteOptions};

fn pad(num: &str, off_x: f64, off_y: f64, w: f64, h: f64, net: &str) -> Pad {
    Pad {
        number: num.into(),
        name: String::new(),
        offset: Point::new(Length::from_mm(off_x), Length::from_mm(off_y)),
        size: (Length::from_mm(w), Length::from_mm(h)),
        layer: CopperLayer::Top,
        net: Some(net.to_string()),
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

/// The jammed package drawn above: `per_side` pins per side at 0.4 mm
/// pitch, a thermal pad grown until it is 0.05 mm from the pads' inner
/// faces, and a shield frame starting 0.15 mm past their outer faces —
/// closer than a 0.15 mm barrel plus its 0.20 mm clearance, so neither the
/// inward nor the outward candidate ranks survive.
fn jammed_package(per_side: usize) -> Board {
    let mut board = Board::new();
    board.outline = Some(Rect::from_corners(
        Point::new(Length::from_mm(0.0), Length::from_mm(0.0)),
        Point::new(Length::from_mm(40.0), Length::from_mm(40.0)),
    ));
    let (pitch, pad_len, pad_w) = (0.40, 0.50, 0.20);
    let ring = 3.5 - pad_len / 2.0; // pad centre, half a pad inside the body edge
    let span = (per_side as f64 - 1.0) * pitch;
    let mut pads = Vec::new();
    let mut n = 0usize;
    for side in 0..4 {
        for i in 0..per_side {
            let along = -span / 2.0 + i as f64 * pitch;
            let net = format!("N{n}");
            n += 1;
            let num = format!("{n}");
            pads.push(match side {
                0 => pad(&num, -ring, along, pad_len, pad_w, &net),
                1 => pad(&num, ring, along, pad_len, pad_w, &net),
                2 => pad(&num, along, -ring, pad_w, pad_len, &net),
                _ => pad(&num, along, ring, pad_w, pad_len, &net),
            });
        }
    }
    pads.push(pad("EP", 0.0, 0.0, 5.9, 5.9, "GND"));
    board.add_footprint(footprint("U1", 20.0, 20.0, pads));
    let (inner, outer) = (3.65, 6.25);
    let mid = f64::midpoint(inner, outer);
    let (thick, long) = (outer - inner, 2.0 * outer);
    board.add_footprint(footprint(
        "SH1",
        20.0,
        20.0,
        vec![
            pad("1", -mid, 0.0, thick, long, "SHIELD"),
            pad("2", mid, 0.0, thick, long, "SHIELD"),
            pad("3", 0.0, -mid, long, thick, "SHIELD"),
            pad("4", 0.0, mid, long, thick, "SHIELD"),
        ],
    ));
    // A landing part per pin net, well outside the shield, so every net is
    // a real two-terminal net the router genuinely tries to route.
    let total = per_side * 4;
    for i in 0..total {
        let angle = std::f64::consts::TAU * i as f64 / total as f64;
        let (x, y) = (20.0 + 13.0 * angle.cos(), 20.0 + 13.0 * angle.sin());
        board.add_footprint(footprint(
            &format!("R{i}"),
            x,
            y,
            vec![
                pad("1", -0.8, 0.0, 0.9, 0.9, &format!("N{i}")),
                pad("2", 0.8, 0.0, 0.9, 0.9, "GND"),
            ],
        ));
    }
    board
}

#[test]
fn stranded_pads_are_reported() {
    let per_side = 2;
    let mut board = jammed_package(per_side);
    let report = route(
        &mut board,
        &RouteOptions {
            cell: Length::from_mm(0.20),
            trace_width: Length::from_mm(0.25),
            clearance: Length::from_mm(0.20),
            // The strand is decided before the search starts, so the budget
            // only bounds how long this test takes.
            max_seconds: Some(10.0),
            ..RouteOptions::default()
        },
    );

    assert!(
        !report.stranded_pads.is_empty(),
        "no pad reported as stranded on a package with no legal escape site: {report:?}"
    );
    // Every pin of the jammed package is doomed, and each is named the way
    // the rest of the report names pads: "REF.NUM".
    let mut expected: Vec<String> = (1..=per_side * 4).map(|n| format!("U1.{n}")).collect();
    expected.sort();
    let mut got = report.stranded_pads.clone();
    got.sort();
    assert_eq!(
        got, expected,
        "stranded list does not name exactly the jammed pins"
    );
    // The strand is a prediction, and it has to come true: a pin with no
    // barrel cannot be reached on any layer, so its net must be reported as
    // failed rather than quietly claimed.
    for i in 0..per_side * 4 {
        let net = format!("N{i}");
        let outcome = report
            .per_net
            .iter()
            .find(|(n, _)| *n == net)
            .map(|(_, o)| o);
        assert!(
            matches!(outcome, Some(Outcome::Failed { .. })),
            "net {net} has a stranded pad but was reported as {outcome:?}"
        );
    }

    // The strand is the FIRST thing the report says — ahead of every hint
    // the search produced about its own failures.
    let hint = report.hints.first().expect("no hints at all");
    assert!(
        hint.starts_with("escape:"),
        "first hint is not the escape verdict: {hint:?}"
    );
    for p in &expected {
        assert!(
            hint.contains(p.as_str()),
            "hint {hint:?} does not name stranded pad {p}"
        );
    }
}
