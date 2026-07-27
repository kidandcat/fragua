//! Honest per-net clearance (spec `stress/DESIGN-clearance-rules.md`,
//! acceptance 2) and the router half of rule areas.
//!
//! The router used to size its search-time clearance disk from the
//! SEARCHING net's clearance alone, so a fine-class net could hug a
//! stricter neighbour: nothing in the grid knew who the copper on the
//! other side belonged to. The rule is pairwise — the strictest of the
//! two nets involved — with a rule area overriding it outright at the
//! point being evaluated. All three properties are pinned here on the
//! same corridor:
//!
//! ```text
//!   ┌──────────── wall (top) ─────────────┐
//!   A                                     B     ← the net under test
//!   └──────────── wall (bottom) ──────────┘
//! ```
//!
//! The gap is wide enough for a 0.1 mm rule and too narrow for a 0.5 mm
//! one, so "did it route" answers "which rule did the search use".

use std::sync::Arc;

use pcb_core::{
    Board, CopperLayer, Footprint, Id, Length, NetClass, Pad, Point, Rect, RuleArea, Schematic,
};
use pcb_router::{route, Outcome, RouteOptions};

const FINE_MM: f64 = 0.1;
const COARSE_MM: f64 = 0.5;

fn pad(num: &str, off_x: f64, off_y: f64, w: f64, h: f64, net: &str, th: bool) -> Pad {
    Pad {
        number: num.into(),
        name: String::new(),
        offset: Point::new(Length::from_mm(off_x), Length::from_mm(off_y)),
        size: (Length::from_mm(w), Length::from_mm(h)),
        layer: CopperLayer::Top,
        net: Some(net.to_string()),
        // Through-hole: the wall exists on every layer, so the search
        // cannot simply via around it and the test stays about clearance.
        drill: th.then(|| Length::from_mm(0.3)),
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

/// Board with a 1.2 mm corridor between two walls on `wall_class`, and a
/// two-pad net `SIG` in `sig_class` that has to get through it.
fn corridor_board() -> Board {
    let mut board = Board::new();
    board.outline = Some(Rect::from_corners(
        Point::new(Length::from_mm(0.0), Length::from_mm(0.0)),
        Point::new(Length::from_mm(24.0), Length::from_mm(8.0)),
    ));
    board.add_footprint(footprint(
        "A",
        3.0,
        4.0,
        vec![pad("1", 0.0, 0.0, 1.0, 1.0, "SIG", false)],
    ));
    board.add_footprint(footprint(
        "B",
        21.0,
        4.0,
        vec![pad("1", 0.0, 0.0, 1.0, 1.0, "SIG", false)],
    ));
    // Walls: each a single-pad net, so the router never tries to route
    // them; they exist purely as foreign copper. Gap = y 3.4 .. 4.6.
    board.add_footprint(footprint(
        "W1",
        12.0,
        6.5,
        vec![pad("1", 0.0, 0.0, 4.0, 3.8, "WALL_TOP", true)],
    ));
    board.add_footprint(footprint(
        "W2",
        12.0,
        1.5,
        vec![pad("1", 0.0, 0.0, 4.0, 3.8, "WALL_BOTTOM", true)],
    ));
    board
}

fn schematic(sig_mm: Option<f64>, wall_mm: Option<f64>) -> Schematic {
    let mut sch = Schematic::new();
    if let Some(mm) = sig_mm {
        sch.set_net_class(NetClass {
            name: "sig".into(),
            clearance_mm: Some(mm),
            ..NetClass::default()
        });
        sch.assign_net_to_class("SIG", "sig");
    }
    if let Some(mm) = wall_mm {
        sch.set_net_class(NetClass {
            name: "wall".into(),
            clearance_mm: Some(mm),
            ..NetClass::default()
        });
        sch.assign_net_to_class("WALL_TOP", "wall");
        sch.assign_net_to_class("WALL_BOTTOM", "wall");
    }
    sch
}

fn opts(sch: Schematic) -> RouteOptions {
    RouteOptions {
        cell: Length::from_mm(0.1),
        trace_width: Length::from_mm(0.2),
        // Board default = the fine rule, so a class can only make a net
        // STRICTER (per the spec's resolver) and the corridor is legal
        // for anything that stays at the default.
        clearance: Length::from_mm(FINE_MM),
        via_drill: Length::from_mm(0.3),
        via_diameter: Length::from_mm(0.6),
        schematic: Some(Arc::new(sch)),
        organic: false,
        max_seconds: Some(30.0),
        ..RouteOptions::default()
    }
}

fn sig_routed(board: &mut Board, opts: &RouteOptions) -> bool {
    let report = route(board, opts);
    report.per_net.iter().any(|(n, o)| {
        n == "SIG" && matches!(o, Outcome::Ok { trace_segments, .. } if *trace_segments >= 1)
    })
}

#[test]
fn fine_net_fits_the_corridor_a_coarse_one_does_not() {
    // Everything at the fine default → through it goes.
    let mut board = corridor_board();
    assert!(
        sig_routed(&mut board, &opts(schematic(None, None))),
        "a 0.1 mm rule fits a 1.2 mm corridor"
    );

    // Same corridor, same geometry, but SIG's own class demands 0.5 mm.
    let mut board = corridor_board();
    assert!(
        !sig_routed(&mut board, &opts(schematic(Some(COARSE_MM), None))),
        "a 0.5 mm rule cannot fit a 1.2 mm corridor"
    );
}

#[test]
fn a_fine_net_still_honours_a_stricter_neighbours_rule() {
    // THE BUG: SIG is fine (0.1 mm) but the copper forming the corridor
    // belongs to a 0.5 mm class. The required gap is the strictest of the
    // two, so SIG must NOT fit — the old model asked only SIG's own class
    // and squeezed through.
    let mut board = corridor_board();
    assert!(
        !sig_routed(&mut board, &opts(schematic(Some(FINE_MM), Some(COARSE_MM)))),
        "the neighbour's 0.5 mm class must be honoured by the searching net"
    );
}

#[test]
fn a_rule_area_relaxes_the_corridor_for_everyone_inside_it() {
    // Coarse class both sides → blocked (previous test).
    // Add a rule area over the corridor: inside it the rule is 0.1 mm
    // absolute, so the same search now gets through.
    let mut board = corridor_board();
    let mut area = RuleArea::new(
        "escape",
        Rect::from_corners(
            Point::new(Length::from_mm(8.0), Length::from_mm(2.0)),
            Point::new(Length::from_mm(16.0), Length::from_mm(6.0)),
        ),
    );
    area.clearance_mm = Some(FINE_MM);
    board.set_rule_area(area);
    assert!(
        sig_routed(
            &mut board,
            &opts(schematic(Some(COARSE_MM), Some(COARSE_MM)))
        ),
        "the area's clearance overrides both classes inside it"
    );
}
