# Design: clearance rule areas + per-net clearance done right

Status: SPEC (2026-07-27, Fable). Implements step 3 of the "any PCB" sequence
(after O9/O8). Goal: make fine-pitch escape LEGAL where fabs allow it, instead
of near-miss DRC errors, and give the driving agent a first-class knob.

## Why

- The RP2040 stress board's 3–7 DRC errors are all 0.125–0.175 mm vs a flat
  0.20 mm rule. JLCPCB standard 2L allows 0.127 mm trace/space and 0.45/0.20 mm
  vias. Real fine-pitch designs use a tighter rule *near the package* and a
  relaxed rule elsewhere. Fragua only has one global clearance + per-class
  clearance that the grid effectively ignores (see next point).
- `RouteOptions.clearance` doc (router.rs): obstacles are inflated by the
  **max** clearance across all overrides — a finer class can never actually
  route finer. Per-net clearance must be honoured per searching net.

## Model (pcb-core)

```rust
pub struct RuleArea {
    pub id: Id,
    pub name: String,            // unique, script handle
    pub rect: Rect,              // world nm; polygon support later
    pub layers: LayerFilter,     // All | Only(Vec<Layer>)
    /// Absolute clearance override inside the area (mm). Overrides class
    /// and default outright — can relax (fine-pitch) or tighten (HV).
    pub clearance_mm: Option<f64>,
    /// Optional min trace width override inside the area (mm).
    pub trace_width_mm: Option<f64>,
    /// Optional via geometry override inside the area (mm).
    pub via_drill_mm: Option<f64>,
    pub via_diameter_mm: Option<f64>,
    /// Higher wins when areas overlap. Ties: smaller area wins (more specific).
    pub priority: i32,
}
```

Board gains `rule_areas: Vec<RuleArea>`.

## Resolution semantics (single source of truth)

One shared resolver in pcb-core, used by router, fanout, organic AND drc —
never re-derive the rule in two places:

```
required_clearance(net_a, net_b, point, layer):
    if let Some(area) = highest_priority_area_containing(point, layer)
         with clearance_mm set:
        return area.clearance_mm            // absolute override
    return max(class_clearance(net_a), class_clearance(net_b),
               board_default_clearance)     // strictest net wins
```

- Location = the point being evaluated (grid cell / DRC violation site), not
  item membership. Local, cheap, unambiguous for items that straddle borders.
- Same shape for `required_width` / via geometry, using the routing net only.

## Router integration (the hard part)

- Precompute a per-layer **clearance field**: quantize the board into the few
  distinct clearance values present (default + classes + areas). For each
  distinct value, stamp an obstacle mask inflated by that value. The A*/Theta*
  search for net N at cell c checks the mask for
  `required_clearance(N, obstacle_net, c, layer)` — resolved via the field,
  not the global max. Memory stays bounded: #distinct values is small (2–4).
- Fanout/dogbone candidate acceptance and stub laying consult the same
  resolver (this is where the QFN win comes from: denser dogbones + smaller
  vias inside the area).
- Organic post-pass clearance checks likewise.

## DRC integration

- Replace flat clearance checks with the resolver. A trace at 0.15 mm inside a
  0.13 mm area is LEGAL; the same trace outside is a violation.
- New check: rule-area values below the active fab preset's minimums
  (warning `RuleBelowFabLimit`).

## Script surface (agent DX)

```
rule-area NAME X1 Y1 X2 Y2 [layers=top|bottom|both] [clearance=N] [width=N]
          [via_drill=N] [via_dia=N] [priority=N]
rule-area-around REF NAME [margin=1.0] [clearance=N] ...   # bbox helper
list-rule-areas          # one per line, parseable, like list-lib
rule-area-remove NAME
```

Text replies list the effective values. `view`/`drc` mention active areas.

## Fab presets (small, ships with this feature)

```
fab-rules jlcpcb-2l   # sets board defaults + fab minimums used by DRC:
                      # trace/space 0.127, via drill 0.20, via dia 0.45
fab-rules jlcpcb-4l
```

Stored on the board; `pack fab=jlcpcb` warns if rules exceed capability.

## Acceptance

1. Resolver unit tests: default / class-vs-class max / area override relax /
   area override tighten / overlapping priorities / layer filter.
2. Router honours a finer class WITHOUT any area (fixes the max-inflation
   bug) — test with two nets of different classes in a corridor only the
   finer one fits through.
3. Stress board: `rule-area-around U1 fine margin=1.5 clearance=0.13
   via_drill=0.20 via_dia=0.45` → route → **DRC errors = 0** and
   fully-connected count strictly above the pre-area baseline.
4. `go test ./internal/core/ ./internal/router/ ./internal/drc/ ./internal/script/` green;
   module-class boards (coarse) route identically to before (no rule areas →
   behavior unchanged except the per-net clearance fix).
