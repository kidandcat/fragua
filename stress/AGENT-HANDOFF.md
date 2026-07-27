# Fragua agent handoff — RP2040 open-hardware stress (2026-07-27)

**Audience:** a capable coding agent continuing Fragua work without prior session context.  
**Language of this doc:** English (repo convention). Jairo speaks Spanish; code/docs stay English.  
**Repo:** `~/pcb` → `github.com/mentasystems/fragua` (personal; push to `master` is OK).  
**Not guarded:** direct edits on `master` are fine (only `~/mono` is worktree-guarded).

---

## 0. Executive summary

Jairo asked to **stress Fragua** on a **complex open-hardware-class board** (Pico / RP2040 style), surface real failures, and **fix Fragua** as we go.

We recreated a **minimal RP2040 board** (bare QFN-56 + QSPI flash + crystal + USB-C + LDO + headers) end-to-end through the agent script API.

| Layer | Verdict |
|-------|---------|
| Script API / agent DX | Much improved this session (see §5) |
| Schematic + ERC | Solid (0 ERC errors on the stress board) |
| Library + placement | Usable; edge-mount was hard, now has `edge-place` |
| Router on **module-style** boards (ESP32-S3-Zero, etc.) | Previously proven (fecha gateway) |
| Router on **bare 0.4 mm QFN-56, 2-layer** | **Still the wall** — partial copper only |
| Wall-clock / agent hang risk | **Fixed** with `max_seconds` + A* caps (was 6–10+ min silent hangs) |

**Latest board metrics** (`stress/rp2040-minimal.fragua`, **v6 pass — negotiated congestion measured, default driver unchanged**; the saved file now carries `fab-rules jlcpcb-2l` + the `fine` rule area around U1 as design intent):

- Outline **80 × 45 mm**, 36 footprints, 39 nets, 36 symbols
- Saved state: **951 traces**, **46 vias** (25 fanout, 25 dogbones with copper stubs), **20/39 nets fully connected** @ `max_seconds=180`
- DRC **4 errors / 74 warnings** — **zero clearance errors** (was 7, all 0.125–0.175 mm at the QFN). The 4 are `NetSplit`: a fanned-out pad whose net the router still fails to finish is an isolated copper island. Same O1 wall, different symptom.
- One `RuleBelowFabLimit` warning: the area's 0.12 mm clearance is 5 µm under JLCPCB's 0.127 standard tier. Deliberate, and re-reported by every `drc` / `view`.
- **Back-to-back baseline on the same binary with the area removed: 1544 traces / 57 vias, 21/39, 7E / 70W** — i.e. the rule area traded one net of connectivity for legality. See §5 O10.

**v6 measurement (2026-07-27, same binary, back to back, idle machine, 2-layer, `max_seconds=180`):**

| Driver | Copper | Fully connected | Passes / iterations | Elapsed | DRC |
|--------|--------|-----------------|---------------------|---------|-----|
| default (rip-up-and-reroute) | 951 traces / 46 vias | **20/39** | 4 passes | 180.2 s | 4E / 74W, 0 clearance errors |
| `route negotiate=true` (PathFinder) | 777 traces / 41 vias | **17/39** | 5 iterations | 180.1 s | 4E / 78W, 0 clearance errors |

The default path is byte-identical to the saved board (same 951/46, same
21 failed nets, same 4 passes), so the v6 refactor is behaviour-neutral and
the saved project was left untouched. Note the two metrics in circulation:
the script's headline `N/39 fully connected` counts board connectivity
(20), while `stress_board_probe`'s `39 - failed` counts routable nets the
search finished (18) — same board, two frames.

**The v6 finding (this is the part that matters):** with sharing allowed —
every net free to route straight through every other net's copper — **12–13
of the 39 nets still cannot be routed at all**, and every one of them fails
at a U1 pad. The plateau is therefore NOT inter-net congestion. See O1
below.

**Previous metrics** (v4 pass — dogbone stubs + stagger + no fine-pitch moat):

- Outline **80 × 45 mm**, 36 footprints, 39 nets, 36 symbols  
- **~1544 traces**, **~57 vias** (25 fanout, 24 dogbones with copper stubs)  
- **14/39 nets fully connected @ `max_seconds=90`; 21/39 @ 180 s**, reproducible run-to-run on an idle machine (see O9: the one-off 13/39 was budget starvation under load, not board state)  
- **Plateau:** `max_seconds=600` → same 21/39, same 18 failed nets → algorithmic wall, not compute  
- **Multi-layer now helps (barely):** `layer add In1/In2` + route @180 s → **22/39** (3L and 4L alike) vs 21/39 on 2L, inside budget — was 1/39 with a 34 s overrun (O8, fixed 2026-07-27)  
- DRC **7 errors** (all marginal 0.125–0.175 mm clearance at the QFN), ~70 warnings  

**Commits (first batch already on `origin/master`, v4 batch local at time of writing):**

1. `adaa48e` — agent library confirm, edge-place, clearer text replies  
2. `15726e5` — 2-layer dogbone fanout, route time budget, passive pull  
3. `534a9b4` — stress README refresh  
4. `8448a2d` — dogbone copper stubs, rank-staggered fanout, board-truthful report  
5. `489c1af` — route/drc text reports the final board (incl. DRC errors)  
6. `71cf4ef` — no body keep-out under fine-pitch SMD packages  
7. `6af669f` — honour the escape budget inside a single footprint  
8. `cc207dc` — choose the dogbone depth and its stub together  
9. `474c7a8` — no panic when the budget dies before the first pass  
10. `c0552ea` — fine-pitch QFN fanout, budget and board-truth regression tests

**v5 commits (clearance rule areas, 2026-07-27):**

11. `757d9bd` — `RuleArea` + the single clearance/width resolver (pcb-core)
12. `e537360` — DRC resolves clearance through areas; `RuleBelowFabLimit`
13. `f00bb55` — pairwise, area-aware clearance in the grid search and fanout
14. `130a1a9` — `rule-area*` / `fab-rules` script verbs, area-aware reports
15. `d5babaf` — via-in-pad must fit its pad; areas own the fab floor inside them  

**v6 commits (negotiated congestion, 2026-07-27):**

16. `828140a` — PathFinder-style negotiated congestion (`route negotiate=true`)
17. `030da22` — regression tests: order-dependence fixed, routing reproducible

**Related pre-existing product TODO** (not finished here): PCB compaction for fecha-gateway-v3 — see root `TODO.md`.

---

## 1. What Fragua is (product + architecture)

**Product thesis** (`VISION.md`): *pencil.dev for hardware* — agent drives schematic → place → route → fab zip; human watches and steers in a Tauri UI.

**Non-negotiables:**

- No external CAD binaries (no KiCad CLI, no FreeRouting JAR).  
- Agent-first surface: **local HTTP script API** on `127.0.0.1:7878`.  
- In-process Rust pipeline: core, placer, router, DRC/ERC, gerber, fab pack.

**Launch (critical gotcha):**

```text
fragua              → prints help + script reference, then EXITS
fragua run          → empty in-memory project + API
fragua run PATH.fragua → load file, autosave to it
```

Binary: `~/pcb/target/release/fragua` (build with `cargo build --release --bin fragua`).  
Override bind: `FRAGUA_API_ADDR`.  
Memory: bare `fragua` without path is memory-only until `POST /save` or script `save PATH`.

**HTTP surface (text/plain replies):**

| Method | Path | Body / notes |
|--------|------|----------------|
| GET | `/` or `/help` | Full script reference |
| GET | `/health` | `ok` |
| POST | `/script` | `{"script": "multi\\nline"}` → per-line `[L n ok\|FAIL tool] …` |
| POST | `/save` | `{"path": "…"}` |
| GET | `/screenshot` | PNG of board/schematic |

**Important:** structured tool data (`with_data`) is **not** in the HTTP body. Agents only see **text**. Anything critical must be in the text reply (we fixed several places that only put data in structured content).

**Workspace layout:**

```text
pcb/
  crates/pcb-core/      Project, board, schematic, library, geometry (nm fixed-point)
  crates/pcb-script/    DSL parse + tool dispatch + help text
  crates/pcb-router/    A*/Theta*, RR&R, fanout/escape, organic, topo engine
  crates/pcb-placer/    ePlace global + SA legalisation
  crates/pcb-drc/ pcb-erc/ pcb-fab/ pcb-gerber/ pcb-render/ …
  src-tauri/            Tauri binary + HTTP server
  frontend/             Vite UI
  stress/               THIS stress campaign + handoff
```

**Library on disk:** `~/.pcb-library/index.json` + `attachments/`. Shared across Fragua sessions. New `lib …` entries go to an **in-memory pending queue** until confirmed.

**Units:** internal geometry is nanometres (`Length(i64)`); script uses millimetres.

---

## 2. How an agent should drive Fragua

### 2.1 Lifecycle

```bash
# Build once after code changes
cd ~/pcb && cargo build --release --bin fragua

# Kill stale server if port busy
lsof -nP -iTCP:7878 -sTCP:LISTEN -t | xargs -r kill

# Start with project file (autosave)
~/pcb/target/release/fragua run ~/pcb/stress/rp2040-minimal.fragua &

# Wait for health
curl -s -m 2 http://127.0.0.1:7878/health
```

### 2.2 Script recipe (high level)

```text
# 1) Libraries
lib KEY …
  pad …
confirm-lib KEY
body-rect KEY …
edge-mount KEY left|right|top|bottom|true|false

# 2) Board + classes
outline W H radius=R
class ground pour=both
class power width=0.35 clearance=0.2

# 3) Schematic
sym U1 ic key=…
  pin N SIDE NAME role=power_in|power_out|input|output|bidir|passive
sym C1 capacitor key=c_0603 value=100nF
net NAME REF.PIN … class=ground|power

erc
save /abs/path.fragua

# 4) Placement
palette U1 key
edge-place J1 bottom          # prefer over hand-tuned place for edge parts
place U1 40 24 0
auto-place C1 C2 … seed=7     # passives only if ICs pinned

# 5) Route (ALWAYS set a budget for complex boards)
clear-route
route max_seconds=90
# optional: cell=0.20 clearance=0.20 engine=grid|topo organic=true|false

drc
screenshot /tmp/board.png width=2000
view
nets
pack fab=jlcpcb out=/tmp   # only when clean enough
```

### 2.3 Symbol kinds (hard limit)

Only: `ic` / `resistor` / `capacitor` / `inductor` / `led` / `diode` (and short forms).  
**No** `crystal`, `switch`, `connector` — model those as `ic` with explicit pins.

LED pins are **`A` / `K`** (not 1/2). Discretes are 1/2.

### 2.4 New verbs added this session

| Verb | Tool | Purpose |
|------|------|---------|
| `list-pending` | `library.list_pending` | Pending lib entries |
| `confirm-lib KEY` | `library.confirm` | Promote pending → `~/.pcb-library` |
| `discard-pending KEY` | `library.discard_pending` | Drop pending |
| `edge-place REF side [along=N]` | `placement.edge_place` | Snap edge-mounted part to outline |
| `route … max_seconds=N` | `route.run` | Soft wall-clock budget (0 = unlimited) |

`list-lib` now prints **one key per line** (was only a count).  
`place` / batch failures now print **`FAIL ref (stage): reason`** in text.

---

## 3. Stress target: RP2040 minimal open hardware

### 3.1 Why this board

- Real open-hardware class (RPi minimal design guide + Mitayi-Pico-D1).  
- **Harder than fecha-gateway modules:** bare **QFN-56 @ 0.4 mm pitch** forces fine-pitch fanout.  
- Enough nets (~39) to stress RR&R without being a full Pico clone with every GPIO.

### 3.2 BOM / refs in the stress design

| Ref | Role | Library key |
|-----|------|-------------|
| U1 | RP2040 | `rp2040_qfn56` (57 pads: 56 + thermal EP) |
| U2 | W25Q16 flash SOIC-8 | `w25q16_soic8` |
| U3 | 3.3 V LDO SOT-23-5 | `ldo_sot23_5` |
| Y1 | 12 MHz crystal 3225 | `xtal_3225` |
| J1 | USB-C simplified | `usbc_16p` (edge, bottom) |
| J2 | SWD 1×03 | `header_1x03_2.54mm` (edge, top) |
| J3/J4 | GPIO 1×10 | `header_1x10_2.54mm` (edge, left/right) |
| SW1/SW2 | Tact switches | `sw_smd_4x4` |
| D1 | LED | `led_0603` |
| C1–C17 | Decoupling / bulk / xtal load | `c_0603` |
| R1–R8 | USB series, CC, pullups, LED | `r_0603` (created this session) |

**Net classes:** `ground` with `pour=both`; `power` wider; `usb` for D+/D− stubs.

**Power architecture (simplified RPi-style):**

- `VBUS` → LDO IN/EN  
- `+3V3` → IOVDD×N, ADC_AVDD, USB_VDD, VREG_VIN, flash VCC, pullups  
- `+1V1` → VREG_VOUT → DVDD×2 (internal regulator out)  
- QSPI: SS, SCLK, SD0–SD3 between U1 and U2  
- BOOTSEL = QSPI_SS + R5 pullup + SW1 to GND  
- RUN = R6 pullup + SW2 to GND  
- TESTEN tied to GND  

### 3.3 Footprint notes (RP2040)

Authored programmatically (see `stress/01_schematic.fragua.txt`):

- Body 7×7 mm, pitch 0.40 mm, pad ~0.20×0.50 after shrink  
- Thermal pad `GND_EP` shrunk to **1.6×1.6** (full EP blocked under-package routing)  
- Pin naming follows common RPi top-view side order (L/T/R/B); **not golden-verified against datasheet PDF pin 1 mark** — if fab is ever the goal, re-check chirality against the official pinout (past fecha bugs: mirrored U2).

Library lives in **`~/.pcb-library/index.json`** (not only in the `.fragua` file). Confirming a lib mutates the global library for all projects.

### 3.4 Artifacts

| Path | Role |
|------|------|
| `stress/rp2040-minimal.fragua` | Current project (gitignored pattern `*.fragua` but force-added) |
| `stress/01_schematic.fragua.txt` | Full rebuild script (libs + sch + save) |
| `stress/rp2040-minimal-v3.png` | Latest render |
| `stress/07_route_v3.txt` | Latest route log |
| `stress/01`–`05_*.txt` | Intermediate API logs (untracked noise OK) |
| `stress/README.md` | Short summary |

---

## 4. Timeline of what we tried (and what broke)

### Phase A — Launch & discovery

1. First launch used bare `fragua` → process exited after printing help; port 7878 never listened.  
2. Correct: `fragua run [file]`.  
3. `list-lib` returned only `"24 entries in library"` — useless for agents (structured data discarded by HTTP).  
4. Existing library was fecha-oriented (esp32_s3_zero, LoRa, OLED…), **no RP2040**.

### Phase B — Schematic

1. Built full lib set + schematic via multi-line script.  
2. **ERC 0 errors**, ~14 warnings (unused J4 pins, PowerIn-without-PowerOut on GND until pour, etc.).  
3. Symbol kind restrictions forced crystal/switch/connector → `ic`.  
4. QSPI pin mapping (flash): SD0=DI=U2.5, SD1=DO=U2.2, SD2=WP=U2.3, SD3=HOLD=U2.7.

### Phase C — Placement pain

1. Hand `place X Y` failed constantly with **silent** `0 placed, 1 failed` (no reason in text).  
2. Causes: body/pad overlap (1.0 mm solder floor), edge-mount bbox not touching outline, wrong rotation for `edge_side`, body past outline.  
3. Headers especially hard: must compute exact origin so pad bbox touches edge within 0.5 mm tolerance; wrong side → long error about local vs world edge.  
4. Auto-place improved HPWL but **scattered** decoupling far from U1.

### Phase D — First routes (pre-fix)

1. Sparse layout + no 2-layer fanout → **~3 traces**, many fails.  
2. Tighter re-layout + headers: still mostly ratsnest.  
3. Default route eventually produced **~62 traces / 3 vias / ~6 full nets** after **~6 minutes**.  
4. Fine `cell=0.15` route: **10+ minutes at 100% CPU**, client timeouts, server kept burning (agent-hostile).

### Phase E — Fixes shipped

See §5.

### Phase F — Re-measure after fixes

1. `route max_seconds=90` returns in **~90–95 s** (budget works).  
2. Dogbone fanout → **~41 vias**, **~402 traces**, still **~5 full nets**.  
3. Intermediate regressions during development (over-aggressive A* pop cap → 0 traces; fine-escape on 2-layer → wall-off) — fixed before commit.

---

## 5. Problems found → status

### 5.1 FIXED this session

| ID | Problem | Fix | Where |
|----|---------|-----|--------|
| P1 | `lib` pending forever for agents (UI-only confirm) | `confirm-lib`, `list-pending`, `discard-pending` | `pcb-script`, `pcb-core` Project APIs already existed |
| P2 | `list-lib` count-only in text | Print key/pads/edge/body per line | `tools.rs` `tool_library_list` |
| P3 | Placement failures silent in HTTP | Print `FAIL ref (stage): reason` | `tool_placement_batch` |
| P4 | Edge-mount placement math hostile | `edge-place REF side [along=N]` computes rot + snap | `Project::place_edge_from_palette`, script verb |
| P5 | Fanout no-op on 2-layer boards | Fanout runs for `layer_count >= 2` | `fanout.rs` |
| P6 | QFN pads too small for 0.30 mm VIP | **Dogbone** via outside pad toward package interior | `fanout.rs` `dogbone_via_position` |
| P7 | Fine-escape on 2-layer expensive + walls top | Fine-escape only if **≥3 layers** and fine cell; 2L uses VIP/dogbone | `escape.rs` |
| P8 | Route hangs 6–10+ min, no progress | `max_seconds` (default 90), progress callback → activity log, skip organic if budget hit | `RouteOptions`, `route()`, `tool_route_run` |
| P9 | A* 50M pop guard → multi-minute fails | Scaled pop cap ~`grid_cells*8` clamped 250k–4M | `astar.rs` |
| P10 | Decoupling scattered by SA | Post-SA pull of 2–4 pad passives toward fixed net anchors | `pcb-placer` `pull_passives_to_anchors` |
| P11 | **Per-net clearance was a lie** — each search checked only its OWN net's clearance, so a fine net could hug a stricter neighbour, and `RouteOptions.clearance` documented a max-inflation the code no longer did | Pairwise rule (strictest of the two nets) resolved per foreign net id AND per cell, via `pcb_core::RuleResolver` + `grid::ClearanceModel` | `pcb-core/src/rules.rs`, `grid.rs`, `router.rs`, `astar.rs` |
| P12 | No way to make a fine-pitch escape LEGAL — the 7 DRC errors were all near-misses of a flat 0.2 mm rule | `RuleArea` (rect, layer filter, clearance/width/via overrides, priority) read by router, fanout, organic, topo and DRC through the one resolver; `rule-area*` / `fab-rules` verbs | `pcb-core`, `pcb-drc`, `pcb-router`, `pcb-script` |
| P13 | A via-in-pad was accepted on a pad narrower than the barrel as soon as a tight rule let it clear the neighbours (32 dogbones → 1, connectivity 16/39) | A VIP must fit inside the pad copper; otherwise the dogbone path takes over as it always assumed | `fanout.rs` `plan_footprint` |
| P14 | Fanout barrels sized from the fab's headline via diameter alone — 0.45 mm on a 0.20 mm drill is a 0.125 mm ring, under JLCPCB's own 0.13 mm | Barrel also satisfies `drill + 2 × annular` | `fanout.rs` `PadRules::at_pad` |

### 5.2 OPEN — product / router correctness

| ID | Problem | Evidence | Suggested direction |
|----|---------|----------|---------------------|
| O10 | **Rule areas make fine pitch legal, not routable** | With `fab-rules jlcpcb-2l` + `rule-area-around U1 fine margin=1.5 clearance=0.12 via_drill=0.20 via_dia=0.45`: clearance errors **7 → 0**, connectivity **21 → 20/39**, and 4 new `NetSplit` errors (a fanned pad whose net still fails is an isolated island). At the default 0.20 mm cell a 0.12 mm rule quantises to the SAME 3-cell search radius as 0.20 mm for a 0.25 mm signal (`ceil((clr + w/2)/cell) + 1` guard cell), so the search sees no extra room; only 0.5 mm power nets drop a cell. `cell=0.15` does exploit it geometrically but starves the budget (2 passes instead of 4) → 19/39 | The rule was never the binding constraint. Nor is escape-corridor *congestion*: v6's negotiation (O1) showed a dozen nets cannot reach their pads even with unlimited sharing. What is left is escape-slot geometry. A cheaper follow-up: drop fanout copper for pads the router never lands on, which would delete the `NetSplit` class outright (deliberately NOT done here — it would change the no-rule-area baseline) |
| O1 | **Cannot fully route bare QFN-56 on 2L** — **20/39**, plateaued. v5 ruled out the clearance rule (O10); **v6 ruled out inter-net congestion** | With PathFinder negotiation (`route negotiate=true`) every net may route straight THROUGH every other net's copper, and **12–13 nets still fail**, all at U1 pads (`GPIO0→U1.2`, `GPIO3→U1.5`, `SWCLK→U1.23`, `SWDIO→U1.24`, `XOUT→U1.20`, `QSPI_SS→U1.54`, `QSPI_SD2→U1.52`, `QSPI_SD3→U1.49`, `+3V3→U1.46`, `HDRB1→U1.26`, plus `USB_DM/USB_DP/VBUS/CC2` at their series resistors). The over-subscribed cells are all on the BOTTOM layer in a ~4 × 4 mm patch centred on U1 (40–43, 21–27 mm) | **It is fixed geometry, not contention.** A 0.25 mm trace at the area's 0.12 mm rule needs 0.245 mm from centreline to any foreign copper edge; two adjacent 0.4 mm-pitch pads leave a 0.2 mm channel — *no trace can pass between two QFN pins*, so every escape must be a via, and the 25 dogbone landings (stamped on every layer, un-rippable) then wall each other off. The next lever is **escape-slot assignment**: choose which pad gets which dogbone, at which depth, so a legal lane survives to each one (a flow / matching problem over the escape ring). Negotiation cannot help until the lanes exist |
| O2 | ~~Report vs board copper counts diverge~~ **FIXED** (`489c1af`, `8448a2d`) | route/drc text now reports final board copper, per-net failures, hints, budget flag | — |
| O3 | ~~Body stamp moat fights fine-pitch escape~~ **FIXED** (`71cf4ef`) | No body stamp for SMD packages with pad pitch < 0.5 mm; modules/TH keep it | — |
| O8 | ~~**≥3-layer path regresses instead of helping**~~ **FIXED** (2026-07-27, commits `8d9fc9e` / `a14c26a` / `62726aa`) | Was: `layer add In1.Cu/In2.Cu signal` + `route max_seconds=180` → **1/39**, search laid 0 segments, elapsed 214 s. Now (same board, same budget, back-to-back on one machine): **2L 21/39 in 4 passes, 3L 22/39, 4L 22/39 in 3 passes**, elapsed 180.0–180.2 s. Root cause was **not** grid size or pop caps: `plan_escapes` swapped strategy at 3 layers and ran the fine-grid stub escape **instead of** the VIP/dogbone fanout. On a 0.4 mm QFN-56 the fan cannot fit (one 14-pin row would need ~13 mm of perpendicular room at the 1.0 mm breakout spread), so it spent **24 s of a 90 s budget to free 5 pads** where the fanout frees **25 in 10 ms** — the coarse router then had nothing to land on. The 34 s overrun was a second bug: the budget was only checked BETWEEN nets, so one long A* ran past it. Fix: the fanout is the baseline on **every** stackup (`fanout::plan_footprint` per footprint); fine escape is an optional per-footprint improvement, kept only when the fanout left pads stranded AND the fan fits AND it frees ≥ as many pads, bounded by a per-footprint time slice and a per-search pop cap — and, since it did not earn its keep on any board we measure, it is now **opt-in** (`route fine_escape=true` / `RouteOptions::fine_escape`, default off). A* searches now also carry the pass deadline (`astar::Limits`), so `max_seconds` is honoured to the second. Pinned by `crates/pcb-router/tests/layer_count_monotonic.rs`. Gotcha still applies: `clear-route` BEFORE `layer remove`, else copper left on the inner layer blocks the removal |
| O9 | ~~`layer add`+`remove` round-trip changes routing results~~ **FIXED / not a state bug** | Root cause: the round-trip mutates **nothing** (project serialisation is byte-identical before/after — pinned by `crates/pcb-script/tests/layer_roundtrip.rs`); the 21/39 → 13/39 drop was the **wall-clock `max_seconds` budget** — under machine load fewer RR&R passes finish inside 180 s, so the best-so-far result is worse. Live re-check: route/round-trip/route on the same board gave **21/39, 18 failed nets, 4 passes** three times in a row (before, control repeat, after the round-trip). Hardened as part of the fix: `layer remove` now also refuses when a **pour or keepout** still references the layer (it only checked footprints/pads/traces), which was a real path to a dangling `Layer{index}` surviving the round-trip | Read the route text: `N pass(es)` + `BUDGET HIT` explain a result change; compare runs at the same pass count, or raise `max_seconds`, before blaming board state |
| O4 | Progress lines not always in `/script` reply | Logged via activity bus; HTTP reply is only final tool text | Optionally append last N progress lines to `route.run` text |
| O5 | Chirality / pin-1 of agent-authored QFN | RP2040 sides authored from secondary docs | Verify against datasheet before any fab; photo-calibrate if possible |
| O6 | `*.fragua` in `.gitignore` | Stress project needs `git add -f` | Consider `!stress/*.fragua` exception |
| O7 | Compact for fecha-gateway-v3 | Root `TODO.md` still open | Separate campaign; uses `compact` verb |

### 5.3 OPEN — agent / DX polish

| ID | Problem | Suggestion |
|----|---------|------------|
| D1 | Long multi-line scripts: one failure doesn't stop rest (by design) | Document; use small scripts for critical path |
| D2 | `palette` then `place` when ref already on board → confusing errors | `list-palette` text should list refs (like list-lib) |
| D3 | No first-class crystal/switch/connector kinds | Add kinds or document ic-wrapper pattern in help |
| D4 | Library confirm still bypasses human photo review | Acceptable for agent-authored geometry; keep photo path for real modules |
| D5 | Default `RouteOptions` clearance 0.40 vs script default 0.20 | Inconsistency between library default and script defaults — know which path you use |

### 5.4 Historical / operational gotchas

- **`fragua` vs `fragua run`** — will waste a full debugging cycle if forgotten.  
- **Mac app open:** memory notes say `open -a Fragua file` is ignored; use `open -na Fragua --args path` or the CLI binary.  
- **HTTP is single long request** for `route`: client timeout must be **> max_seconds** (e.g. max_seconds=90 → client timeout ≥ 110). Server continues if client dies mid-route (pre-budget); with budget, server should stop ~on time.  
- **GND pour:** declare `class ground pour=both` **before** route; router skips pour nets (by design).  
- **fecha context:** door/gateway boards used modules, not bare QFN; U2 chirality bugs were a past real fab issue — agents must respect pin-1.

---

## 6. Router internals (what continues the fight)

### 6.1 Pipeline (`pcb_router::route`)

1. Collect nets from pad assignments.  
2. **`plan_escapes`** → either fine-escape (≥3L + fine cell) or **`plan_fanout`** (VIP + dogbone on 2L).  
3. Order nets (few pads first; fanned nets prioritized; diff-pair adjacency).  
3b. **Optional** (`route negotiate=true`): PathFinder negotiated congestion (`negotiate.rs`) — see §6.5. Converged ⇒ done; otherwise the passes below still run on the remaining budget and the better board wins.  
4. Up to **3 RR&R passes** with congestion bias (skipped early if budget exhausted).  
5. Optional clean-board rip-up pass.  
6. Commit best board; add fanout vias + escape stubs.  
7. Organic string-pull + fillets (skipped if budget hit); stitching vias for pours.

### 6.2 Why QFN-56 @ 0.4 mm is special

- Pad pitch 0.4 mm, pad width ~0.2 mm → **surface parallel escape is geometrically impossible** at 0.20 mm clearance.  
- VIP barrel is 0.30 mm → **does not fit** in 0.2 mm pad → need dogbone.  
- Dogbones toward package interior compete for space under body; body stamp + moat still blocks channels.  
- Thermal pad (if full size) obliterates under-package bottom routing — we shrank EP to 1.6 mm.  
- 2-layer only has Top/Bottom; multi-layer fine-escape was built for inner planes.

### 6.3 Key files

| File | Role |
|------|------|
| `crates/pcb-router/src/router.rs` | Driver, `RouteOptions`, budget, progress |
| `crates/pcb-router/src/fanout.rs` | VIP + **dogbone**, cluster detection |
| `crates/pcb-router/src/escape.rs` | Fine-grid stubs (3L+); budget fraction for escape |
| `crates/pcb-router/src/astar.rs` | Search + pop cap; `Negotiate` = sharing-aware mode |
| `crates/pcb-router/src/negotiate.rs` | **PathFinder negotiated congestion** — sharing, history, conflict-driven rip-up, legal extraction, corridor autopsy |
| `crates/pcb-router/src/grid.rs` | `stamp_bodies` / pads / drilled disks — **moat knobs**; `ClearanceModel` + `AreaField` (per-net, per-cell clearance) |
| `crates/pcb-core/src/rules.rs` | **`RuleArea` + `RuleResolver`** — the ONLY place the clearance rule is derived. Router, fanout, organic, topo and DRC all go through it; never re-derive it |
| `crates/pcb-placer/src/lib.rs` | SA + **pull_passives_to_anchors** |
| `crates/pcb-script/src/tools.rs` | HTTP-facing tool text, route wiring |
| `crates/pcb-script/src/script.rs` | DSL parse |

### 6.5 Negotiated congestion (`negotiate.rs`) — what it does and what it costs

`route negotiate=true`. One iteration = route every pending net with foreign
TRACE/VIA copper treated as *shareable at a price*, then find who still
shares, raise the per-cell **history** cost there, rip up only those nets,
repeat. Present price ramps 1 → 3 → 9 → … cells of detour per shared cell;
history adds 2 per iteration (capped 96).

Three things are worth knowing before touching it:

1. **Clearance vs sharing.** The resource a net consumes is its copper plus
   a *pairwise* halo. Rather than inflating claims (far too conservative —
   two nets whose halos touch can be perfectly legal), the code keeps the
   exact asymmetric test the DRC uses (copper stamped bare, each net scans
   its own `clearance + half-width` disk) and only softens the verdict:
   foreign trace copper in the disk = shareable, foreign PAD copper = hard.
   A pad cannot be ripped up, so a pad-halo violation could never be
   negotiated away. Because the price comes from the same disk test that
   decides legality, **"zero conflicts" is exactly "DRC-legal copper"**.
2. **A shared state is never a result.** `extract_legal` lays negotiated
   geometry only where it passes the ordinary hard clearance test and
   hard-routes the rest, so the driver's best-so-far comparison only ever
   sees legal boards. `max_seconds` is honoured to the second (measured
   180.1 s on a 180 s budget).
3. **It is off by default and the measurement says why** — see §0 and O1.
   Its real product value here is the autopsy it prints into the route
   hints: `congestion: layer L around (x, y) mm — N over-subscribed
   cell-iteration(s)` and `congestion: net X is blocked even when allowed
   to share every foreign trace — …`. That is how the O1 verdict was
   reached, and it is the cheapest way to re-check it after any change to
   the fanout or the placement.

### 6.4 RouteOptions knobs (script)

```text
route
  trace_width=0.25
  clearance=0.20
  cell=0.20
  via_drill=0.30 via_diameter=0.60 via_cost=8
  max_seconds=90          # 0 = unlimited
  negotiate=false         # true = PathFinder negotiated congestion (§6.5)
  engine=grid|topo
  organic=true|false
  fillet=3
```

Defaults in script layer: cell 0.20, clearance 0.20, max_seconds 90.  
`clearance` is the board DEFAULT; the gap actually enforced between two nets is
`max(default, class A, class B)` unless a `RuleArea` covering the point overrides
it outright (`rule-area` / `rule-area-around`, `list-rule-areas`). Watch the
quantization: the search radius is `ceil((clearance + width/2) / cell) + 1`
cells, so at `cell=0.20` a 0.12 and a 0.20 rule are the same 3 cells for a
0.25 mm trace — lower the cell too if you want a fine rule to buy real room.  
`RouteOptions::default()` in the router crate still has clearance 0.40 if you call Rust directly — beware.

---

## 7. Recommended next work (prioritized)

*(2026-07-27 v4 update: items 1–3 of the old Priority A are DONE — instrumentation, no fine-pitch body stamp, dogbone stubs + stagger. Result 5→21/39 with a hard plateau. Item 4 was probed and REGRESSES — see O8.)*

### Priority A — Break the 20/39 plateau (it is an ESCAPE problem, not a routing one)

1. ~~**Corridor autopsy.**~~ **DONE (v6).** `route negotiate=true` prints it:
   the over-subscribed cells are all bottom-layer inside a ~4 × 4 mm patch on
   U1 (40–43, 21–27 mm), and 12–13 nets cannot reach their U1 pad even when
   allowed to share every foreign trace. The hypothesis that the +3V3 ring
   hogs corridors is **wrong** — the corridors are walled by pads and by the
   fanout's own via landings, none of which any router can move.
2. ~~**Rip-up that can evict fat nets.**~~ **SUPERSEDED (v6).** Negotiated
   congestion is the general form of this (every net can evict every other,
   by price) and it does not move the number: what fails, fails against
   fixed copper.
3. **Escape-slot assignment (the actual next step).** `fanout::plan_footprint`
   picks a dogbone per pad in isolation. It should pick the *set* — which pad
   escapes at which depth/direction — so that a legal lane survives to every
   landing: a flow / matching problem over the escape ring, with the lane
   width taken from the resolved rule (0.245 mm centreline clearance at the
   `fine` area, versus a 0.2 mm inter-pad channel — so lanes must be vias,
   and the vias must be staggered to leave routing space between them).
   Verify with `route negotiate=true`: the count of "blocked even when
   allowed to share" is the direct measure of progress, and it should fall
   toward zero before any effort goes back into the search.
3. ~~**Reduced clearance class near fine-pitch.**~~ **DONE (v5, see O10).** `rule-area` + `fab-rules` shipped and the 7 clearance errors are gone, but connectivity did not move: the tighter rule quantises away at a 0.20 mm cell, so it removes the *legality* problem and leaves the *congestion* problem. Do 1/2 (corridor autopsy, evicting rip-up) next.
4. **More dogbone depth slots.** 24 dogbones landed; U1 alone has ~34 signal pads. Widen `dogbone_via_candidates` depth ladder so a full QFN row fans out.

### Priority A2 — Fix the 4-layer path (O8) — **DONE 2026-07-27**

Result: 2L **21/39**, 3L **22/39**, 4L **22/39** at `max_seconds=180`, all inside budget (180.0–180.2 s). Extra layers now help slightly instead of collapsing the board. What was actually wrong, and what to remember:

1. **It was strategy, not compute.** The pop caps and the doubled grid were *not* the cause — forcing the fanout path on a 4-layer board (with the caps untouched) already recovered 22/39. The cause was `plan_escapes` replacing the proven VIP/dogbone fanout with the fine-grid stub escape as soon as `layer_count >= 3`.
2. **Fanout is now the baseline on every stackup**, planned per footprint (`fanout::plan_footprint`). Fine escape may only *add* escapes for a footprint the fanout left stranded, and only when its fan geometrically fits and it frees at least as many pads; it is opt-in (`route fine_escape=true`) until some board shows it winning.
3. **Budgets are enforced inside a search** (`astar::Limits { max_pops, deadline }`), not just between nets — that is what removed the 34 s overrun.
4. **A 4-layer pass costs ~1.5× a 2-layer pass** (68 s vs 44 s for pass 1 on the stress board), so 4L completes 3 RR&R passes where 2L completes 4. That is why 4L only edges ahead. The next win on multi-layer is making a pass cheaper (the per-expansion all-layer scans were deduped in `8d9fc9e`; the remaining cost is the doubled state space itself), or spending inner layers deliberately rather than letting A* explore all of them.

**Remaining multi-layer wart (not a router bug):** `layer add In1.Cu signal` appends at the BOTTOM of the stack, so the stackup ends up `[F.Cu, B.Cu, In1.Cu, In2.Cu]` — the layer *named* `B.Cu` is now an inner layer and the physical bottom is named `In2.Cu`. Harmless to the router (it only counts layers) but wrong for fab output; fix `layer.add` to insert before the bottom layer before anyone exports a 4-layer board.

### Priority B — Placement quality

1. Improve `pull_passives_to_anchors`: prefer **same-net power pin** of IC within 3 mm, not centroid of all anchors.  
2. `list-palette` should list references in text.  
3. Optional `place-near REF ANCHOR_REF [dx dy]` helper.

### Priority C — Alternate stress (product-realistic)

If bare QFN remains a research problem, stress **module-class** complexity that ships:

- Waveshare-style **RP2040-Zero** castellated module (like existing `esp32_s3_zero`) + peripherals.  
- Or resume **fecha-gateway compact** from `TODO.md` (real order blocked on compact).

### Priority D — Hardening

1. Unit test: dogbone placed for pad with w,h < via diameter.  
2. Unit test: `max_seconds=0.01` returns quickly with timeout failures.  
3. Integration: load `stress/rp2040-minimal.fragua`, route with budget, assert vias ≥ 10 and elapsed < 120 s.

---

## 8. How to reproduce the stress quickly

```bash
cd ~/pcb
cargo build --release --bin fragua

# Terminal A
./target/release/fragua run stress/rp2040-minimal.fragua

# Terminal B
curl -s http://127.0.0.1:7878/health
curl -s http://127.0.0.1:7878/script \
  -H 'content-type: application/json' \
  -d '{"script":"status\nview\nclear-route\nroute max_seconds=90\nscreenshot stress/rp2040-out.png width=2000\nview\nsave stress/rp2040-minimal.fragua"}'
```

**No-server measurements** (how the O8 numbers were taken — no autosave, no client timeout, time-stamped progress):

```bash
O8_LAYERS=4 O8_SECS=180 cargo test --release -p pcb-router \
    --test stress_board_probe -- --ignored --nocapture
```

To take the same numbers with negotiated congestion, add `negotiate=true`
to the script `route` line, or set `negotiate: true` in the probe's
`RouteOptions` — the probe prints the corridor autopsy lines either way.

`O8_LAYERS` appends inner signal layers exactly like `layer add In1.Cu signal`. Connectivity is printed as `failed/routable`; `39 - failed` is the same number the script reports as `N/39 net(s) fully connected`. Numbers are **load-sensitive** (see O9) — take the stackups you are comparing back-to-back on an idle machine, and compare runs at the same pass count.

**Rebuild schematic from scratch** (destroys placement): run contents of `stress/01_schematic.fragua.txt` via POST `/script` (long), then re-place using `edge-place` + clustered place script (see session logs `05_edgeplace_result.txt` patterns).

**Library keys** must already exist in `~/.pcb-library` (confirm-lib was run). If missing, re-run the `lib`…`confirm-lib` section of `01_schematic.fragua.txt`.

---

## 9. Related projects on disk (context, not this campaign)

| Path | Relation |
|------|----------|
| `~/fecha/firmware/sf7/yellow/fecha-gateway-v2/` | Production-ish Fragua board (modules); compact TODO |
| `~/fecha/firmware/sf7/yellow/fecha-door-v3/` | Door board redesign in Fragua |
| `~/.pcb-library/` | Global component library |
| `VISION.md` / `ARCHITECTURE.md` / `README.md` | Product docs |

**Accounts:** personal GitHub `kidandcat` / mentasystems fragua. Use `personal` if push perms wrong.

---

## 10. Definition of done for “Fragua handles complex PCBs”

Suggested acceptance criteria an agent can optimize toward:

1. **RP2040 stress (2-layer):**  
   - `route max_seconds=180`  
   - **0 failed nets** OR only documented intentional NC nets  
   - DRC errors = 0 (or only waived thermal/annular with explicit allowlist)  
   - Wall-clock ≤ 3 minutes  

2. **Agent DX:**  
   - Any failing tool prints reason in text  
   - Route always terminates within `max_seconds + 15s`  
   - `list-lib` / `list-palette` / `list-pending` human- and agent-parseable  

3. **Regression:**  
   - `cargo test -p pcb-router --tests` green  
   - fecha-gateway style module board still routes clean (don't break module path for QFN experiments)

---

## 11. Quick “if you only read one section”

Fragua **can** do agent-driven schematic + place + partial route on a Pico-class design. The **hard open problem** is **2-layer fine-pitch QFN routing**, not the script API. The v4 pass (true dogbone stubs, rank-staggered fanout, no body stamp under fine-pitch, board-truthful reports) took connectivity **5/39 → 21/39 @ 180 s**, but it **plateaus**: 600 s returns the identical 18 failed nets, and 4 layers only buy one more net (22/39, O8 — the collapse to 1/39 is fixed, but extra copper layers are not the answer either). v6 then settled *what* the wall is: PathFinder-style negotiated congestion (`route negotiate=true`) lets every net route through every other net's copper, and **a dozen nets still cannot reach their U1 pads**. So it is not contention and not the rule — it is the **escape geometry** (0.4 mm pins leave a 0.2 mm channel where a legal trace needs 0.245 mm, so every escape is a via, and the 25 dogbone landings wall each other off). Next step is escape-slot assignment (§7 Priority A.3), not another search.

---

## 12. Session command cheatsheet

```bash
# Health
curl -s http://127.0.0.1:7878/health

# Status / view
curl -s http://127.0.0.1:7878/script -H 'content-type: application/json' \
  -d '{"script":"status\nview\nnets"}'

# Bounded route
curl -s http://127.0.0.1:7878/script -H 'content-type: application/json' \
  -d '{"script":"clear-route\nroute max_seconds=90\nview"}'

# Screenshot
curl -s http://127.0.0.1:7878/script -H 'content-type: application/json' \
  -d '{"script":"screenshot /tmp/fragua-board.png width=2000"}'

# Library
curl -s http://127.0.0.1:7878/script -H 'content-type: application/json' \
  -d '{"script":"list-lib\nlist-pending"}'
```

---

*Document written 2026-07-27 for handoff; v6 (negotiated congestion) folded in the same day. Update this file when O1 (full QFN route) moves.*
