//! Design-rule areas and the single clearance/width resolver every
//! consumer (router, fanout, organic, DRC) must go through.
//!
//! Two things live here:
//!
//! - [`RuleArea`] — a rectangular region of the board that overrides the
//!   clearance / trace width / via geometry inside it. Real designs use a
//!   tighter rule near a fine-pitch package (so a 0.4 mm QFN escape is
//!   *legal* instead of a near-miss) and a relaxed one elsewhere; the
//!   inverse (a high-voltage moat demanding MORE clearance) is the same
//!   mechanism with a bigger number.
//! - [`RuleResolver`] — the one place the rule is derived. Never re-derive
//!   it in a consumer: the router's grid, the fanout pass and DRC must
//!   agree to the nanometre or the router lays copper DRC then rejects.
//!
//! Resolution semantics (see `stress/DESIGN-clearance-rules.md`):
//!
//! ```text
//! required_clearance(net_a, net_b, point, layer):
//!     if let Some(area) = highest_priority_area_containing(point, layer)
//!          with clearance_mm set:
//!         return area.clearance_mm          // absolute override
//!     return max(class_clearance(net_a), class_clearance(net_b),
//!                board_default_clearance)   // strictest net wins
//! ```
//!
//! Location — not item membership — decides which area applies: the point
//! being evaluated (a grid cell, a DRC violation site, a candidate via
//! centre). That keeps the answer local, cheap and unambiguous for copper
//! that straddles an area border.

use std::collections::HashMap;

use serde::{Deserialize, Serialize};

use crate::board::{CopperLayer, Id};
use crate::geometry::{Point, Rect};
use crate::schematic::Schematic;
use crate::units::Length;

/// A rectangular design-rule override.
///
/// `None` fields fall through to the class/board default, so an area can
/// override only what it cares about (typically just `clearance_mm`).
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct RuleArea {
    pub id: Id,
    /// Unique, agent-facing handle (`rule-area-remove NAME`).
    pub name: String,
    /// Region in board coordinates. Polygons may come later; a rect is
    /// what "the area around this package" needs.
    pub rect: Rect,
    /// Copper layers the area applies to. Empty = every layer (same
    /// convention as [`crate::board::Keepout::layers`]).
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub layers: Vec<CopperLayer>,
    /// Absolute clearance override inside the area (mm). Overrides the
    /// class and the board default outright — it can relax (fine pitch)
    /// or tighten (high voltage).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub clearance_mm: Option<f64>,
    /// Minimum / preferred trace width inside the area (mm).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub trace_width_mm: Option<f64>,
    /// Via drill inside the area (mm).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub via_drill_mm: Option<f64>,
    /// Via copper diameter inside the area (mm).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub via_diameter_mm: Option<f64>,
    /// Higher wins when areas overlap. Ties break toward the SMALLER
    /// area — the more specific statement about that spot.
    #[serde(default)]
    pub priority: i32,
}

impl RuleArea {
    /// Empty area named `name` covering `rect` on every layer.
    #[must_use]
    pub fn new(name: impl Into<String>, rect: Rect) -> Self {
        Self {
            id: Id::new(),
            name: name.into(),
            rect,
            layers: Vec::new(),
            clearance_mm: None,
            trace_width_mm: None,
            via_drill_mm: None,
            via_diameter_mm: None,
            priority: 0,
        }
    }

    /// True if the area governs `layer`. `None` = the caller's check is
    /// not layer-specific (pad pairs across layers, a board-wide query),
    /// in which case any layer filter still matches.
    #[must_use]
    pub fn covers_layer(&self, layer: Option<CopperLayer>) -> bool {
        if self.layers.is_empty() {
            return true;
        }
        match layer {
            None => true,
            Some(l) => self.layers.contains(&l),
        }
    }

    /// True if `p` lies inside the area (border inclusive) on `layer`.
    #[must_use]
    pub fn contains(&self, p: Point, layer: Option<CopperLayer>) -> bool {
        self.covers_layer(layer)
            && p.x.0 >= self.rect.min.x.0
            && p.x.0 <= self.rect.max.x.0
            && p.y.0 >= self.rect.min.y.0
            && p.y.0 <= self.rect.max.y.0
    }

    /// Area of the rect in nm², used for the "smaller wins" tie-break.
    #[must_use]
    pub fn area_nm2(&self) -> i128 {
        i128::from(self.rect.width().0) * i128::from(self.rect.height().0)
    }

    /// True if the area declares no override at all — a no-op the
    /// script layer rejects rather than silently storing.
    #[must_use]
    pub fn is_empty_override(&self) -> bool {
        self.clearance_mm.is_none()
            && self.trace_width_mm.is_none()
            && self.via_drill_mm.is_none()
            && self.via_diameter_mm.is_none()
    }
}

/// Fab capability floor adopted by the board (`fab-rules jlcpcb-2l`).
///
/// This is what the fab will *accept*, not what the design wants: DRC
/// gates every minimum-style check against it, and rule areas that go
/// below it raise `RuleBelowFabLimit`. Deliberately does NOT loosen the
/// design's default clearance — re-ruling a whole board to the fab's
/// floor is a decision the designer makes explicitly, not a side effect
/// of naming a fab.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct FabRules {
    /// Preset key (`jlcpcb-2l`, `jlcpcb-4l`, ...).
    pub preset: String,
    pub min_trace_width_mm: f64,
    pub min_clearance_mm: f64,
    pub min_via_drill_mm: f64,
    pub min_via_diameter_mm: f64,
    pub min_annular_ring_mm: f64,
    pub min_edge_clearance_mm: f64,
    pub max_board_size_mm: (f64, f64),
}

impl FabRules {
    /// Built-in presets. Numbers are each fab's published standard-tier
    /// capability (JLCPCB 2024-2025): 0.127 mm (5 mil) trace/space,
    /// 0.20 mm drill, 0.45 mm via pad.
    #[must_use]
    pub fn preset(name: &str) -> Option<Self> {
        let key = name.to_ascii_lowercase();
        match key.as_str() {
            "jlcpcb-2l" | "jlcpcb_2l" | "jlcpcb-2" | "jlcpcb" | "jlc" => Some(Self {
                preset: "jlcpcb-2l".into(),
                min_trace_width_mm: 0.127,
                min_clearance_mm: 0.127,
                min_via_drill_mm: 0.20,
                min_via_diameter_mm: 0.45,
                min_annular_ring_mm: 0.13,
                min_edge_clearance_mm: 0.20,
                max_board_size_mm: (100.0, 100.0),
            }),
            // JLCPCB's 4-layer standard tier is slightly tighter on
            // trace/space (3.5 mil) and keeps the same drill floor.
            "jlcpcb-4l" | "jlcpcb_4l" | "jlcpcb-4" => Some(Self {
                preset: "jlcpcb-4l".into(),
                min_trace_width_mm: 0.0889,
                min_clearance_mm: 0.0889,
                min_via_drill_mm: 0.20,
                min_via_diameter_mm: 0.45,
                min_annular_ring_mm: 0.13,
                min_edge_clearance_mm: 0.20,
                max_board_size_mm: (100.0, 100.0),
            }),
            _ => None,
        }
    }

    /// Names accepted by [`FabRules::preset`], for error text.
    #[must_use]
    pub fn preset_names() -> &'static [&'static str] {
        &["jlcpcb-2l", "jlcpcb-4l"]
    }
}

/// Call-site defaults the resolver falls back to. Mirrors the fields of
/// `RouteOptions` / `DrcOptions` so both crates can build one.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct RuleDefaults {
    pub clearance: Length,
    pub trace_width: Length,
    pub via_diameter: Length,
    pub via_drill: Length,
}

impl Default for RuleDefaults {
    fn default() -> Self {
        Self {
            clearance: Length::from_mm(0.2),
            trace_width: Length::from_mm(0.25),
            via_diameter: Length::from_mm(0.6),
            via_drill: Length::from_mm(0.3),
        }
    }
}

/// The one resolver. Borrowed cheaply by every consumer; build it once
/// per route pass / DRC run.
#[derive(Debug, Clone)]
pub struct RuleResolver<'a> {
    areas: &'a [RuleArea],
    schematic: Option<&'a Schematic>,
    /// Legacy per-net clearance overrides (the `net_overrides` maps in
    /// `pcb-router` / `pcb-drc`). Merged with the schematic classes by
    /// the same "strictest wins" rule.
    net_clearance: Option<&'a HashMap<String, Length>>,
    defaults: RuleDefaults,
}

/// Empty slice used when a consumer has no board (unit tests, callers
/// that only want class resolution).
const NO_AREAS: &[RuleArea] = &[];

impl<'a> RuleResolver<'a> {
    #[must_use]
    pub fn new(areas: &'a [RuleArea], defaults: RuleDefaults) -> Self {
        Self {
            areas,
            schematic: None,
            net_clearance: None,
            defaults,
        }
    }

    /// Resolver with no rule areas — class + defaults only.
    #[must_use]
    pub fn classes_only(defaults: RuleDefaults) -> Self {
        Self::new(NO_AREAS, defaults)
    }

    #[must_use]
    pub fn with_schematic(mut self, schematic: Option<&'a Schematic>) -> Self {
        self.schematic = schematic;
        self
    }

    #[must_use]
    pub fn with_net_clearance(mut self, map: Option<&'a HashMap<String, Length>>) -> Self {
        self.net_clearance = map;
        self
    }

    #[must_use]
    pub fn defaults(&self) -> RuleDefaults {
        self.defaults
    }

    #[must_use]
    pub fn areas(&self) -> &'a [RuleArea] {
        self.areas
    }

    #[must_use]
    pub fn has_areas(&self) -> bool {
        !self.areas.is_empty()
    }

    /// Highest-priority area containing `p` on `layer`; ties break
    /// toward the smaller (more specific) rect, then the earlier one so
    /// the answer is deterministic.
    #[must_use]
    pub fn area_at(&self, p: Point, layer: Option<CopperLayer>) -> Option<&'a RuleArea> {
        let mut best: Option<&RuleArea> = None;
        for a in self.areas {
            if !a.contains(p, layer) {
                continue;
            }
            best = match best {
                None => Some(a),
                Some(b) => {
                    if (a.priority, -a.area_nm2()) > (b.priority, -b.area_nm2()) {
                        Some(a)
                    } else {
                        Some(b)
                    }
                }
            };
        }
        best
    }

    /// Highest-priority area at `p` that actually sets `field`. An area
    /// that overrides only the clearance must not shadow a lower-priority
    /// area's via override — each field resolves independently.
    fn area_with<T>(
        &self,
        p: Point,
        layer: Option<CopperLayer>,
        field: impl Fn(&RuleArea) -> Option<T>,
    ) -> Option<(&'a RuleArea, T)> {
        let mut best: Option<(&RuleArea, T)> = None;
        for a in self.areas {
            if !a.contains(p, layer) {
                continue;
            }
            let Some(v) = field(a) else { continue };
            best = match best {
                None => Some((a, v)),
                Some((b, bv)) => {
                    if (a.priority, -a.area_nm2()) > (b.priority, -b.area_nm2()) {
                        Some((a, v))
                    } else {
                        Some((b, bv))
                    }
                }
            };
        }
        best
    }

    /// Clearance a net's class demands, if any (schematic class first,
    /// then the legacy override map — strictest of the two).
    #[must_use]
    pub fn class_clearance(&self, net: &str) -> Option<Length> {
        let mut out: Option<Length> = None;
        if let Some(sch) = self.schematic {
            if let Some(mm) = sch.class_for(net).clearance_mm {
                out = Some(Length::from_mm(mm));
            }
        }
        if let Some(map) = self.net_clearance {
            if let Some(l) = map.get(net) {
                out = Some(match out {
                    Some(c) if c.0 >= l.0 => c,
                    _ => *l,
                });
            }
        }
        out
    }

    /// **The** clearance rule. `net_a` / `net_b` are the two nets whose
    /// copper is being separated (`None` = copper with no net, e.g. an
    /// NC pad — it demands the default but no class). `p` is the point
    /// being evaluated.
    #[must_use]
    pub fn clearance(
        &self,
        net_a: Option<&str>,
        net_b: Option<&str>,
        p: Point,
        layer: Option<CopperLayer>,
    ) -> Length {
        if let Some((_, mm)) = self.area_with(p, layer, |a| a.clearance_mm) {
            return Length::from_mm(mm);
        }
        self.pair_clearance(net_a, net_b)
    }

    /// The class/default half of the rule, ignoring rule areas. Exposed
    /// so the router can precompute a per-net-pair table once and only
    /// consult the (positional) area field per cell.
    #[must_use]
    pub fn pair_clearance(&self, net_a: Option<&str>, net_b: Option<&str>) -> Length {
        let mut c = self.defaults.clearance;
        for n in [net_a, net_b].into_iter().flatten() {
            if let Some(l) = self.class_clearance(n) {
                if l.0 > c.0 {
                    c = l;
                }
            }
        }
        c
    }

    /// Trace width for `net` at `p`. Area override wins, then the net's
    /// class, then the default.
    #[must_use]
    pub fn trace_width(&self, net: Option<&str>, p: Point, layer: Option<CopperLayer>) -> Length {
        if let Some((_, mm)) = self.area_with(p, layer, |a| a.trace_width_mm) {
            return Length::from_mm(mm);
        }
        net.and_then(|n| self.schematic?.class_for(n).trace_width_mm)
            .map_or(self.defaults.trace_width, Length::from_mm)
    }

    /// Via copper diameter for `net` at `p`.
    #[must_use]
    pub fn via_diameter(&self, net: Option<&str>, p: Point, layer: Option<CopperLayer>) -> Length {
        if let Some((_, mm)) = self.area_with(p, layer, |a| a.via_diameter_mm) {
            return Length::from_mm(mm);
        }
        net.and_then(|n| self.schematic?.class_for(n).via_diameter_mm)
            .map_or(self.defaults.via_diameter, Length::from_mm)
    }

    /// Via drill for `net` at `p`.
    #[must_use]
    pub fn via_drill(&self, net: Option<&str>, p: Point, layer: Option<CopperLayer>) -> Length {
        if let Some((_, mm)) = self.area_with(p, layer, |a| a.via_drill_mm) {
            return Length::from_mm(mm);
        }
        net.and_then(|n| self.schematic?.class_for(n).via_drill_mm)
            .map_or(self.defaults.via_drill, Length::from_mm)
    }

    /// Largest clearance any point on the board can demand. Sizes the
    /// router's search-time clearance disk so it never undersells a
    /// tightening area.
    #[must_use]
    pub fn max_clearance(&self, nets: &[&str]) -> Length {
        let mut c = self.defaults.clearance;
        for n in nets {
            if let Some(l) = self.class_clearance(n) {
                if l.0 > c.0 {
                    c = l;
                }
            }
        }
        for a in self.areas {
            if let Some(mm) = a.clearance_mm {
                let l = Length::from_mm(mm);
                if l.0 > c.0 {
                    c = l;
                }
            }
        }
        c
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::schematic::NetClass;

    fn pt(x: f64, y: f64) -> Point {
        Point::new(Length::from_mm(x), Length::from_mm(y))
    }

    fn rect(x1: f64, y1: f64, x2: f64, y2: f64) -> Rect {
        Rect::from_corners(pt(x1, y1), pt(x2, y2))
    }

    fn schematic_with_classes() -> Schematic {
        let mut sch = Schematic::new();
        sch.set_net_class(NetClass {
            name: "hv".into(),
            clearance_mm: Some(0.4),
            ..NetClass::default()
        });
        sch.set_net_class(NetClass {
            name: "fine".into(),
            clearance_mm: Some(0.1),
            ..NetClass::default()
        });
        sch.assign_net_to_class("HV", "hv");
        sch.assign_net_to_class("SIG", "fine");
        sch
    }

    #[test]
    fn default_clearance_when_no_class_and_no_area() {
        let r = RuleResolver::classes_only(RuleDefaults::default());
        assert_eq!(
            r.clearance(Some("A"), Some("B"), pt(1.0, 1.0), None),
            Length::from_mm(0.2)
        );
    }

    #[test]
    fn class_pair_takes_the_strictest_of_both_nets() {
        let sch = schematic_with_classes();
        let r = RuleResolver::classes_only(RuleDefaults::default()).with_schematic(Some(&sch));
        // HV (0.4) vs unclassed: 0.4 wins over the 0.2 default.
        assert_eq!(
            r.clearance(Some("HV"), Some("PLAIN"), pt(1.0, 1.0), None),
            Length::from_mm(0.4)
        );
        // A finer class can never go below the board default on its own —
        // only a rule area relaxes.
        assert_eq!(
            r.clearance(Some("SIG"), Some("PLAIN"), pt(1.0, 1.0), None),
            Length::from_mm(0.2)
        );
        // Fine vs HV: the strict net wins (this is the pairwise rule the
        // router used to ignore).
        assert_eq!(
            r.clearance(Some("SIG"), Some("HV"), pt(1.0, 1.0), None),
            Length::from_mm(0.4)
        );
    }

    #[test]
    fn area_relaxes_absolutely_inside_and_not_outside() {
        let mut a = RuleArea::new("fine", rect(10.0, 10.0, 20.0, 20.0));
        a.clearance_mm = Some(0.13);
        let areas = vec![a];
        let sch = schematic_with_classes();
        let r = RuleResolver::new(&areas, RuleDefaults::default()).with_schematic(Some(&sch));
        // Inside: absolute override, even against the 0.4 mm HV class.
        assert_eq!(
            r.clearance(Some("HV"), Some("SIG"), pt(15.0, 15.0), None),
            Length::from_mm(0.13)
        );
        // Outside: back to the pairwise class rule.
        assert_eq!(
            r.clearance(Some("HV"), Some("SIG"), pt(25.0, 15.0), None),
            Length::from_mm(0.4)
        );
        // On the border: inclusive.
        assert_eq!(
            r.clearance(None, None, pt(20.0, 20.0), None),
            Length::from_mm(0.13)
        );
    }

    #[test]
    fn area_can_tighten_too() {
        let mut a = RuleArea::new("hv-moat", rect(0.0, 0.0, 5.0, 5.0));
        a.clearance_mm = Some(1.0);
        let areas = vec![a];
        let r = RuleResolver::new(&areas, RuleDefaults::default());
        assert_eq!(
            r.clearance(Some("A"), Some("B"), pt(2.0, 2.0), None),
            Length::from_mm(1.0)
        );
    }

    #[test]
    fn overlapping_areas_resolve_by_priority_then_size() {
        let mut big = RuleArea::new("big", rect(0.0, 0.0, 30.0, 30.0));
        big.clearance_mm = Some(0.3);
        let mut small = RuleArea::new("small", rect(10.0, 10.0, 12.0, 12.0));
        small.clearance_mm = Some(0.15);
        let areas = vec![big.clone(), small.clone()];
        let r = RuleResolver::new(&areas, RuleDefaults::default());
        // Same priority: the smaller (more specific) area wins.
        assert_eq!(
            r.clearance(None, None, pt(11.0, 11.0), None),
            Length::from_mm(0.15)
        );

        // Explicit priority beats size.
        let mut big_hi = big.clone();
        big_hi.priority = 5;
        let areas = vec![big_hi, small];
        let r = RuleResolver::new(&areas, RuleDefaults::default());
        assert_eq!(
            r.clearance(None, None, pt(11.0, 11.0), None),
            Length::from_mm(0.3)
        );
    }

    #[test]
    fn layer_filter_scopes_the_area() {
        let mut a = RuleArea::new("top-only", rect(0.0, 0.0, 10.0, 10.0));
        a.clearance_mm = Some(0.1);
        a.layers = vec![CopperLayer::TOP];
        let areas = vec![a];
        let r = RuleResolver::new(&areas, RuleDefaults::default());
        assert_eq!(
            r.clearance(None, None, pt(5.0, 5.0), Some(CopperLayer::TOP)),
            Length::from_mm(0.1)
        );
        assert_eq!(
            r.clearance(None, None, pt(5.0, 5.0), Some(CopperLayer::Bottom)),
            Length::from_mm(0.2)
        );
        // Layer-agnostic queries still see it (most permissive match).
        assert_eq!(
            r.clearance(None, None, pt(5.0, 5.0), None),
            Length::from_mm(0.1)
        );
    }

    #[test]
    fn fields_resolve_independently_across_areas() {
        let mut clr = RuleArea::new("clr", rect(0.0, 0.0, 10.0, 10.0));
        clr.clearance_mm = Some(0.12);
        clr.priority = 10;
        let mut vias = RuleArea::new("vias", rect(0.0, 0.0, 20.0, 20.0));
        vias.via_diameter_mm = Some(0.45);
        vias.via_drill_mm = Some(0.2);
        let areas = vec![clr, vias];
        let r = RuleResolver::new(&areas, RuleDefaults::default());
        let p = pt(5.0, 5.0);
        assert_eq!(r.clearance(None, None, p, None), Length::from_mm(0.12));
        // The higher-priority area sets no via geometry, so the other
        // area's override still applies instead of the default 0.6.
        assert_eq!(r.via_diameter(None, p, None), Length::from_mm(0.45));
        assert_eq!(r.via_drill(None, p, None), Length::from_mm(0.2));
    }

    #[test]
    fn width_and_via_fall_back_to_class_then_default() {
        let mut sch = Schematic::new();
        sch.set_net_class(NetClass {
            name: "power".into(),
            trace_width_mm: Some(0.5),
            via_diameter_mm: Some(0.8),
            ..NetClass::default()
        });
        sch.assign_net_to_class("VBUS", "power");
        let r = RuleResolver::classes_only(RuleDefaults::default()).with_schematic(Some(&sch));
        let p = pt(1.0, 1.0);
        assert_eq!(r.trace_width(Some("VBUS"), p, None), Length::from_mm(0.5));
        assert_eq!(r.via_diameter(Some("VBUS"), p, None), Length::from_mm(0.8));
        assert_eq!(r.trace_width(Some("SIG"), p, None), Length::from_mm(0.25));
        assert_eq!(r.via_drill(Some("SIG"), p, None), Length::from_mm(0.3));
    }

    #[test]
    fn max_clearance_covers_classes_and_areas() {
        let mut a = RuleArea::new("moat", rect(0.0, 0.0, 5.0, 5.0));
        a.clearance_mm = Some(0.5);
        let areas = vec![a];
        let sch = schematic_with_classes();
        let r = RuleResolver::new(&areas, RuleDefaults::default()).with_schematic(Some(&sch));
        assert_eq!(r.max_clearance(&["HV", "SIG"]), Length::from_mm(0.5));
        let r = RuleResolver::classes_only(RuleDefaults::default()).with_schematic(Some(&sch));
        assert_eq!(r.max_clearance(&["HV", "SIG"]), Length::from_mm(0.4));
    }

    #[test]
    fn board_round_trips_rule_areas_and_fab_rules() {
        use crate::board::Board;
        let mut board = Board::new();
        let mut a = RuleArea::new("fine", rect(10.0, 10.0, 20.0, 20.0));
        a.clearance_mm = Some(0.13);
        a.via_drill_mm = Some(0.2);
        a.via_diameter_mm = Some(0.45);
        a.layers = vec![CopperLayer::TOP];
        a.priority = 3;
        board.set_rule_area(a.clone());
        board.fab_rules = FabRules::preset("jlcpcb-2l");

        let json = serde_json::to_string(&board).expect("serialise");
        let back: Board = serde_json::from_str(&json).expect("deserialise");
        assert_eq!(back.rule_areas, vec![a]);
        assert_eq!(back.fab_rules, board.fab_rules);

        // Re-declaring the same name edits in place instead of stacking.
        let mut b = RuleArea::new("fine", rect(0.0, 0.0, 5.0, 5.0));
        b.clearance_mm = Some(0.2);
        board.set_rule_area(b);
        assert_eq!(board.rule_areas.len(), 1);
        assert!(board.remove_rule_area("fine"));
        assert!(!board.remove_rule_area("fine"));
    }

    #[test]
    fn boards_saved_before_rule_areas_still_load() {
        use crate::board::Board;
        // A pre-feature project file: no `rule_areas`, no `fab_rules`.
        let legacy = r#"{"outline":null,"footprints":{},"footprint_order":[],
                         "traces":[],"vias":[]}"#;
        let board: Board = serde_json::from_str(legacy).expect("legacy board must load");
        assert!(board.rule_areas.is_empty());
        assert!(board.fab_rules.is_none());
        // And a board without areas serialises without the new keys, so
        // existing files stay byte-identical.
        let json = serde_json::to_string(&board).expect("serialise");
        assert!(!json.contains("rule_areas"), "{json}");
        assert!(!json.contains("fab_rules"), "{json}");
    }

    #[test]
    fn fab_presets_resolve_by_alias() {
        assert_eq!(FabRules::preset("jlcpcb-2l").unwrap().preset, "jlcpcb-2l");
        assert_eq!(FabRules::preset("JLCPCB-4L").unwrap().preset, "jlcpcb-4l");
        assert!(FabRules::preset("nope").is_none());
    }
}
