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

**Latest board metrics** (`stress/rp2040-minimal.fragua`, v4 pass — dogbone stubs + stagger + no fine-pitch moat):

- Outline **80 × 45 mm**, 36 footprints, 39 nets, 36 symbols  
- **~1544 traces**, **~57 vias** (25 fanout, 24 dogbones with copper stubs)  
- **14/39 nets fully connected @ `max_seconds=90`; 21/39 @ 180 s**, reproducible run-to-run on an idle machine (see O9: the one-off 13/39 was budget starvation under load, not board state)  
- **Plateau:** `max_seconds=600` → same 21/39, same 18 failed nets → algorithmic wall, not compute  
- **4-layer probe REGRESSES:** `layer add In1/In2` + route → 1/39, budget overrun (see O8)  
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

### 5.2 OPEN — product / router correctness

| ID | Problem | Evidence | Suggested direction |
|----|---------|----------|---------------------|
| O1 | **Cannot fully route bare QFN-56 on 2L** — now **21/39** (was 5/39) but **plateaued** | Same 18 nets fail at 180 s and 600 s (QSPI, USB, SWD, XIN/RUN, GPIOs on U1 left/top/bottom, +1V1, VBUS, CC2); hints blame U1 pads as outliers | Escape congestion, not search time: the +3V3 ring and early-routed nets hog the corridors. Try reduced clearance class near fine-pitch, targeted rip-up of fat power nets, more dogbone depth slots |
| O2 | ~~Report vs board copper counts diverge~~ **FIXED** (`489c1af`, `8448a2d`) | route/drc text now reports final board copper, per-net failures, hints, budget flag | — |
| O3 | ~~Body stamp moat fights fine-pitch escape~~ **FIXED** (`71cf4ef`) | No body stamp for SMD packages with pad pitch < 0.5 mm; modules/TH keep it | — |
| O8 | **≥3-layer path regresses instead of helping** | `layer add In1.Cu/In2.Cu signal` + `route max_seconds=180` → **1/39**, search laid 0 segments, elapsed 214 s (budget overrun); a 3-layer board reproduces the same collapse | Fine-escape (≥3L) + bigger grid starves A*; pop caps/budget fractions were tuned for 2L. Needs its own campaign before extra layers can be the fine-pitch answer. Gotcha: `clear-route` BEFORE `layer remove`, else copper left on the inner layer blocks the removal |
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
| `crates/pcb-router/src/astar.rs` | Search + pop cap |
| `crates/pcb-router/src/grid.rs` | `stamp_bodies` / pads / drilled disks — **moat knobs** |
| `crates/pcb-placer/src/lib.rs` | SA + **pull_passives_to_anchors** |
| `crates/pcb-script/src/tools.rs` | HTTP-facing tool text, route wiring |
| `crates/pcb-script/src/script.rs` | DSL parse |

### 6.4 RouteOptions knobs (script)

```text
route
  trace_width=0.25
  clearance=0.20
  cell=0.20
  via_drill=0.30 via_diameter=0.60 via_cost=8
  max_seconds=90          # 0 = unlimited
  engine=grid|topo
  organic=true|false
  fillet=3
```

Defaults in script layer: cell 0.20, clearance 0.20, max_seconds 90.  
`RouteOptions::default()` in the router crate still has clearance 0.40 if you call Rust directly — beware.

---

## 7. Recommended next work (prioritized)

*(2026-07-27 v4 update: items 1–3 of the old Priority A are DONE — instrumentation, no fine-pitch body stamp, dogbone stubs + stagger. Result 5→21/39 with a hard plateau. Item 4 was probed and REGRESSES — see O8.)*

### Priority A — Break the 21/39 plateau (router)

1. **Corridor autopsy.** The same 18 nets fail at any budget. Dump which cells around U1 are blocked per layer after fanout (debug render or grid stats) and identify what owns the corridors — hypothesis: the +3V3 ring (11 pads, routed early, wide class) plus GND stubs wall off the QFN quadrants. The route hints already name U1 outlier pads.
2. **Rip-up that can evict fat nets.** RR&R currently biases by congestion but the failing fine-pitch nets never get the corridor back. Let late passes rip *routed* wide-class nets crossing a failing net's bounding corridor, not only failed ones.
3. **Reduced clearance class near fine-pitch.** The 7 DRC errors are 0.125–0.175 mm vs 0.2 — real boards use a finer rule (0.1 mm) inside the QFN escape zone. Support per-area or per-class clearance so escape lanes are legal instead of near-miss.
4. **More dogbone depth slots.** 24 dogbones landed; U1 alone has ~34 signal pads. Widen `dogbone_via_candidates` depth ladder so a full QFN row fans out.

### Priority A2 — Fix the 4-layer path (O8)

- 4L route collapses to 1/39 and overruns the budget: fine-escape triggers with cell=0.20, the 4-layer grid doubles cells, and the A* pop caps / per-stage budget fractions tuned for 2L starve every pass. Re-tune caps by layer count, or gate fine-escape behind an explicit opt-in until it earns its keep.

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

Fragua **can** do agent-driven schematic + place + partial route on a Pico-class design. The **hard open problem** is **2-layer fine-pitch QFN routing**, not the script API. The v4 pass (true dogbone stubs, rank-staggered fanout, no body stamp under fine-pitch, board-truthful reports) took connectivity **5/39 → 21/39 @ 180 s**, but it **plateaus**: 600 s returns the identical 18 failed nets, and the 4-layer probe *regresses* to 1/39 (O8). The wall is **escape-corridor congestion around U1** — attack rip-up of fat routed nets, reduced clearance near fine-pitch, and more dogbone depth slots (§7) before inventing a new product surface.

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

*Document written 2026-07-27 for handoff. Update this file when O1 (full QFN route) moves.*
