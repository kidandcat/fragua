//! Render a `Schematic` to SVG.
//!
//! Symbol bodies are simple boxes with reference + value inside; pins
//! are 2.54 mm stubs poking out of each side, with the connected net
//! name (or pin name, if unconnected) as a label at the stub's tip.
//!
//! Nets with 2+ pins get orthogonal (manhattan) wire geometry so the
//! sheet reads as a real schematic, not an unconnected parts dump.
//!
//! The renderer never moves symbols — it draws what the agent placed.
//! Auto-placement (when the agent omits a position) happens in
//! `pcb-script` before the symbol enters the model.

use std::fmt::Write;

use pcb_core::{
    schematic::{PinSide, SchPin, Symbol, SymbolKind},
    Schematic,
};

const PIN_LEN_MM: f64 = 2.54;
const PIN_PITCH_MM: f64 = 2.54;
const DISCRETE_BODY_W_MM: f64 = 7.62; // 3 × 2.54
const DISCRETE_BODY_H_MM: f64 = 2.54;
const IC_BODY_W_MM: f64 = 12.7; // 5 × 2.54

/// Render `schematic` as an SVG document. Coordinates use SVG-default Y
/// going down (schematic editors traditionally do; matches KiCad's eeschema).
#[must_use]
pub fn render_schematic_svg(schematic: &Schematic) -> String {
    let view = view_box(schematic);
    let mut svg = String::with_capacity(8192);
    let _ = write!(
        svg,
        r#"<svg xmlns="http://www.w3.org/2000/svg" viewBox="{:.2} {:.2} {:.2} {:.2}" width="100%" height="100%">"#,
        view.0, view.1, view.2, view.3,
    );
    let _ = write!(
        svg,
        r##"<rect x="{:.2}" y="{:.2}" width="{:.2}" height="{:.2}" fill="#0e1116"/>"##,
        view.0, view.1, view.2, view.3,
    );
    // Light dot grid so the human can read pitch at a glance.
    write_grid(&mut svg, view);
    // Wires under symbols so bodies stay readable.
    write_net_wires(&mut svg, schematic);

    for sym in schematic.symbols_in_order() {
        write_symbol(&mut svg, schematic, sym);
    }
    svg.push_str("</svg>");
    svg
}

/// Returns (x, y, w, h) in mm. Falls back to a 100 × 70 mm sheet when
/// the schematic is empty.
fn view_box(schematic: &Schematic) -> (f64, f64, f64, f64) {
    if schematic.symbol_order.is_empty() {
        return (0.0, 0.0, 100.0, 70.0);
    }
    let mut min_x = f64::INFINITY;
    let mut min_y = f64::INFINITY;
    let mut max_x = f64::NEG_INFINITY;
    let mut max_y = f64::NEG_INFINITY;
    for sym in schematic.symbols_in_order() {
        let (bw, bh) = body_size(&sym.kind);
        let cx = sym.position.x.to_mm();
        let cy = sym.position.y.to_mm();
        // Include the pin stubs in the bbox so they don't get cropped.
        min_x = min_x.min(cx - bw / 2.0 - PIN_LEN_MM - 5.0);
        max_x = max_x.max(cx + bw / 2.0 + PIN_LEN_MM + 5.0);
        min_y = min_y.min(cy - bh / 2.0 - PIN_LEN_MM - 3.0);
        max_y = max_y.max(cy + bh / 2.0 + PIN_LEN_MM + 3.0);
    }
    (min_x, min_y, max_x - min_x, max_y - min_y)
}

fn body_size(kind: &SymbolKind) -> (f64, f64) {
    match kind {
        SymbolKind::Resistor
        | SymbolKind::Capacitor
        | SymbolKind::Inductor
        | SymbolKind::Led
        | SymbolKind::Diode => (DISCRETE_BODY_W_MM, DISCRETE_BODY_H_MM),
        SymbolKind::GenericIc { pins } => {
            let left = pins.iter().filter(|p| p.side == PinSide::Left).count();
            let right = pins.iter().filter(|p| p.side == PinSide::Right).count();
            let top = pins.iter().filter(|p| p.side == PinSide::Top).count();
            let bottom = pins.iter().filter(|p| p.side == PinSide::Bottom).count();
            #[allow(clippy::cast_precision_loss)]
            let h_pins = left.max(right) as f64;
            #[allow(clippy::cast_precision_loss)]
            let w_pins = top.max(bottom) as f64;
            let h = (h_pins.max(2.0)) * PIN_PITCH_MM + PIN_PITCH_MM;
            let w = (w_pins.max(2.0)) * PIN_PITCH_MM + IC_BODY_W_MM;
            (w, h)
        }
    }
}

fn write_grid(svg: &mut String, view: (f64, f64, f64, f64)) {
    let (vx, vy, vw, vh) = view;
    let step = PIN_PITCH_MM;
    let _ = write!(
        svg,
        r##"<g stroke="#1a1f27" stroke-width="0.05" fill="none">"##
    );
    let mut x = (vx / step).floor() * step;
    while x <= vx + vw {
        let _ = write!(
            svg,
            r#"<line x1="{:.2}" y1="{:.2}" x2="{:.2}" y2="{:.2}"/>"#,
            x,
            vy,
            x,
            vy + vh
        );
        x += step;
    }
    let mut y = (vy / step).floor() * step;
    while y <= vy + vh {
        let _ = write!(
            svg,
            r#"<line x1="{:.2}" y1="{:.2}" x2="{:.2}" y2="{:.2}"/>"#,
            vx,
            y,
            vx + vw,
            y
        );
        y += step;
    }
    svg.push_str("</g>");
}

fn write_symbol(svg: &mut String, schematic: &Schematic, sym: &Symbol) {
    let cx = sym.position.x.to_mm();
    let cy = sym.position.y.to_mm();
    let (bw, bh) = body_size(&sym.kind);
    let bx = cx - bw / 2.0;
    let by = cy - bh / 2.0;

    // Body box.
    let _ = write!(
        svg,
        r##"<rect x="{bx:.2}" y="{by:.2}" width="{bw:.2}" height="{bh:.2}" fill="#161b22" stroke="#7d8590" stroke-width="0.15"/>"##
    );

    // Reference designator above the symbol.
    let _ = write!(
        svg,
        r##"<text x="{:.2}" y="{:.2}" text-anchor="middle" font-family="ui-monospace, monospace" font-size="1.4" fill="#e6edf3">{}</text>"##,
        cx,
        by - 0.6,
        escape(&sym.reference)
    );
    // Value below the symbol.
    let _ = write!(
        svg,
        r##"<text x="{:.2}" y="{:.2}" text-anchor="middle" font-family="ui-monospace, monospace" font-size="1.2" fill="#8b949e">{}</text>"##,
        cx,
        by + bh + 1.6,
        escape(&sym.value)
    );

    let pins = sym.kind.pins();
    let mut idx_per_side = SideCounter::default();
    for pin in &pins {
        write_pin(
            svg,
            schematic,
            sym,
            pin,
            (bx, by, bw, bh),
            &mut idx_per_side,
        );
    }
}

#[derive(Default)]
struct SideCounter {
    left: usize,
    right: usize,
    top: usize,
    bottom: usize,
}

impl SideCounter {
    fn next(&mut self, side: PinSide) -> usize {
        let slot = match side {
            PinSide::Left => &mut self.left,
            PinSide::Right => &mut self.right,
            PinSide::Top => &mut self.top,
            PinSide::Bottom => &mut self.bottom,
        };
        let i = *slot;
        *slot += 1;
        i
    }
}

/// Tip of a pin stub in schematic space (mm), plus the body edge root.
fn pin_geometry(sym: &Symbol, pin: &SchPin, pin_index_on_side: usize) -> (f64, f64, f64, f64) {
    let cx = sym.position.x.to_mm();
    let cy = sym.position.y.to_mm();
    let (bw, bh) = body_size(&sym.kind);
    let bx = cx - bw / 2.0;
    let by = cy - bh / 2.0;
    #[allow(clippy::cast_precision_loss)]
    let i_f = pin_index_on_side as f64;
    match pin.side {
        PinSide::Left => {
            let y = by + PIN_PITCH_MM * (i_f + 1.0);
            (bx, y, bx - PIN_LEN_MM, y)
        }
        PinSide::Right => {
            let y = by + PIN_PITCH_MM * (i_f + 1.0);
            (bx + bw, y, bx + bw + PIN_LEN_MM, y)
        }
        PinSide::Top => {
            let x = bx + PIN_PITCH_MM * (i_f + 1.0);
            (x, by, x, by - PIN_LEN_MM)
        }
        PinSide::Bottom => {
            let x = bx + PIN_PITCH_MM * (i_f + 1.0);
            (x, by + bh, x, by + bh + PIN_LEN_MM)
        }
    }
}

/// Map each `(symbol_id, pin_number)` → index among pins on the same side
/// (same ordering as `write_pin` / `SideCounter`).
fn pin_side_indices(sym: &Symbol) -> std::collections::HashMap<String, usize> {
    let mut counts = SideCounter::default();
    let mut out = std::collections::HashMap::new();
    for pin in sym.kind.pins() {
        let i = counts.next(pin.side);
        out.insert(pin.number.clone(), i);
    }
    out
}

/// Draw orthogonal wires for every multi-pin net. Pins are chained by
/// nearest-neighbour so long power nets don't star into a spaghetti hub.
fn write_net_wires(svg: &mut String, schematic: &Schematic) {
    svg.push_str(r##"<g fill="none" stroke-linecap="round" stroke-linejoin="round">"##);

    // Precompute side indices per symbol once.
    let mut side_idx: std::collections::HashMap<
        pcb_core::Id,
        std::collections::HashMap<String, usize>,
    > = std::collections::HashMap::new();
    for sym in schematic.symbols_in_order() {
        side_idx.insert(sym.id, pin_side_indices(sym));
    }

    let mut net_names: Vec<&String> = schematic.nets.keys().collect();
    net_names.sort();

    for name in net_names {
        let Some(net) = schematic.nets.get(name) else {
            continue;
        };
        if net.connections.len() < 2 {
            continue;
        }

        let mut tips: Vec<(f64, f64)> = Vec::with_capacity(net.connections.len());
        for c in &net.connections {
            let Some(sym) = schematic.symbols.get(&c.symbol_id) else {
                continue;
            };
            let Some(pin) = sym
                .kind
                .pins()
                .into_iter()
                .find(|p| p.number == c.pin_number)
            else {
                continue;
            };
            let idx = side_idx
                .get(&c.symbol_id)
                .and_then(|m| m.get(&c.pin_number).copied())
                .unwrap_or(0);
            let (_sx, _sy, tip_x, tip_y) = pin_geometry(sym, &pin, idx);
            tips.push((tip_x, tip_y));
        }
        if tips.len() < 2 {
            continue;
        }

        // Nearest-neighbour order starting from left-most tip.
        let mut order = Vec::with_capacity(tips.len());
        let mut used = vec![false; tips.len()];
        let mut cur = tips
            .iter()
            .enumerate()
            .min_by(|a, b| {
                a.1 .0
                    .partial_cmp(&b.1 .0)
                    .unwrap_or(std::cmp::Ordering::Equal)
                    .then_with(|| {
                        a.1 .1
                            .partial_cmp(&b.1 .1)
                            .unwrap_or(std::cmp::Ordering::Equal)
                    })
            })
            .map(|(i, _)| i)
            .unwrap_or(0);
        order.push(cur);
        used[cur] = true;
        while order.len() < tips.len() {
            let (cx, cy) = tips[cur];
            let mut best = None;
            let mut best_d = f64::INFINITY;
            for (i, &(x, y)) in tips.iter().enumerate() {
                if used[i] {
                    continue;
                }
                let d = (x - cx).hypot(y - cy);
                if d < best_d {
                    best_d = d;
                    best = Some(i);
                }
            }
            let Some(n) = best else {
                break;
            };
            used[n] = true;
            order.push(n);
            cur = n;
        }

        let stroke = wire_color(name);
        let width = if is_power_net(name) { 0.28 } else { 0.18 };

        for w in order.windows(2) {
            let (x1, y1) = tips[w[0]];
            let (x2, y2) = tips[w[1]];
            write_manhattan(svg, x1, y1, x2, y2, stroke, width);
        }
    }

    svg.push_str("</g>");
}

fn is_power_net(name: &str) -> bool {
    let u = name.to_ascii_uppercase();
    u == "GND"
        || u == "VSS"
        || u == "VDD"
        || u == "VCC"
        || u == "VBUS"
        || u.starts_with('+')
        || u.starts_with("V") && u.chars().any(|c| c.is_ascii_digit())
        || u.contains("3V3")
        || u.contains("1V8")
        || u.contains("5V")
}

fn wire_color(name: &str) -> &'static str {
    if is_power_net(name) {
        "#3fb950" // phosphor green — power
    } else {
        "#58a6ff" // signal blue
    }
}

fn write_manhattan(svg: &mut String, x1: f64, y1: f64, x2: f64, y2: f64, stroke: &str, width: f64) {
    if (x1 - x2).abs() < 0.05 {
        let _ = write!(
            svg,
            r##"<line x1="{x1:.2}" y1="{y1:.2}" x2="{x2:.2}" y2="{y2:.2}" stroke="{stroke}" stroke-width="{width:.2}"/>"##
        );
        return;
    }
    if (y1 - y2).abs() < 0.05 {
        let _ = write!(
            svg,
            r##"<line x1="{x1:.2}" y1="{y1:.2}" x2="{x2:.2}" y2="{y2:.2}" stroke="{stroke}" stroke-width="{width:.2}"/>"##
        );
        return;
    }
    // Prefer horizontal-first then vertical (classic schematic habit).
    let _ = write!(
        svg,
        r##"<polyline points="{x1:.2},{y1:.2} {x2:.2},{y1:.2} {x2:.2},{y2:.2}" stroke="{stroke}" stroke-width="{width:.2}"/>"##
    );
}

fn write_pin(
    svg: &mut String,
    schematic: &Schematic,
    sym: &Symbol,
    pin: &SchPin,
    body: (f64, f64, f64, f64),
    counts: &mut SideCounter,
) {
    let i = counts.next(pin.side);
    let (start_x, start_y, end_x, end_y) = pin_geometry(sym, pin, i);
    let (bx, by, bw, bh) = body;
    let (label_x, label_y, label_anchor) = match pin.side {
        PinSide::Left => (end_x - 0.4, end_y + 0.4, "end"),
        PinSide::Right => (end_x + 0.4, end_y + 0.4, "start"),
        PinSide::Top => (end_x, end_y - 0.4, "middle"),
        PinSide::Bottom => (end_x, end_y + 1.2, "middle"),
    };

    // Pin stub line.
    let _ = write!(
        svg,
        r##"<line x1="{start_x:.2}" y1="{start_y:.2}" x2="{end_x:.2}" y2="{end_y:.2}" stroke="#c97a2b" stroke-width="0.2"/>"##
    );
    // Pin number, just inside the body next to the stub root.
    let (num_x, num_y, num_anchor) = match pin.side {
        PinSide::Left => (bx + 0.4, start_y - 0.2, "start"),
        PinSide::Right => (bx + bw - 0.4, start_y - 0.2, "end"),
        PinSide::Top => (start_x + 0.3, by + 1.0, "start"),
        PinSide::Bottom => (start_x + 0.3, by + bh - 0.4, "start"),
    };
    let _ = write!(
        svg,
        r##"<text x="{:.2}" y="{:.2}" text-anchor="{}" font-family="ui-monospace, monospace" font-size="0.7" fill="#8b949e">{}</text>"##,
        num_x,
        num_y,
        num_anchor,
        escape(&pin.number)
    );

    // Label at the tip: the net name if connected, otherwise the
    // human-readable pin name. Empty pin name + no net = nothing drawn.
    let label = schematic
        .net_for_pin(sym.id, &pin.number)
        .map_or_else(|| pin.name.clone(), str::to_string);
    if !label.is_empty() {
        let fill = if schematic.net_for_pin(sym.id, &pin.number).is_some() {
            "#3fb950"
        } else {
            "#8b949e"
        };
        let _ = write!(
            svg,
            r#"<text x="{:.2}" y="{:.2}" text-anchor="{}" font-family="ui-monospace, monospace" font-size="1.0" fill="{}">{}</text>"#,
            label_x,
            label_y,
            label_anchor,
            fill,
            escape(&label)
        );
    }
}

fn escape(s: &str) -> String {
    s.replace('&', "&amp;")
        .replace('<', "&lt;")
        .replace('>', "&gt;")
        .replace('"', "&quot;")
}
