//! Tool surface — what the agent calls (over the local HTTP API).
//!
//! Each tool is intentionally thin: parse the input, call into
//! `pcb-core` to mutate the project, return the result. The agent owns
//! all the design reasoning; tools are pure data primitives.

use std::collections::HashMap;

use pcb_core::schematic::{Net, NetConnection, PinSide, SchPin, Symbol, SymbolKind};
use pcb_core::{
    ActivityLevel, CopperLayer, Footprint, FootprintSilk, Length, LibrarySilk, Pad, Point, Pour,
    Project, SilkAnchor, SilkLayer, SilkLine, SilkText, Trace, Via,
};
use serde::Deserialize;
use serde_json::{json, Value};

// Internal error markers (kept compatible with JSON-RPC numeric codes
// so callers that already understood them keep working).
pub mod error_code {
    pub const METHOD_NOT_FOUND: i64 = -32601;
    pub const INVALID_PARAMS: i64 = -32602;
    pub const INTERNAL_ERROR: i64 = -32603;
}

use error_code::INVALID_PARAMS;

pub struct ToolError {
    pub code: i64,
    pub message: String,
}

impl ToolError {
    pub fn invalid_params(msg: impl Into<String>) -> Self {
        Self {
            code: INVALID_PARAMS,
            message: msg.into(),
        }
    }
}

/// Snapshot the library-key → placement-margin lookup the renderer and
/// DRC consume. Cheap: one pass over the library entries; library
/// reads are RwLock-shared so concurrent tools don't block each other.
/// Keys with all-zero margins are omitted so the renderer can skip the
/// outline draw cheaply via `HashMap::get`.
fn build_placement_margin_map(project: &Project) -> pcb_render::PlacementMarginMap {
    let mut out = pcb_render::PlacementMarginMap::default();
    for entry in project.library().list() {
        if entry.placement_margin.is_zero() {
            continue;
        }
        out.insert(entry.key, entry.placement_margin);
    }
    out
}

/// `HashMap<String, PlacementMargin>` flavour suitable for DRC, which
/// keeps its own copy in `DrcOptions`. Mirrors
/// `build_placement_margin_map` so both consumers see the same set of
/// margins.
fn build_drc_margin_map(
    project: &Project,
) -> std::collections::HashMap<String, pcb_core::PlacementMargin> {
    let mut out = std::collections::HashMap::new();
    for entry in project.library().list() {
        if entry.placement_margin.is_zero() {
            continue;
        }
        out.insert(entry.key, entry.placement_margin);
    }
    out
}

/// Reference for the `script` action language — every verb the agent
/// can put on a line. Served verbatim at `GET /` of the local API.
pub const SCRIPT_REFERENCE: &str = "Run a multi-line PCB design script — the ONLY surface you need. \
The script is plain text, one action per line; multi-line blocks (`sym`, `lib`) \
take indented sub-lines (`pin`, `pad`). Strings with spaces use double quotes; \
trailing key=value pairs override defaults; `#` starts a comment.\n\
\n\
=== EXAMPLE ===\
reset\n\
outline 90 30 radius=2                    # rounded corners\n\
\n\
class ground pour=both                    # GND on top + bottom plane\n\
class power width=0.4                     # +3V3 with wider traces\n\
\n\
sym U1 ic key=esp32_s3_zero desc=\"ESP32 main MCU; USB-C edge\"\n\
  pin 1 L V5     role=power_in\n\
  pin 2 L GND    role=power_in\n\
  pin 3 L 3V3    role=power_in\n\
  pin 4 L TX     role=output\n\
sym C1 capacitor key=c_0603 value=100nF desc=\"HF decoupling near U1.3V3\"\n\
\n\
net GND  U1.GND U2.GND_1 C1.2 class=ground\n\
net +3V3 U1.3V3 U2.VCC   C1.1 class=power\n\
\n\
erc                                       # catch netlist bugs early\n\
\n\
palette U1 esp32_s3_zero rot=90\n\
palette C1 c_0603 value=100nF\n\
place U1 11.5 15 90\n\
place C1 48 14\n\
auto-place C1 seed=42                     # SA on the parts you don't pin\n\
route                                     # auto-pour materialises ground first\n\
=== END EXAMPLE ===\n\
\n\
=== ACTIONS (verb args) ===\n\
\n\
PROJECT / READS:\n\
  reset                                        — wipe schematic, board, palette\n\
  status                                       — terse project summary (footprint count, name)\n\
  view                                         — full board summary: outline + nets + DRC counts (NO svg)\n\
  snap                                         — full SVG + structured pad/trace data (heavy)\n\
  sch                                          — schematic SVG\n\
  sch-status                                   — schematic counts + unconnected pins\n\
  nets                                         — per-net pad-by-pad connection report\n\
  list-lib                                     — every library entry (key, desc, pad count, attachments).\n\
                                                 Text reply lists one key per line so agents can parse it\n\
                                                 without JSON (HTTP surface is text/plain).\n\
  list-pending                                 — library entries queued for confirmation (not on disk yet)\n\
  confirm-lib KEY                              — promote a pending entry to the on-disk library.\n\
                                                 Agents MAY call this after self-checking pad numbering\n\
                                                 / chirality (esp. pin-1 corner and mirrored modules).\n\
                                                 Prefer attaching a photo first when one exists.\n\
  discard-pending KEY                          — drop a pending entry without saving it\n\
  list-palette                                 — items waiting in the palette\n\
  save PATH                                    — write the project to PATH (atomic .tmp+rename).\n\
                                                 Use this when fragua was launched with no file\n\
                                                 argument (no autosave); afterwards re-launch with\n\
                                                 `fragua PATH` to keep autosaving.\n\
  screenshot PATH [view=board|schematic]       — rasterise the current project to a PNG on disk.\n\
            [width=PX]                           Same content the webview shows (board SVG or\n\
                                                 schematic SVG), rendered headlessly via resvg —\n\
                                                 no OS permission needed. Default view=board,\n\
                                                 width=1600 (max 8192). Also exposed as\n\
                                                 `GET /screenshot[?view=...&width=...]` returning\n\
                                                 `image/png` for direct curl.\n\
\n\
BOARD:\n\
  outline W H [radius=R]                       — set Edge.Cuts rectangle in mm. Optional uniform\n\
                                                 corner radius (mm) rounds all four corners; default\n\
                                                 0 = sharp. Clamped to min(W, H) / 2.\n\
\n\
LIBRARY (build first, reuse forever):\n\
  lib KEY [value=V] [rot=N] [edge=true|false] [desc=\"...\"] [lcsc=Cxxxx] [mpn=...]\n\
    pad NUMBER X Y W H [name=NAME] [drill=MM]  — repeat for every pad\n\
    # `drill` = presence makes the pad a plated through-hole (a bare pad\n\
    #   with no `drill` is SMD). Value is the hole diameter in mm; it must\n\
    #   be > 0, below both pad dimensions (annular ring), and >= 0.2 (fab\n\
    #   min). A 2.54 mm header pin is typically drill=1.0.\n\
    # `lcsc` = LCSC catalogue ID (e.g. C25804 for 10k 0603). Required\n\
    #   for JLCPCB SMT assembly to know what part to load. Optional\n\
    #   but strongly recommended once the part is real.\n\
    # `mpn` = manufacturer part number (e.g. RC0603FR-0710KL). Carries\n\
    #   into the BOM as a fallback when no `lcsc` is set.\n\
    silk-line LAYER X1 Y1 X2 Y2 [width=N]      — body outline / pin-1 marker, in footprint-local mm.\n\
                                                 Same syntax as the top-level verb, but coords are\n\
                                                 relative to the footprint origin and follow it when\n\
                                                 placed/rotated.\n\
    silk-text LAYER X Y \"TEXT\" [size=N] [rot=N] [anchor=start|middle|end] [width=N]\n\
                                               — footprint-local text. Use \"{REF}\" / \"{VAL}\" to\n\
                                                 emit the placed instance's reference / value\n\
                                                 (e.g. one library entry can ship `silk-text top 0 3 \"{REF}\"`\n\
                                                 and every spawn renders \"U1\", \"U2\", ...).\n\
  attach KEY KIND PATH                         — file from disk; mime auto-detected\n\
                                                 KIND is free text: photo / datasheet / note / ...\n\
  detach KEY ATTACHMENT_ID\n\
  calibrate-photo KEY ATT AX AY BX BY PAD_A PAD_B\n\
                                               — pin a top-down photo to the footprint via two\n\
                                                 correspondences. ATT = attachment id (a unique id\n\
                                                 prefix or the filename also work). AX AY / BX BY are\n\
                                                 the RAW-image pixel coords (origin top-left, y-down)\n\
                                                 of two pin-centre marks; PAD_A / PAD_B are the pad\n\
                                                 numbers those marks sit on. Reports the derived scale\n\
                                                 (mm/px) and the implied photo width in mm. Pick two\n\
                                                 pads as far apart as possible for accuracy.\n\
  rectify-photo KEY ATT X1 Y1 X2 Y2 X3 Y3 X4 Y4 W_MM H_MM\n\
                                               — ID-scanner-style crop + deskew. Removes the\n\
                                                 perspective distortion of a handheld photo so the\n\
                                                 overlay is exact. ATT resolves like calibrate-photo\n\
                                                 (id prefix / filename). X1..Y4 are the module's four\n\
                                                 BOARD corners in RAW source-image pixels (y-down),\n\
                                                 given TL,TR,BR,BL as they should appear in the OUTPUT\n\
                                                 (this order sets the rectified orientation — make it\n\
                                                 match the footprint TOP view: pin-1 corner at the\n\
                                                 output top-left). W_MM/H_MM are the board's real\n\
                                                 size (use the entry's body-rect dims). Writes a NEW\n\
                                                 `photo-rectified` attachment at 40 px/mm and, if the\n\
                                                 source photo was calibrated, remaps the calibration\n\
                                                 onto it (should come out axis-aligned; the residual\n\
                                                 from square is reported — > 2° means the corners or\n\
                                                 their order are wrong). Rectified attachments take\n\
                                                 priority over plain photos in the board overlay.\n\
  edge-mount KEY true|false|top|right|bottom|left\n\
                                               — library entry must sit on board outline\n\
                                                 (screw terminals, USB modules, edge headers).\n\
                                                 A side names WHICH local side (Y-up pad frame)\n\
                                                 must face the outline — the wire-entry side of a\n\
                                                 terminal, the plug face of a USB module. Placement\n\
                                                 then rejects wrong orientations, auto-place rotates\n\
                                                 the part to honour it, and the DRC allows the body\n\
                                                 to overhang the cut line on that side only.\n\
                                                 auto-place re-syncs placed footprints from this\n\
                                                 flag and snaps them to the nearest edge.\n\
  body-rect KEY MINX MINY MAXX MAXY            — physical module body in footprint-local mm (Y-up,\n\
                                                 same frame as `pad`). Stores the body rect AND\n\
                                                 auto-derives the per-side placement margin (how far\n\
                                                 the body overhangs the pad bbox) so placer/DRC/render\n\
                                                 respect the true footprint. `body-rect KEY clear`\n\
                                                 drops it.\n\
  # calibrate-photo / rectify-photo / body-rect target CONFIRMED entries\n\
  #   only: a fresh `lib` sits in the review queue and its `attach`ed\n\
  #   photos have no ids until a human confirms. Recipe for a\n\
  #   photo-calibrated, perspective-corrected part:\n\
  #     lib mymod ...pads...        # queue for review\n\
  #     attach mymod photo top.jpg  # staged on the pending entry\n\
  #   → confirm in the review pane, THEN:\n\
  #     calibrate-photo mymod top.jpg 412 980 1631 980 1 8   # two pins one known pitch apart\n\
  #     body-rect mymod -3.0 -2.5 3.0 2.5\n\
  #     # optional but recommended for handheld photos — crop+deskew so the\n\
  #     # overlay is exact (measure the 4 board corners in the source photo):\n\
  #     rectify-photo mymod top.jpg 120 90 1840 60 1900 1520 90 1560 6.0 5.0\n\
  #     # the rectified attachment auto-inherits the calibration; if it\n\
  #     # reports no calibration, calibrate-photo the new attachment.\n\
  delete-lib KEY\n\
  find-lib KEY                                 — full record + pads + silk\n\
  # Library example with body outline + pin 1 dot + auto-ref label:\n\
  #   lib so8 desc=\"SO-8 IC\"\n\
  #     pad 1 -1.905 1.27 1.55 0.6\n\
  #     pad 2 -1.905 0    1.55 0.6\n\
  #     ...\n\
  #     silk-line top -2.5 -2.5  2.5 -2.5\n\
  #     silk-line top  2.5 -2.5  2.5  2.5\n\
  #     silk-line top  2.5  2.5 -2.5  2.5\n\
  #     silk-line top -2.5  2.5 -2.5 -2.5\n\
  #     silk-text top -2.0  2.0 \"*\" size=0.6   # pin-1 dot\n\
  #     silk-text top  0    3.5 \"{REF}\" size=1.0\n\
\n\
SCHEMATIC:\n\
  sym REF KIND [key=K] [value=V] [rot=N] [x=N] [y=N] [desc=\"...\"]\n\
    pin NUMBER SIDE [NAME] [role=ROLE]         — only for KIND=ic; SIDE = L|R|T|B (or full names).\n\
                                                 ROLE = passive (default) | input (in) | output (out)\n\
                                                 | bidir (io) | power_out (power, pwr) | power_in (pwr_in).\n\
                                                 ERC uses roles to catch shorts the geometry can't: 2+\n\
                                                 outputs on one net = error, PowerIn pins on a net with no\n\
                                                 PowerOut source = warning, Input pin with no driver = warning.\n\
                                                 Discretes (R, C, L, LED, D) are always passive, no need to set.\n\
                                                 KIND aliases: ic, r, c, l, led, d\n\
  net NAME PIN1 PIN2 ... [class=NAME]          — PIN = REF.PIN_NUMBER or REF.PIN_NAME (case-insensitive).\n\
                                                 `class` attaches a net class (see below) so the\n\
                                                 router/DRC use its trace_width / clearance for\n\
                                                 this net.\n\
  class NAME [width=N] [clearance=N] [pour=top|bottom|both]\n\
                                               — declare or replace a net class. Set on a net via\n\
                                                 `net NAME ... class=NAME`. Unset fields fall back\n\
                                                 to the route/drc defaults at the call site. `pour`\n\
                                                 makes every net in the class ride a copper pour on\n\
                                                 the chosen layer(s) instead of routed traces; the\n\
                                                 router skips those nets. `pour=both` is the\n\
                                                 standard GND-on-both-layers pattern that connects\n\
                                                 same-net pads regardless of which side they sit on.\n\
\n\
DESIGN-RULE AREAS (make fine pitch LEGAL, not near-miss):\n\
  rule-area NAME X1 Y1 X2 Y2 [clearance=N] [width=N] [via_drill=N] [via_dia=N]\n\
           [layers=top|bottom|both] [priority=N]\n\
                                               — rectangular rule override. INSIDE the rect the\n\
                                                 clearance is absolute: it overrides both nets'\n\
                                                 classes and the board default, so it can relax\n\
                                                 (0.4 mm-pitch QFN escape) or tighten (HV moat).\n\
                                                 Router, fanout/dogbone, organic pass and DRC all\n\
                                                 read the same rule, so what routes is what passes.\n\
                                                 Outside any area the rule is the strictest of the\n\
                                                 two nets' classes and the default. Overlaps: higher\n\
                                                 `priority` wins, ties go to the smaller rect.\n\
                                                 Re-declaring a NAME edits it in place.\n\
  rule-area-around REF NAME [margin=1.0] [clearance=N] ...\n\
                                               — same, sized to a placed footprint's pad bbox plus\n\
                                                 `margin` mm. This is the fine-pitch-escape helper:\n\
                                                 `rule-area-around U1 fine margin=1.5 clearance=0.13`.\n\
  list-rule-areas                              — one parseable line per area + the adopted fab rules\n\
  rule-area-remove NAME\n\
  fab-rules jlcpcb-2l|jlcpcb-4l|clear|list     — adopt a fab's capability floor (persisted with the\n\
                                                 project). DRC gates every minimum against it, the\n\
                                                 fanout drops the smallest via it allows, and any\n\
                                                 rule area below it raises a RuleBelowFabLimit\n\
                                                 warning. It does NOT loosen your design defaults —\n\
                                                 re-ruling a whole board is an explicit decision.\n\
\n\
PALETTE / PLACEMENT:\n\
  palette REF KEY [rot=N] [value=V] [layer=top|bottom]\n\
                                               — spawn a palette item from a library entry; the\n\
                                                 schematic must already have a symbol with REF.\n\
                                                 The entry's `footprint_view_transform` (set via the\n\
                                                 review pane's flip/rotate buttons) is baked into the\n\
                                                 spawned pad geometry and silk: the native library\n\
                                                 data stays untouched in index.json, but the placed\n\
                                                 footprint matches what the user saw in the review.\n\
                                                 The optional `rot=` is then layered on top of that\n\
                                                 view transform, same as `place X Y ROT`.\n\
  clear-palette\n\
  place REF X Y [ROT_DEG]                      — drop palette item at (x, y) mm; rejects if it\n\
  edge-place REF left|right|top|bottom [along=N]\n\
                                               — place an edge-mounted palette item on a board\n\
                                                 outline edge. Computes rotation (so the library\n\
                                                 `edge-mount` side faces the outline) and the snap\n\
                                                 coordinate automatically. `along` is the free-axis\n\
                                                 position (x for top/bottom, y for left/right);\n\
                                                 defaults to centred. Prefer this over hand-tuned\n\
                                                 `place` for USB / headers / terminals.\n\
                                                 overlaps another footprint or violates the\n\
                                                 edge_mounted constraint\n\
  move REF X Y\n\
  rotate REF DEG                               — absolute rotation, multiples of 90 recommended\n\
  delete REF [REF ...]                         — remove placed footprint(s) by ref; also drops every\n\
                                                 trace / via whose endpoint landed on one of their\n\
                                                 pads. Errors if any REF is not on the board; warns\n\
                                                 (in the reply) for nets that lose their last pad.\n\
  clear-board                                  — drop every placed footprint AND all routing;\n\
                                                 outline / silk / schematic / library are kept.\n\
                                                 Useful after editing a library entry: clear-board\n\
                                                 then re-spawn from the palette to pick up the\n\
                                                 updated geometry.\n\
  edge-plan REF [REF...] [seed=N]               — planning pass ONLY (no SA): for each edge-mounted\n\
                                                 ref, choose WHICH outline edge and where along it,\n\
                                                 minimising wirelength + bundle crossings, and\n\
                                                 spread parts sharing an edge. Reports the side +\n\
                                                 position per ref. Runs automatically inside\n\
                                                 auto-place; use it alone to fix connector sides\n\
                                                 without moving anything else.\n\
  auto-place REF [REF...] [iters=N] [seed=N] [max_step=N] [min_step=N] [min_gap=N] [solder_gap=N] [gap_penalty=N] [congestion=N] [congestion_res=N]\n\
             [crossing=N] [edge_plan=true|false] [global=true|false] [global_iters=N] [bins=N] [density=N] [overflow=N]\n\
                                               — two-stage placer over the listed refs: an\n\
                                                 electrostatic global stage (ePlace-style: smooth\n\
                                                 wirelength gradient + Poisson density field, with\n\
                                                 90° rotation probing) finds the structure, then\n\
                                                 simulated annealing legalises and polishes.\n\
                                                 Pinned refs (everything not listed) stay put.\n\
                                                 Optimises HPWL + a soft body-to-body gap penalty\n\
                                                 + a congestion proxy (how many net pad-bboxes\n\
                                                 share the same routing cell; cells in the escape\n\
                                                 ring of a fine-pitch package count double)\n\
                                                 + a bundle-crossing penalty (`crossing`, mm per\n\
                                                 crossing: a header wired 1:1 to an IC ends up in\n\
                                                 matching pin order, not criss-crossed). Movable\n\
                                                 edge-mounted parts get their edge chosen first\n\
                                                 (see edge-plan; edge_plan=false to skip). Obeys outline +\n\
                                                 edge_mounted; hard-rejects pad overlap. Defaults:\n\
                                                 global=true (global_iters=600, bins=64,\n\
                                                 density=1.0, overflow=0.08),\n\
                                                 iters=8000 (~3 s for ~20 components), seed=clock,\n\
                                                 max_step=20 mm, min_gap=2.0 mm, gap_penalty=16,\n\
                                                 congestion=1, congestion_res=32, crossing=2,\n\
                                                 edge_plan=true. solder_gap=1.0 mm\n\
                                                 is the HARD floor: parts never end up closer than\n\
                                                 this so you can get a soldering iron between them\n\
                                                 (set solder_gap=0 for the old 0.5 mm floor).\n\
                                                 Bump congestion if SA produces tight HPWL but the\n\
                                                 router struggles; set congestion_res=0 to disable.\n\
  compact [min_w=MM] [min_h=MM] [step=MM] [seed=N] [iters=N] [aspect=keep|free] [min_gap=MM] [solder_gap=MM]\n\
          [allow_failed=N] [route_seconds=N]\n\
                                               — shrink the board outline / pack parts as tightly as\n\
                                                 the design allows while staying manufacturable. For\n\
                                                 every candidate size it re-places ALL footprints,\n\
                                                 re-routes and re-runs DRC; a size is kept ONLY when\n\
                                                 it routes with 0 failed nets AND 0 DRC errors. Binary-\n\
                                                 searches a uniform scale (aspect=keep, default), then\n\
                                                 greedily trims each axis (aspect=free trims axes\n\
                                                 independently). Never shrinks below the geometric\n\
                                                 floor (widest/tallest part + edge clearance) or the\n\
                                                 min_w/min_h you set. Keeps the corner radius; snaps\n\
                                                 edge_mounted parts to the new edge; pulls board silk\n\
                                                 inside. Defaults: step=1.0 mm, seed=1, iters=8000,\n\
                                                 aspect=keep, solder_gap=1.0 mm (hard hand-solder\n\
                                                 access floor — bodies never pack closer than this).\n\
                                                 allow_failed=N (default 0) tolerates N unrouted nets\n\
                                                 per candidate — use it on a board that never fully\n\
                                                 routes (set N to the failures it already has) so it\n\
                                                 can still shrink; the DRC gate then ignores the\n\
                                                 NetSplit opens those nets imply but still demands 0\n\
                                                 clearance/edge/short errors. route_seconds=N (default\n\
                                                 30) is the router budget PER candidate: total wall\n\
                                                 clock ≈ checks × route_seconds, so raise it for\n\
                                                 fine-pitch boards and expect the run to take longer.\n\
                                                 Rule areas declared with rule-area-around follow\n\
                                                 their part; plain rule-area rects and keepouts are\n\
                                                 translated with the copper. Same seed → same result.\n\
                                                 Run it AFTER auto-place + route; if no smaller size\n\
                                                 is feasible the board is left untouched.\n\
\n\
ROUTING:\n\
  route [trace_width=N] [clearance=N] [via_drill=N] [via_diameter=N] [via_cost=N] [cell=N]\n\
            [max_seconds=N] [organic=true|false] [fillet=MM] [engine=grid|topo]\n\
            [fine_escape=true|false] [negotiate=true|false]\n\
                                               — auto-route every net (Theta* on 2 layers), then an\n\
                                                 organic post-pass (rubber-band string-pulling +\n\
                                                 arc fillets, TopoR-style flowing traces; clearance\n\
                                                 checked, DRC-neutral). organic=false for raw grid\n\
                                                 geometry; fillet caps the arc radius (default 3).\n\
                                                 engine=topo is the TOPOLOGICAL engine: homotopy\n\
                                                 search over a Delaunay dual with rubber-band\n\
                                                 realisation and targeted rip-up — free-angle\n\
                                                 curved copper, vias planned where the topology\n\
                                                 needs them (experimental; may leave nets unrouted\n\
                                                 on very tight boards — rerun with engine=grid to\n\
                                                 fill in). max_seconds caps wall-clock (default 90;\n\
                                                 0 = unlimited) and streams progress into the\n\
                                                 activity log. Defaults trace_width=0.25,\n\
                                                 clearance=0.20, via_drill=0.30, via_diameter=0.60,\n\
                                                 cell=0.20, via_cost=8. fine_escape=true opts into\n\
                                                 the fine-grid stub escape on 3+ layer stackups\n\
                                                 (off by default: on a 0.4 mm QFN it costs budget\n\
                                                 and loses to the dogbone fanout).\n\
                                                 negotiate=true swaps the rip-up-and-reroute driver\n\
                                                 for PathFinder-style NEGOTIATED CONGESTION: every\n\
                                                 net takes its shortest path first, then corridors\n\
                                                 they fight over get progressively more expensive\n\
                                                 until the nets with an alternative detour, so the\n\
                                                 routing ORDER stops deciding who gets the good\n\
                                                 lane. Converges to legal copper or falls back to\n\
                                                 the classic passes with the budget it has left\n\
                                                 (best board wins either way). Off by default: it\n\
                                                 pays on boards whose wall is inter-net contention,\n\
                                                 and on the RP2040 stress board it is a diagnosis\n\
                                                 rather than a cure — it reports which corridors\n\
                                                 stay over-subscribed and which nets cannot route\n\
                                                 even when allowed to share every foreign trace\n\
                                                 (see the route hints)\n\
  clear-route                                  — drop all traces and vias\n\
  clear-net NET                                — clear one net's traces/vias\n\
  trace top|bottom NET X1 Y1 X2 Y2 [width=N]   — manual trace segment\n\
  via NET X Y [drill=N] [diameter=N]           — manual via\n\
  delete-trace ID                              — id from `snap` structured data\n\
  delete-via ID\n\
  auto-pour                                    — materialise a `Pour` for every net whose class has\n\
                                                 `pour=...` set (run implicitly by `route` too).\n\
  pour NET top|bottom                          — declare a copper pour (ground/power plane); pads\n\
                                                 of NET on that layer count as connected without\n\
                                                 a routed trace. Cross-layer pads still need a via.\n\
                                                 Drop a `pour GND bottom` early on dense boards so\n\
                                                 the router does not have to thread GND everywhere.\n\
  clear-pour NET top|bottom                    — remove a pour\n\
\n\
SILK:\n\
  Two scopes: BOARD-level silk lives directly on the board (frames,\n\
  version markings, logos, fiducial labels) — use the top-level verbs\n\
  below. FOOTPRINT-level silk lives on a library entry and follows the\n\
  spawned footprint when it moves/rotates — author it under a `lib`\n\
  block (see LIBRARY above).\n\
  silk-line LAYER X1 Y1 X2 Y2 [width=N]        — silkscreen segment in mm; LAYER = top|bottom;\n\
                                                 default width 0.15 mm.\n\
  silk-text LAYER X Y \"TEXT\" [size=1.2] [rot=0] [anchor=start|middle|end] [width=...]\n\
                                               — silkscreen text vectorised through the built-in\n\
                                                 stroke font. ASCII printable only; default size\n\
                                                 1.2 mm cap height, default stroke ~size/8.\n\
\n\
VALIDATION / EXPORT:\n\
  erc                                          — electrical rules check (schematic side):\n\
                                                 floating pin/net, duplicate pin assignment,\n\
                                                 empty net, orphan symbol, phantom net (board\n\
                                                 pad on a net the schematic doesn't declare).\n\
                                                 Run before placement to catch netlist bugs early.\n\
  drc [clearance=N] [edge=N] [trace_width=N] [drill=N]\n\
                                               — design rules check (board side); defaults\n\
                                                 clearance=0.20, edge=0.30, trace_width=0.10,\n\
                                                 drill=0.20\n\
  export DIR [name=STEM]                       — write the raw fab outputs (gerbers + drill + BOM +\n\
                                                 CPL) to a directory in KiCad-default format. Use\n\
                                                 `pack` instead when you want a ready-to-upload zip.\n\
  pack [fab=jlcpcb|pcbway|generic] [out=DIR]   — run ERC + DRC + manufacturing-DRC, generate every\n\
                                                 fab artefact, format the BOM and CPL for the chosen\n\
                                                 provider, and zip the lot ready to upload. Defaults\n\
                                                 fab=jlcpcb, out=~/Downloads. The result is a single\n\
                                                 `<project>-<fab>.zip` plus a README.txt inside it\n\
                                                 explaining which file is which and how to order.\n\
                                                 If any check fires errors, the zip is still written\n\
                                                 (so you can see the partial output) but the reply\n\
                                                 says NOT READY and lists the blockers.\n\
\n\
=== RULES ===\n\
- One action per line. Indent (2 spaces / tab) = sub-line of previous block.\n\
- Strings with spaces: double-quote them. Comments: `#` at line start.\n\
- Numbers are decimals (mm or degrees). Booleans: true/false (or 1/0, yes/no).\n\
- The result is per-line; if line N fails, lines N+1..end still run.\n\
- For a single action, send a one-line script.\n\
- Order matters: lib before palette, sym before net, palette before place, place before route.\n\
- Footprints need ≥1.0 mm body-to-body gap (hand-solder floor, same as auto-place solder_gap); edge-mounted parts must touch the outline.\n\
\n\
=== DESIGN STRATEGY (how to fix things) ===\n\
The pipeline runs ERC → placement → routing → DRC. Each layer catches\n\
a different bug class; running them in order saves cycles vs jumping\n\
straight to `route` and debugging the failures.\n\
\n\
1. `erc` — schematic-side validation (after `sym`/`net`, before `place`).\n\
   Catches floating pin/net, duplicate pin, empty net, orphan symbol,\n\
   and (with `pin ... role=...` set) multiple drivers, unpowered\n\
   power nets, undriven inputs. Fix Errors before continuing.\n\
\n\
2. Power planes — declare BEFORE routing, not after:\n\
     class ground pour=both\n\
     net GND ... class=ground\n\
   `pour=both` lays a GND plane on top + bottom; the router skips\n\
   the net entirely (every GND pad connects through the pour) and\n\
   ERC won't fire UnpoweredPowerNet because the pour counts as a\n\
   source. Drops ~15-25 % of total wire on a typical design.\n\
   `class power width=0.4 clearance=0.3` for +3V3/+5V if you want\n\
   wider rails — the router and DRC honour it per net.\n\
\n\
3. `auto-place REF1 REF2 ...` — when you have a rough placement and\n\
   want SA to optimise. Score = HPWL + gap penalty + congestion\n\
   proxy. The default `min_gap=2` keeps parts far enough apart that\n\
   the router has corridors. Reproducible with `seed=N`.\n\
\n\
4. `route` runs the auto-router (RR&R + negotiated congestion +\n\
   Steiner-style multi-source A*), then runs DRC inline. Its output\n\
   includes per-net detour ratio (actual / HPWL) and `route.hint`\n\
   warnings naming the outlier component on every detoured/failed\n\
   net — that's the part to move next.\n\
\n\
5. `compact` — once ERC/route/DRC are clean, shrink the board:\n\
   ERC → power planes → auto-place → route → compact → pack. It only\n\
   commits a smaller outline that still routes clean at 0 DRC errors,\n\
   so it is safe to run right before `pack`. Untouched if nothing\n\
   smaller is feasible.\n\
\n\
When `route` still reports failures or hint warnings:\n\
  a. Read the hints — each one names the outlier component and its\n\
     coords. Move that part toward the rest of its net.\n\
  b. `auto-place <outlier_ref>` if you can't decide where; SA will\n\
     pull it in.\n\
  c. `clear-route` + `route` again. Loop until 0 hints.\n\
\n\
Hand-routing (`trace`, `via`) only works for short bridges in\n\
known-empty zones; on a populated board you will almost always hit\n\
`trace_trace_clearance` errors. Re-place first, route second.\n\
\n\
Rounded boards: declare `outline W H radius=R` BEFORE placing\n\
components. The router's region inset accounts for the corner\n\
curve; placing parts on sharp corners and then adding `radius`\n\
later will leave them outside the routable region (DRC catches it,\n\
but it's wasted work).
";

#[must_use]
pub fn script_reference() -> &'static str {
    SCRIPT_REFERENCE
}

/// Dispatch a `tools/call` to the right handler.
pub async fn dispatch(project: &Project, name: &str, args: &Value) -> Result<Value, ToolError> {
    match name {
        "script" => tool_script(project, args).await,
        "batch" => tool_batch(project, args).await,
        "project.status" => tool_project_status(project),
        "project.reset" => tool_project_reset(project),
        "project.save" => tool_project_save(project, args),
        "project.screenshot" => tool_screenshot(project, args),
        "board.set_outline" => tool_board_set_outline(project, args),
        "placement.add" => tool_placement_add(project, args),
        "view.snapshot" => tool_view_snapshot(project),
        "view.summary" => tool_view_summary(project),
        "net.status" => tool_net_status(project),
        "schematic.add_symbol" => tool_schematic_add_symbol(project, args),
        "schematic.connect" => tool_schematic_connect(project, args),
        "schematic.status" => tool_schematic_status(project),
        "schematic.snapshot" => tool_schematic_snapshot(project),
        "palette.add" => tool_palette_add(project, args),
        "palette.list" => tool_palette_list(project),
        "palette.clear" => tool_palette_clear(project),
        "palette.add_from_library" => tool_palette_add_from_library(project, args),
        "library.list" => tool_library_list(project),
        "library.list_pending" => tool_library_list_pending(project),
        "library.confirm" => tool_library_confirm(project, args),
        "library.discard_pending" => tool_library_discard_pending(project, args),
        "library.find" => tool_library_find(project, args),
        "library.create" => tool_library_create(project, args),
        "library.attach" => tool_library_attach(project, args),
        "library.calibrate_photo" => tool_library_calibrate_photo(project, args),
        "library.rectify_photo" => tool_library_rectify_photo(project, args),
        "library.set_body_rect" => tool_library_set_body_rect(project, args),
        "library.set_edge_mounted" => tool_library_set_edge_mounted(project, args),
        "library.delete_attachment" => tool_library_delete_attachment(project, args),
        "library.delete" => tool_library_delete(project, args),
        "placement.place_from_palette" => tool_place_from_palette(project, args),
        "placement.edge_place" => tool_placement_edge_place(project, args),
        "placement.edge_plan" => tool_placement_edge_plan(project, args),
        "placement.batch" => tool_placement_batch(project, args),
        "placement.move" => tool_placement_move(project, args),
        "placement.rotate" => tool_placement_rotate(project, args),
        "placement.delete" => tool_placement_delete(project, args),
        "placement.clear_board" => tool_placement_clear_board(project),
        "route.clear_net" => tool_route_clear_net(project, args),
        "route.delete_trace" => tool_route_delete_trace(project, args),
        "route.delete_via" => tool_route_delete_via(project, args),
        "route.add_trace" => tool_route_add_trace(project, args),
        "route.add_via" => tool_route_add_via(project, args),
        "route.stitch_isolated_pads" => tool_route_stitch_isolated_pads(project),
        "route.clear" => tool_route_clear(project),
        "placement.auto" => tool_placement_auto(project, args),
        "compact.run" => tool_compact_run(project, args),
        "route.run" => tool_route_run(project, args),
        "pour.add" => tool_pour_add(project, args),
        "pour.remove" => tool_pour_remove(project, args),
        "pour.relief" => tool_pour_relief(project, args),
        "pour.stitch" => tool_pour_stitch(project, args),
        "keepout.add" => tool_keepout_add(project, args),
        "keepout.list" => tool_keepout_list(project),
        "keepout.remove" => tool_keepout_remove(project, args),
        "silk.add_line" => tool_silk_add_line(project, args),
        "silk.add_text" => tool_silk_add_text(project, args),
        "drc.run" => tool_drc_run(project, args),
        "erc.run" => tool_erc_run(project, args),
        "fab.pack" => tool_fab_pack(project, args),
        "schematic.set_class" => tool_schematic_set_class(project, args),
        "schematic.assign_net_class" => tool_schematic_assign_net_class(project, args),
        "pour.auto" => tool_auto_pour(project, args),
        "output.fab_pack" => tool_output_fab_pack(project, args),
        "sheet.add" => tool_sheet_add(project, args),
        "sheet.port" => tool_sheet_port(project, args),
        "sheet.bind" => tool_sheet_bind(project, args),
        "stackup.show" => tool_stackup_show(project),
        "stackup.set" => tool_stackup_set(project, args),
        "layer.list" => tool_layer_list(project),
        "layer.add" => tool_layer_add(project, args),
        "layer.remove" => tool_layer_remove(project, args),
        "layer.rename" => tool_layer_rename(project, args),
        "impedance.suggest" => tool_impedance_suggest(project, args),
        "rules.area_set" => tool_rule_area_set(project, args),
        "rules.area_around" => tool_rule_area_around(project, args),
        "rules.area_remove" => tool_rule_area_remove(project, args),
        "rules.area_list" => tool_rule_area_list(project),
        "rules.fab_set" => tool_fab_rules_set(project, args),
        "fab.profile" => tool_fab_profile(project, args),
        "fab.profile_clear" => tool_fab_profile_clear(project),
        _ => Err(ToolError {
            code: error_code::METHOD_NOT_FOUND,
            message: format!("unknown tool: {name}"),
        }),
    }
}

fn tool_project_reset(project: &Project) -> Result<Value, ToolError> {
    project.reset();
    project.log(ActivityLevel::Info, "project.reset");
    Ok(text_result("Project reset").into())
}

#[derive(Debug, Deserialize)]
struct SaveInput {
    path: String,
}

/// Write the current project to an arbitrary path. Useful when the app
/// was launched without a file argument (no autosave): the agent runs a
/// `save /path/to/board.fragua` line once it has something worth keeping.
fn tool_project_save(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: SaveInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("save: {e}")))?;
    if input.path.trim().is_empty() {
        return Err(ToolError::invalid_params("save: path is empty"));
    }
    let path = std::path::PathBuf::from(&input.path);
    let written = project
        .save_to_path(&path)
        .map_err(|e| ToolError::invalid_params(format!("save: {e}")))?;
    project.log(
        ActivityLevel::Info,
        format!("project.save: wrote {}", written.display()),
    );
    Ok(text_result(format!("Saved to {}", written.display())).into())
}

#[derive(Debug, Deserialize)]
struct ScreenshotInput {
    /// Where to write the PNG. Created/truncated; parent dirs must
    /// already exist.
    path: String,
    /// Which surface to render: `board` (default) or `schematic`.
    #[serde(default)]
    view: Option<String>,
    /// Image width in pixels (height follows the SVG aspect ratio).
    /// Defaults to `pcb_render::DEFAULT_PNG_WIDTH`. Accepted as a
    /// number so the script-DSL `width=2000` (parsed as f64) round-trips
    /// cleanly without needing an integer-typed `AttrType`.
    #[serde(default)]
    width: Option<f64>,
}

/// Rasterise the current project to a PNG file on disk. This is the
/// script-side counterpart to `GET /screenshot` on the HTTP API — the
/// agent uses it inline (`screenshot path=/tmp/x.png`) so a single
/// script run can mutate the board, screenshot it, then keep going.
fn tool_screenshot(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: ScreenshotInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("screenshot: {e}")))?;
    if input.path.trim().is_empty() {
        return Err(ToolError::invalid_params("screenshot: path is empty"));
    }
    let view = input.view.as_deref().unwrap_or("board");
    let width = input.width.map_or(pcb_render::DEFAULT_PNG_WIDTH, |w| {
        w.round()
            .clamp(1.0, f64::from(pcb_render::MAX_PNG_DIMENSION)) as u32
    });

    let snap = project.read();
    let margins = build_placement_margin_map(project);
    let png_result = match view {
        "board" => pcb_render::render_board_png_with_margins(snap.board(), &margins, width),
        "schematic" | "sch" => pcb_render::render_schematic_png(snap.schematic(), width),
        other => {
            return Err(ToolError::invalid_params(format!(
                "screenshot: unknown view `{other}` (use `board` or `schematic`)"
            )));
        }
    };
    drop(snap);
    let png =
        png_result.map_err(|e| ToolError::invalid_params(format!("screenshot: render: {e}")))?;

    let path = std::path::PathBuf::from(&input.path);
    std::fs::write(&path, &png).map_err(|e| {
        ToolError::invalid_params(format!("screenshot: write {}: {e}", path.display()))
    })?;
    project.log(
        ActivityLevel::Info,
        format!(
            "screenshot: wrote {} ({} bytes, view={view}, width={width})",
            path.display(),
            png.len()
        ),
    );
    Ok(text_result(format!(
        "Wrote {view} screenshot ({bytes} bytes) to {p}",
        bytes = png.len(),
        p = path.display()
    ))
    .into())
}

#[derive(Debug, Deserialize)]
struct BatchInput {
    ops: Vec<BatchOp>,
}

#[derive(Debug, Deserialize)]
struct BatchOp {
    tool: String,
    #[serde(default)]
    args: Value,
}

/// Run many tool calls sequentially in a single API request. Each op
/// is `{tool, args}`; the result mirrors the per-op outcome so the
/// agent can react granularly. `batch` itself is rejected as an op
/// (no nesting).
#[derive(Debug, Deserialize)]
struct ScriptInput {
    script: String,
}

/// Parse a multi-line DSL script and dispatch each line as a tool call.
/// On parse error, no commands run — we surface the line + message so
/// the agent can fix the script. On dispatch errors, later lines still
/// run; per-line outcomes come back in the result list.
async fn tool_script(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: ScriptInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("script: {e}")))?;

    let cmds = match crate::script::parse(&input.script) {
        Ok(cmds) => cmds,
        Err(e) => {
            let msg = format!("script: parse error at line {}: {}", e.line, e.message);
            project.log(ActivityLevel::Error, msg.clone());
            return Err(ToolError::invalid_params(msg));
        }
    };

    let mut results = Vec::with_capacity(cmds.len());
    let mut ok_count = 0_usize;
    let mut fail_count = 0_usize;
    for cmd in cmds {
        match Box::pin(dispatch(project, &cmd.tool, &cmd.args)).await {
            Ok(v) => {
                ok_count += 1;
                results.push(json!({
                    "line": cmd.line,
                    "tool": cmd.tool,
                    "ok": true,
                    "result": v,
                }));
            }
            Err(e) => {
                fail_count += 1;
                results.push(json!({
                    "line": cmd.line,
                    "tool": cmd.tool,
                    "ok": false,
                    "error": e.message,
                    "code": e.code,
                }));
            }
        }
    }
    project.log(
        ActivityLevel::Info,
        format!("script: {ok_count} ok, {fail_count} failed"),
    );
    Ok(
        text_result(format!("script: {ok_count} ok, {fail_count} failed")).with_data(json!({
            "ok_count": ok_count,
            "fail_count": fail_count,
            "results": results,
        })),
    )
}

async fn tool_batch(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: BatchInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("batch: {e}")))?;
    let mut results = Vec::with_capacity(input.ops.len());
    let mut ok_count = 0_usize;
    let mut fail_count = 0_usize;
    for op in input.ops {
        if op.tool == "batch" {
            fail_count += 1;
            results.push(json!({
                "tool": "batch",
                "ok": false,
                "error": "batch cannot call itself",
                "code": INVALID_PARAMS,
            }));
            continue;
        }
        // `Box::pin` lets dispatch recurse into itself — async recursion
        // requires a heap-allocated future to give the type a known size.
        match Box::pin(dispatch(project, &op.tool, &op.args)).await {
            Ok(v) => {
                ok_count += 1;
                results.push(json!({
                    "tool": op.tool,
                    "ok": true,
                    "result": v,
                }));
            }
            Err(e) => {
                fail_count += 1;
                results.push(json!({
                    "tool": op.tool,
                    "ok": false,
                    "error": e.message,
                    "code": e.code,
                }));
            }
        }
    }
    project.log(
        ActivityLevel::Info,
        format!("batch: {ok_count} ok, {fail_count} failed"),
    );
    Ok(
        text_result(format!("batch: {ok_count} ok, {fail_count} failed")).with_data(json!({
            "ok_count": ok_count,
            "fail_count": fail_count,
            "results": results,
        })),
    )
}

#[derive(Debug, Deserialize)]
struct SetOutlineInput {
    w_mm: f64,
    h_mm: f64,
    /// Optional uniform corner radius in mm (default 0 = sharp).
    /// Clamped by `Project::set_outline_with_radius` to half the
    /// shorter side, so even a comically-large value produces a
    /// valid outline (a stadium / pill shape at the limit).
    #[serde(default)]
    corner_radius_mm: Option<f64>,
}

fn tool_board_set_outline(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: SetOutlineInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("board.set_outline: {e}")))?;
    let outline = pcb_core::Rect::from_corners(
        Point::new(Length::from_mm(0.0), Length::from_mm(0.0)),
        Point::new(Length::from_mm(input.w_mm), Length::from_mm(input.h_mm)),
    );
    let radius = Length::from_mm(input.corner_radius_mm.unwrap_or(0.0).max(0.0));
    project.set_outline_with_radius(outline, radius);
    let radius_mm = radius.to_mm();
    project.log(
        ActivityLevel::Info,
        format!(
            "board.set_outline: {:.1} × {:.1} mm{}",
            input.w_mm,
            input.h_mm,
            if radius_mm > 0.0 {
                format!(" (radius {radius_mm:.2} mm)")
            } else {
                String::new()
            },
        ),
    );
    let mut text = format!(
        "Board outline set to {:.1} × {:.1} mm",
        input.w_mm, input.h_mm
    );
    if radius_mm > 0.0 {
        text.push_str(&format!(", corner radius {radius_mm:.2} mm"));
    }
    Ok(text_result(text).with_data(json!({
        "w_mm": input.w_mm,
        "h_mm": input.h_mm,
        "corner_radius_mm": radius_mm,
    })))
}

fn tool_project_status(project: &Project) -> Result<Value, ToolError> {
    let snap = project.read();
    let board = snap.board();
    let bounds = board.content_bounds().map(|r| {
        json!({
            "x_mm": r.min.x.to_mm(),
            "y_mm": r.min.y.to_mm(),
            "w_mm": r.width().to_mm(),
            "h_mm": r.height().to_mm(),
        })
    });
    Ok(text_result(format!(
        "project {name}: {n} footprint(s)",
        name = snap.name(),
        n = board.footprints.len(),
    ))
    .with_data(json!({
        "name": snap.name(),
        "footprint_count": board.footprints.len(),
        "content_bounds_mm": bounds,
    })))
}

#[derive(Debug, Deserialize)]
struct PlacementInput {
    reference: String,
    #[serde(default)]
    value: String,
    library: String,
    x_mm: f64,
    y_mm: f64,
    #[serde(default)]
    rotation: f32,
    #[serde(default = "default_layer")]
    layer: LayerInput,
    pads: Vec<PadInput>,
}

#[derive(Debug, Deserialize)]
struct PadInput {
    number: String,
    #[serde(default)]
    name: String,
    x_mm: f64,
    y_mm: f64,
    w_mm: f64,
    h_mm: f64,
    #[serde(default = "default_layer")]
    layer: LayerInput,
    #[serde(default)]
    net: Option<String>,
    /// Plated through-hole drill diameter in mm. Omit for a pure SMD
    /// pad. Set to make a perforated (hybrid SMD + PTH) pad.
    #[serde(default)]
    drill_mm: Option<f64>,
}

/// Multi-layer-friendly layer selector. Pre-Phase-4 callers passed
/// `"top"` or `"bottom"` (lowercase), which now alias to layer index 0
/// and `bottom_of(stackup)`. Inner layers can be addressed by either
/// the stackup's `name` string (e.g. `"In1.Cu"`) or by 0-based index.
#[derive(Debug, Deserialize, Clone)]
#[serde(untagged)]
enum LayerInput {
    Named(String),
    Index(u8),
}

impl From<LayerInput> for CopperLayer {
    fn from(value: LayerInput) -> Self {
        match value {
            LayerInput::Index(n) => CopperLayer { index: n },
            LayerInput::Named(s) => match s.to_ascii_lowercase().as_str() {
                "top" | "f.cu" => CopperLayer::Top,
                "bottom" | "b.cu" => CopperLayer::Bottom,
                // Inner layer alias "in1" => index 2, "in2" => 3, ...
                inner if inner.starts_with("in") => {
                    let n: u8 = inner[2..].parse().unwrap_or(1);
                    CopperLayer { index: n + 1 }
                }
                _ => CopperLayer::Top,
            },
        }
    }
}

impl From<LayerInput> for SilkLayer {
    fn from(value: LayerInput) -> Self {
        match value {
            LayerInput::Index(n) => {
                if n == 0 {
                    Self::Top
                } else {
                    Self::Bottom
                }
            }
            LayerInput::Named(s) => match s.to_ascii_lowercase().as_str() {
                "top" | "f.silks" => Self::Top,
                _ => Self::Bottom,
            },
        }
    }
}

#[derive(Debug, Deserialize, Clone, Copy)]
#[serde(rename_all = "lowercase")]
enum AnchorInput {
    Start,
    Middle,
    End,
}

impl From<AnchorInput> for SilkAnchor {
    fn from(value: AnchorInput) -> Self {
        match value {
            AnchorInput::Start => Self::Start,
            AnchorInput::Middle => Self::Middle,
            AnchorInput::End => Self::End,
        }
    }
}

fn default_anchor_middle() -> AnchorInput {
    AnchorInput::Middle
}

fn default_layer() -> LayerInput {
    LayerInput::Named("top".into())
}

fn tool_placement_add(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: PlacementInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("placement.add: {e}")))?;

    let pads = input
        .pads
        .into_iter()
        .map(|p| Pad {
            number: p.number,
            name: p.name,
            offset: Point::new(Length::from_mm(p.x_mm), Length::from_mm(p.y_mm)),
            size: (Length::from_mm(p.w_mm), Length::from_mm(p.h_mm)),
            layer: p.layer.into(),
            net: p.net,
            drill: p.drill_mm.map(Length::from_mm),
        })
        .collect();

    let footprint = Footprint {
        id: pcb_core::Id::new(),
        reference: input.reference.clone(),
        value: input.value,
        library: input.library,
        position: Point::new(Length::from_mm(input.x_mm), Length::from_mm(input.y_mm)),
        rotation: input.rotation,
        layer: input.layer.into(),
        pads,
        key: String::new(),
        description: String::new(),
        edge_mounted: false,
        edge_side: None,
        silk: Vec::new(),
    };

    let id = project.add_footprint(footprint);
    project.log(
        ActivityLevel::Info,
        format!(
            "placement.add: {} at ({:.2}, {:.2}) mm",
            input.reference, input.x_mm, input.y_mm
        ),
    );
    Ok(
        text_result(format!("Placed {} ({})", input.reference, id.0))
            .with_data(json!({ "id": id.0.to_string(), "reference": input.reference })),
    )
}

fn tool_view_snapshot(project: &Project) -> Result<Value, ToolError> {
    let margins = build_placement_margin_map(project);
    let snap = project.read();
    let board = snap.board();
    let svg = pcb_render::render_svg_with_margins(board, &margins);

    // Structured introspection so the agent can act on the board
    // without parsing SVG: every footprint, trace, via with id, world
    // position, net.
    let outline = board.outline.map(|r| {
        json!({
            "x_mm": r.min.x.to_mm(),
            "y_mm": r.min.y.to_mm(),
            "w_mm": r.width().to_mm(),
            "h_mm": r.height().to_mm(),
        })
    });
    let footprints: Vec<Value> = board
        .footprints_in_order()
        .map(|fp| {
            // bbox in world coords gives the agent the rectangle the
            // footprint actually occupies after rotation, so it can
            // place neighbours without recomputing it.
            let bounds = fp.bounds();
            let bbox = bounds.map(|r| {
                json!({
                    "x_mm": r.min.x.to_mm(),
                    "y_mm": r.min.y.to_mm(),
                    "w_mm": r.width().to_mm(),
                    "h_mm": r.height().to_mm(),
                })
            });
            json!({
                "id": fp.id.0.to_string(),
                "reference": fp.reference,
                "value": fp.value,
                "library": fp.library,
                "key": fp.key,
                "description": fp.description,
                "edge_mounted": fp.edge_mounted,
                "edge_side": fp.edge_side.map(pcb_core::EdgeSide::name),
                "x_mm": fp.position.x.to_mm(),
                "y_mm": fp.position.y.to_mm(),
                "rotation": fp.rotation,
                "bbox": bbox,
                "pads": fp.pads.iter().map(|p| {
                    let world = fp.pad_world_center(p);
                    let (pw, ph) = fp.pad_world_size(p);
                    json!({
                        "number": p.number,
                        "net": p.net,
                        "x_mm": world.x.to_mm(),
                        "y_mm": world.y.to_mm(),
                        "w_mm": pw.to_mm(),
                        "h_mm": ph.to_mm(),
                    })
                }).collect::<Vec<_>>(),
            })
        })
        .collect();
    let traces: Vec<Value> = board
        .traces
        .iter()
        .map(|t| {
            json!({
                "id": t.id.0.to_string(),
                "net": t.net,
                "layer": if t.layer.is_top() { "top" } else { "bottom" },
                "x1_mm": t.start.x.to_mm(), "y1_mm": t.start.y.to_mm(),
                "x2_mm": t.end.x.to_mm(),   "y2_mm": t.end.y.to_mm(),
                "width_mm": t.width.to_mm(),
            })
        })
        .collect();
    let vias: Vec<Value> = board
        .vias
        .iter()
        .map(|v| {
            json!({
                "id": v.id.0.to_string(),
                "net": v.net,
                "x_mm": v.position.x.to_mm(),
                "y_mm": v.position.y.to_mm(),
                "drill_mm": v.drill.to_mm(),
                "diameter_mm": v.diameter.to_mm(),
            })
        })
        .collect();

    Ok(text_result(svg).with_data(json!({
        "outline": outline,
        "footprints": footprints,
        "traces": traces,
        "vias": vias,
    })))
}

/// Compact "where are we" digest — outline, schematic counts, palette
/// items, footprint count, per-net connection status, DRC counts. No
/// SVG, no per-trace coords. Designed for the agent to call between
/// every action without burning tokens.
fn tool_view_summary(project: &Project) -> Result<Value, ToolError> {
    let snap = project.read();
    let board = snap.board();
    let sch = snap.schematic();

    let outline = board.outline.map(|r| {
        json!({
            "x_mm": r.min.x.to_mm(),
            "y_mm": r.min.y.to_mm(),
            "w_mm": r.width().to_mm(),
            "h_mm": r.height().to_mm(),
        })
    });

    let palette: Vec<Value> = snap
        .palette()
        .iter()
        .map(|fp| {
            let bounds = fp.bounds();
            let (bw, bh) = bounds.map_or((0.0, 0.0), |r| (r.width().to_mm(), r.height().to_mm()));
            json!({
                "reference": fp.reference,
                "key": fp.key,
                "edge_mounted": fp.edge_mounted,
                "edge_side": fp.edge_side.map(pcb_core::EdgeSide::name),
                "bbox_w_mm": bw,
                "bbox_h_mm": bh,
            })
        })
        .collect();

    let footprints: Vec<Value> = board
        .footprints_in_order()
        .map(|fp| {
            json!({
                "reference": fp.reference,
                "key": fp.key,
                "x_mm": fp.position.x.to_mm(),
                "y_mm": fp.position.y.to_mm(),
                "rotation": fp.rotation,
            })
        })
        .collect();

    let nets = collect_net_status(board, sch);

    // DRC summary only — no per-violation list. Use drc.run for details.
    // Same options `drc` uses (schematic classes + the board's own fab
    // rules) so the two never disagree on the counts.
    let drc_opts = pcb_drc::DrcOptions {
        placement_margins: build_drc_margin_map(project),
        schematic: Some(std::sync::Arc::new(sch.clone())),
        ..pcb_drc::DrcOptions::default()
    };
    let drc = pcb_drc::run(board, &drc_opts);

    let total_nets = nets.len();
    let unconnected_nets: usize = nets
        .iter()
        .filter(|n| {
            n["unconnected_pads"]
                .as_array()
                .is_some_and(|a| !a.is_empty())
        })
        .count();

    // Rule areas change what "legal" means, so the board summary has to
    // name them — an agent reading only text must not have to guess why
    // a 0.13 mm gap passed.
    let mut rules_line = String::new();
    if !board.rule_areas.is_empty() {
        rules_line.push_str(&format!(
            "\nrule areas: {}",
            board
                .rule_areas
                .iter()
                .map(|a| describe_rule_area(a))
                .collect::<Vec<_>>()
                .join("; ")
        ));
    }
    if let Some(f) = board.fab_rules.as_ref() {
        rules_line.push_str(&format!("\nfab-rules: {}", f.preset));
    }

    Ok(text_result(format!(
        "{} symbols, {} nets ({} fully connected), {} placed, {} in palette; DRC {}E {}W{rules_line}",
        sch.symbols.len(),
        total_nets,
        total_nets - unconnected_nets,
        board.footprints.len(),
        snap.palette().len(),
        drc.error_count,
        drc.warning_count,
    ))
    .with_data(json!({
        "outline": outline,
        "schematic": {
            "symbol_count": sch.symbols.len(),
            "net_count": sch.nets.len(),
        },
        "palette": palette,
        "footprints": footprints,
        "nets": nets,
        "drc": {
            "error_count": drc.error_count,
            "warning_count": drc.warning_count,
        },
    })))
}

/// Per-net "what's expected, what landed, what's missing". Built once
/// and reused by `view.summary` and the dedicated `net.status` tool.
fn collect_net_status(board: &pcb_core::Board, sch: &pcb_core::schematic::Schematic) -> Vec<Value> {
    use std::collections::HashSet;
    // First: which nets have any copper laid down? Pads that participate
    // in laid traces are counted as connected via that copper.
    let mut net_with_copper: HashSet<&str> = HashSet::new();
    for t in &board.traces {
        net_with_copper.insert(t.net.as_str());
    }
    for v in &board.vias {
        net_with_copper.insert(v.net.as_str());
    }
    // Map (footprint reference, pad number) → does this pad sit on
    // copper of its declared net? We approximate: a pad is "connected"
    // if its net has any copper at all AND the pad itself overlaps a
    // trace endpoint or via on the same net. The looser version (net
    // has any copper) is the cheaper signal we already report; the
    // tighter check exists in DRC's unconnected_pad warning, so we
    // mirror the DRC view by re-running the relevant geometry.
    let drc_report = pcb_drc::run(board, &pcb_drc::DrcOptions::default());
    let mut unconnected_pads: HashSet<(String, String)> = HashSet::new();
    for v in &drc_report.violations {
        if v.kind == pcb_drc::ViolationKind::UnconnectedPad {
            // involved is ["Ref.Pin"] for unconnected_pad
            if let Some(rp) = v.involved.first() {
                if let Some((r, p)) = rp.split_once('.') {
                    unconnected_pads.insert((r.to_string(), p.to_string()));
                }
            }
        }
    }

    let mut out = Vec::with_capacity(sch.nets.len());
    for (net_name, net) in &sch.nets {
        let mut pads_expected: Vec<Value> = Vec::with_capacity(net.connections.len());
        let mut connected_count = 0_usize;
        let mut unconnected = Vec::new();
        for conn in &net.connections {
            let Some(symbol) = sch.symbols.get(&conn.symbol_id) else {
                continue;
            };
            let pad_ref = format!("{}.{}", symbol.reference, conn.pin_number);
            let pin_name = symbol
                .kind
                .pins()
                .iter()
                .find(|p| p.number == conn.pin_number)
                .map(|p| p.name.clone())
                .unwrap_or_default();
            let is_unconnected =
                unconnected_pads.contains(&(symbol.reference.clone(), conn.pin_number.clone()));
            if is_unconnected {
                unconnected.push(pad_ref.clone());
            } else {
                connected_count += 1;
            }
            pads_expected.push(json!({
                "ref": pad_ref,
                "pin_name": pin_name,
                "connected": !is_unconnected,
            }));
        }
        out.push(json!({
            "net": net_name,
            "pad_count": net.connections.len(),
            "connected_count": connected_count,
            "has_copper": net_with_copper.contains(net_name.as_str()),
            "pads": pads_expected,
            "unconnected_pads": unconnected,
        }));
    }
    out
}

fn tool_net_status(project: &Project) -> Result<Value, ToolError> {
    let snap = project.read();
    let nets = collect_net_status(snap.board(), snap.schematic());
    let unconnected: Vec<&str> = nets
        .iter()
        .filter(|n| {
            n["unconnected_pads"]
                .as_array()
                .is_some_and(|a| !a.is_empty())
        })
        .filter_map(|n| n["net"].as_str())
        .collect();
    Ok(text_result(format!(
        "{} nets total, {} with unconnected pads ({})",
        nets.len(),
        unconnected.len(),
        if unconnected.is_empty() {
            "all clean".to_string()
        } else {
            unconnected.join(", ")
        },
    ))
    .with_data(json!({ "nets": nets })))
}

#[derive(Debug, Deserialize)]
struct SymbolInput {
    reference: String,
    #[serde(default)]
    value: String,
    kind: SymbolKindInput,
    #[serde(default)]
    pins: Vec<PinInput>,
    #[serde(default)]
    x_mm: Option<f64>,
    #[serde(default)]
    y_mm: Option<f64>,
    #[serde(default)]
    rotation: f32,
    /// Library key the agent picked (`snake_case`, e.g.
    /// "`esp32_s3_zero`"). Empty string means "no library entry".
    #[serde(default)]
    key: String,
    /// Free-form intent / role / orientation notes.
    #[serde(default)]
    description: String,
}

#[derive(Debug, Deserialize, Clone, Copy)]
#[serde(rename_all = "snake_case")]
enum SymbolKindInput {
    Resistor,
    Capacitor,
    Inductor,
    Led,
    Diode,
    GenericIc,
}

#[derive(Debug, Deserialize)]
struct PinInput {
    number: String,
    #[serde(default)]
    name: String,
    side: PinSideInput,
    /// ERC role for the pin. Optional in the JSON; defaults to
    /// `passive` so existing scripts that didn't set a role keep
    /// their semantics.
    #[serde(default)]
    role: PinRoleInput,
}

#[derive(Debug, Deserialize, Clone, Copy, Default)]
#[serde(rename_all = "snake_case")]
enum PinRoleInput {
    #[default]
    Passive,
    Input,
    Output,
    Bidir,
    PowerOut,
    PowerIn,
}

impl From<PinRoleInput> for pcb_core::PinRole {
    fn from(v: PinRoleInput) -> Self {
        match v {
            PinRoleInput::Passive => Self::Passive,
            PinRoleInput::Input => Self::Input,
            PinRoleInput::Output => Self::Output,
            PinRoleInput::Bidir => Self::Bidir,
            PinRoleInput::PowerOut => Self::PowerOut,
            PinRoleInput::PowerIn => Self::PowerIn,
        }
    }
}

#[derive(Debug, Deserialize, Clone, Copy)]
#[serde(rename_all = "lowercase")]
enum PinSideInput {
    Left,
    Right,
    Top,
    Bottom,
}

impl From<PinSideInput> for PinSide {
    fn from(v: PinSideInput) -> Self {
        match v {
            PinSideInput::Left => Self::Left,
            PinSideInput::Right => Self::Right,
            PinSideInput::Top => Self::Top,
            PinSideInput::Bottom => Self::Bottom,
        }
    }
}

fn tool_schematic_add_symbol(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: SymbolInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("schematic.add_symbol: {e}")))?;

    let kind = match input.kind {
        SymbolKindInput::Resistor => SymbolKind::Resistor,
        SymbolKindInput::Capacitor => SymbolKind::Capacitor,
        SymbolKindInput::Inductor => SymbolKind::Inductor,
        SymbolKindInput::Led => SymbolKind::Led,
        SymbolKindInput::Diode => SymbolKind::Diode,
        SymbolKindInput::GenericIc => {
            if input.pins.is_empty() {
                return Err(ToolError::invalid_params(
                    "schematic.add_symbol: kind=generic_ic requires a non-empty pins array",
                ));
            }
            let pins = input
                .pins
                .iter()
                .map(|p| SchPin {
                    number: p.number.clone(),
                    name: p.name.clone(),
                    side: p.side.into(),
                    role: p.role.into(),
                })
                .collect();
            SymbolKind::GenericIc { pins }
        }
    };

    let position = match (input.x_mm, input.y_mm) {
        (Some(x), Some(y)) => Point::new(Length::from_mm(x), Length::from_mm(y)),
        _ => auto_place(project),
    };

    let symbol = Symbol {
        id: pcb_core::Id::new(),
        reference: input.reference.clone(),
        value: input.value,
        kind,
        position,
        rotation: input.rotation,
        key: input.key.clone(),
        description: input.description.clone(),
    };
    let id = project.add_symbol(symbol);
    project.log(
        ActivityLevel::Info,
        format!(
            "schematic.add_symbol: {} at ({:.2}, {:.2}) mm",
            input.reference,
            position.x.to_mm(),
            position.y.to_mm()
        ),
    );
    Ok(text_result(format!("Added {} ({})", input.reference, id.0))
        .with_data(json!({ "id": id.0.to_string(), "reference": input.reference })))
}

/// Default placement: lay symbols out in rows of 6, 25 mm apart
/// horizontally and 20 mm vertically. The agent can always pass
/// explicit positions; this is just a "don't crash if you forget".
fn auto_place(project: &Project) -> Point {
    let snap = project.read();
    #[allow(clippy::cast_precision_loss)]
    let n = snap.schematic().symbol_order.len() as f64;
    let row = (n / 6.0).floor();
    let col = n - row * 6.0;
    Point::new(
        Length::from_mm(15.0 + col * 25.0),
        Length::from_mm(15.0 + row * 20.0),
    )
}

#[derive(Debug, Deserialize)]
struct ConnectInput {
    net: String,
    pins: Vec<String>,
    /// Optional `NetClass` name to attach to this net. If unset (or
    /// the named class doesn't exist) the router and DRC fall back to
    /// their default `trace_width/clearance`.
    #[serde(default)]
    class: Option<String>,
}

fn tool_schematic_connect(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: ConnectInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("schematic.connect: {e}")))?;

    let mut connections = Vec::with_capacity(input.pins.len());
    {
        let snap = project.read();
        let sch = snap.schematic();
        for pin_ref in &input.pins {
            let (sym_ref, pin_token) = pin_ref.split_once('.').ok_or_else(|| {
                ToolError::invalid_params(format!("expected REF.PIN format, got {pin_ref:?}"))
            })?;
            let symbol = sch
                .find_by_reference(sym_ref)
                .ok_or_else(|| ToolError::invalid_params(format!("unknown symbol {sym_ref}")))?;
            // Accept either the pin number (e.g. "U1.16") or the pin
            // name (e.g. "U1.GPIO13"). Names are matched
            // case-insensitively to be forgiving with how the agent
            // typed them. If neither matches a declared pin, fall back
            // to using the token verbatim as a pin number — discrete
            // primitives have implicit pins ("1"/"2") that aren't in
            // the SchPin list.
            let pin_number = symbol
                .kind
                .pins()
                .iter()
                .find(|p| p.number == pin_token || p.name.eq_ignore_ascii_case(pin_token))
                .map_or_else(|| pin_token.to_string(), |p| p.number.clone());
            connections.push(NetConnection {
                symbol_id: symbol.id,
                pin_number,
            });
        }
    }
    let count = connections.len();
    project
        .set_net(Net {
            name: input.net.clone(),
            connections,
            class: input.class.clone(),
        })
        .map_err(ToolError::invalid_params)?;
    project.log(
        ActivityLevel::Info,
        format!("schematic.connect: {} ({} pin(s))", input.net, count),
    );
    Ok(
        text_result(format!("Net {} now has {} connection(s)", input.net, count))
            .with_data(json!({ "net": input.net, "connection_count": count })),
    )
}

fn tool_schematic_status(project: &Project) -> Result<Value, ToolError> {
    let snap = project.read();
    let sch = snap.schematic();
    let symbol_count = sch.symbols.len();
    let net_count = sch.nets.len();
    let mut unconnected = Vec::new();
    for sym in sch.symbols_in_order() {
        for pin in sym.kind.pins() {
            if sch.net_for_pin(sym.id, &pin.number).is_none() {
                unconnected.push(format!("{}.{}", sym.reference, pin.number));
            }
        }
    }
    Ok(text_result(format!(
        "schematic: {symbol_count} symbol(s), {net_count} net(s), {} unconnected pin(s)",
        unconnected.len()
    ))
    .with_data(json!({
        "symbol_count": symbol_count,
        "net_count": net_count,
        "unconnected": unconnected,
    })))
}

fn tool_schematic_snapshot(project: &Project) -> Result<Value, ToolError> {
    let snap = project.read();
    let svg = pcb_render::render_schematic_svg(snap.schematic());
    Ok(text_result(svg).into())
}

#[derive(Debug, Deserialize)]
struct PaletteAddInput {
    footprints: Vec<PaletteFootprint>,
}

#[derive(Debug, Deserialize)]
struct PaletteFootprint {
    reference: String,
    library: String,
    #[serde(default)]
    rotation: f32,
    #[serde(default = "default_layer")]
    layer: LayerInput,
    pads: Vec<PadPlan>,
    /// Override `edge_mounted` from the schematic side. Useful when the
    /// agent decides at placement time that this instance must hug a
    /// specific edge (e.g. the on-module USB).
    #[serde(default)]
    edge_mounted: Option<bool>,
}

fn tool_palette_add(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: PaletteAddInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("palette.add: {e}")))?;

    let mut added = Vec::with_capacity(input.footprints.len());
    for plan in input.footprints {
        // Pull the value + net assignments + key/description from the
        // schematic so the palette item carries the full intent.
        let (value, pads, key, description) = {
            let snap = project.read();
            let sch = snap.schematic();
            let symbol = sch.find_by_reference(&plan.reference).ok_or_else(|| {
                ToolError::invalid_params(format!(
                    "palette.add: no schematic symbol named {}",
                    plan.reference
                ))
            })?;
            let value = symbol.value.clone();
            let key = symbol.key.clone();
            let description = symbol.description.clone();
            let pads: Vec<Pad> = plan
                .pads
                .iter()
                .map(|pad_plan| {
                    let net = sch
                        .net_for_pin(symbol.id, &pad_plan.number)
                        .or_else(|| {
                            if pad_plan.name.is_empty() {
                                None
                            } else {
                                sch.net_for_pin(symbol.id, &pad_plan.name)
                            }
                        })
                        .map(str::to_string);
                    Pad {
                        number: pad_plan.number.clone(),
                        name: pad_plan.name.clone(),
                        offset: Point::new(
                            Length::from_mm(pad_plan.x_mm),
                            Length::from_mm(pad_plan.y_mm),
                        ),
                        size: (
                            Length::from_mm(pad_plan.w_mm),
                            Length::from_mm(pad_plan.h_mm),
                        ),
                        layer: pad_plan.layer.clone().into(),
                        net,
                        drill: pad_plan.drill_mm.map(Length::from_mm),
                    }
                })
                .collect();
            (value, pads, key, description)
        };
        let footprint = Footprint {
            id: pcb_core::Id::new(),
            reference: plan.reference.clone(),
            value,
            library: plan.library,
            // Initial position will be overridden by the UI strip
            // (laid out left-to-right above the board) so any value is
            // fine; we put it off-canvas to avoid a flash of bad layout.
            position: Point::new(Length::from_mm(-100.0), Length::from_mm(-100.0)),
            rotation: plan.rotation,
            layer: plan.layer.into(),
            pads,
            key,
            description,
            edge_mounted: plan.edge_mounted.unwrap_or(false),
            edge_side: None,
            silk: Vec::new(),
        };
        project
            .palette_add(footprint)
            .map_err(ToolError::invalid_params)?;
        added.push(plan.reference);
    }
    project.log(
        ActivityLevel::Info,
        format!("palette.add: {} component(s)", added.len()),
    );
    Ok(
        text_result(format!("Added {} item(s) to palette", added.len()))
            .with_data(json!({ "added": added })),
    )
}

fn tool_palette_clear(project: &Project) -> Result<Value, ToolError> {
    project.palette_clear();
    project.log(ActivityLevel::Info, "palette.clear");
    Ok(text_result("Palette cleared").into())
}

fn tool_palette_list(project: &Project) -> Result<Value, ToolError> {
    let snap = project.read();
    let entries: Vec<Value> = snap
        .palette()
        .iter()
        .map(|fp| {
            let bounds = fp.bounds();
            let (bw, bh) = bounds.map_or((0.0, 0.0), |r| (r.width().to_mm(), r.height().to_mm()));
            let mut nets: Vec<&str> = fp.pads.iter().filter_map(|p| p.net.as_deref()).collect();
            nets.sort_unstable();
            nets.dedup();
            json!({
                "reference": fp.reference,
                "value": fp.value,
                "library": fp.library,
                "rotation": fp.rotation,
                "bbox_w_mm": bw,
                "bbox_h_mm": bh,
                "pad_count": fp.pads.len(),
                "nets": nets,
            })
        })
        .collect();
    Ok(
        text_result(format!("{} item(s) waiting in the palette", entries.len()))
            .with_data(json!({ "items": entries })),
    )
}

#[derive(Debug, Deserialize)]
struct PlaceFromPaletteInput {
    reference: String,
    x_mm: f64,
    y_mm: f64,
}

// ─── Library tools ─────────────────────────────────────────────────────

fn library_entry_summary(e: &pcb_core::LibraryEntry) -> Value {
    json!({
        "key": e.key,
        "description": e.description,
        "default_value": e.default_value,
        "default_rotation_deg": e.default_rotation_deg,
        "edge_mounted": e.edge_mounted,
        "edge_side": e.edge_side.map(pcb_core::EdgeSide::name),
        "pad_count": e.pads.len(),
        "attachment_count": e.attachments.len(),
        "attachments": e.attachments.iter().map(|a| json!({
            "id": a.id,
            "kind": a.kind,
            "filename": a.filename,
            "mime": a.mime,
            "added_at": a.added_at,
            "calibrated": a.calibration.is_some(),
        })).collect::<Vec<_>>(),
        "has_body_rect": e.body_rect.is_some(),
        "body_rect": e.body_rect.map(|b| json!({
            "min_x_mm": b.min_x_mm,
            "min_y_mm": b.min_y_mm,
            "max_x_mm": b.max_x_mm,
            "max_y_mm": b.max_y_mm,
        })),
        "created_at": e.created_at,
    })
}

fn library_entry_full(e: &pcb_core::LibraryEntry) -> Value {
    let mut v = library_entry_summary(e);
    let pads: Vec<Value> = e
        .pads
        .iter()
        .map(|p| {
            json!({
                "number": p.number,
                "name": p.name,
                "x_mm": p.x_mm,
                "y_mm": p.y_mm,
                "w_mm": p.w_mm,
                "h_mm": p.h_mm,
            })
        })
        .collect();
    let silk: Vec<Value> = e
        .silk
        .iter()
        .map(|s| match s {
            LibrarySilk::Line {
                layer,
                x1_mm,
                y1_mm,
                x2_mm,
                y2_mm,
                width_mm,
            } => json!({
                "kind": "line",
                "layer": layer_to_str_silk(*layer),
                "x1_mm": x1_mm, "y1_mm": y1_mm, "x2_mm": x2_mm, "y2_mm": y2_mm,
                "width_mm": width_mm,
            }),
            LibrarySilk::Text {
                layer,
                x_mm,
                y_mm,
                text,
                size_mm,
                rotation_deg,
                anchor,
                width_mm,
            } => json!({
                "kind": "text",
                "layer": layer_to_str_silk(*layer),
                "x_mm": x_mm, "y_mm": y_mm,
                "text": text, "size_mm": size_mm,
                "rotation_deg": rotation_deg,
                "anchor": anchor_to_str(*anchor),
                "width_mm": width_mm,
            }),
        })
        .collect();
    if let Some(obj) = v.as_object_mut() {
        obj.insert("pads".into(), Value::Array(pads));
        obj.insert("silk".into(), Value::Array(silk));
    }
    v
}

fn anchor_to_str(a: SilkAnchor) -> &'static str {
    match a {
        SilkAnchor::Start => "start",
        SilkAnchor::Middle => "middle",
        SilkAnchor::End => "end",
    }
}

/// Convert a library-frame silk item into the runtime
/// footprint-local `FootprintSilk` representation. Library coords are
/// already footprint-local mm; we just rebox into nanometre `Length`.
/// Convert a `LibrarySilk` (footprint-local mm) into the runtime
/// `FootprintSilk` representation, applying the library entry's
/// `footprint_view_transform` so the body outline / pin-1 markers
/// track the visual orientation the user picked in the review pane.
/// Pass `ViewTransform::default()` for callers that have no view
/// transform context (currently none — the only call site is the
/// palette spawn).
fn library_silk_to_footprint_with_view(
    s: &LibrarySilk,
    vt: pcb_core::ViewTransform,
) -> FootprintSilk {
    match s {
        LibrarySilk::Line {
            layer,
            x1_mm,
            y1_mm,
            x2_mm,
            y2_mm,
            width_mm,
        } => {
            let (x1, y1) = vt.apply_point_mm(*x1_mm, *y1_mm);
            let (x2, y2) = vt.apply_point_mm(*x2_mm, *y2_mm);
            FootprintSilk::Line {
                layer: *layer,
                start: Point::new(Length::from_mm(x1), Length::from_mm(y1)),
                end: Point::new(Length::from_mm(x2), Length::from_mm(y2)),
                width: Length::from_mm(*width_mm),
            }
        }
        LibrarySilk::Text {
            layer,
            x_mm,
            y_mm,
            text,
            size_mm,
            rotation_deg,
            anchor,
            width_mm,
        } => {
            let (x, y) = vt.apply_point_mm(*x_mm, *y_mm);
            FootprintSilk::Text {
                layer: *layer,
                position: Point::new(Length::from_mm(x), Length::from_mm(y)),
                text: text.clone(),
                size: Length::from_mm(*size_mm),
                rotation: vt.apply_angle_deg(*rotation_deg),
                anchor: *anchor,
                width: Length::from_mm(*width_mm),
            }
        }
    }
}

fn tool_library_list(project: &Project) -> Result<Value, ToolError> {
    let entries = project.library().list();
    let items: Vec<Value> = entries.iter().map(library_entry_summary).collect();
    // HTTP replies are text/plain — put keys in the text so agents can
    // parse without structured data. One line per entry.
    let mut lines = vec![format!("{} entries in library:", entries.len())];
    for e in &entries {
        let edge = if e.edge_mounted { " edge" } else { "" };
        let body = if e.body_rect.is_some() { " body" } else { "" };
        let desc = if e.description.is_empty() {
            String::new()
        } else {
            format!(" — {}", e.description.replace('\n', " "))
        };
        lines.push(format!(
            "  {}  pads={}{}{}{}",
            e.key,
            e.pads.len(),
            edge,
            body,
            desc
        ));
    }
    Ok(text_result(lines.join("\n")).with_data(json!({ "entries": items })))
}

fn tool_library_list_pending(project: &Project) -> Result<Value, ToolError> {
    let pending = project.pending_library_entries();
    let mut lines = vec![format!("{} pending library entries:", pending.len())];
    let mut items = Vec::new();
    for p in &pending {
        let e = &p.entry;
        lines.push(format!(
            "  {}  pads={}  staged_attachments={}",
            e.key,
            e.pads.len(),
            p.attachments.len()
        ));
        items.push(library_entry_summary(e));
    }
    if pending.is_empty() {
        lines.push("  (none — `lib KEY` queues entries here until `confirm-lib KEY`)".into());
    }
    Ok(text_result(lines.join("\n")).with_data(json!({ "entries": items })))
}

#[derive(Debug, Deserialize)]
struct LibraryKeyInput {
    key: String,
}

fn tool_library_confirm(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: LibraryKeyInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("library.confirm: {e}")))?;
    let ok = project
        .confirm_pending_library_entry(&input.key)
        .map_err(ToolError::invalid_params)?;
    if !ok {
        return Err(ToolError::invalid_params(format!(
            "library.confirm: no pending entry with key {} (use list-pending)",
            input.key
        )));
    }
    project.log(
        ActivityLevel::Info,
        format!("library.confirm: {} (saved)", input.key),
    );
    Ok(text_result(format!(
        "Confirmed {} — now in on-disk library (palette/place/body-rect ready)",
        input.key
    ))
    .into())
}

fn tool_library_discard_pending(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: LibraryKeyInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("library.discard_pending: {e}")))?;
    let ok = project.discard_pending_library_entry(&input.key);
    if !ok {
        return Err(ToolError::invalid_params(format!(
            "library.discard_pending: no pending entry with key {}",
            input.key
        )));
    }
    project.log(
        ActivityLevel::Info,
        format!("library.discard_pending: {} (dropped)", input.key),
    );
    Ok(text_result(format!("Discarded pending entry {}", input.key)).into())
}

#[derive(Debug, Deserialize)]
struct LibraryFindInput {
    key: String,
}

fn tool_library_find(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: LibraryFindInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("library.find: {e}")))?;
    match project.library().find(&input.key) {
        Some(e) => Ok(text_result(format!("Found {}", e.key)).with_data(library_entry_full(&e))),
        None => Err(ToolError::invalid_params(format!(
            "library.find: no entry with key {}",
            input.key
        ))),
    }
}

#[derive(Debug, Deserialize)]
struct LibraryCreatePadInput {
    number: String,
    #[serde(default)]
    name: String,
    x_mm: f64,
    y_mm: f64,
    w_mm: f64,
    h_mm: f64,
    /// Plated through-hole drill diameter in mm. Omit for SMD.
    #[serde(default)]
    drill_mm: Option<f64>,
}

#[derive(Debug, Deserialize)]
struct LibraryCreateInput {
    key: String,
    description: String,
    #[serde(default)]
    default_value: String,
    #[serde(default)]
    default_rotation_deg: f32,
    #[serde(default)]
    edge_mounted: bool,
    pads: Vec<LibraryCreatePadInput>,
    /// Library-authored silk strokes. Coordinates are in
    /// footprint-local mm; the spawn step converts them into
    /// world-aware `FootprintSilk` items.
    #[serde(default)]
    silk: Vec<LibrarySilkInput>,
    /// Optional LCSC catalogue ID (e.g. "C25804"). Plumbed straight
    /// to the JLCPCB BOM writer so SMT assembly knows what part to
    /// load. Routing/placement ignore it.
    #[serde(default)]
    lcsc_id: Option<String>,
    /// Optional manufacturer part number (e.g. "RC0603FR-0710KL").
    /// Fab-agnostic identifier used by every assembler.
    #[serde(default)]
    mpn: Option<String>,
}

/// Wire-format mirror of `pcb_core::LibrarySilk` — kept separate from
/// the core type so we can accept the looser `Option<f64>` for an
/// auto-derived stroke width without touching the on-disk schema.
#[derive(Debug, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
enum LibrarySilkInput {
    Line {
        #[serde(default = "default_layer")]
        layer: LayerInput,
        x1_mm: f64,
        y1_mm: f64,
        x2_mm: f64,
        y2_mm: f64,
        #[serde(default = "default_silk_width")]
        width_mm: f64,
    },
    Text {
        #[serde(default = "default_layer")]
        layer: LayerInput,
        x_mm: f64,
        y_mm: f64,
        text: String,
        #[serde(default = "default_silk_size")]
        size_mm: f64,
        #[serde(default)]
        rotation_deg: f32,
        #[serde(default = "default_anchor_middle")]
        anchor: AnchorInput,
        #[serde(default)]
        width_mm: Option<f64>,
    },
}

impl From<LibrarySilkInput> for LibrarySilk {
    fn from(v: LibrarySilkInput) -> Self {
        match v {
            LibrarySilkInput::Line {
                layer,
                x1_mm,
                y1_mm,
                x2_mm,
                y2_mm,
                width_mm,
            } => LibrarySilk::Line {
                layer: layer.into(),
                x1_mm,
                y1_mm,
                x2_mm,
                y2_mm,
                width_mm,
            },
            LibrarySilkInput::Text {
                layer,
                x_mm,
                y_mm,
                text,
                size_mm,
                rotation_deg,
                anchor,
                width_mm,
            } => LibrarySilk::Text {
                layer: layer.into(),
                x_mm,
                y_mm,
                text,
                size_mm,
                rotation_deg,
                anchor: anchor.into(),
                width_mm: width_mm.unwrap_or(size_mm / 8.0),
            },
        }
    }
}

fn tool_library_create(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: LibraryCreateInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("library.create: {e}")))?;
    let pads = input
        .pads
        .into_iter()
        .map(|p| pcb_core::LibraryPad {
            number: p.number,
            name: p.name,
            x_mm: p.x_mm,
            y_mm: p.y_mm,
            w_mm: p.w_mm,
            h_mm: p.h_mm,
            drill_mm: p.drill_mm,
        })
        .collect();
    let silk: Vec<LibrarySilk> = input.silk.into_iter().map(Into::into).collect();
    let entry = pcb_core::LibraryEntry {
        key: input.key.clone(),
        description: input.description,
        default_value: input.default_value,
        default_rotation_deg: input.default_rotation_deg,
        edge_mounted: input.edge_mounted,
        edge_side: None,
        pads,
        silk,
        lcsc_id: input.lcsc_id,
        mpn: input.mpn,
        attachments: Vec::new(),
        created_at: 0,
        footprint_view_transform: pcb_core::ViewTransform::default(),
        placement_margin: pcb_core::PlacementMargin::default(),
        body_rect: None,
    };
    // Queue for confirmation: mirrored / mis-numbered footprints have
    // shipped fab scrap before. The UI review pane is the preferred
    // path when a photo is available; agents may also call
    // `confirm-lib KEY` after self-checking pad numbering/chirality.
    let pad_count = entry.pads.len();
    let key = entry.key.clone();
    let pending = pcb_core::PendingLibraryEntry {
        entry: entry.clone(),
        attachments: Vec::new(),
    };
    let pending_count = project.queue_pending_library_entry(pending);
    project.log(
        ActivityLevel::Info,
        format!(
            "library.create: {key} ({pad_count} pads) — pending confirmation ({pending_count} queued)"
        ),
    );
    Ok(text_result(format!(
        "Queued {key} for review ({pad_count} pads, {pending_count} pending total). \
Confirm via UI review pane OR `confirm-lib {key}` after checking pin-1 / chirality. \
Optional: `attach {key} photo <path>` before confirming."
    ))
    .with_data(library_entry_full(&entry)))
}

#[derive(Debug, Deserialize)]
struct LibraryAttachInput {
    key: String,
    kind: String,
    filename: String,
    mime: String,
    data_base64: String,
}

fn tool_library_attach(project: &Project, args: &Value) -> Result<Value, ToolError> {
    use base64::Engine;
    let input: LibraryAttachInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("library.attach: {e}")))?;
    let bytes = base64::engine::general_purpose::STANDARD
        .decode(input.data_base64.as_bytes())
        .map_err(|e| ToolError::invalid_params(format!("library.attach: invalid base64: {e}")))?;
    // If the target entry is still in the pending-confirmation buffer
    // (the typical case: `library.create` → `library.attach photo` →
    // human reviews), stage the attachment on the pending record so the
    // review modal can display it. Only after `confirm_pending_library_entry`
    // does the file get written to disk via `Library::attach`. If the
    // entry was already confirmed earlier, fall through to the live
    // library so the agent can patch existing parts.
    if let Some(mut pending) = project.find_pending_library_entry(&input.key) {
        let byte_len = bytes.len();
        pending.attachments.push(pcb_core::PendingAttachment {
            kind: input.kind.clone(),
            filename: input.filename.clone(),
            mime: input.mime.clone(),
            data: bytes,
        });
        project.queue_pending_library_entry(pending);
        project.log(
            ActivityLevel::Info,
            format!(
                "library.attach: {} ← {} ({} bytes) [pending review]",
                input.key, input.filename, byte_len
            ),
        );
        return Ok(text_result(format!(
            "Staged {} on pending entry {} — will be persisted when the entry is confirmed",
            input.filename, input.key
        ))
        .with_data(json!({
            "kind": input.kind,
            "filename": input.filename,
            "mime": input.mime,
            "pending": true,
        })));
    }
    let att = project
        .library()
        .attach(&input.key, input.kind, input.filename, input.mime, &bytes)
        .map_err(ToolError::invalid_params)?;
    let count = project.library().list().len();
    project
        .events()
        .publish(pcb_core::Event::LibraryChanged { count });
    project.log(
        ActivityLevel::Info,
        format!(
            "library.attach: {} ← {} ({} bytes)",
            input.key,
            att.filename,
            bytes.len()
        ),
    );
    Ok(
        text_result(format!("Attached {}", att.filename)).with_data(json!({
            "id": att.id,
            "kind": att.kind,
            "filename": att.filename,
            "mime": att.mime,
            "added_at": att.added_at,
        })),
    )
}

#[derive(Debug, Deserialize)]
struct LibraryDeleteAttachmentInput {
    key: String,
    attachment_id: String,
}

fn tool_library_delete_attachment(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: LibraryDeleteAttachmentInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("library.delete_attachment: {e}")))?;
    let removed = project
        .library()
        .delete_attachment(&input.key, &input.attachment_id)
        .map_err(ToolError::invalid_params)?;
    if removed {
        let count = project.library().list().len();
        project
            .events()
            .publish(pcb_core::Event::LibraryChanged { count });
    }
    Ok(text_result(
        if removed {
            "Attachment removed"
        } else {
            "No matching attachment"
        }
        .to_string(),
    )
    .with_data(json!({ "removed": removed })))
}

/// Both `calibrate-photo` and `body-rect` mutate an entry that already
/// lives in the on-disk library. A freshly-`lib`'d entry sits in the
/// pending-review buffer (its staged photos have no ids yet, and its
/// body rect isn't persisted until confirm), so refuse clearly and tell
/// the agent to confirm it first instead of silently doing nothing.
fn ensure_confirmed(
    project: &Project,
    verb: &str,
    key: &str,
) -> Result<pcb_core::LibraryEntry, ToolError> {
    if let Some(entry) = project.library().find(key) {
        return Ok(entry);
    }
    if project.find_pending_library_entry(key).is_some() {
        return Err(ToolError::invalid_params(format!(
            "{verb}: {key} is still pending review — confirm it in the library review pane first \
             (attachments only get ids, and calibration/body rects only persist, once the entry lands on disk)"
        )));
    }
    Err(ToolError::invalid_params(format!(
        "{verb}: no library entry with key {key}"
    )))
}

/// Resolve a user-supplied attachment token to a full attachment id.
/// Accepts an exact id, a unique id prefix, or an (unambiguous) filename.
/// Errors listing the candidates when the token is ambiguous or matches
/// nothing, so the agent can pick the right one.
fn resolve_attachment_id(entry: &pcb_core::LibraryEntry, token: &str) -> Result<String, String> {
    if entry.attachments.is_empty() {
        return Err(format!(
            "{} has no attachments — `attach {} photo <path>` and confirm the entry first",
            entry.key, entry.key
        ));
    }
    // An exact id match wins outright.
    if let Some(a) = entry.attachments.iter().find(|a| a.id == token) {
        return Ok(a.id.clone());
    }
    let matches: Vec<&pcb_core::Attachment> = entry
        .attachments
        .iter()
        .filter(|a| a.id.starts_with(token) || a.filename == token)
        .collect();
    match matches.as_slice() {
        [one] => Ok(one.id.clone()),
        [] => Err(format!(
            "no attachment on {} matches `{token}` — candidates: {}",
            entry.key,
            attachment_candidates(entry)
        )),
        many => Err(format!(
            "`{token}` is ambiguous on {} ({} matches) — candidates: {}",
            entry.key,
            many.len(),
            attachment_candidates(entry)
        )),
    }
}

/// `<id8> (filename)` list for the "no/ambiguous match" errors.
fn attachment_candidates(entry: &pcb_core::LibraryEntry) -> String {
    entry
        .attachments
        .iter()
        .map(|a| format!("{} ({})", &a.id[..a.id.len().min(8)], a.filename))
        .collect::<Vec<_>>()
        .join(", ")
}

#[derive(Debug, Deserialize)]
struct LibraryCalibratePhotoInput {
    key: String,
    att: String,
    a_px_x: f64,
    a_px_y: f64,
    b_px_x: f64,
    b_px_y: f64,
    a_pad: String,
    b_pad: String,
}

fn tool_library_calibrate_photo(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: LibraryCalibratePhotoInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("calibrate-photo: {e}")))?;
    // Calibration needs a real attachment id, which only exists once the
    // entry is confirmed to the on-disk library (pending entries stage
    // photos without ids). Refuse clearly if it's still pending.
    let entry = ensure_confirmed(project, "calibrate-photo", &input.key)?;
    let att_id = resolve_attachment_id(&entry, &input.att).map_err(ToolError::invalid_params)?;
    let calibration = pcb_core::PhotoCalibration {
        a_px: (input.a_px_x, input.a_px_y),
        b_px: (input.b_px_x, input.b_px_y),
        a_pad: input.a_pad,
        b_pad: input.b_pad,
    };
    // Shared core validation path (same one the Tauri command uses).
    let transform = project
        .library()
        .calibrate_photo(&input.key, &att_id, calibration)
        .map_err(ToolError::invalid_params)?;
    project.notify_library_changed();

    let att = entry.attachments.iter().find(|a| a.id == att_id);
    let filename = att.map_or(att_id.as_str(), |a| a.filename.as_str());
    // Implied photo width in mm = raw pixel width × scale. Read just the
    // image header (imagesize) — best-effort, so a non-image or missing
    // file only drops the width from the report.
    let width_px = att
        .and_then(|a| imagesize::size(project.library().attachment_path(a)).ok())
        .map(|d| d.width);
    let scale = transform.scale_mm_per_px;
    let width_mm = width_px.map(|w| w as f64 * scale);
    let msg = match width_mm {
        Some(w) => format!(
            "Calibrated {filename} on {}: scale {scale:.5} mm/px → photo ≈ {w:.2} mm wide",
            input.key
        ),
        None => format!(
            "Calibrated {filename} on {}: scale {scale:.5} mm/px",
            input.key
        ),
    };
    project.log(ActivityLevel::Info, msg.clone());
    Ok(text_result(msg).with_data(json!({
        "key": input.key,
        "attachment_id": att_id,
        "scale_mm_per_px": scale,
        "rotation_deg": transform.rotation_deg,
        "photo_width_px": width_px,
        "photo_width_mm": width_mm,
    })))
}

#[derive(Debug, Deserialize)]
struct LibraryRectifyPhotoInput {
    key: String,
    att: String,
    x1: f64,
    y1: f64,
    x2: f64,
    y2: f64,
    x3: f64,
    y3: f64,
    x4: f64,
    y4: f64,
    w_mm: f64,
    h_mm: f64,
}

fn tool_library_rectify_photo(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: LibraryRectifyPhotoInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("rectify-photo: {e}")))?;
    // Rectification needs a real attachment id, which only exists once the
    // entry is confirmed to the on-disk library (same gate as calibrate).
    let entry = ensure_confirmed(project, "rectify-photo", &input.key)?;
    let att_id = resolve_attachment_id(&entry, &input.att).map_err(ToolError::invalid_params)?;
    let src_filename = entry
        .attachments
        .iter()
        .find(|a| a.id == att_id)
        .map_or(att_id.clone(), |a| a.filename.clone());
    let corners = [
        (input.x1, input.y1),
        (input.x2, input.y2),
        (input.x3, input.y3),
        (input.x4, input.y4),
    ];
    let outcome = project
        .library()
        .rectify_photo(&input.key, &att_id, corners, input.w_mm, input.h_mm)
        .map_err(ToolError::invalid_params)?;
    project.notify_library_changed();

    // Human-readable summary, including the calibration-remap verdict.
    let cal_msg = match &outcome.calibration {
        Some(c) => {
            let verdict = if c.residual_deg <= 2.0 {
                "auto-remapped, axis-aligned"
            } else {
                "REMAPPED BUT OFF-AXIS — re-check corners/order"
            };
            format!(
                "; calibration {verdict}: scale {:.5} mm/px, residual {:.2}° from square",
                c.scale_mm_per_px, c.residual_deg
            )
        }
        None => {
            "; original had no calibration — run calibrate-photo on the rectified attachment".into()
        }
    };
    let msg = format!(
        "Rectified {src_filename} → {} ({}×{} px, {:.1} px/mm) on {}{cal_msg}",
        outcome.filename, outcome.width_px, outcome.height_px, outcome.px_per_mm, input.key
    );
    project.log(ActivityLevel::Info, msg.clone());
    Ok(text_result(msg).with_data(json!({
        "key": input.key,
        "source_attachment_id": att_id,
        "attachment_id": outcome.attachment_id,
        "filename": outcome.filename,
        "width_px": outcome.width_px,
        "height_px": outcome.height_px,
        "px_per_mm": outcome.px_per_mm,
        "calibration": outcome.calibration.map(|c| json!({
            "remapped": true,
            "scale_mm_per_px": c.scale_mm_per_px,
            "rotation_deg": c.rotation_deg,
            "residual_deg": c.residual_deg,
        })),
    })))
}

#[derive(Debug, Deserialize)]
struct LibrarySetBodyRectInput {
    key: String,
    #[serde(default)]
    min_x_mm: Option<f64>,
    #[serde(default)]
    min_y_mm: Option<f64>,
    #[serde(default)]
    max_x_mm: Option<f64>,
    #[serde(default)]
    max_y_mm: Option<f64>,
    #[serde(default)]
    clear: bool,
}

#[derive(Debug, Deserialize)]
struct LibrarySetEdgeMountedInput {
    key: String,
    edge_mounted: bool,
    /// Which LOCAL side must face the outline: "top" | "right" |
    /// "bottom" | "left". Absent = any side (legacy).
    #[serde(default)]
    side: Option<String>,
}

fn tool_library_set_edge_mounted(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: LibrarySetEdgeMountedInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("edge-mount: {e}")))?;
    ensure_confirmed(project, "edge-mount", &input.key)?;
    let side = match input.side.as_deref() {
        None => None,
        Some(raw) => Some(pcb_core::EdgeSide::parse(raw).ok_or_else(|| {
            ToolError::invalid_params(format!(
                "edge-mount: unknown side `{raw}` (want top|right|bottom|left)"
            ))
        })?),
    };
    project
        .library()
        .set_edge_mount(&input.key, input.edge_mounted, side)
        .map_err(ToolError::invalid_params)?;
    project.notify_library_changed();
    let msg = format!(
        "edge-mount on {}: {}{}",
        input.key,
        if input.edge_mounted { "true" } else { "false" },
        side.map(|s| format!(" (local {} side on the outline)", s.name()))
            .unwrap_or_default(),
    );
    project.log(ActivityLevel::Info, msg.clone());
    Ok(text_result(msg).with_data(json!({
        "key": input.key,
        "edge_mounted": input.edge_mounted,
        "edge_side": side.map(pcb_core::EdgeSide::name),
    })))
}

fn tool_library_set_body_rect(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: LibrarySetBodyRectInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("body-rect: {e}")))?;
    ensure_confirmed(project, "body-rect", &input.key)?;

    if input.clear {
        project
            .library()
            .clear_body_rect(&input.key)
            .map_err(ToolError::invalid_params)?;
        project.notify_library_changed();
        let msg = format!("Cleared body rect on {}", input.key);
        project.log(ActivityLevel::Info, msg.clone());
        return Ok(text_result(msg).with_data(json!({ "key": input.key, "cleared": true })));
    }

    let (Some(min_x), Some(min_y), Some(max_x), Some(max_y)) = (
        input.min_x_mm,
        input.min_y_mm,
        input.max_x_mm,
        input.max_y_mm,
    ) else {
        return Err(ToolError::invalid_params(
            "body-rect: need MINX MINY MAXX MAXY (or `clear`)",
        ));
    };
    // Normalise so min/max can be given in any order (matches the Tauri
    // command). set_body_rect atomically derives the placement margin.
    let body = pcb_core::BodyRect {
        min_x_mm: min_x.min(max_x),
        min_y_mm: min_y.min(max_y),
        max_x_mm: min_x.max(max_x),
        max_y_mm: min_y.max(max_y),
    };
    project
        .library()
        .set_body_rect(&input.key, body)
        .map_err(ToolError::invalid_params)?;
    project.notify_library_changed();

    let margin = project
        .library()
        .find(&input.key)
        .map(|e| e.placement_margin)
        .unwrap_or_default();
    let w = body.max_x_mm - body.min_x_mm;
    let h = body.max_y_mm - body.min_y_mm;
    let msg = format!(
        "Body rect on {}: {w:.2}×{h:.2} mm → placement margin T{:.2} R{:.2} B{:.2} L{:.2} mm",
        input.key, margin.top_mm, margin.right_mm, margin.bottom_mm, margin.left_mm
    );
    project.log(ActivityLevel::Info, msg.clone());
    Ok(text_result(msg).with_data(json!({
        "key": input.key,
        "body_rect": {
            "min_x_mm": body.min_x_mm,
            "min_y_mm": body.min_y_mm,
            "max_x_mm": body.max_x_mm,
            "max_y_mm": body.max_y_mm,
        },
        "width_mm": w,
        "height_mm": h,
        "placement_margin": {
            "top_mm": margin.top_mm,
            "right_mm": margin.right_mm,
            "bottom_mm": margin.bottom_mm,
            "left_mm": margin.left_mm,
        },
    })))
}

#[derive(Debug, Deserialize)]
struct LibraryDeleteInput {
    key: String,
}

fn tool_library_delete(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: LibraryDeleteInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("library.delete: {e}")))?;
    let removed = project
        .library()
        .delete(&input.key)
        .map_err(ToolError::invalid_params)?;
    if removed {
        let count = project.library().list().len();
        project
            .events()
            .publish(pcb_core::Event::LibraryChanged { count });
        project.log(
            ActivityLevel::Info,
            format!("library.delete: {}", input.key),
        );
    }
    Ok(text_result(
        if removed {
            "Entry removed"
        } else {
            "No matching entry"
        }
        .to_string(),
    )
    .with_data(json!({ "removed": removed })))
}

#[derive(Debug, Deserialize)]
struct PaletteAddFromLibraryInput {
    reference: String,
    key: String,
    #[serde(default)]
    value: Option<String>,
    #[serde(default)]
    rotation: Option<f32>,
    #[serde(default = "default_layer")]
    layer: LayerInput,
}

fn tool_palette_add_from_library(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: PaletteAddFromLibraryInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("palette.add_from_library: {e}")))?;
    let entry = project.library().find(&input.key).ok_or_else(|| {
        ToolError::invalid_params(format!(
            "palette.add_from_library: no library entry with key {}",
            input.key
        ))
    })?;

    // Pull value/key/description/edge from the schematic symbol if it
    // exists, falling back to the library entry's defaults. The
    // schematic also carries the per-pad net assignment.
    let (resolved_value, key_field, description_field, pads, edge_from_schematic) = {
        let snap = project.read();
        let sch = snap.schematic();
        let symbol = sch.find_by_reference(&input.reference).ok_or_else(|| {
            ToolError::invalid_params(format!(
                "palette.add_from_library: no schematic symbol named {}",
                input.reference
            ))
        })?;
        let value = input
            .value
            .clone()
            .filter(|s| !s.is_empty())
            .or_else(|| (!symbol.value.is_empty()).then(|| symbol.value.clone()))
            .unwrap_or_else(|| entry.default_value.clone());
        let key_field = if symbol.key.is_empty() {
            input.key.clone()
        } else {
            symbol.key.clone()
        };
        let description_field = if symbol.description.is_empty() {
            entry.description.clone()
        } else {
            symbol.description.clone()
        };
        let vt = entry.footprint_view_transform;
        let pads: Vec<Pad> = entry
            .pads
            .iter()
            .map(|p| {
                // Library pads are numbered ("1", "2", ...) but the
                // schematic side may use names ("A"/"K" for LEDs, "VBAT"
                // for power pins). Look up by number first, then by the
                // pad's name so net wiring survives across either
                // convention.
                let net = sch
                    .net_for_pin(symbol.id, &p.number)
                    .or_else(|| {
                        if p.name.is_empty() {
                            None
                        } else {
                            sch.net_for_pin(symbol.id, &p.name)
                        }
                    })
                    .map(str::to_string);
                // Apply the library entry's footprint_view_transform to
                // the native pad geometry. This is the orientation the
                // user dialled in via the review pane; the placer then
                // layers `place X Y ROT` on top of this. The original
                // `LibraryPad` (and `index.json`) stay untouched so the
                // review pane still drives off the native data.
                let (x_mm, y_mm) = vt.apply_point_mm(p.x_mm, p.y_mm);
                let (w_mm, h_mm) = vt.apply_size_mm(p.w_mm, p.h_mm);
                Pad {
                    number: p.number.clone(),
                    name: p.name.clone(),
                    offset: Point::new(Length::from_mm(x_mm), Length::from_mm(y_mm)),
                    size: (Length::from_mm(w_mm), Length::from_mm(h_mm)),
                    layer: input.layer.clone().into(),
                    net,
                    drill: p.drill_mm.map(Length::from_mm),
                }
            })
            .collect();
        // edge_mounted: schematic doesn't have this yet; just inherit
        // from library.
        (
            value,
            key_field,
            description_field,
            pads,
            entry.edge_mounted,
        )
    };

    // Library silk lives in footprint-local mm just like the pads, so it
    // gets the same view transform — body outlines and pin-1 dots stay
    // visually attached to the pads after a flip / rotate.
    let vt = entry.footprint_view_transform;
    let silk: Vec<FootprintSilk> = entry
        .silk
        .iter()
        .map(|s| library_silk_to_footprint_with_view(s, vt))
        .collect();
    let footprint = Footprint {
        id: pcb_core::Id::new(),
        reference: input.reference.clone(),
        value: resolved_value,
        library: format!("library:{}", input.key),
        position: Point::new(Length::from_mm(-100.0), Length::from_mm(-100.0)),
        rotation: input.rotation.unwrap_or(entry.default_rotation_deg),
        layer: input.layer.clone().into(),
        pads,
        key: key_field,
        description: description_field,
        edge_mounted: edge_from_schematic,
        edge_side: entry.edge_side,
        silk,
    };
    project
        .palette_add(footprint)
        .map_err(ToolError::invalid_params)?;
    project.log(
        ActivityLevel::Info,
        format!(
            "palette.add_from_library: {} ← {}",
            input.reference, input.key
        ),
    );
    Ok(
        text_result(format!("Spawned {} from {}", input.reference, input.key))
            .with_data(json!({ "reference": input.reference, "key": input.key })),
    )
}

fn tool_place_from_palette(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: PlaceFromPaletteInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("placement.place_from_palette: {e}")))?;
    let id = project
        .place_from_palette(
            &input.reference,
            Point::new(Length::from_mm(input.x_mm), Length::from_mm(input.y_mm)),
        )
        .map_err(ToolError::invalid_params)?;
    project.log(
        ActivityLevel::Info,
        format!(
            "placement.place_from_palette: {} at ({:.2}, {:.2}) mm",
            input.reference, input.x_mm, input.y_mm
        ),
    );
    Ok(text_result(format!("Placed {}", input.reference))
        .with_data(json!({"id": id.0.to_string()})))
}

#[derive(Debug, Deserialize)]
struct EdgePlaceInput {
    reference: String,
    side: String,
    #[serde(default)]
    along_mm: Option<f64>,
}

fn tool_placement_edge_place(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: EdgePlaceInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("placement.edge_place: {e}")))?;
    let side = pcb_core::EdgeSide::parse(&input.side).ok_or_else(|| {
        ToolError::invalid_params(format!(
            "edge-place: side must be left|right|top|bottom, got {}",
            input.side
        ))
    })?;
    let (id, x_mm, y_mm, rot) = project
        .place_edge_from_palette(&input.reference, side, input.along_mm)
        .map_err(ToolError::invalid_params)?;
    project.log(
        ActivityLevel::Info,
        format!(
            "placement.edge_place: {} on {} at ({:.2}, {:.2}) mm rot={rot:.0}",
            input.reference,
            side.name(),
            x_mm,
            y_mm
        ),
    );
    Ok(text_result(format!(
        "Edge-placed {} on {} at ({:.2}, {:.2}) mm rot={rot:.0}",
        input.reference,
        side.name(),
        x_mm,
        y_mm
    ))
    .with_data(json!({
        "id": id.0.to_string(),
        "x_mm": x_mm,
        "y_mm": y_mm,
        "rotation_deg": rot,
        "side": side.name(),
    })))
}

#[derive(Debug, Deserialize)]
struct PlacementBatchItem {
    reference: String,
    x_mm: f64,
    y_mm: f64,
    #[serde(default)]
    rotation: Option<f32>,
}

#[derive(Debug, Deserialize)]
struct PlacementBatchInput {
    items: Vec<PlacementBatchItem>,
}

fn tool_placement_batch(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: PlacementBatchInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("placement.batch: {e}")))?;
    let mut results = Vec::with_capacity(input.items.len());
    let mut ok_count = 0_usize;
    let mut fail_count = 0_usize;
    for item in input.items {
        let pos = Point::new(Length::from_mm(item.x_mm), Length::from_mm(item.y_mm));
        let placed = project.place_from_palette(&item.reference, pos);
        match placed {
            Ok(id) => {
                // Apply rotation after placement so it shares the same
                // overlap-vs-edge gates as a manual call would.
                if let Some(deg) = item.rotation {
                    if let Err(rot_err) = project.rotate_footprint(&item.reference, deg) {
                        fail_count += 1;
                        results.push(json!({
                            "reference": item.reference,
                            "ok": false,
                            "stage": "rotate",
                            "error": rot_err,
                            "id": id.0.to_string(),
                        }));
                        continue;
                    }
                }
                ok_count += 1;
                results.push(json!({
                    "reference": item.reference,
                    "ok": true,
                    "id": id.0.to_string(),
                }));
            }
            Err(e) => {
                fail_count += 1;
                results.push(json!({
                    "reference": item.reference,
                    "ok": false,
                    "stage": "place",
                    "error": e,
                }));
            }
        }
    }
    project.log(
        ActivityLevel::Info,
        format!("placement.batch: {ok_count} placed, {fail_count} failed"),
    );
    // Surface per-item errors in the text reply — HTTP is text/plain and
    // agents cannot see structuredContent.without parsing JSON envelopes.
    let mut lines = vec![format!("{ok_count} placed, {fail_count} failed")];
    for r in &results {
        let reference = r.get("reference").and_then(|v| v.as_str()).unwrap_or("?");
        if r.get("ok").and_then(|v| v.as_bool()) == Some(true) {
            lines.push(format!("  ok {reference}"));
        } else {
            let stage = r.get("stage").and_then(|v| v.as_str()).unwrap_or("?");
            let err = r.get("error").and_then(|v| v.as_str()).unwrap_or("unknown");
            lines.push(format!("  FAIL {reference} ({stage}): {err}"));
        }
    }
    Ok(text_result(lines.join("\n")).with_data(json!({
        "ok_count": ok_count,
        "fail_count": fail_count,
        "results": results,
    })))
}

#[derive(Debug, Deserialize)]
struct PlacementMoveInput {
    reference: String,
    x_mm: f64,
    y_mm: f64,
}

fn tool_placement_move(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: PlacementMoveInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("placement.move: {e}")))?;
    project
        .move_footprint_to(
            &input.reference,
            Point::new(Length::from_mm(input.x_mm), Length::from_mm(input.y_mm)),
        )
        .map_err(ToolError::invalid_params)?;
    Ok(text_result(format!(
        "Moved {} to ({:.2}, {:.2}) mm",
        input.reference, input.x_mm, input.y_mm
    ))
    .into())
}

#[derive(Debug, Deserialize)]
struct PlacementRotateInput {
    reference: String,
    degrees: f32,
}

fn tool_placement_rotate(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: PlacementRotateInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("placement.rotate: {e}")))?;
    let normalised = input.degrees.rem_euclid(360.0);
    project
        .rotate_footprint(&input.reference, normalised)
        .map_err(ToolError::invalid_params)?;
    project.log(
        ActivityLevel::Info,
        format!("placement.rotate: {} → {normalised:.0}°", input.reference),
    );
    Ok(text_result(format!("Rotated {} to {normalised:.0}°", input.reference)).into())
}

#[derive(Debug, Deserialize)]
struct PlacementDeleteInput {
    refs: Vec<String>,
}

fn tool_placement_delete(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: PlacementDeleteInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("placement.delete: {e}")))?;
    if input.refs.is_empty() {
        return Err(ToolError::invalid_params(
            "placement.delete: at least one reference required".to_string(),
        ));
    }
    // Stop at the first ref that doesn't exist so the human sees the
    // typo immediately rather than getting a half-applied delete.
    let mut summaries: Vec<pcb_core::DeletedFootprint> = Vec::with_capacity(input.refs.len());
    for r in &input.refs {
        match project.delete_footprint_by_ref(r) {
            Ok(s) => summaries.push(s),
            Err(e) => {
                return Err(ToolError::invalid_params(e));
            }
        }
    }
    let mut total_traces = 0_usize;
    let mut total_vias = 0_usize;
    let mut total_pads = 0_usize;
    let mut orphaned: Vec<String> = Vec::new();
    let mut per_ref = Vec::with_capacity(summaries.len());
    for s in &summaries {
        total_traces += s.traces_removed;
        total_vias += s.vias_removed;
        total_pads += s.pad_count;
        for n in &s.orphaned_nets {
            if !orphaned.contains(n) {
                orphaned.push(n.clone());
            }
        }
        let key_display = if s.key.is_empty() {
            s.library.clone()
        } else {
            s.key.clone()
        };
        per_ref.push(json!({
            "reference": s.reference,
            "id": s.id.0.to_string(),
            "key": key_display,
            "pads": s.pad_count,
            "traces_removed": s.traces_removed,
            "vias_removed": s.vias_removed,
            "orphaned_nets": s.orphaned_nets,
        }));
    }
    let refs_csv = summaries
        .iter()
        .map(|s| s.reference.as_str())
        .collect::<Vec<_>>()
        .join(", ");
    // Use the first summary's key/lib for the single-ref reply shape
    // requested in the spec; for multi-ref we degrade to a roll-up.
    let mut msg = if summaries.len() == 1 {
        let s = &summaries[0];
        let key_display = if s.key.is_empty() {
            s.library.clone()
        } else {
            s.key.clone()
        };
        format!(
            "removed {} ({}, {} pads) + {} traces + {} vias",
            s.reference, key_display, s.pad_count, s.traces_removed, s.vias_removed,
        )
    } else {
        format!(
            "removed {} footprint(s) [{}] + {} traces + {} vias ({} pads total)",
            summaries.len(),
            refs_csv,
            total_traces,
            total_vias,
            total_pads,
        )
    };
    if !orphaned.is_empty() {
        msg.push_str(&format!(
            " — WARNING: net(s) {} now have no pads on the board",
            orphaned.join(", ")
        ));
    }
    project.log(
        ActivityLevel::Info,
        format!("placement.delete: {refs_csv} ({total_traces} traces, {total_vias} vias cleared)"),
    );
    Ok(text_result(msg).with_data(json!({
        "removed": per_ref,
        "total_traces_removed": total_traces,
        "total_vias_removed": total_vias,
        "orphaned_nets": orphaned,
    })))
}

fn tool_placement_clear_board(project: &Project) -> Result<Value, ToolError> {
    let refs = project.clear_board_placements();
    let msg = if refs.is_empty() {
        "board already empty".to_string()
    } else {
        format!("cleared {} footprint(s) and all routing", refs.len())
    };
    project.log(
        ActivityLevel::Info,
        format!("placement.clear_board: {} removed", refs.len()),
    );
    Ok(text_result(msg).with_data(json!({"removed": refs})))
}

#[derive(Debug, Deserialize)]
struct ClearNetInput {
    net: String,
}

fn tool_route_clear_net(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: ClearNetInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("route.clear_net: {e}")))?;
    let removed = project.clear_net_routing(&input.net);
    project.log(
        ActivityLevel::Info,
        format!("route.clear_net: {} ({} item(s))", input.net, removed),
    );
    Ok(
        text_result(format!("Cleared {removed} item(s) from net {}", input.net))
            .with_data(json!({"removed": removed})),
    )
}

#[derive(Debug, Deserialize)]
struct DeleteByIdInput {
    id: String,
}

fn parse_id(s: &str) -> Result<pcb_core::Id, ToolError> {
    pcb_core::Id::parse(s).map_err(|e| ToolError::invalid_params(format!("invalid id {s}: {e}")))
}

fn tool_route_delete_trace(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: DeleteByIdInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("route.delete_trace: {e}")))?;
    let id = parse_id(&input.id)?;
    let ok = project.delete_trace(id);
    Ok(text_result(
        if ok {
            "Trace removed"
        } else {
            "Trace not found"
        }
        .to_string(),
    )
    .with_data(json!({"removed": ok})))
}

fn tool_route_delete_via(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: DeleteByIdInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("route.delete_via: {e}")))?;
    let id = parse_id(&input.id)?;
    let ok = project.delete_via(id);
    Ok(
        text_result(if ok { "Via removed" } else { "Via not found" }.to_string())
            .with_data(json!({"removed": ok})),
    )
}

#[derive(Debug, Deserialize)]
struct PadPlan {
    number: String,
    #[serde(default)]
    name: String,
    x_mm: f64,
    y_mm: f64,
    w_mm: f64,
    h_mm: f64,
    #[serde(default = "default_layer")]
    layer: LayerInput,
    /// Plated through-hole drill diameter in mm. Omit for SMD.
    #[serde(default)]
    drill_mm: Option<f64>,
}

#[derive(Debug, Deserialize)]
struct AddTraceInput {
    net: String,
    layer: LayerInput,
    x1_mm: f64,
    y1_mm: f64,
    x2_mm: f64,
    y2_mm: f64,
    width_mm: f64,
}

fn tool_route_add_trace(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: AddTraceInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("route.add_trace: {e}")))?;
    let layer: CopperLayer = input.layer.clone().into();
    let id = project.add_trace(Trace {
        id: pcb_core::Id::new(),
        layer,
        start: Point::new(Length::from_mm(input.x1_mm), Length::from_mm(input.y1_mm)),
        end: Point::new(Length::from_mm(input.x2_mm), Length::from_mm(input.y2_mm)),
        width: Length::from_mm(input.width_mm),
        net: input.net.clone(),
    });
    Ok(text_result(format!(
        "trace {} on {} ({})",
        id.0,
        layer_to_str(layer),
        input.net
    ))
    .with_data(json!({"id": id.0.to_string()})))
}

#[derive(Debug, Deserialize)]
struct AddViaInput {
    net: String,
    x_mm: f64,
    y_mm: f64,
    drill_mm: f64,
    diameter_mm: f64,
}

fn tool_route_add_via(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: AddViaInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("route.add_via: {e}")))?;
    let id = project.add_via(Via {
        id: pcb_core::Id::new(),
        position: Point::new(Length::from_mm(input.x_mm), Length::from_mm(input.y_mm)),
        drill: Length::from_mm(input.drill_mm),
        diameter: Length::from_mm(input.diameter_mm),
        net: input.net.clone(),
    });
    Ok(text_result(format!("via {} ({})", id.0, input.net))
        .with_data(json!({"id": id.0.to_string()})))
}

/// Auto-stitch every isolated plane pad: drop a same-net via (beside the
/// pad with a stub, or via-in-pad when fully boxed) that ties the pad to
/// the pour on another copper layer. Pads no via can reach are reported
/// as needing a manual reroute.
fn tool_route_stitch_isolated_pads(project: &Project) -> Result<Value, ToolError> {
    let plan = {
        let snap = project.read();
        pcb_core::stitch::plan_stitches(snap.board(), pcb_core::stitch::StitchParams::default())
    };
    let added = plan.proposals.len();
    for s in &plan.proposals {
        project.add_via(s.via.clone());
        if let Some(stub) = &s.stub {
            project.add_trace(stub.clone());
        }
    }
    let msg = if plan.unreachable.is_empty() {
        format!("stitched {added} isolated pad(s)")
    } else {
        format!(
            "stitched {added} isolated pad(s); {} still unreachable (reroute needed): {}",
            plan.unreachable.len(),
            plan.unreachable.join(", ")
        )
    };
    project.log(ActivityLevel::Info, &msg);
    Ok(text_result(msg).with_data(json!({
        "stitched": added,
        "unreachable": plan.unreachable,
    })))
}

fn tool_route_clear(project: &Project) -> Result<Value, ToolError> {
    project.clear_routing();
    project.log(ActivityLevel::Info, "route.clear");
    Ok(text_result("Cleared all traces and vias").into())
}

#[derive(Debug, Deserialize)]
struct PourInput {
    net: String,
    layer: LayerInput,
}

fn tool_pour_add(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: PourInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("pour.add: {e}")))?;
    let layer: CopperLayer = input.layer.into();
    project.add_pour(Pour {
        net: input.net.clone(),
        layer,
        thermal_relief: pcb_core::ThermalRelief::default(),
        stitching: pcb_core::StitchPolicy::None,
    });
    project.log(
        ActivityLevel::Info,
        format!("pour.add {} on {:?}", input.net, layer),
    );
    Ok(
        text_result(format!("Pour added: net={} layer={:?}", input.net, layer))
            .with_data(json!({"net": input.net, "layer": layer_to_str(layer)})),
    )
}

#[derive(Debug, Deserialize)]
struct KeepoutAddInput {
    /// Vertices as `[[x_mm, y_mm], ...]`. Three or more.
    points: Vec<[f64; 2]>,
    #[serde(default)]
    layer: Option<String>,
    #[serde(default)]
    label: Option<String>,
}

fn tool_keepout_add(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: KeepoutAddInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("keepout.add: {e}")))?;
    if input.points.len() < 3 {
        return Err(ToolError::invalid_params(
            "keepout.add: need at least 3 points",
        ));
    }
    let polygon: Vec<Point> = input
        .points
        .iter()
        .map(|[x, y]| Point::new(Length::from_mm(*x), Length::from_mm(*y)))
        .collect();
    let layers: Vec<CopperLayer> = match input.layer.as_deref().unwrap_or("both") {
        "top" => vec![CopperLayer::Top],
        "bottom" => vec![CopperLayer::Bottom],
        "both" | "" => vec![],
        other => {
            return Err(ToolError::invalid_params(format!(
                "keepout.add: layer must be top|bottom|both, got `{other}`"
            )));
        }
    };
    let kp = pcb_core::Keepout {
        id: pcb_core::Id::new(),
        polygon,
        layers,
        nets_allowed: Vec::new(),
        label: input.label.unwrap_or_default(),
    };
    let id = project.add_keepout(kp);
    project.log(ActivityLevel::Info, format!("keepout.add: {}", id.0));
    Ok(
        text_result(format!("Keepout added: {}", id.0))
            .with_data(json!({ "id": id.0.to_string() })),
    )
}

fn tool_keepout_list(project: &Project) -> Result<Value, ToolError> {
    let snap = project.read();
    let board = snap.board();
    let items: Vec<Value> = board
        .keepouts
        .iter()
        .map(|kp| {
            json!({
                "id": kp.id.0.to_string(),
                "label": kp.label,
                "points": kp.polygon.iter()
                    .map(|p| json!([p.x.to_mm(), p.y.to_mm()]))
                    .collect::<Vec<_>>(),
                "layers": kp.layers.iter().map(|l| layer_to_str(*l)).collect::<Vec<_>>(),
                "nets_allowed": kp.nets_allowed.clone(),
            })
        })
        .collect();
    Ok(text_result(format!("{} keepout(s)", items.len())).with_data(json!({ "keepouts": items })))
}

#[derive(Debug, Deserialize)]
struct KeepoutRemoveInput {
    id: String,
}

fn tool_keepout_remove(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: KeepoutRemoveInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("keepout.remove: {e}")))?;
    let id = pcb_core::Id::parse(&input.id).map_err(ToolError::invalid_params)?;
    let removed = project.remove_keepout(id);
    Ok(text_result(if removed {
        format!("Keepout {} removed", input.id)
    } else {
        format!("No keepout with id {}", input.id)
    })
    .with_data(json!({ "removed": removed })))
}

#[derive(Debug, Deserialize)]
struct PourReliefInput {
    net: String,
    style: String,
    #[serde(default)]
    spoke_width_mm: Option<f64>,
    #[serde(default)]
    gap_mm: Option<f64>,
}

fn tool_pour_relief(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: PourReliefInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("pour.relief: {e}")))?;
    let relief = match input.style.as_str() {
        "solid" => pcb_core::ThermalRelief::Solid,
        "spokes" => pcb_core::ThermalRelief::Spokes4 {
            spoke_width_mm: input.spoke_width_mm.unwrap_or(0.4),
            gap_mm: input.gap_mm.unwrap_or(0.4),
        },
        other => {
            return Err(ToolError::invalid_params(format!(
                "pour.relief: style must be solid|spokes, got `{other}`"
            )));
        }
    };
    let changed = project.set_pour_relief(&input.net, relief);
    project.log(
        ActivityLevel::Info,
        format!("pour.relief: net={} style={}", input.net, input.style),
    );
    Ok(text_result(if changed > 0 {
        format!(
            "Updated {} pour(s) on net `{}` to {}",
            changed, input.net, input.style
        )
    } else {
        format!("No pour found on net `{}`", input.net)
    })
    .with_data(json!({"changed": changed, "net": input.net, "style": input.style})))
}

#[derive(Debug, Deserialize)]
struct PourStitchInput {
    net: String,
    policy: String,
    #[serde(default)]
    pitch_mm: Option<f64>,
    #[serde(default)]
    clearance_mm: Option<f64>,
}

fn tool_pour_stitch(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: PourStitchInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("pour.stitch: {e}")))?;
    let policy = match input.policy.as_str() {
        "none" => pcb_core::StitchPolicy::None,
        "grid" => pcb_core::StitchPolicy::Grid {
            pitch_mm: input.pitch_mm.unwrap_or(5.0),
            clearance_mm: input.clearance_mm.unwrap_or(0.5),
        },
        other => {
            return Err(ToolError::invalid_params(format!(
                "pour.stitch: policy must be none|grid, got `{other}`"
            )));
        }
    };
    let changed = project.set_pour_stitching(&input.net, policy);
    project.log(
        ActivityLevel::Info,
        format!("pour.stitch: net={} policy={}", input.net, input.policy),
    );
    Ok(text_result(if changed > 0 {
        format!(
            "Updated {} pour(s) on net `{}` to stitching={}",
            changed, input.net, input.policy
        )
    } else {
        format!("No pour found on net `{}`", input.net)
    })
    .with_data(json!({"changed": changed, "net": input.net, "policy": input.policy})))
}

fn tool_pour_remove(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: PourInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("pour.remove: {e}")))?;
    let layer: CopperLayer = input.layer.into();
    let removed = project.remove_pour(&input.net, layer);
    Ok(text_result(if removed {
        format!("Pour removed: net={} layer={:?}", input.net, layer)
    } else {
        format!("No pour for net={} layer={:?}", input.net, layer)
    })
    .with_data(json!({"removed": removed})))
}

#[derive(Debug, Deserialize)]
struct SilkLineInput {
    layer: LayerInput,
    x1_mm: f64,
    y1_mm: f64,
    x2_mm: f64,
    y2_mm: f64,
    #[serde(default = "default_silk_width")]
    width_mm: f64,
}

#[derive(Debug, Deserialize)]
struct SilkTextInput {
    layer: LayerInput,
    x_mm: f64,
    y_mm: f64,
    text: String,
    #[serde(default = "default_silk_size")]
    size_mm: f64,
    #[serde(default)]
    rotation: f32,
    #[serde(default = "default_anchor_middle")]
    anchor: AnchorInput,
    #[serde(default)]
    width_mm: Option<f64>,
}

fn default_silk_width() -> f64 {
    0.15
}
fn default_silk_size() -> f64 {
    1.2
}

fn tool_silk_add_line(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: SilkLineInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("silk.add_line: {e}")))?;
    let layer: SilkLayer = input.layer.into();
    let line = SilkLine {
        layer,
        start: Point::new(Length::from_mm(input.x1_mm), Length::from_mm(input.y1_mm)),
        end: Point::new(Length::from_mm(input.x2_mm), Length::from_mm(input.y2_mm)),
        width: Length::from_mm(input.width_mm),
    };
    project.add_silk_line(line);
    project.log(
        ActivityLevel::Info,
        format!(
            "silk.add_line {:?} ({:.2},{:.2})→({:.2},{:.2})",
            layer, input.x1_mm, input.y1_mm, input.x2_mm, input.y2_mm
        ),
    );
    Ok(text_result("Silk line added").with_data(json!({
        "layer": layer_to_str_silk(layer),
    })))
}

fn tool_silk_add_text(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: SilkTextInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("silk.add_text: {e}")))?;
    let layer: SilkLayer = input.layer.into();
    let size = Length::from_mm(input.size_mm);
    let text = SilkText {
        layer,
        position: Point::new(Length::from_mm(input.x_mm), Length::from_mm(input.y_mm)),
        text: input.text.clone(),
        size,
        rotation: input.rotation,
        anchor: input.anchor.into(),
        width: input
            .width_mm
            .map_or_else(|| SilkText::default_stroke(size), Length::from_mm),
    };
    project.add_silk_text(text);
    project.log(
        ActivityLevel::Info,
        format!("silk.add_text {:?} \"{}\"", layer, input.text),
    );
    Ok(
        text_result(format!("Silk text added: \"{}\"", input.text)).with_data(json!({
            "layer": layer_to_str_silk(layer),
            "text": input.text,
        })),
    )
}

fn layer_to_str_silk(layer: SilkLayer) -> &'static str {
    match layer {
        SilkLayer::Top => "top",
        SilkLayer::Bottom => "bottom",
    }
}

fn layer_to_str(layer: CopperLayer) -> &'static str {
    // Multi-layer: keep the legacy "top" / "bottom" labels for outer
    // layers (the only ones today's UI knows about). Inner layers
    // become "in1", "in2", etc.
    if layer.is_top() {
        "top"
    } else if layer.index == 1 {
        "bottom"
    } else {
        match layer.index {
            2 => "in1",
            3 => "in2",
            4 => "in3",
            5 => "in4",
            6 => "in5",
            7 => "in6",
            _ => "inner",
        }
    }
}

#[derive(Debug, Deserialize)]
struct RouteRunInput {
    #[serde(default = "default_cell")]
    cell_mm: f64,
    #[serde(default = "default_trace_w")]
    trace_width_mm: f64,
    #[serde(default = "default_clearance")]
    clearance_mm: f64,
    // Script DSL always emits numbers as floats; accept either and
    // round down so `via_cost=8` works whether typed as 8 or 8.0.
    #[serde(default = "default_via_cost", deserialize_with = "de_u32_lenient")]
    via_cost: u32,
    #[serde(default = "default_via_drill")]
    via_drill_mm: f64,
    #[serde(default = "default_via_diameter")]
    via_diameter_mm: f64,
    /// Comma-separated list of net names. When present, seeds the
    /// router's first-pass ordering — useful for GA-driven tuning.
    #[serde(default)]
    order: Option<String>,
    /// Organic post-pass (string-pulling + arc fillets). Default on.
    #[serde(default)]
    organic: Option<bool>,
    /// Largest fillet radius for the organic pass, mm.
    #[serde(default)]
    organic_fillet_mm: Option<f64>,
    /// "grid" (default) or "topo" — the rubber-band topological engine.
    #[serde(default)]
    engine: Option<String>,
    /// Soft wall-clock budget in seconds. Default 90. Pass 0 for unlimited.
    #[serde(default)]
    max_seconds: Option<f64>,
    /// Opt in to the fine-grid escape pass on ≥3-layer stackups. Off by
    /// default — see `RouteOptions::fine_escape`.
    #[serde(default)]
    fine_escape: Option<bool>,
    /// Opt in to PathFinder-style negotiated congestion. Off by default —
    /// see `RouteOptions::negotiate`.
    #[serde(default)]
    negotiate: Option<bool>,
}

fn de_u32_lenient<'de, D>(d: D) -> Result<u32, D::Error>
where
    D: serde::Deserializer<'de>,
{
    let v = serde_json::Value::deserialize(d)?;
    match v {
        serde_json::Value::Number(n) => {
            if let Some(u) = n.as_u64() {
                Ok(u as u32)
            } else if let Some(f) = n.as_f64() {
                if f.is_finite() && f >= 0.0 {
                    Ok(f as u32)
                } else {
                    Err(serde::de::Error::custom(format!("invalid via_cost: {f}")))
                }
            } else {
                Err(serde::de::Error::custom("via_cost: not a number"))
            }
        }
        other => Err(serde::de::Error::custom(format!(
            "via_cost: expected number, got {other}"
        ))),
    }
}

fn default_cell() -> f64 {
    0.20
}
fn default_trace_w() -> f64 {
    0.25
}
fn default_clearance() -> f64 {
    0.20
}
fn default_via_cost() -> u32 {
    8
}
fn default_via_drill() -> f64 {
    0.30
}
fn default_via_diameter() -> f64 {
    0.60
}

#[derive(Debug, Deserialize)]
struct AutoPlaceInput {
    refs: Vec<String>,
    /// Floats so the script parser (which emits `42` as `42.0` for any
    /// numeric kv) can hand them to us; we cast to the integer types
    /// the placer wants below. Negative or NaN values are clamped.
    #[serde(default)]
    iters: Option<f64>,
    #[serde(default)]
    seed: Option<f64>,
    #[serde(default)]
    max_step_mm: Option<f64>,
    #[serde(default)]
    min_step_mm: Option<f64>,
    #[serde(default)]
    min_gap_mm: Option<f64>,
    #[serde(default)]
    solder_gap_mm: Option<f64>,
    #[serde(default)]
    gap_penalty_factor: Option<f64>,
    #[serde(default)]
    congestion_penalty_factor: Option<f64>,
    #[serde(default)]
    congestion_resolution: Option<f64>,
    #[serde(default)]
    crossing_penalty_factor: Option<f64>,
    #[serde(default)]
    edge_plan: Option<bool>,
    #[serde(default)]
    global: Option<bool>,
    #[serde(default)]
    global_iters: Option<f64>,
    #[serde(default)]
    density_bins: Option<f64>,
    #[serde(default)]
    target_density: Option<f64>,
    #[serde(default)]
    target_overflow: Option<f64>,
}

fn tool_placement_auto(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: AutoPlaceInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("auto-place: {e}")))?;

    let mut opts = pcb_placer::PlaceOptions::default();
    if let Some(v) = input.iters {
        opts.max_iterations = v.max(0.0) as usize;
    }
    if let Some(v) = input.seed {
        opts.seed = v.max(0.0) as u64;
    }
    if let Some(v) = input.max_step_mm {
        opts.max_step_mm = v;
    }
    if let Some(v) = input.min_step_mm {
        opts.min_step_mm = v;
    }
    if let Some(v) = input.min_gap_mm {
        opts.min_gap_mm = v;
    }
    if let Some(v) = input.solder_gap_mm {
        opts.solder_gap_mm = v;
    }
    if let Some(v) = input.gap_penalty_factor {
        opts.gap_penalty_factor = v;
    }
    if let Some(v) = input.congestion_penalty_factor {
        opts.congestion_penalty_factor = v;
    }
    if let Some(v) = input.congestion_resolution {
        opts.congestion_resolution = v.max(0.0) as u32;
    }
    if let Some(v) = input.crossing_penalty_factor {
        opts.crossing_penalty_factor = v.max(0.0);
    }
    if let Some(v) = input.edge_plan {
        opts.edge_plan = v;
    }
    if let Some(v) = input.global {
        opts.global_stage = v;
    }
    if let Some(v) = input.global_iters {
        opts.global_iterations = v.max(1.0) as usize;
    }
    if let Some(v) = input.density_bins {
        opts.density_bins = v.max(16.0) as usize;
    }
    if let Some(v) = input.target_density {
        opts.target_density = v.clamp(0.3, 1.5);
    }
    if let Some(v) = input.target_overflow {
        opts.target_overflow = v.clamp(0.005, 1.0);
    }

    // Place on a clone so the project lock is released quickly. Apply
    // the resulting positions back through the regular `move_footprint_to`
    // / `rotate_footprint` APIs so the UI sees the moves event by event.
    let mut work = project.read().board().clone();
    // Re-sync `edge_mounted` from the live library onto every
    // footprint that has a library key. Footprints bake the flag at
    // palette-spawn time; if the library entry is later flipped to
    // edge-mounted (e.g. screw terminals), already-placed parts would
    // otherwise keep floating interior forever. Also push the flag
    // back onto the live project so compact / DRC see it.
    {
        let lib = project.library();
        let mut live_updates: Vec<(pcb_core::Id, bool, Option<pcb_core::EdgeSide>)> = Vec::new();
        for fp in work.footprints.values_mut() {
            if fp.key.is_empty() {
                continue;
            }
            let Some(entry) = lib.find(&fp.key) else {
                continue;
            };
            if fp.edge_mounted != entry.edge_mounted || fp.edge_side != entry.edge_side {
                fp.edge_mounted = entry.edge_mounted;
                fp.edge_side = entry.edge_side;
                live_updates.push((fp.id, entry.edge_mounted, entry.edge_side));
            }
        }
        for (id, flag, side) in live_updates {
            project.set_footprint_edge_mount(id, flag, side);
        }
    }
    // Build a per-id margin map from the library so footprints linked
    // to a `LibraryEntry::placement_margin` get extra body-to-body
    // breathing room during the SA search.
    let margins: pcb_placer::MarginMap = work
        .footprints_in_order()
        .filter_map(|fp| {
            if fp.key.is_empty() {
                return None;
            }
            let entry = project.library().find(&fp.key)?;
            let m = entry.placement_margin;
            if m.top_mm <= 0.0 && m.right_mm <= 0.0 && m.bottom_mm <= 0.0 && m.left_mm <= 0.0 {
                return None;
            }
            Some((fp.id, [m.top_mm, m.right_mm, m.bottom_mm, m.left_mm]))
        })
        .collect();
    let report = pcb_placer::place(&mut work, &input.refs, &opts, &margins)
        .map_err(|e| ToolError::invalid_params(format!("auto-place: {e}")))?;

    // Push the placer's FINAL state back onto the live project for
    // every movable ref — not only `report.moved`. The report's moved
    // list compares against pre-snap starting positions and can miss
    // real SA improvements (or under-report after edge snaps). Applying
    // every final pose is idempotent when nothing changed and is the
    // only way the agent sees the full result. Use the id-based,
    // unchecked Project APIs so intermediate crossings don't false-reject.
    let mut applied_moves = 0usize;
    let mut applied_rotations = 0usize;
    let live_by_ref: HashMap<String, (pcb_core::Id, pcb_core::Point, f32)> = project
        .read()
        .board()
        .footprints_in_order()
        .map(|fp| (fp.reference.clone(), (fp.id, fp.position, fp.rotation)))
        .collect();
    for r in &input.refs {
        let Some(target) = work
            .footprints_in_order()
            .find(|fp| &fp.reference == r)
            .cloned()
        else {
            continue;
        };
        let Some(&(id, live_pos, live_rot)) = live_by_ref.get(r) else {
            continue;
        };
        let pos_changed = (target.position.x.to_mm() - live_pos.x.to_mm()).abs()
            + (target.position.y.to_mm() - live_pos.y.to_mm()).abs()
            >= 0.01;
        let rot_changed = (target.rotation - live_rot).abs() > 0.5;
        // The edge planner can re-declare which local side of an
        // edge-mounted part faces the cut (see `pcb_placer::EdgePlacement`);
        // keep the live board in step so DRC / a later move agree with the
        // pose we are applying.
        if target.edge_mounted {
            let live_side = project
                .read()
                .board()
                .footprints
                .get(&id)
                .and_then(|fp| fp.edge_side);
            if live_side != target.edge_side {
                project.set_footprint_edge_mount(id, true, target.edge_side);
            }
        }
        if pos_changed && project.move_footprint(id, target.position) {
            applied_moves += 1;
        }
        if rot_changed && project.set_footprint_rotation(id, target.rotation) {
            applied_rotations += 1;
        }
    }
    let errors: Vec<String> = Vec::new();

    project.log(
        ActivityLevel::Info,
        format!(
            "auto-place: HPWL {:.1} → {:.1} mm ({:+.1} mm){}, congestion {:.0} → {:.0} ({:+.0}), {} accepted of {} iters, applied {} moves",
            report.initial_hpwl_mm,
            report.final_hpwl_mm,
            report.final_hpwl_mm - report.initial_hpwl_mm,
            report
                .global
                .as_ref()
                .map(|g| format!(" (global: {} iters → {:.1} mm, overflow {:.3})", g.iterations, g.hpwl_mm, g.overflow))
                .unwrap_or_default(),
            report.initial_congestion,
            report.final_congestion,
            report.final_congestion - report.initial_congestion,
            report.accepted,
            report.iterations,
            applied_moves,
        ),
    );
    // Crossings only make sense to mention when the board HAS bundles;
    // "0 → 0" on a board of 2-pin passives is noise.
    let crossings_text = if report.initial_crossings > 0 || report.final_crossings > 0 {
        format!(
            ", bundle crossings {} → {}",
            report.initial_crossings, report.final_crossings
        )
    } else {
        String::new()
    };

    let mut text = format!(
        "auto-place: HPWL {:.1} mm → {:.1} mm ({:+.1} mm), congestion {:.0} → {:.0} ({:+.0} cells){crossings_text}; moved {} footprint(s)",
        report.initial_hpwl_mm,
        report.final_hpwl_mm,
        report.final_hpwl_mm - report.initial_hpwl_mm,
        report.initial_congestion,
        report.final_congestion,
        report.final_congestion - report.initial_congestion,
        applied_moves,
    );
    if !report.skipped.is_empty() {
        text.push_str(&format!(
            "\n  skipped {} unknown ref(s): {}",
            report.skipped.len(),
            report.skipped.join(", "),
        ));
    }
    if !errors.is_empty() {
        text.push_str("\n  errors:");
        for e in &errors {
            text.push_str("\n    ");
            text.push_str(e);
        }
    }

    Ok(text_result(text).with_data(json!({
        "initial_hpwl_mm": round2(report.initial_hpwl_mm),
        "final_hpwl_mm": round2(report.final_hpwl_mm),
        "delta_mm": round2(report.final_hpwl_mm - report.initial_hpwl_mm),
        "global": report.global.as_ref().map(|g| json!({
            "iterations": g.iterations,
            "overflow": round2(g.overflow),
            "hpwl_mm": round2(g.hpwl_mm),
        })),
        "initial_congestion": round2(report.initial_congestion),
        "final_congestion": round2(report.final_congestion),
        "initial_crossings": report.initial_crossings,
        "final_crossings": report.final_crossings,
        "iterations": report.iterations,
        "accepted": report.accepted,
        "moved": report.moved,
        "applied_moves": applied_moves,
        "applied_rotations": applied_rotations,
        "skipped": report.skipped,
        "errors": errors,
    })))
}

#[derive(Debug, Deserialize)]
struct EdgePlanInput {
    refs: Vec<String>,
    /// Accepted for symmetry with `auto-place`; the planner is fully
    /// deterministic (no RNG), so it only travels into `PlaceOptions` for
    /// the record and cannot change the result.
    #[serde(default)]
    seed: Option<f64>,
}

/// `edge-plan REF [REF...]` — run ONLY the placer's edge-planning pass and
/// report the side + position it chose for each ref. Nothing but the named
/// parts moves, which is what makes this safe to run on a routed board's
/// connectors before committing to a full auto-place.
fn tool_placement_edge_plan(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: EdgePlanInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("edge-plan: {e}")))?;
    let mut opts = pcb_placer::PlaceOptions::default();
    if let Some(v) = input.seed {
        opts.seed = v.max(0.0) as u64;
    }

    // Plan on a clone (the project lock is released immediately) and apply
    // the result through the regular id-based move/rotate APIs so the UI
    // sees the moves event by event — same pattern as auto-place.
    let mut work = project.read().board().clone();
    // Re-sync `edge_mounted` / `edge_side` from the live library: footprints
    // bake those flags at palette-spawn time, and a part whose library entry
    // was flipped to edge-mounted afterwards would otherwise be invisible to
    // the planner. Push the flags back onto the project too, so DRC and
    // compact agree with what we just planned against.
    {
        let lib = project.library();
        let mut live_updates: Vec<(pcb_core::Id, bool, Option<pcb_core::EdgeSide>)> = Vec::new();
        for fp in work.footprints.values_mut() {
            if fp.key.is_empty() {
                continue;
            }
            let Some(entry) = lib.find(&fp.key) else {
                continue;
            };
            if fp.edge_mounted != entry.edge_mounted || fp.edge_side != entry.edge_side {
                fp.edge_mounted = entry.edge_mounted;
                fp.edge_side = entry.edge_side;
                live_updates.push((fp.id, entry.edge_mounted, entry.edge_side));
            }
        }
        for (id, flag, side) in live_updates {
            project.set_footprint_edge_mount(id, flag, side);
        }
    }
    let margins: pcb_placer::MarginMap = work
        .footprints_in_order()
        .filter_map(|fp| {
            if fp.key.is_empty() {
                return None;
            }
            let entry = project.library().find(&fp.key)?;
            let m = entry.placement_margin;
            if m.top_mm <= 0.0 && m.right_mm <= 0.0 && m.bottom_mm <= 0.0 && m.left_mm <= 0.0 {
                return None;
            }
            Some((fp.id, [m.top_mm, m.right_mm, m.bottom_mm, m.left_mm]))
        })
        .collect();

    let report = pcb_placer::plan_edge_sides(&mut work, &input.refs, &opts, &margins)
        .map_err(|e| ToolError::invalid_params(format!("edge-plan: {e}")))?;

    let live_by_ref: HashMap<
        String,
        (
            pcb_core::Id,
            pcb_core::Point,
            f32,
            Option<pcb_core::EdgeSide>,
        ),
    > = project
        .read()
        .board()
        .footprints_in_order()
        .map(|fp| {
            (
                fp.reference.clone(),
                (fp.id, fp.position, fp.rotation, fp.edge_side),
            )
        })
        .collect();
    let mut applied = 0usize;
    for p in &report.placed {
        let Some(&(id, live_pos, live_rot, live_side)) = live_by_ref.get(&p.reference) else {
            continue;
        };
        // The planner may have re-declared WHICH local side faces the cut
        // (the library's hint would have left a pin header end-on). Push
        // that back or the live board fails `edge_mount_violation` on the
        // pose we just committed.
        if p.edge_side != live_side {
            project.set_footprint_edge_mount(id, true, p.edge_side);
        }
        let pos_changed = (p.position.x.to_mm() - live_pos.x.to_mm()).abs()
            + (p.position.y.to_mm() - live_pos.y.to_mm()).abs()
            >= 0.01;
        let rot_changed = (p.rotation - live_rot).abs() > 0.5;
        if pos_changed && project.move_footprint(id, p.position) {
            applied += 1;
        }
        if rot_changed {
            project.set_footprint_rotation(id, p.rotation);
        }
    }

    project.log(
        ActivityLevel::Info,
        format!(
            "placement.edge_plan: {} part(s), cost {:.1} → {:.1}, applied {applied} move(s)",
            report.placed.len(),
            report.initial_cost,
            report.final_cost,
        ),
    );

    let mut text = format!(
        "edge-plan: {} part(s) planned, cost {:.1} → {:.1} mm-equivalent (wirelength + crossings)",
        report.placed.len(),
        report.initial_cost,
        report.final_cost,
    );
    for p in &report.placed {
        text.push_str(&format!(
            "\n  {} → {} edge, along {:.2} mm, at ({:.2}, {:.2}) mm rot={:.0}",
            p.reference,
            p.side.name(),
            p.along_mm,
            p.position.x.to_mm(),
            p.position.y.to_mm(),
            p.rotation,
        ));
    }
    for s in &report.skipped {
        text.push_str(&format!("\n  skipped {s}"));
    }

    Ok(text_result(text).with_data(json!({
        "planned": report.placed.iter().map(|p| json!({
            "reference": p.reference,
            "side": p.side.name(),
            "along_mm": round2(p.along_mm),
            "x_mm": round2(p.position.x.to_mm()),
            "y_mm": round2(p.position.y.to_mm()),
            "rotation": p.rotation,
        })).collect::<Vec<_>>(),
        "initial_cost": round2(report.initial_cost),
        "final_cost": round2(report.final_cost),
        "applied_moves": applied,
        "skipped": report.skipped,
    })))
}

#[derive(Debug, Deserialize)]
struct CompactInput {
    #[serde(default)]
    min_w_mm: Option<f64>,
    #[serde(default)]
    min_h_mm: Option<f64>,
    #[serde(default)]
    step_mm: Option<f64>,
    #[serde(default)]
    seed: Option<f64>,
    #[serde(default)]
    iters: Option<f64>,
    /// `keep` (default) or `free`.
    #[serde(default)]
    aspect: Option<String>,
    #[serde(default)]
    min_gap_mm: Option<f64>,
    #[serde(default)]
    solder_gap_mm: Option<f64>,
    /// Nets a candidate may leave unrouted and still count as feasible.
    #[serde(default)]
    allow_failed: Option<f64>,
    /// Router budget per feasibility probe, seconds.
    #[serde(default)]
    route_seconds: Option<f64>,
}

/// Build the `pcb_drc::FabProfile` the compaction DRC gate should use,
/// mirroring the conversion in `tool_drc_run` so the gate matches the
/// real `drc` verb.
fn drc_fab_profile(project: &Project) -> Option<pcb_drc::FabProfile> {
    project.fab_profile().map(|p| pcb_drc::FabProfile {
        name: p.name,
        min_trace_width_mm: p.min_trace_width_mm,
        min_clearance_mm: p.min_clearance_mm,
        min_drill_mm: p.min_drill_mm,
        min_annular_ring_mm: p.min_annular_ring_mm,
        min_via_diameter_mm: p.min_via_diameter_mm,
        min_edge_clearance_mm: p.min_edge_clearance_mm,
        max_board_size_mm: p.max_board_size_mm,
    })
}

fn tool_compact_run(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: CompactInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("compact: {e}")))?;

    // Materialise any class-declared pours before we clone the board, so
    // the compaction router sees the same plane-vs-signal picture the
    // `route` verb does (idempotent).
    let _ = materialize_class_pours(project);

    let mut opts = compact_options_from(&input);
    // Aspect flag: `free` shrinks each dimension independently.
    if let Some(a) = input.aspect.as_deref() {
        match a {
            "free" => opts.aspect_free = true,
            "keep" => opts.aspect_free = false,
            other => {
                return Err(ToolError::invalid_params(format!(
                    "compact: aspect must be `keep` or `free`, got `{other}`"
                )))
            }
        }
    }

    // Work on a clone so the project lock is released quickly. Re-sync
    // edge_mounted from the library first (same as auto-place) so a
    // screw terminal flipped to edge-mounted in the library still
    // snaps to the outline during compaction.
    let mut board = project.read().board().clone();
    {
        let lib = project.library();
        let mut live_updates: Vec<(pcb_core::Id, bool, Option<pcb_core::EdgeSide>)> = Vec::new();
        for fp in board.footprints.values_mut() {
            if fp.key.is_empty() {
                continue;
            }
            let Some(entry) = lib.find(&fp.key) else {
                continue;
            };
            if fp.edge_mounted != entry.edge_mounted || fp.edge_side != entry.edge_side {
                fp.edge_mounted = entry.edge_mounted;
                fp.edge_side = entry.edge_side;
                live_updates.push((fp.id, entry.edge_mounted, entry.edge_side));
            }
        }
        for (id, flag, side) in live_updates {
            project.set_footprint_edge_mount(id, flag, side);
        }
    }
    let schematic = {
        let snap = project.read();
        std::sync::Arc::new(snap.schematic().clone())
    };
    // Placer body-to-body margins from the library (same map auto-place
    // builds); DRC placement margins from the same source `drc` uses.
    let place_margins: pcb_placer::MarginMap = board
        .footprints_in_order()
        .filter_map(|fp| {
            if fp.key.is_empty() {
                return None;
            }
            let entry = project.library().find(&fp.key)?;
            let m = entry.placement_margin;
            if m.top_mm <= 0.0 && m.right_mm <= 0.0 && m.bottom_mm <= 0.0 && m.left_mm <= 0.0 {
                return None;
            }
            Some((fp.id, [m.top_mm, m.right_mm, m.bottom_mm, m.left_mm]))
        })
        .collect();
    let drc_margins = build_drc_margin_map(project);
    let fab_profile = drc_fab_profile(project);

    let outcome = crate::compact::compact(
        &board,
        &schematic,
        &place_margins,
        &drc_margins,
        fab_profile.as_ref(),
        &opts,
    )
    .map_err(|e| ToolError::invalid_params(format!("compact: {e}")))?;

    let m = &outcome.metrics;
    if !outcome.shrunk {
        project.log(
            ActivityLevel::Info,
            format!(
                "compact: no smaller feasible outline found ({:.1} × {:.1} mm, floor {:.1} × {:.1} mm, {} checks)",
                m.old_w_mm, m.old_h_mm, m.lower_bound_w_mm, m.lower_bound_h_mm, m.checks,
            ),
        );
        return Ok(text_result(format!(
            "compact: no shrink feasible — board left at {:.1} × {:.1} mm ({:.0} mm²); tried {} candidate size(s)",
            m.old_w_mm, m.old_h_mm, m.old_area_mm2, m.checks,
        ))
        .with_data(compact_data(m, false)));
    }

    // Apply the best feasible board back to the live project via the
    // granular APIs (outline, positions/rotations, routing, silk).
    let new_outline = outcome
        .board
        .outline
        .expect("compacted board has an outline");
    project.set_outline_with_radius(new_outline, outcome.board.outline_corner_radius);

    let live_id_of_ref: HashMap<String, pcb_core::Id> = project
        .read()
        .board()
        .footprints_in_order()
        .map(|fp| (fp.reference.clone(), fp.id))
        .collect();
    let mut applied_moves = 0usize;
    for fp in outcome.board.footprints_in_order() {
        let Some(&id) = live_id_of_ref.get(&fp.reference) else {
            continue;
        };
        if project.move_footprint(id, fp.position) {
            applied_moves += 1;
        }
        project.set_footprint_rotation(id, fp.rotation);
    }
    project.replace_routing(outcome.board.traces.clone(), outcome.board.vias.clone());
    project.set_silk_texts(outcome.board.silk_texts.clone());

    // Rule areas and keepouts travelled with the copper during compaction
    // (an area anchored to a package re-derived from its new pose, a plain
    // one took the rigid trim delta). Push that geometry back, or the live
    // board keeps a fine-pitch escape zone sitting where the QFN *used* to
    // be — silently changing what the router and DRC are allowed to do.
    let mut moved_areas = 0usize;
    let live_area_rects: HashMap<String, pcb_core::Rect> = project
        .read()
        .board()
        .rule_areas
        .iter()
        .map(|a| (a.name.clone(), a.rect))
        .collect();
    for area in &outcome.board.rule_areas {
        if live_area_rects.get(&area.name) == Some(&area.rect) {
            continue;
        }
        project.set_rule_area(area.clone());
        moved_areas += 1;
    }
    let mut moved_keepouts = 0usize;
    let live_keepouts: HashMap<pcb_core::Id, Vec<Point>> = project
        .read()
        .board()
        .keepouts
        .iter()
        .map(|k| (k.id, k.polygon.clone()))
        .collect();
    for k in &outcome.board.keepouts {
        if live_keepouts.get(&k.id) == Some(&k.polygon) {
            continue;
        }
        // No in-place keepout update API; remove + re-add keeps the id
        // (`Board::add_keepout` preserves it) so references stay valid.
        if project.remove_keepout(k.id) {
            project.add_keepout(k.clone());
            moved_keepouts += 1;
        }
    }
    let geometry_tail = match (moved_areas, moved_keepouts) {
        (0, 0) => String::new(),
        (a, 0) => format!(", {a} rule area(s) re-fitted"),
        (0, k) => format!(", {k} keepout(s) moved"),
        (a, k) => format!(", {a} rule area(s) re-fitted, {k} keepout(s) moved"),
    };

    let conn = compact_connectivity_text(m);
    project.log(
        ActivityLevel::Info,
        format!(
            "compact: {:.1} × {:.1} mm ({:.0} mm²) → {:.1} × {:.1} mm ({:.0} mm²), −{:.1}% area; {} traces, {} vias, {conn}; {} checks, moved {}{geometry_tail}",
            m.old_w_mm, m.old_h_mm, m.old_area_mm2,
            m.new_w_mm, m.new_h_mm, m.new_area_mm2,
            m.area_reduction_pct, m.trace_count, m.via_count, m.checks, applied_moves,
        ),
    );

    Ok(text_result(format!(
        "Compacted: {:.1} × {:.1} mm → {:.1} × {:.1} mm ({:.0} → {:.0} mm², −{:.1}% area); {} traces, {} vias, {conn} (tried {} candidate size(s)){geometry_tail}",
        m.old_w_mm, m.old_h_mm, m.new_w_mm, m.new_h_mm,
        m.old_area_mm2, m.new_area_mm2, m.area_reduction_pct,
        m.trace_count, m.via_count, m.checks,
    ))
    .with_data(compact_data(m, true)))
}

/// Connectivity clause of the compact report. Says "0 failed nets" on the
/// strict path; when `allow_failed` let some through it names the actual
/// count, so the text never implies a fully routed board.
fn compact_connectivity_text(m: &crate::compact::CompactMetrics) -> String {
    if m.failed_nets == 0 {
        "0 failed nets, 0 DRC errors".to_string()
    } else {
        format!(
            "{} failed net(s) left unrouted (allow_failed), 0 clearance/other DRC errors",
            m.failed_nets
        )
    }
}

/// Translate the parsed `CompactInput` into `CompactOptions`, keeping the
/// defaults defined by the compaction core.
fn compact_options_from(input: &CompactInput) -> crate::compact::CompactOptions {
    let mut opts = crate::compact::CompactOptions {
        min_w_mm: input.min_w_mm,
        min_h_mm: input.min_h_mm,
        min_gap_mm: input.min_gap_mm,
        ..crate::compact::CompactOptions::default()
    };
    if let Some(v) = input.step_mm {
        if v > 0.0 {
            opts.step_mm = v;
        }
    }
    if let Some(v) = input.seed {
        opts.seed = v.max(0.0) as u64;
    }
    if let Some(v) = input.iters {
        opts.place_iters = v.max(0.0) as usize;
    }
    if let Some(v) = input.solder_gap_mm {
        opts.solder_gap_mm = v.max(0.0);
    }
    if let Some(v) = input.allow_failed {
        opts.allow_failed = v.max(0.0) as usize;
    }
    // A zero/negative budget would make every probe time out instantly, so
    // it is ignored rather than honoured.
    if let Some(v) = input.route_seconds {
        if v > 0.0 {
            opts.route_seconds = v;
        }
    }
    opts
}

fn compact_data(m: &crate::compact::CompactMetrics, shrunk: bool) -> Value {
    json!({
        "shrunk": shrunk,
        "old_w_mm": round2(m.old_w_mm),
        "old_h_mm": round2(m.old_h_mm),
        "old_area_mm2": round2(m.old_area_mm2),
        "new_w_mm": round2(m.new_w_mm),
        "new_h_mm": round2(m.new_h_mm),
        "new_area_mm2": round2(m.new_area_mm2),
        "area_reduction_pct": round2(m.area_reduction_pct),
        "trace_count": m.trace_count,
        "via_count": m.via_count,
        "total_length_mm": round2(m.total_length_mm),
        "failed_nets": m.failed_nets,
        "drc_errors": m.drc_errors,
        "lower_bound_w_mm": round2(m.lower_bound_w_mm),
        "lower_bound_h_mm": round2(m.lower_bound_h_mm),
        "checks": m.checks,
    })
}

fn tool_route_run(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: RouteRunInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("route.run: {e}")))?;

    // Materialise any class-declared pours BEFORE the router clones
    // the board — otherwise the router lays redundant traces on what
    // should have been a pour-only net. Idempotent.
    let _ = materialize_class_pours(project);

    // Snapshot the schematic so the router can resolve per-net classes
    // itself. This replaces the previous "rebuild a net_overrides map
    // from class fields" path — the router now consults the schematic
    // directly via `RouteOptions::schematic`.
    let schematic_arc = {
        let snap = project.read();
        std::sync::Arc::new(snap.schematic().clone())
    };
    // Legacy: also build the overrides map for any caller code that
    // still expects to see it populated (keeps router-tune working in
    // mixed setups). Empty when no overrides are needed.
    let net_overrides: std::collections::HashMap<String, pcb_router::NetOverride> =
        std::collections::HashMap::new();

    let initial_net_order = input.order.as_ref().map(|s| {
        s.split(',')
            .map(|t| t.trim().to_string())
            .filter(|t| !t.is_empty())
            .collect::<Vec<String>>()
    });

    let max_seconds = match input.max_seconds {
        None => Some(90.0),
        Some(s) if s <= 0.0 => None, // 0 / negative = unlimited
        Some(s) => Some(s),
    };
    let progress_project = project.clone();
    let opts = pcb_router::RouteOptions {
        cell: Length::from_mm(input.cell_mm),
        trace_width: Length::from_mm(input.trace_width_mm),
        clearance: Length::from_mm(input.clearance_mm),
        via_cost: input.via_cost,
        via_drill: Length::from_mm(input.via_drill_mm),
        via_diameter: Length::from_mm(input.via_diameter_mm),
        net_overrides,
        schematic: Some(schematic_arc),
        organic: input.organic.unwrap_or(true),
        engine: match input.engine.as_deref() {
            None | Some("grid") => pcb_router::RouteEngine::Grid,
            Some("topo") => pcb_router::RouteEngine::Topo,
            Some(other) => {
                return Err(ToolError::invalid_params(format!(
                    "route: unknown engine `{other}` (want grid|topo)"
                )))
            }
        },
        organic_fillet_mm: input.organic_fillet_mm.unwrap_or(3.0),
        initial_net_order,
        // Greedy-search weight. 1.0 = admissible/optimal A*. Left at 1.0
        // because W>1 regresses wall-time on this multi-source Steiner
        // router (the partial-tree seeding makes an inflated heuristic
        // re-expand, and weighted detours trip the RR&R inefficiency
        // threshold). The knob exists for future open-board experiments.
        heuristic_weight: 1.0,
        max_seconds,
        fine_escape: input.fine_escape.unwrap_or(false),
        negotiate: input.negotiate.unwrap_or(false),
        on_progress: Some(std::sync::Arc::new(move |msg: &str| {
            progress_project.log(ActivityLevel::Info, msg);
        })),
    };

    // Route on a clone so the lock is released quickly; then push the
    // result back into the live Project via the regular APIs (which
    // emit RoutingChanged events for the UI).
    let mut work = project.read().board().clone();
    let report = pcb_router::route(&mut work, &opts);

    project.clear_routing();
    for trace in &work.traces {
        project.add_trace(trace.clone());
    }
    for via in &work.vias {
        project.add_via(via.clone());
    }

    let per_net: Vec<Value> = report
        .per_net
        .iter()
        .map(|(name, outcome)| match outcome {
            pcb_router::Outcome::Ok {
                trace_segments,
                vias,
                length_mm,
                lower_bound_mm,
            } => {
                let detour = if *lower_bound_mm > 0.0 {
                    length_mm / lower_bound_mm
                } else {
                    1.0
                };
                json!({
                    "net": name, "ok": true,
                    "trace_segments": trace_segments, "vias": vias,
                    "length_mm": round2(*length_mm),
                    "lower_bound_mm": round2(*lower_bound_mm),
                    "detour_ratio": round2(detour),
                })
            }
            pcb_router::Outcome::Failed { reason } => json!({
                "net": name, "ok": false, "reason": reason,
            }),
        })
        .collect();
    let failed: Vec<&str> = report
        .per_net
        .iter()
        .filter_map(|(n, o)| matches!(o, pcb_router::Outcome::Failed { .. }).then_some(n.as_str()))
        .collect();
    let total_detour = if report.total_lower_bound_mm > 0.0 {
        report.total_length_mm / report.total_lower_bound_mm
    } else {
        1.0
    };

    // Run DRC right after the route so the agent gets the verdict
    // in a single round-trip and can iterate without a second call.
    let drc_report = {
        let margins = build_drc_margin_map(project);
        let snap = project.read();
        let opts = pcb_drc::DrcOptions {
            placement_margins: margins,
            // Classes and rule areas decide what counts as a violation,
            // so the post-route verdict must see them — otherwise it
            // disagrees with the `drc` verb on the very same copper.
            schematic: Some(std::sync::Arc::new(snap.schematic().clone())),
            ..pcb_drc::DrcOptions::default()
        };
        pcb_drc::run(snap.board(), &opts)
    };
    project.log(
        ActivityLevel::Info,
        format!(
            "route.run: {} traces, {} vias on board, {:.1} mm wire (detour {:.2}×), {} pass(es), {} net(s) failed; DRC {}E {}W",
            report.board_trace_count,
            report.board_via_count,
            report.total_length_mm,
            total_detour,
            report.iterations,
            failed.len(),
            drc_report.error_count,
            drc_report.warning_count,
        ),
    );
    // Surface placement hints in the activity log too: the agent's
    // most-recent action ends with these so failures lead directly to
    // a concrete next move.
    for hint in &report.hints {
        project.log(ActivityLevel::Warn, format!("route.hint: {hint}"));
    }
    let hints_block = if report.hints.is_empty() {
        String::new()
    } else {
        let lines: Vec<String> = report.hints.iter().map(|h| format!("  - {h}")).collect();
        format!("\nhints:\n{}", lines.join("\n"))
    };
    let organic_note = report
        .organic
        .as_ref()
        .map(|o| {
            format!(
                "; organic: {} chains smoothed, {:.1} → {:.1} mm",
                o.chains, o.length_before_mm, o.length_after_mm
            )
        })
        .unwrap_or_default();
    // Connectivity as the FINAL board sees it — same computation `view`
    // and `nets` use, so the three never disagree. Agents only ever see
    // this text, so the headline numbers must be the board's own.
    let (nets_total, nets_full) = {
        let snap = project.read();
        let statuses = collect_net_status(snap.board(), snap.schematic());
        let total = statuses.len();
        let full = statuses
            .iter()
            .filter(|n| {
                n["unconnected_pads"]
                    .as_array()
                    .is_some_and(|a| a.is_empty())
            })
            .count();
        (total, full)
    };
    Ok(text_result(format!(
        "Routed: {} traces, {} vias on board ({} fanout via(s), {} dogbone, {} escape stub(s)); \
         escape: {} pad(s) got a slot, {} stranded{}; \
         search laid {} trace segment(s) + {} via(s); {:.1} mm wire, {} of {} routable net(s) failed \
         (detour {:.2}× over {:.1} mm lower bound), {} pass(es); connectivity: {}/{} net(s) fully connected; \
         {:.1}s elapsed{}{}{}; DRC: {} error(s), {} warning(s){}",
        report.board_trace_count,
        report.board_via_count,
        report.fanout_via_count,
        report.dogbone_via_count,
        report.escape_stub_count,
        report.escaped_pad_count,
        report.stranded_pads.len(),
        if report.stranded_pads.is_empty() {
            String::new()
        } else {
            format!(" ({})", report.stranded_pads.join(", "))
        },
        report.trace_count,
        report.via_count,
        report.total_length_mm,
        failed.len(),
        report.routable_net_count,
        total_detour,
        report.total_lower_bound_mm,
        report.iterations,
        nets_full,
        nets_total,
        report.elapsed_seconds,
        if report.budget_hit {
            " (BUDGET HIT — result is best-so-far, raise max_seconds for more)"
        } else {
            ""
        },
        if failed.is_empty() {
            String::new()
        } else {
            format!(" ({} failed: {})", failed.len(), failed.join(", "))
        },
        organic_note,
        drc_report.error_count,
        drc_report.warning_count,
        hints_block,
    ))
    .with_data(json!({
        "trace_count": report.trace_count,
        "via_count": report.via_count,
        "board_trace_count": report.board_trace_count,
        "board_via_count": report.board_via_count,
        "fanout_via_count": report.fanout_via_count,
        "dogbone_via_count": report.dogbone_via_count,
        "escape_stub_count": report.escape_stub_count,
        "escaped_pad_count": report.escaped_pad_count,
        "stranded_pad_count": report.stranded_pads.len(),
        "stranded_pads": report.stranded_pads,
        "routable_net_count": report.routable_net_count,
        "nets_total": nets_total,
        "nets_fully_connected": nets_full,
        "elapsed_seconds": round2(report.elapsed_seconds),
        "budget_hit": report.budget_hit,
        "total_length_mm": round2(report.total_length_mm),
        "total_lower_bound_mm": round2(report.total_lower_bound_mm),
        "total_detour_ratio": round2(total_detour),
        "iterations": report.iterations,
        "per_net": per_net,
        "hints": report.hints,
        "organic": report.organic.as_ref().map(|o| json!({
            "chains": o.chains,
            "segments_before": o.segments_before,
            "segments_after": o.segments_after,
            "length_before_mm": round2(o.length_before_mm),
            "length_after_mm": round2(o.length_after_mm),
        })),
        "drc": serde_json::to_value(&drc_report).unwrap_or(json!({})),
    })))
}

fn round2(v: f64) -> f64 {
    (v * 100.0).round() / 100.0
}

#[derive(Debug, Deserialize)]
struct DrcInput {
    #[serde(default)]
    min_clearance_mm: Option<f64>,
    #[serde(default)]
    edge_clearance_mm: Option<f64>,
    #[serde(default)]
    min_trace_width_mm: Option<f64>,
    #[serde(default)]
    min_drill_mm: Option<f64>,
    #[serde(default)]
    routing_inefficient_ratio: Option<f32>,
}

fn tool_drc_run(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: DrcInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("drc.run: {e}")))?;

    let mut opts = pcb_drc::DrcOptions {
        placement_margins: build_drc_margin_map(project),
        ..pcb_drc::DrcOptions::default()
    };
    if let Some(v) = input.min_clearance_mm {
        opts.min_clearance = Length::from_mm(v);
    }
    if let Some(v) = input.edge_clearance_mm {
        opts.edge_clearance = Length::from_mm(v);
    }
    if let Some(v) = input.min_trace_width_mm {
        opts.min_trace_width = Length::from_mm(v);
    }
    if let Some(v) = input.min_drill_mm {
        opts.min_drill = Length::from_mm(v);
    }
    if let Some(v) = input.routing_inefficient_ratio {
        opts.routing_inefficient_ratio = v;
    }

    if let Some(p) = project.fab_profile() {
        opts.fab_profile = Some(pcb_drc::FabProfile {
            name: p.name,
            min_trace_width_mm: p.min_trace_width_mm,
            min_clearance_mm: p.min_clearance_mm,
            min_drill_mm: p.min_drill_mm,
            min_annular_ring_mm: p.min_annular_ring_mm,
            min_via_diameter_mm: p.min_via_diameter_mm,
            min_edge_clearance_mm: p.min_edge_clearance_mm,
            max_board_size_mm: p.max_board_size_mm,
        });
    }
    let snap = project.read();
    // Hand the schematic to DRC so per-net class clearances are
    // enforced (no per-net override map needed).
    opts.schematic = Some(std::sync::Arc::new(snap.schematic().clone()));
    let report = pcb_drc::run(snap.board(), &opts);
    drop(snap);

    project.log(
        ActivityLevel::Info,
        format!(
            "drc.run: {} error(s), {} warning(s)",
            report.error_count, report.warning_count
        ),
    );
    // Agents only ever see this text, so list the errors (and a warning
    // histogram) instead of making them guess what the counts mean.
    let mut summary = format!(
        "DRC: {} error(s), {} warning(s)",
        report.error_count, report.warning_count
    );
    {
        // Name the rule areas in force: they are why a 0.13 mm gap can be
        // legal here and a violation two millimetres away.
        let snap = project.read();
        let board = snap.board();
        if let Some(f) = board.fab_rules.as_ref() {
            summary.push_str(&format!("\nfab-rules: {}", f.preset));
        }
        for a in &board.rule_areas {
            summary.push_str(&format!("\nrule area: {}", describe_rule_area(a)));
        }
    }
    const MAX_LISTED: usize = 25;
    let errors: Vec<&pcb_drc::Violation> = report
        .violations
        .iter()
        .filter(|v| v.severity == pcb_drc::Severity::Error)
        .collect();
    if !errors.is_empty() {
        summary.push_str("\nerrors:");
        for v in errors.iter().take(MAX_LISTED) {
            summary.push_str(&format!(
                "\n  - {:?} @ {:.2},{:.2} mm: {}",
                v.kind, v.x_mm, v.y_mm, v.message
            ));
        }
        if errors.len() > MAX_LISTED {
            summary.push_str(&format!("\n  … {} more", errors.len() - MAX_LISTED));
        }
    }
    let mut warn_kinds: std::collections::BTreeMap<String, usize> =
        std::collections::BTreeMap::new();
    for v in report
        .violations
        .iter()
        .filter(|v| v.severity == pcb_drc::Severity::Warning)
    {
        *warn_kinds.entry(format!("{:?}", v.kind)).or_default() += 1;
    }
    if !warn_kinds.is_empty() {
        let parts: Vec<String> = warn_kinds
            .iter()
            .map(|(k, n)| format!("{k} ×{n}"))
            .collect();
        summary.push_str(&format!("\nwarnings by kind: {}", parts.join(", ")));
    }
    Ok(text_result(summary).with_data(serde_json::to_value(&report).unwrap_or(json!({}))))
}

#[derive(Debug, Deserialize)]
struct SetClassInput {
    name: String,
    #[serde(default)]
    trace_width_mm: Option<f64>,
    #[serde(default)]
    clearance_mm: Option<f64>,
    /// Via copper-pad diameter (mm). `None` → use route defaults.
    #[serde(default)]
    via_diameter_mm: Option<f64>,
    /// Via drill diameter (mm). `None` → use route defaults.
    #[serde(default)]
    via_drill_mm: Option<f64>,
    /// Z0 single-ended impedance target. Schema-only for now.
    #[serde(default)]
    target_impedance_ohms: Option<f64>,
    /// Partner net for differential pair routing. Schema-only for now.
    #[serde(default)]
    diff_pair_with: Option<String>,
    /// Diff-pair edge-to-edge gap (mm). Schema-only for now.
    #[serde(default)]
    diff_gap_mm: Option<f64>,
    /// Layer(s) for the auto-pour: "top", "bottom", or "both". When
    /// set, every net assigned to this class gets a `Pour`
    /// materialised on the listed layer(s) by `auto-pour` (and
    /// implicitly by `route`).
    #[serde(default)]
    pour: Option<PourLayersInput>,
    /// Length-match target (mm). When set, the router post-pass
    /// extends nets in this class with a serpentine to reach this
    /// length.
    #[serde(default)]
    target_length_mm: Option<f64>,
    /// Tolerance (mm) for length match. Defaults to the NetClass
    /// default (0.5 mm) when absent.
    #[serde(default)]
    length_tolerance_mm: Option<f64>,
}

#[derive(Debug, Deserialize, Clone, Copy)]
#[serde(rename_all = "lowercase")]
enum PourLayersInput {
    Top,
    Bottom,
    Both,
}

impl PourLayersInput {
    fn to_layers(self) -> Vec<CopperLayer> {
        match self {
            Self::Top => vec![CopperLayer::Top],
            Self::Bottom => vec![CopperLayer::Bottom],
            Self::Both => vec![CopperLayer::Top, CopperLayer::Bottom],
        }
    }
}

#[derive(Debug, Deserialize)]
struct AssignNetClassInput {
    net: String,
    class: String,
}

fn tool_schematic_assign_net_class(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: AssignNetClassInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("net-class: {e}")))?;
    project
        .assign_net_to_class(input.net.clone(), input.class.clone())
        .map_err(ToolError::invalid_params)?;
    Ok(
        text_result(format!("net `{}` → class `{}`", input.net, input.class))
            .with_data(json!({ "net": input.net, "class": input.class })),
    )
}

fn tool_schematic_set_class(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: SetClassInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("class: {e}")))?;
    if input.name.trim().is_empty() {
        return Err(ToolError::invalid_params("class: name is empty"));
    }
    let class = pcb_core::NetClass {
        name: input.name.clone(),
        trace_width_mm: input.trace_width_mm,
        clearance_mm: input.clearance_mm,
        via_diameter_mm: input.via_diameter_mm,
        via_drill_mm: input.via_drill_mm,
        target_impedance_ohms: input.target_impedance_ohms,
        diff_pair_with: input.diff_pair_with.clone(),
        diff_gap_mm: input.diff_gap_mm,
        pour_layers: input
            .pour
            .map(PourLayersInput::to_layers)
            .unwrap_or_default(),
        target_length_mm: input.target_length_mm,
        length_tolerance_mm: input
            .length_tolerance_mm
            .unwrap_or(pcb_core::NetClass::default().length_tolerance_mm),
    };
    project.set_net_class(class);
    let mut text = format!("class {} set", input.name);
    let mut bits: Vec<String> = Vec::new();
    if let Some(w) = input.trace_width_mm {
        bits.push(format!("width={w} mm"));
    }
    if let Some(c) = input.clearance_mm {
        bits.push(format!("clearance={c} mm"));
    }
    if !bits.is_empty() {
        text.push_str(&format!(" ({})", bits.join(", ")));
    }
    Ok(text_result(text).with_data(json!({
        "name": input.name,
        "trace_width_mm": input.trace_width_mm,
        "clearance_mm": input.clearance_mm,
        "pour_layers": input.pour
            .map(|p| p.to_layers().into_iter().map(layer_to_str).collect::<Vec<_>>())
            .unwrap_or_default(),
    })))
}

/// Look up every net assigned to a `NetClass` whose `pour_layer` is
/// set, and add a `Pour { net, layer }` for each. Idempotent (the
/// project's `add_pour` replaces same-key pours rather than
/// duplicating). Returns the list of nets that newly got pours, the
/// list that already had matching pours, and the list of class refs
/// pointing at undeclared classes (skipped).
fn materialize_class_pours(project: &Project) -> ClassPourSummary {
    use std::collections::HashSet;
    let mut summary = ClassPourSummary::default();
    let snap = project.read();
    let sch = snap.schematic();
    let board = snap.board();
    let existing: HashSet<(String, CopperLayer)> = board
        .pours
        .iter()
        .map(|p| (p.net.clone(), p.layer))
        .collect();
    let mut to_add: Vec<(String, CopperLayer)> = Vec::new();
    for net_name in sch.nets.keys() {
        let Some(class) = sch.class_for_net(net_name) else {
            continue;
        };
        for layer in &class.pour_layers {
            if existing.contains(&(net_name.clone(), *layer)) {
                summary
                    .already_present
                    .push(format!("{net_name}/{}", layer_to_str(*layer)));
            } else {
                to_add.push((net_name.clone(), *layer));
            }
        }
    }
    drop(snap);
    for (net, layer) in to_add {
        project.add_pour(Pour {
            net: net.clone(),
            layer,
            thermal_relief: pcb_core::ThermalRelief::default(),
            stitching: pcb_core::StitchPolicy::None,
        });
        summary.added.push(format!("{net}/{}", layer_to_str(layer)));
    }
    summary
}

#[derive(Debug, Default)]
struct ClassPourSummary {
    added: Vec<String>,
    already_present: Vec<String>,
}

fn tool_auto_pour(project: &Project, _args: &Value) -> Result<Value, ToolError> {
    let summary = materialize_class_pours(project);
    project.log(
        ActivityLevel::Info,
        format!(
            "auto-pour: added {} pour(s), {} already present",
            summary.added.len(),
            summary.already_present.len(),
        ),
    );
    let text = format!(
        "auto-pour: added {} ({}); already present {} ({})",
        summary.added.len(),
        if summary.added.is_empty() {
            "—".to_string()
        } else {
            summary.added.join(", ")
        },
        summary.already_present.len(),
        if summary.already_present.is_empty() {
            "—".to_string()
        } else {
            summary.already_present.join(", ")
        },
    );
    Ok(text_result(text).with_data(json!({
        "added": summary.added,
        "already_present": summary.already_present,
    })))
}

#[derive(Debug, Deserialize)]
struct FabPackArgs {
    /// Provider name: "jlcpcb" / "pcbway" / "generic". Default
    /// jlcpcb because that's what most non-technical users will pick.
    #[serde(default)]
    fab: Option<String>,
    /// Directory to drop the zip in. Default `~/Downloads`.
    #[serde(default)]
    out_dir: Option<String>,
}

fn tool_fab_pack(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: FabPackArgs = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("pack: {e}")))?;

    let provider = match input.fab.as_deref() {
        None => pcb_fab::Provider::Jlcpcb,
        Some(s) => pcb_fab::Provider::from_name(s).ok_or_else(|| {
            ToolError::invalid_params(format!(
                "pack: unknown fab `{s}` — supported: jlcpcb, pcbway, generic"
            ))
        })?,
    };

    let out_dir = match input.out_dir {
        Some(p) => std::path::PathBuf::from(p),
        None => std::env::var_os("HOME").map_or_else(
            || std::path::PathBuf::from("/tmp"),
            |h| std::path::PathBuf::from(h).join("Downloads"),
        ),
    };

    let report = pcb_fab::pack(project, provider, &out_dir)
        .map_err(|e| ToolError::invalid_params(format!("pack: {e}")))?;

    let summary = if report.blocking {
        format!(
            "pack: NOT READY — wrote {} ({} files), but blocking issues: {}",
            report.zip_path.display(),
            report.files.len(),
            report.blocking_reasons.join("; "),
        )
    } else {
        format!(
            "pack: ready — wrote {} ({} files); upload to {}",
            report.zip_path.display(),
            report.files.len(),
            report.provider,
        )
    };

    project.log(
        ActivityLevel::Info,
        format!(
            "fab.pack: {} → {} ({} blocking)",
            report.provider,
            report.zip_path.display(),
            report.blocking_reasons.len(),
        ),
    );
    Ok(text_result(summary).with_data(serde_json::to_value(&report).unwrap_or(json!({}))))
}

fn tool_erc_run(project: &Project, _args: &Value) -> Result<Value, ToolError> {
    let snap = project.read();
    let report = pcb_erc::run(
        snap.board(),
        snap.schematic(),
        &pcb_erc::ErcOptions::default(),
    );
    drop(snap);

    project.log(
        ActivityLevel::Info,
        format!(
            "erc.run: {} error(s), {} warning(s)",
            report.error_count, report.warning_count
        ),
    );

    // Surface the first ~6 violation messages inline so a one-shot
    // `erc` call gives the agent something actionable without having
    // to read the structured `violations` array.
    let mut summary = format!(
        "ERC: {} error(s), {} warning(s)",
        report.error_count, report.warning_count,
    );
    for v in report.violations.iter().take(6) {
        summary.push_str(&format!("\n  [{:?}] {}", v.severity, v.message,));
    }
    if report.violations.len() > 6 {
        summary.push_str(&format!(
            "\n  ... and {} more (see structuredContent.violations)",
            report.violations.len() - 6,
        ));
    }
    Ok(text_result(summary).with_data(serde_json::to_value(&report).unwrap_or(json!({}))))
}

#[derive(Debug, Deserialize)]
struct FabPackInput {
    out_dir: String,
    #[serde(default)]
    name: Option<String>,
}

fn tool_output_fab_pack(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: FabPackInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("output.fab_pack: {e}")))?;

    let snap = project.read();
    let stem = input.name.unwrap_or_else(|| snap.name().to_string());
    let out_dir = std::path::PathBuf::from(&input.out_dir);

    let paths =
        pcb_gerber::write_fab_pack(snap.board(), &stem, &out_dir).map_err(|e| ToolError {
            code: error_code::INTERNAL_ERROR,
            message: format!("write_fab_pack: {e}"),
        })?;

    let path_strings: Vec<String> = paths.iter().map(|p| p.display().to_string()).collect();
    project.log(
        ActivityLevel::Info,
        format!(
            "output.fab_pack: wrote {} files to {}",
            path_strings.len(),
            out_dir.display()
        ),
    );

    Ok(text_result(format!(
        "Wrote {} files:\n{}",
        path_strings.len(),
        path_strings.join("\n")
    ))
    .with_data(json!({
        "out_dir": out_dir.display().to_string(),
        "files": path_strings,
    })))
}

// ─── Feature 8 — hierarchical schematic ─────────────────────────────

#[derive(Debug, Deserialize)]
struct SheetAddInput {
    reference: String,
    /// Display name for the sub-sheet — surfaces in UI labels.
    #[serde(default)]
    name: Option<String>,
}

fn tool_sheet_add(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: SheetAddInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("sheet.add: {e}")))?;
    if input.reference.trim().is_empty() {
        return Err(ToolError::invalid_params("sheet.add: reference is empty"));
    }
    let sheet = pcb_core::Sheet {
        id: pcb_core::Id::new(),
        reference: input.reference.clone(),
        schematic: pcb_core::Schematic::new(),
        port_bindings: std::collections::HashMap::new(),
    };
    let _ = input.name; // reserved for future UI labelling
    project
        .add_sub_sheet(sheet)
        .map_err(ToolError::invalid_params)?;
    project.log(
        ActivityLevel::Info,
        format!("sheet.add: {}", input.reference),
    );
    Ok(
        text_result(format!("sheet `{}` added", input.reference)).with_data(json!({
            "reference": input.reference,
        })),
    )
}

#[derive(Debug, Deserialize)]
struct SheetPortInput {
    /// Sub-sheet reference to attach the port to. Empty = top-level.
    #[serde(default)]
    sheet: String,
    /// Port name as referenced from the parent via bindings.
    name: String,
    /// Direction from the sub-sheet's perspective.
    direction: String,
    /// Internal net inside the sub-sheet that this port wires to.
    net: String,
}

fn tool_sheet_port(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: SheetPortInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("sheet.port: {e}")))?;
    let dir = match input.direction.to_ascii_lowercase().as_str() {
        "in" => pcb_core::PortDirection::In,
        "out" => pcb_core::PortDirection::Out,
        "bidir" | "io" => pcb_core::PortDirection::Bidir,
        "power" | "pwr" => pcb_core::PortDirection::Power,
        other => {
            return Err(ToolError::invalid_params(format!(
                "sheet.port: unknown direction `{other}` (in|out|bidir|power)"
            )))
        }
    };
    let port = pcb_core::Port {
        name: input.name.clone(),
        direction: dir,
        net: input.net.clone(),
    };
    project
        .set_sheet_port(&input.sheet, port)
        .map_err(ToolError::invalid_params)?;
    project.log(
        ActivityLevel::Info,
        format!(
            "sheet.port: {}.{} -> {}",
            if input.sheet.is_empty() {
                "root"
            } else {
                input.sheet.as_str()
            },
            input.name,
            input.net
        ),
    );
    Ok(text_result(format!(
        "port `{}` ({}) on sheet `{}` declared (internal net `{}`)",
        input.name,
        input.direction,
        if input.sheet.is_empty() {
            "root"
        } else {
            input.sheet.as_str()
        },
        input.net,
    ))
    .with_data(json!({
        "sheet": input.sheet,
        "name": input.name,
        "direction": input.direction,
        "net": input.net,
    })))
}

#[derive(Debug, Deserialize)]
struct SheetBindInput {
    /// Sub-sheet instance reference to bind a port on.
    reference: String,
    /// Port name (as declared inside the sub-sheet).
    port: String,
    /// Parent net to wire the port to.
    net: String,
}

fn tool_sheet_bind(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: SheetBindInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("sheet.bind: {e}")))?;
    project
        .bind_sheet_port(&input.reference, &input.port, &input.net)
        .map_err(ToolError::invalid_params)?;
    project.log(
        ActivityLevel::Info,
        format!(
            "sheet.bind: {}.{} -> {}",
            input.reference, input.port, input.net
        ),
    );
    Ok(text_result(format!(
        "sheet `{}` port `{}` bound to parent net `{}`",
        input.reference, input.port, input.net
    ))
    .with_data(json!({
        "reference": input.reference,
        "port": input.port,
        "net": input.net,
    })))
}

// ─── Feature 10 — stackup + impedance ───────────────────────────────

fn tool_stackup_show(project: &Project) -> Result<Value, ToolError> {
    let snap = project.read();
    let s = snap.board().stackup.clone();
    drop(snap);
    let layers_json: Vec<Value> = s
        .layers
        .iter()
        .map(|l| {
            json!({
                "name": l.name,
                "kind": match l.kind {
                    pcb_core::LayerKind::Signal => "signal",
                    pcb_core::LayerKind::Plane => "plane",
                    pcb_core::LayerKind::Mixed => "mixed",
                },
                "copper_thickness_mm": l.copper_thickness_mm,
            })
        })
        .collect();
    let dielectrics_json: Vec<Value> = s
        .dielectrics
        .iter()
        .map(|d| json!({ "thickness_mm": d.thickness_mm, "er": d.er }))
        .collect();
    let text = format!(
        "stackup: {n} layer(s), copper {:.3} mm, dielectric {:.3} mm (Er {:.2}), mask {:.3} mm (Er {:.2})",
        s.copper_thickness_mm(),
        s.dielectric_thickness_mm(),
        s.dielectric_er(),
        s.soldermask_thickness_mm,
        s.soldermask_er,
        n = s.layers.len(),
    );
    Ok(text_result(text).with_data(json!({
        "copper_thickness_mm": s.copper_thickness_mm(),
        "dielectric_thickness_mm": s.dielectric_thickness_mm(),
        "dielectric_er": s.dielectric_er(),
        "soldermask_thickness_mm": s.soldermask_thickness_mm,
        "soldermask_er": s.soldermask_er,
        "layer_count": s.layers.len(),
        "layers": layers_json,
        "dielectrics": dielectrics_json,
    })))
}

#[derive(Debug, Deserialize)]
struct StackupSetInput {
    #[serde(default)]
    copper_thickness_mm: Option<f64>,
    #[serde(default)]
    dielectric_thickness_mm: Option<f64>,
    #[serde(default)]
    dielectric_er: Option<f64>,
    #[serde(default)]
    soldermask_thickness_mm: Option<f64>,
    #[serde(default)]
    soldermask_er: Option<f64>,
}

fn tool_stackup_set(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: StackupSetInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("stackup.set: {e}")))?;
    project.update_stackup(|s| {
        // Multi-layer model: the legacy "single copper thickness" maps
        // to every layer; the legacy "single dielectric" maps to every
        // slab. Phase-4 boards that want per-layer control should use
        // the `layer add` verbs instead.
        if let Some(v) = input.copper_thickness_mm {
            for l in &mut s.layers {
                l.copper_thickness_mm = v;
            }
        }
        if let Some(v) = input.dielectric_thickness_mm {
            // Spread the requested total evenly across all slabs.
            let per = if s.dielectrics.is_empty() {
                v
            } else {
                v / s.dielectrics.len() as f64
            };
            for d in &mut s.dielectrics {
                d.thickness_mm = per;
            }
        }
        if let Some(v) = input.dielectric_er {
            for d in &mut s.dielectrics {
                d.er = v;
            }
        }
        if let Some(v) = input.soldermask_thickness_mm {
            s.soldermask_thickness_mm = v;
        }
        if let Some(v) = input.soldermask_er {
            s.soldermask_er = v;
        }
    });
    project.log(ActivityLevel::Info, "stackup.set");
    tool_stackup_show(project)
}

#[derive(Debug, Deserialize)]
struct ImpedanceSuggestInput {
    /// Net name — included only for log clarity; the suggestion is a
    /// pure function of the stackup and target impedance.
    #[serde(default)]
    net: Option<String>,
    target_ohms: f64,
}

// === Phase 4: multi-layer stackup verbs ===

fn tool_layer_list(project: &Project) -> Result<Value, ToolError> {
    let snap = project.read();
    let s = snap.board().stackup.clone();
    drop(snap);
    let mut lines = Vec::with_capacity(s.layers.len());
    let layers_json: Vec<Value> = s
        .layers
        .iter()
        .enumerate()
        .map(|(i, l)| {
            let kind = match l.kind {
                pcb_core::LayerKind::Signal => "signal",
                pcb_core::LayerKind::Plane => "plane",
                pcb_core::LayerKind::Mixed => "mixed",
            };
            lines.push(format!(
                "  [{i}] {name} ({kind}, {th:.3} mm)",
                name = l.name,
                kind = kind,
                th = l.copper_thickness_mm,
            ));
            json!({
                "index": i,
                "name": l.name,
                "kind": kind,
                "copper_thickness_mm": l.copper_thickness_mm,
            })
        })
        .collect();
    let text = format!("stackup: {} layer(s)\n{}", s.layers.len(), lines.join("\n"));
    Ok(text_result(text).with_data(json!({
        "layer_count": s.layers.len(),
        "layers": layers_json,
    })))
}

#[derive(Debug, Deserialize)]
struct LayerAddInput {
    name: String,
    #[serde(default = "default_signal_kind")]
    kind: String,
    #[serde(default)]
    thickness_mm: Option<f64>,
}

fn default_signal_kind() -> String {
    "signal".into()
}

fn parse_layer_kind(s: &str) -> Result<pcb_core::LayerKind, ToolError> {
    match s.to_ascii_lowercase().as_str() {
        "signal" => Ok(pcb_core::LayerKind::Signal),
        "plane" => Ok(pcb_core::LayerKind::Plane),
        "mixed" => Ok(pcb_core::LayerKind::Mixed),
        other => Err(ToolError::invalid_params(format!(
            "layer.add: kind must be signal|plane|mixed, got `{other}`"
        ))),
    }
}

fn tool_layer_add(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: LayerAddInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("layer.add: {e}")))?;
    let kind = parse_layer_kind(&input.kind)?;
    let thickness = input.thickness_mm.unwrap_or(0.035);
    let name = input.name.clone();
    project.update_stackup(|s| {
        // Default new dielectric slab: average of existing slabs, or
        // the FR-4 default if there are none.
        let new_slab = pcb_core::Dielectric {
            thickness_mm: if s.dielectrics.is_empty() {
                1.5
            } else {
                s.dielectrics.iter().map(|d| d.thickness_mm).sum::<f64>()
                    / s.dielectrics.len() as f64
            },
            er: if s.dielectrics.is_empty() {
                4.5
            } else {
                s.dielectrics.iter().map(|d| d.er).sum::<f64>() / s.dielectrics.len() as f64
            },
        };
        s.push_layer(
            pcb_core::LayerSpec {
                name: name.clone(),
                kind,
                copper_thickness_mm: thickness,
            },
            new_slab,
        );
    });
    project.log(ActivityLevel::Info, "layer.add");
    Ok(text_result(format!("added layer `{name}`")).with_data(json!({ "name": input.name })))
}

#[derive(Debug, Deserialize)]
struct LayerNameInput {
    name: String,
}

fn tool_layer_remove(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: LayerNameInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("layer.remove: {e}")))?;
    // Refuse if any copper item references the layer.
    let snap = project.read();
    let board = snap.board();
    let Some(target) = board.stackup.find_by_name(&input.name) else {
        return Err(ToolError::invalid_params(format!(
            "layer.remove: no layer named `{}`",
            input.name
        )));
    };
    // Every item that stores a layer reference has to be checked, not
    // just the obvious copper: a pour or a keepout left behind on a
    // removed layer keeps a now-dangling `Layer{index}` in the saved
    // project, which silently changes what the router sees on later
    // runs (issue O9 — the stackup round-trip must be a true no-op).
    let mut uses = 0usize;
    for fp in board.footprints_in_order() {
        if fp.layer == target {
            uses += 1;
        }
        for pad in &fp.pads {
            if pad.layer == target {
                uses += 1;
            }
        }
    }
    for t in &board.traces {
        if t.layer == target {
            uses += 1;
        }
    }
    let pour_uses = board.pours.iter().filter(|p| p.layer == target).count();
    let keepout_uses = board
        .keepouts
        .iter()
        .filter(|k| k.layers.contains(&target))
        .count();
    uses += pour_uses + keepout_uses;
    drop(snap);
    if uses > 0 {
        let mut detail = String::new();
        if pour_uses > 0 {
            detail.push_str(&format!(
                "; {pour_uses} pour(s) — drop them with `clear-pour`"
            ));
        }
        if keepout_uses > 0 {
            detail.push_str(&format!(
                "; {keepout_uses} keepout(s) — drop them with `keepout remove`"
            ));
        }
        return Err(ToolError::invalid_params(format!(
            "layer.remove: {uses} item(s) still on layer `{}`{detail}",
            input.name
        )));
    }
    let mut removed = false;
    let name_for_closure = input.name.clone();
    project.update_stackup(|s| {
        removed = s.remove_named(&name_for_closure);
    });
    if !removed {
        return Err(ToolError::invalid_params(format!(
            "layer.remove: refused to remove `{}` (would leave fewer than 2 layers)",
            input.name
        )));
    }
    project.log(ActivityLevel::Info, "layer.remove");
    Ok(text_result(format!("removed layer `{}`", input.name))
        .with_data(json!({ "name": input.name })))
}

#[derive(Debug, Deserialize)]
struct LayerRenameInput {
    old: String,
    new: String,
}

fn tool_layer_rename(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: LayerRenameInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("layer.rename: {e}")))?;
    let mut renamed = false;
    let old_for_closure = input.old.clone();
    let new_for_closure = input.new.clone();
    project.update_stackup(|s| {
        for l in &mut s.layers {
            if l.name == old_for_closure {
                l.name.clone_from(&new_for_closure);
                renamed = true;
                break;
            }
        }
    });
    if !renamed {
        return Err(ToolError::invalid_params(format!(
            "layer.rename: no layer named `{}`",
            input.old
        )));
    }
    project.log(ActivityLevel::Info, "layer.rename");
    Ok(
        text_result(format!("renamed `{}` → `{}`", input.old, input.new))
            .with_data(json!({ "old": input.old, "new": input.new })),
    )
}

fn tool_impedance_suggest(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: ImpedanceSuggestInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("impedance.suggest: {e}")))?;
    let snap = project.read();
    let stackup = snap.board().stackup.clone();
    drop(snap);
    let w = pcb_drc::suggest_trace_width_for_impedance(input.target_ohms, &stackup);
    let z = pcb_drc::compute_microstrip_z0(
        w,
        stackup.dielectric_thickness_mm(),
        stackup.dielectric_er(),
        stackup.copper_thickness_mm(),
    );
    let label = input.net.as_deref().unwrap_or("(unspecified)");
    Ok(text_result(format!(
        "net {label}: width {w:.3} mm → Z0 ≈ {z:.1} Ω (target {:.1} Ω)",
        input.target_ohms
    ))
    .with_data(json!({
        "net": input.net,
        "target_ohms": input.target_ohms,
        "trace_width_mm": w,
        "z0_ohms": z,
    })))
}

// ─── Feature 11 — fab capability profiles ───────────────────────────

#[derive(Debug, Deserialize)]
struct FabProfileInput {
    name: String,
}

// ── Design-rule areas ─────────────────────────────────────────────────
//
// A rule area is a rectangle whose clearance / width / via geometry
// overrides the board default INSIDE it. That is how a real design makes
// a 0.4 mm-pitch QFN escape legal (tight rule near the package, normal
// rule everywhere else) instead of shipping near-miss DRC errors. All of
// router, fanout, organic and DRC read them through the one resolver in
// `pcb_core::rules`.

#[derive(Debug, Deserialize)]
struct RuleAreaInput {
    name: String,
    x1_mm: f64,
    y1_mm: f64,
    x2_mm: f64,
    y2_mm: f64,
    #[serde(default)]
    layers: Option<String>,
    #[serde(default)]
    clearance_mm: Option<f64>,
    #[serde(default)]
    trace_width_mm: Option<f64>,
    #[serde(default)]
    via_drill_mm: Option<f64>,
    #[serde(default)]
    via_diameter_mm: Option<f64>,
    #[serde(default)]
    priority: Option<f64>,
}

#[derive(Debug, Deserialize)]
struct RuleAreaAroundInput {
    reference: String,
    name: String,
    #[serde(default)]
    margin_mm: Option<f64>,
    #[serde(default)]
    layers: Option<String>,
    #[serde(default)]
    clearance_mm: Option<f64>,
    #[serde(default)]
    trace_width_mm: Option<f64>,
    #[serde(default)]
    via_drill_mm: Option<f64>,
    #[serde(default)]
    via_diameter_mm: Option<f64>,
    #[serde(default)]
    priority: Option<f64>,
}

fn rule_area_layers(spec: Option<&str>, what: &str) -> Result<Vec<CopperLayer>, ToolError> {
    Ok(match spec.unwrap_or("both") {
        "top" => vec![CopperLayer::Top],
        "bottom" => vec![CopperLayer::Bottom],
        "both" | "all" | "" => vec![],
        other => {
            return Err(ToolError::invalid_params(format!(
                "{what}: layers must be top|bottom|both, got `{other}`"
            )))
        }
    })
}

/// Store `area`, print the effective values so the agent sees exactly
/// what is now in force (the HTTP surface is text-only).
fn store_rule_area(
    project: &Project,
    area: pcb_core::RuleArea,
    what: &str,
) -> Result<Value, ToolError> {
    if area.is_empty_override() {
        return Err(ToolError::invalid_params(format!(
            "{what}: an area must override something — set at least one of clearance=, width=, via_drill=, via_dia="
        )));
    }
    let text = format!("Rule area {}", describe_rule_area(&area));
    let data = rule_area_json(&area);
    project.set_rule_area(area);
    project.log(ActivityLevel::Info, text.clone());
    let warn = fab_limit_warning(project);
    Ok(text_result(format!("{text}{warn}")).with_data(data))
}

/// One parseable line per area — same shape as `list-lib`.
fn describe_rule_area(a: &pcb_core::RuleArea) -> String {
    let mut out = format!(
        "{} {:.2},{:.2} {:.2},{:.2}",
        a.name,
        a.rect.min.x.to_mm(),
        a.rect.min.y.to_mm(),
        a.rect.max.x.to_mm(),
        a.rect.max.y.to_mm(),
    );
    out.push_str(&format!(
        " layers={}",
        if a.layers.is_empty() {
            "both".to_string()
        } else {
            a.layers
                .iter()
                .map(|l| layer_to_str(*l))
                .collect::<Vec<_>>()
                .join("+")
        }
    ));
    if let Some(v) = a.clearance_mm {
        out.push_str(&format!(" clearance={v:.3}"));
    }
    if let Some(v) = a.trace_width_mm {
        out.push_str(&format!(" width={v:.3}"));
    }
    if let Some(v) = a.via_drill_mm {
        out.push_str(&format!(" via_drill={v:.3}"));
    }
    if let Some(v) = a.via_diameter_mm {
        out.push_str(&format!(" via_dia={v:.3}"));
    }
    if a.priority != 0 {
        out.push_str(&format!(" priority={}", a.priority));
    }
    out
}

fn rule_area_json(a: &pcb_core::RuleArea) -> Value {
    json!({
        "name": a.name,
        "x1_mm": a.rect.min.x.to_mm(),
        "y1_mm": a.rect.min.y.to_mm(),
        "x2_mm": a.rect.max.x.to_mm(),
        "y2_mm": a.rect.max.y.to_mm(),
        "layers": a.layers.iter().map(|l| layer_to_str(*l)).collect::<Vec<_>>(),
        "clearance_mm": a.clearance_mm,
        "trace_width_mm": a.trace_width_mm,
        "via_drill_mm": a.via_drill_mm,
        "via_diameter_mm": a.via_diameter_mm,
        "priority": a.priority,
        // Present only for `rule-area-around`: the rect tracks this part.
        "anchor_ref": a.anchor_ref,
        "anchor_margin_mm": a.anchor_margin_mm,
    })
}

/// Text tail listing any rule-area value the adopted fab cannot build.
/// Same rule as the DRC's `RuleBelowFabLimit`, surfaced at declaration
/// time so the agent doesn't have to run DRC to find out.
fn fab_limit_warning(project: &Project) -> String {
    let snap = project.read();
    let board = snap.board();
    let Some(fab) = board.fab_rules.as_ref() else {
        return String::new();
    };
    let mut hits: Vec<String> = Vec::new();
    for a in &board.rule_areas {
        let mut check = |what: &str, v: Option<f64>, min: f64| {
            if let Some(v) = v {
                if v + 1e-9 < min {
                    hits.push(format!("{} {what} {v:.3} < {min:.3}", a.name));
                }
            }
        };
        check("clearance", a.clearance_mm, fab.min_clearance_mm);
        check("width", a.trace_width_mm, fab.min_trace_width_mm);
        check("via_drill", a.via_drill_mm, fab.min_via_drill_mm);
        check("via_dia", a.via_diameter_mm, fab.min_via_diameter_mm);
    }
    if hits.is_empty() {
        String::new()
    } else {
        format!(
            "\nWARNING below {} limits (DRC RuleBelowFabLimit): {}",
            fab.preset,
            hits.join("; ")
        )
    }
}

fn tool_rule_area_set(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: RuleAreaInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("rule-area: {e}")))?;
    let rect = pcb_core::Rect::from_corners(
        Point::new(Length::from_mm(input.x1_mm), Length::from_mm(input.y1_mm)),
        Point::new(Length::from_mm(input.x2_mm), Length::from_mm(input.y2_mm)),
    );
    let mut area = pcb_core::RuleArea::new(input.name, rect);
    area.layers = rule_area_layers(input.layers.as_deref(), "rule-area")?;
    area.clearance_mm = input.clearance_mm;
    area.trace_width_mm = input.trace_width_mm;
    area.via_drill_mm = input.via_drill_mm;
    area.via_diameter_mm = input.via_diameter_mm;
    area.priority = input.priority.unwrap_or(0.0) as i32;
    store_rule_area(project, area, "rule-area")
}

fn tool_rule_area_around(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: RuleAreaAroundInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("rule-area-around: {e}")))?;
    let margin_mm = input.margin_mm.unwrap_or(1.0);
    // Anchored to the footprint: the rect is a DERIVED value, so anything
    // that later moves the part (compaction re-places every footprint and
    // then slides all copper against the edges) re-derives it instead of
    // leaving the zone behind. `rule-area` with literal coordinates stays
    // un-anchored — the user asked for that spot on the board.
    let mut area = {
        let snap = project.read();
        let fp = snap
            .board()
            .footprints_in_order()
            .find(|f| f.reference == input.reference)
            .ok_or_else(|| {
                ToolError::invalid_params(format!(
                    "rule-area-around: no placed footprint `{}`",
                    input.reference
                ))
            })?;
        pcb_core::RuleArea::around_footprint(input.name, fp, margin_mm).ok_or_else(|| {
            ToolError::invalid_params(format!(
                "rule-area-around: `{}` has no pads to bound",
                input.reference
            ))
        })?
    };
    area.layers = rule_area_layers(input.layers.as_deref(), "rule-area-around")?;
    area.clearance_mm = input.clearance_mm;
    area.trace_width_mm = input.trace_width_mm;
    area.via_drill_mm = input.via_drill_mm;
    area.via_diameter_mm = input.via_diameter_mm;
    area.priority = input.priority.unwrap_or(0.0) as i32;
    store_rule_area(project, area, "rule-area-around")
}

#[derive(Debug, Deserialize)]
struct RuleAreaRemoveInput {
    name: String,
}

fn tool_rule_area_remove(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: RuleAreaRemoveInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("rule-area-remove: {e}")))?;
    if project.remove_rule_area(&input.name) {
        Ok(text_result(format!("Rule area `{}` removed", input.name)).into())
    } else {
        Err(ToolError::invalid_params(format!(
            "rule-area-remove: no area named `{}`",
            input.name
        )))
    }
}

fn tool_rule_area_list(project: &Project) -> Result<Value, ToolError> {
    let snap = project.read();
    let board = snap.board();
    let mut text = if board.rule_areas.is_empty() {
        "0 rule area(s)".to_string()
    } else {
        format!("{} rule area(s):", board.rule_areas.len())
    };
    for a in &board.rule_areas {
        text.push_str(&format!("\n  {}", describe_rule_area(a)));
    }
    if let Some(f) = board.fab_rules.as_ref() {
        text.push_str(&format!(
            "\nfab-rules {}: trace/space {:.3}, via drill {:.3}, via dia {:.3} mm",
            f.preset, f.min_trace_width_mm, f.min_via_drill_mm, f.min_via_diameter_mm,
        ));
    } else {
        text.push_str("\nfab-rules: none adopted");
    }
    let items: Vec<Value> = board.rule_areas.iter().map(rule_area_json).collect();
    drop(snap);
    Ok(text_result(text).with_data(json!({ "rule_areas": items })))
}

#[derive(Debug, Deserialize)]
struct FabRulesInput {
    preset: String,
}

fn tool_fab_rules_set(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: FabRulesInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("fab-rules: {e}")))?;
    let key = input.preset.to_ascii_lowercase();
    if key == "clear" || key == "none" {
        project.set_fab_rules(None);
        return Ok(text_result("fab rules cleared").into());
    }
    if key == "list" {
        return Ok(text_result(format!(
            "fab-rules presets: {}",
            pcb_core::FabRules::preset_names().join(", ")
        ))
        .into());
    }
    let rules = pcb_core::FabRules::preset(&key).ok_or_else(|| {
        ToolError::invalid_params(format!(
            "fab-rules: unknown preset `{}` (have {})",
            input.preset,
            pcb_core::FabRules::preset_names().join(", ")
        ))
    })?;
    let text = format!(
        "fab rules `{}`: trace {:.3} mm, space {:.3} mm, via drill {:.3} mm, via dia {:.3} mm, annular {:.3} mm, edge {:.3} mm, max {:.0}×{:.0} mm\nDRC now gates every minimum against this preset; the fanout uses the smallest via it allows.",
        rules.preset,
        rules.min_trace_width_mm,
        rules.min_clearance_mm,
        rules.min_via_drill_mm,
        rules.min_via_diameter_mm,
        rules.min_annular_ring_mm,
        rules.min_edge_clearance_mm,
        rules.max_board_size_mm.0,
        rules.max_board_size_mm.1,
    );
    let data = json!({
        "preset": rules.preset,
        "min_trace_width_mm": rules.min_trace_width_mm,
        "min_clearance_mm": rules.min_clearance_mm,
        "min_via_drill_mm": rules.min_via_drill_mm,
        "min_via_diameter_mm": rules.min_via_diameter_mm,
    });
    project.set_fab_rules(Some(rules));
    project.log(ActivityLevel::Info, text.clone());
    let warn = fab_limit_warning(project);
    Ok(text_result(format!("{text}{warn}")).with_data(data))
}

fn tool_fab_profile(project: &Project, args: &Value) -> Result<Value, ToolError> {
    let input: FabProfileInput = serde_json::from_value(args.clone())
        .map_err(|e| ToolError::invalid_params(format!("fab.profile: {e}")))?;
    let profile = pcb_fab::profiles::by_name(&input.name).ok_or_else(|| {
        ToolError::invalid_params(format!(
            "fab.profile: unknown `{}` — supported: jlcpcb, pcbway, oshpark",
            input.name
        ))
    })?;
    let handle = pcb_core::FabProfileHandle {
        name: profile.name.clone(),
        min_trace_width_mm: profile.min_trace_width_mm,
        min_clearance_mm: profile.min_clearance_mm,
        min_drill_mm: profile.min_drill_mm,
        min_annular_ring_mm: profile.min_annular_ring_mm,
        min_via_diameter_mm: profile.min_via_diameter_mm,
        min_edge_clearance_mm: profile.min_edge_clearance_mm,
        max_board_size_mm: profile.max_board_size_mm,
    };
    project.set_fab_profile(Some(handle.clone()));
    project.log(
        ActivityLevel::Info,
        format!("fab.profile: adopted {}", handle.name),
    );
    Ok(text_result(format!(
        "adopted fab profile `{}` (min trace {:.3} mm, min drill {:.3} mm)",
        handle.name, handle.min_trace_width_mm, handle.min_drill_mm
    ))
    .with_data(json!({
        "name": handle.name,
        "min_trace_width_mm": handle.min_trace_width_mm,
        "min_clearance_mm": handle.min_clearance_mm,
        "min_drill_mm": handle.min_drill_mm,
        "min_annular_ring_mm": handle.min_annular_ring_mm,
        "min_via_diameter_mm": handle.min_via_diameter_mm,
        "min_edge_clearance_mm": handle.min_edge_clearance_mm,
        "max_board_size_mm": [handle.max_board_size_mm.0, handle.max_board_size_mm.1],
    })))
}

fn tool_fab_profile_clear(project: &Project) -> Result<Value, ToolError> {
    project.set_fab_profile(None);
    project.log(ActivityLevel::Info, "fab.profile_clear");
    Ok(text_result("fab profile cleared".to_string()).into())
}

/// Builds the tool result envelope returned to the script API caller.
/// The text content is what the agent sees verbatim; `with_data`
/// attaches structured metadata that the UI bridge or follow-up tool
/// calls can read.
struct ToolResult {
    text: String,
    data: Option<Value>,
}

fn text_result(text: impl Into<String>) -> ToolResult {
    ToolResult {
        text: text.into(),
        data: None,
    }
}

impl ToolResult {
    fn with_data(mut self, data: Value) -> Value {
        self.data = Some(data);
        self.into_value()
    }

    fn into_value(self) -> Value {
        let mut obj = json!({
            "content": [{ "type": "text", "text": self.text }],
        });
        if let Some(data) = self.data {
            obj.as_object_mut()
                .expect("ToolResult shape")
                .insert("structuredContent".into(), data);
        }
        obj
    }
}

impl From<ToolResult> for Value {
    fn from(value: ToolResult) -> Self {
        value.into_value()
    }
}
