# fragua — Architecture

This document maps [VISION.md](VISION.md) onto a concrete Rust workspace, an
in-process data flow, and the implementation as it stands (v1.1.x).

## Process model

One process. One Tauri app. Inside it:

- A **shared in-memory project** (`pcb-core::Project`) — single source of
  truth for the schematic, board, nets, design rules, stackup, and routing
  state.
- A **local HTTP API task** on `127.0.0.1:7878` (override: `FRAGUA_API_ADDR`)
  — agents and humans run script verbs through `POST /script`; the server is
  stateless beyond the live project. Also: `GET /` and `GET /help` (usage +
  script reference), `GET /health`, `GET /screenshot`, `POST /save`.
- A **Tauri command surface** — JS-callable handlers that read the same
  project and stream change events to the frontend (including JLCPCB pack,
  ODB++ `.tgz`, and GA autoroute via `pcb-router-tune`).
- A **frontend** (Vite + TS) — renders the project, listens for change
  events, and sends user actions (drag, library review, export) back through
  Tauri commands, which mutate the project and notify any waiting agent.

The agent and the human edit the same `Project`. Every mutation, regardless
of source, emits a change event consumed by the UI.

CLI: **`fragua run [file.fragua]`** launches the app. Bare `fragua` (or
`fragua help`) prints usage + the full script reference and exits so agents
can discover the surface without starting the server.

## Workspace layout

```
pcb/   (repo: mentasystems/fragua)
├── Cargo.toml             workspace manifest (version 1.1.x)
├── rust-toolchain.toml    pinned toolchain
├── crates/
│   ├── pcb-core/          project model, geometry, units, ids, change events
│   ├── pcb-router/        autorouting (grid + topo engines, fanout, organic)
│   ├── pcb-router-tune/   GA / random search over router + placement genes
│   ├── pcb-placer/        global ePlace + SA legalisation + edge planning
│   ├── pcb-drc/           design rule check (geometry-based)
│   ├── pcb-erc/           electrical rule check (schematic-side)
│   ├── pcb-fab/           fab provider abstraction (JLCPCB / PCBWay / Generic)
│   ├── pcb-gerber/        RS-274X + Excellon writer + BOM/CPL CSV
│   ├── pcb-odb/           ODB++ v8 subset → .tgz (JLCPCB-ingestible)
│   ├── pcb-render/        SVG / PNG render (board + schematic)
│   └── pcb-script/        line-oriented DSL + tool dispatch + reference docs
├── src-tauri/             Tauri binary crate (host + HTTP API)
├── frontend/              Vite + TypeScript UI
├── stress/                RP2040 open-hardware stress campaign
├── docs/                  GitHub Pages landing
├── VISION.md
├── ARCHITECTURE.md
└── README.md
```

## Crate responsibilities

### `pcb-core`
The model. Owns:
- Units (mm, fixed-point internal representation, `Length(i64)` in nm).
- Geometry primitives (point, segment, rect, polygonal shapes with cutouts).
- `Project { schematic, board, library, save_path }` plus an event bus.
- `Schematic { symbols, nets, net_classes, sheets }` — symbols carry pins with
  electrical roles (Passive / Input / Output / Bidir / PowerOut / PowerIn).
- `Board { footprints, traces, vias, pours, silk, outline, outline_corner_radius,
  cutouts, holes, keepouts, rule_areas, stackup, … }`.
- Design-rule areas and fab rule floors (`RuleArea`, `FabRules`, shared
  `RuleResolver` for router / fanout / organic / DRC).
- `Library` — disk-backed component catalogue with attachments (photos /
  datasheets), `lcsc_id` / `mpn`, body rects, edge-mount side, elevated flag.
- `Hershey` stroke font for silkscreen text; thermal reliefs; pour stitching.

No I/O on the critical path. File save/load lives behind `Project::load_from_path`
/ `save_to_path` and is content-based (JSON), so legacy `.json` and the
canonical `.fragua` extension both load.

### `pcb-router`
Auto-routing. Receives a `Board`, produces traces and vias.

- **Default engine:** multi-source Theta\* / A\* on a discretised multi-layer
  grid, bend penalty and via cost, rip-up-and-reroute, optional PathFinder
  negotiated congestion (`route negotiate=true`), Steiner-style reuse of
  same-net copper.
- **Fine-pitch path:** dogbone fanout + global escape-slot matching
  (`slots.rs`), reachability-proved plan legalisation (`reach.rs`),
  rip-and-reassign lever between RR&R passes.
- **Organic post-pass:** rubber-band string-pull + arc fillets, clearance-
  checked; rolled back if it would make the finished board illegal.
- **Experimental topological engine** (`route engine=topo`): homotopy A\*
  over a Delaunay dual with layers as graph moves, rubber-band geometric
  realisation, exact clearance validator as commit authority.
- Per-net and rule-area overrides for `trace_width` / `clearance` / vias.
- Wall-clock budget (`max_seconds`) and progress reporting for agents.

### `pcb-placer`
Two-stage footprint placer:

1. **Global (ePlace-style):** weighted wirelength gradient + spectral /
   Poisson density field, per-part trust region, 90° rotation probing.
2. **Legalisation:** simulated annealing on HPWL + soft body gap + congestion
   proxy + bundle-crossing term; best-seen clamp so a run never returns a
   worse-than-input layout.

Also: body-aware **edge planning** (`edge-plan` / `edge-place`) for
`edge_mounted` parts (which side faces the outline), **decoupling-ring**
seating for 2-pad passives at IC power pins, **elevated** bodies (modules
on headers may shadow flat parts). Deterministic for a fixed seed; global
stage RNG-free.

### `pcb-drc`
Geometric design rule check over a `Board`: clearance (per-net + rule areas),
track width, drill sizes, via annular ring, edge clearance, unconnected pads,
routing efficiency, body-off-board / body-overlap (edge-side and elevated
aware), pads on NPTH holes, bodies in milled cutouts, castellated/slot-edge
pads legal on cutout edges. Emits violations with positions for the UI.

### `pcb-erc`
Schematic-side validation. Strict checks: floating pin/net, duplicate pin,
empty net, orphan symbol, phantom net. Role-based: multiple drivers,
unpowered power net, undriven input. Heuristic (default on): missing
decoupling cap near a PowerIn pin, missing pull-up on I²C-named nets.

### `pcb-fab`
Fab-house provider abstraction. `Provider { Jlcpcb, Pcbway, Generic }` with
per-house `FabRules`, BOM and CPL formatters, and `pack(project, provider,
out_dir)` that runs ERC + DRC + manufacturing-DRC and ships a single `.zip`.

### `pcb-gerber`
Manufacturing output. One Gerber file per copper/mask/silk/edge layer
(RS-274X), Excellon drill files (plated and non-plated), generic BOM and
pick-and-place CSV. Rounded outlines emit arcs; polygonal Edge.Cuts and
cutouts are supported.

### `pcb-odb`
ODB++ exporter — minimum viable subset of the v8.0 job tree that JLCPCB’s
uploader accepts. Emits a gzipped tar (`.tgz`) with stackup, copper features,
drill, outline, silk, soldermask. Invoked from the Tauri UI
(`export_odb_pack`); not yet a first-class script verb.

### `pcb-render`
Board and schematic rendering. SVG for the webview (pan/zoom, click handlers);
PNG for `GET /screenshot` and the `screenshot` script verb. Substrate (rounded
or polygonal), cutouts, copper, vias, pads, silk, DRC markers, photo overlays.
Silk text that would clip the outline is auto-relocated.

### `pcb-script`
The agent surface. Single line-oriented DSL: `verb args [kv=val]`, indented
sub-lines for blocks (`lib`/`sym`). The parser produces `Cmd { tool, args }`
records; `dispatch` routes them through the workspace. `script_reference()`
is printed at startup and served at `GET /` / `GET /help`. Notable verbs:
placement (`place`, `auto-place`, `edge-place`, `edge-plan`, `compact`),
routing (`route`, `trace`, `via`, …), geometry (`outline`, `outline-poly`,
`cutout`, `hole`, `keepout`), rules (`rule-area*`, `fab-rules`), stackup /
layers, library review (`confirm-lib`, `list-pending`, …), `pack` / `export`.

### `pcb-router-tune`
Genetic algorithm (and random search) over router genes (cell pitch, via cost,
clearance) and footprint rotations. Shared by a CLI binary and the desktop
“Auto Routing” button (stoppable mid-search).

### `src-tauri`
Tauri binary. Owns the `Project`, hosts the HTTP API, registers Tauri commands
for the frontend (state, DRC, route, autoroute GA, JLCPCB zip, ODB++ tgz,
library calibration, …), forwards events. Launch gated on `run` subcommand.

### `frontend`
TypeScript + Vite. SVG canvas (board / schematic / library review), activity
log, palette, DRC list, export buttons. Default panes are tab-driven from the
topbar.

## Data flow: a typical agent action

1. Agent sends `POST /script` with a multi-line script body.
2. The HTTP handler in `src-tauri` dispatches each line via
   `pcb_script::tools::dispatch`.
3. Each tool validates inputs and calls a `Project` mutator (or router /
   placer / pack entry point).
4. `Project` emits the matching `Event`.
5. The Tauri event pump forwards each event to the webview.
6. The frontend re-fetches `project_state` and repaints.
7. If the human drags a footprint, the frontend uses the same
   `Project::move_footprint_to` API the script tools use; the agent sees the
   new position on the next `view` / `snap` / `status` call.

## Where we ended up vs the original plan

The plan documented an MCP server as the primary surface. We ran it that
way for a while, then dropped it: the agent the user runs already has
tool-call primitives, and a stateless local HTTP endpoint replying in
`text/plain` was easier to use than MCP framing. The endpoint lives on
`127.0.0.1:7878` and speaks plain HTTP.

The script DSL exists for the same reason — small surface area, one verb
per concept, deterministic parsing. The agent reasons about the design;
the script just commits each step.

## Implementation phases (historical, for context)

1. Skeleton + pcb-core data model.
2. Stateless HTTP API replacing the original MCP server.
3. Pcb-gerber → first end-to-end fab pack from a placed-only board.
4. Pcb-drc with the core geometric checks.
5. Pcb-router (A\* → RR&R + negotiated congestion + Steiner-ish multi-source
   → fine-pitch fanout / escape slots / reachability / organic).
6. Footprint silk + library attachments + photos.
7. Net classes + per-net trace_width / clearance; rule areas + fab-rules.
8. Pin roles + role-based ERC; heuristic ERC (decoupling, I²C pull-ups).
9. Pcb-placer (SA → two-stage ePlace + SA, edge-plan, decoupling ring,
   elevated bodies).
10. Pcb-fab provider abstraction + manufacturing-DRC + zip pack.
11. Rounded board outlines + silk-text relocation.
12. Multi-layer stackup, compact verb, stress campaign (RP2040) through v9.
13. Polygonal outlines, milled cutouts, NPTH mount holes, castellated pads.
14. ODB++ exporter (`pcb-odb`) + GA router tuner (`pcb-router-tune`).
15. Headless `GET /screenshot` and `screenshot` script verb.

Each step kept the human-visible end-to-end demo working — no long
construction phase with nothing to show.
