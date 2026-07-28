# Fragua stress: RP2040 Minimal (open hardware)

> **Full agent handoff:** see [`AGENT-HANDOFF.md`](./AGENT-HANDOFF.md) — problems, fixes, router internals, next steps.

Recreation of a **Raspberry Pi Pico–class** open-hardware board in Fragua:

- Bare **RP2040 QFN-56** (0.4 mm pitch)
- **W25Q16** SOIC-8 QSPI flash
- 12 MHz **crystal** + load caps
- **USB-C** + series resistors + CC pull-downs
- **MIC5219-class LDO** SOT-23-5
- BOOTSEL / RESET switches, activity LED
- SWD 1×03 + dual 1×10 GPIO headers

Inspired by the [RPi “Hardware design with RP2040” minimal example](https://datasheets.raspberrypi.com/rp2040/hardware-design-with-rp2040.pdf)
and [Mitayi-Pico-D1](https://github.com/CIRCUITSTATE/Mitayi-Pico-D1) (MIT).

## Files

| Path | What |
|------|------|
| `rp2040-minimal.fragua` | Live project (schematic + placement + route) |
| `01_schematic.fragua.txt` | Script that builds libs + schematic |
| `rp2040-out.png` | Latest board screenshot (v7 route) |
| `rp2040-minimal-v3.png` | v3 board screenshot |
| `07_route_v3.txt` | Latest route API log |

## How to drive

```sh
./target/release/fragua run stress/rp2040-minimal.fragua

curl -s http://127.0.0.1:7878/script \
  -H 'content-type: application/json' \
  -d '{"script":"status\nview\nroute max_seconds=90"}'
```

## Results (2026-07-27)

| Stage | v1 (first pass) | v3 (router/placer fixes) | v4 (stubs, stagger, no fine-pitch moat) |
|-------|-----------------|--------------------------|------------------------------------------|
| Library | OK via `confirm-lib` | same | same |
| Schematic | ERC 0 | same | same |
| Placement | 36 fps, `edge-place` | same + passive pull-to-anchor | same |
| Route wall | **~6 min**, no budget | **~90 s** with `max_seconds=90` | budget honoured (90/180 s) |
| Copper | 62 traces / 3 vias | 402 traces / 41 vias | **1544 traces / 57 vias** (24 dogbones + stubs) |
| Fully connected nets | ~6/39 | ~5/39 | **14/39 @ 90 s, best 21/39 @ 180 s** (plateau: 600 s adds nothing; saved file 13/39 after the layer round-trip, handoff O9) |
| DRC | 3E | 2E | 7E (marginal clearance at the QFN, 0.125–0.175 vs 0.2 mm) — **0 clearance errors in v5, see below** |

### v5 (2026-07-27) — clearance rule areas + honest per-net clearance

| Run (2-layer, `route max_seconds=180`, idle machine) | Copper | Fully connected | DRC |
|------------------------------------------------------|--------|-----------------|-----|
| Baseline, no rule area | 1544 traces / 57 vias (24 dogbones) | **21/39** | **7E** / 70W — every error a 0.125–0.175 mm clearance at the QFN |
| `fab-rules jlcpcb-2l` + `rule-area-around U1 fine margin=1.5 clearance=0.12 via_drill=0.20 via_dia=0.45` | 951 traces / 46 vias (25 dogbones) | **20/39** | **4E** / 74W — **zero clearance errors**; the 4 are `NetSplit` |

What the rule area bought and what it did not:

- **Clearance errors: 7 → 0.** The near-miss geometry at the QFN is now
  *legal by declaration* instead of a violation, and the router, the fanout
  and DRC all read that declaration from one resolver.
- **Fab honesty:** with `fab-rules jlcpcb-2l` adopted, the fanout drops the
  smallest via the fab can actually build (0.45 mm pad / 0.20 mm drill,
  ring 0.13 mm), so the 25 `SmallDrill` warnings of the 0.15 mm barrels are
  gone. The one `RuleBelowFabLimit` warning is the area's 0.12 mm clearance,
  5 µm under JLCPCB's standard tier — deliberate, and reported every time.
- **Connectivity did not improve** (20/39 vs 21/39, one net inside run-to-run
  noise). At the default 0.20 mm cell the tighter rule quantises to the same
  3-cell search radius for signal nets, so the escape-corridor wall (O1) is
  untouched. The remaining 4 errors are `NetSplit`: a fanned-out pad whose net
  the router still fails to finish leaves an isolated island. That is the same
  O1 wall wearing a different hat — it needs PathFinder / negotiated
  congestion, not a rule.
- A finer route cell (`cell=0.15`) does exploit the tighter rule
  geometrically but starves the budget (2 passes instead of 4) → 19/39.

**Plateau evidence:** `max_seconds=600` returns the exact same 18 failed nets as
180 s — the remaining wall is algorithmic (escape congestion around U1), not
compute time. **4-layer probe:** fixed in the O8 pass — `layer add In1/In2`
now gives 22/39 at 180 s (was a 1/39 collapse), so extra copper layers help,
barely; they are not the answer either.

### v6 (2026-07-27) — PathFinder negotiated congestion, and what it proved

`route negotiate=true` swaps the rip-up-and-reroute driver for
**negotiated congestion**: every net takes its shortest path first (foreign
traces are shareable at a price, foreign pads are not), the corridors they
fight over get progressively more expensive, and only the nets that share
copper are ripped and rerouted. It converges to legal copper or falls back
to the classic passes with the budget it has left — the better board wins
either way, and a shared (illegal) state is never offered as a result.

| Run (2-layer, `route max_seconds=180`, same binary, back to back) | Copper | Fully connected | Passes / iterations | DRC |
|---|---|---|---|---|
| default driver (RR&R) | 951 traces / 46 vias | **20/39** | 4 passes | 4E / 74W, **0 clearance errors** |
| `route negotiate=true` | 777 traces / 41 vias | **17/39** | 5 iterations | 4E / 78W, **0 clearance errors** |

**Negotiation loses here, and the reason is the interesting part.** With
sharing allowed — i.e. every net free to route straight through every other
net's copper — **12–13 of the 39 nets still cannot be routed at all**:

```
congestion: net GPIO0 is blocked even when allowed to share every foreign
            trace — no path to pad U1.2 at (36.55, 21.80) mm
congestion: net SWCLK  ... no path to pad U1.23 at (40.60, 27.45) mm
congestion: net QSPI_SS ... no path to pad U1.54 at (38.20, 20.55) mm
congestion: layer 1 around (41.0, 23.0) mm — 85 over-subscribed cell-iteration(s)
```

Every one of them is a U1 pad, and the over-subscribed cells are all on the
bottom layer inside a ~4 × 4 mm patch centred on U1 (40–43, 21–27 mm). So
the wall is **not** inter-net contention, which is exactly what PathFinder
exists to resolve — it is **fixed geometry**: the QFN's own pad halos and
the fanout's 25 dogbone via landings, which are stamped on every layer and
cannot be ripped up. Do the arithmetic and it is unavoidable: a 0.25 mm
trace at the area's 0.12 mm rule needs 0.245 mm from its centreline to any
foreign copper edge, while two adjacent 0.4 mm-pitch pads leave a 0.2 mm
channel. **No trace can pass between two QFN pins**, so every escape has to
be a via, and the vias then wall each other off.

That is a result for the *escape planner*, not the router: the dogbone
landings are chosen per pad without checking that a legal lane survives to
each one. Assigning escape slots so that lanes exist (a flow / matching
problem) is the next lever — negotiation cannot help until the lanes are
there.

Negotiation is therefore **opt-in**, not the default: it is the right
algorithm when the wall really is contention (see
`crates/pcb-router/tests/negotiated_congestion.rs`, where it takes the
corridor away from the net that can afford a detour and gives it to the one
that cannot — from either routing order), and on this board it is worth
running for the autopsy alone.

### v7 (2026-07-27) — escape-slot matching: the plateau breaks

The v6 verdict said the wall was escape geometry — dogbones picked per pad,
in pad order, walling their own neighbours off. v7 replaces that with a
**global escape-slot assignment** (`pcb-router/src/slots.rs`): candidate
barrel sites form a lattice per package side (5 outward + 3 inward depth
ranks × 13 sub-pitch lateral offsets, so a barrel may sit *between* two
pins), and pad → slot is solved as a **min-cost max-cardinality matching**
whose costs price stub length, the direction the net actually travels,
exit-ray blockage, and lane tightness — two barrels closer than one
routable lane wall each other off even when via-to-via clearance passes.
Assignment runs three rounds, pricing the previous round's exit pressure
PathFinder-style; pads with no legal site are **stranded** and named in
the route text before the search wastes budget on them.

Chasing the survivors with a flood-fill autopsy (`src/diag.rs`, test-only)
then turned up three more mechanisms, all fixed:

- **The routing grid did not contain every pad.** The outline inset plus
  truncating snap left edge-header pads out of the grid on the high side
  of the board only — HDRB1/SWCLK/SWDIO were failing at *J4/J2*, while
  every autopsy blamed their U1 pads (the search names its target, not
  the entombed seed). The grid now grows in whole cells to cover every
  pad; the margin is stamped unroutable, only pad copper reachable.
- **Edge-mounted parts never got escapes.** The escape axis aims at the
  footprint pad centroid; for a USB-C receptacle that points at the board
  edge, so every barrel site failed edge clearance — and the surface probe
  happily believed escape rays that leave the board (nothing out there to
  block them). `escape_axis` flips off-board directions; the whole USB
  group (J1 + series resistors) finally fans out.
- **Escape stubs did not travel with their barrels.** The dogbone stubs
  were hoisted into a list the driver never updated, which turned into
  illegal copper the moment the new **rip-and-reassign lever** started
  moving barrels: a net that failed two RR&R passes at a fanned pad gets
  its barrel ripped, the site banned, and the assignment re-run against
  the surviving copper (a pad with no better site gets its exact barrel
  back). Barrel + stub are now one unit snapshotted with every best
  board, so the committed board is always the exact copper that was
  measured — at any budget, any number of lever firings.

| Run (2-layer, `route max_seconds=180`, idle box, back-to-back) | Copper | Fully connected | DRC |
|---|---|---|---|
| v6 baseline (greedy dogbones) | 951 traces / 46 vias (25 dogbones) | **20-21/39** | 4E (NetSplit) / 74W, 0 clearance |
| escape-slot matching alone | 1150 / 63 (32 escapes, 0 stranded) | **26/39** | 3E (NetSplit) / 65W, 0 clearance |
| + grid/edge fixes + rip-and-reassign | 1310 / 58 (33 escapes, 0 stranded) | **28/39** | 3E (NetSplit) / 61W, **0 clearance** |

Deterministic to the trace (probe and server agree byte-for-byte, twice),
and 300 s lands on the same board as 180 s — the lever fires at ~95 s,
moves 7 stuck barrels, and the post-lever pass completes inside the
budget. "Blocked even when sharing" fell 12–13 → ~5, and part of that
residue is provably an over-report (foreign barrels are hard in negotiate
mode; the flood shows GPIO1/HDRB1/SWDIO connectable end-to-end).

The remaining 11 nets are two honest walls and a budget line: **SWCLK**
(U1.23's barrel sits in a 19-cell pocket walled by +1V1/SWDIO pads and
RUN/SWDIO barrels — every alternative site on that side is taken or
fails via-to-via clearance) and **+3V3** (U1.21, same shape, and the net
needs all 23 of its pads); **USB_DP/VBUS** at J1, where 0.7 mm pitch
with a 0.46 mm barrel leaves exactly one rank of legal sites and not
enough of them (the honest fix is a rule area over J1, a board edit, not
a router change); and the GPIO/QSPI churn, which is now plain
congestion-under-budget — the first frame in this campaign where the
limit is compute, not geometry.

### v8 (2026-07-28) — compact, escape-aware placement

Jairo's review of the v7 render: the 80×45 mm outline with headers parked on
the far edges is obviously suboptimal, and the deliverable is the TOOL —
placement must come from Fragua's verbs, never from hand-tuning the board.

**Product (pcb-placer + script):** bundle-crossing term in the placement
score, decoupling-ring placement for 2-pad passives at their IC power pin,
`edge-plan REF... [seed=N]` (picks WHICH edge each edge-mounted part gets by
minimizing net-bundle wirelength+crossings), compact `allow_failed=N` /
`route_seconds=N` knobs with footprint-anchored rule areas, and the
`bench-dev` cargo profile (no LTO, 16 CGUs: incremental rebuilds 2–3 min →
~5 s, near-release runtime; publish final numbers with `--release`).

**Board result (same schematic as v7, all-algorithmic placement):**

| Board | Area | Fully connected @ 180 s |
|-------|------|--------------------------|
| v7, 80×45 mm | 3600 mm² | 28/39 |
| **v8, 36×30 mm** | **1080 mm² (3.3× smaller)** | **25/39** (24/39 @ 480 s earlier run — run-to-run ±1) |
| probe, 44×34 mm re-place | 1496 mm² | 21/39 (invalid point — see auto-place bug below) |

Escape stays perfect on the compact board: 33/33 pads slotted, 0 stranded.
The ~3-net cost vs v7 is the same U1 cluster squeezed into a third of the
area — an honest compactness↔connectivity trade-off, not a regression.

**New open problems found while measuring:**

- **auto-place can return a worse-than-initial solution** (measured: HPWL
  +78 mm, congestion +606 cells, crossings 4→5 on the 44×34 re-place). The
  SA needs a best-seen clamp. Until fixed, curve points that re-place from
  a good layout are unreliable.
- **Shutdown autosave race:** the server autosaves on SIGTERM, so
  `git checkout <file>` + restart silently loses the restore unless the
  process is confirmed dead FIRST (stop unit → verify no `fragua` process →
  restore file → start).
- `compact` pinned at its own size runs 0 candidates (it only re-places
  while shrinking) — growing a board needs `outline` + `edge-plan` +
  `auto-place`, there is no single re-place-at-size verb yet.

### What works

- End-to-end agent loop: libs → confirm → schematic → place → route → screenshot
- Complex multi-pin ICs and many passives
- GND pour + net classes
- `edge-place REF side`
- **`route max_seconds=N`** returns on time; progress in the activity log
- **2-layer dogbone fanout** for pads too small for via-in-pad (QFN 0.4 mm)

### Still hard

1. **QFN-56 full connectivity on 2 layers** — v7's escape-slot matching
   broke the 21/39 plateau (**28/39**), and the wall changed character:
   two pads are genuinely entombed (SWCLK, +3V3 — see v7), two J1 pins
   need a rule area the board doesn't declare, and the rest is congestion
   under a 180 s budget. The next levers are compute-shaped (cheaper
   passes, a smarter second-pass order), plus the J1 rule area as a board
   edit.
2. **4-layer path** — fixed (O8): 3L/4L now reach 22/39 inside budget, one
   net better than 2L. Extra layers cost ~1.5× per pass, so they buy far
   less than the fine-pitch wall costs.
3. **Passive clustering** — auto-place pulls 2–4 pad parts toward fixed IC
   pads; still not as tight as a human decoupling ring.

## Code changes from this stress pass

- **v7 pass:** global escape-slot assignment in `pcb-router/src/slots.rs`
  (per-side candidate lattice, min-cost max-cardinality matching, exit-lane
  pricing, stranded-pad reporting in text/report/hints); whole-board grid
  (`compute_region` grows to cover every pad, margin stamped unroutable);
  `escape_axis` + outline-aware surface probe (edge-mounted parts fan out);
  dogbone stubs live inside the fanout plan and travel with the best board;
  rip-and-reassign lever between RR&R passes; flood-fill escape autopsy
  (`src/diag.rs`, test-only); probe prints the script's connectivity/DRC
  headline from the same `pcb_drc` source of truth
- `confirm-lib` / `list-pending` / `discard-pending`
- `list-lib` + placement FAIL text
- `edge-place REF left|right|top|bottom [along=N]`
- **Router:** 2-layer dogbone fanout; `max_seconds` + progress; scaled A*
  pop cap; skip organic when budget hit
- **Placer:** pull passives toward net anchors after SA
- **v6 pass:** PathFinder-style negotiated congestion in
  `pcb-router/src/negotiate.rs` (`route negotiate=true`) — sharing-aware
  A* mode, per-cell history, conflict-driven rip-up, legal extraction,
  corridor autopsy in the route hints; regression tests for the
  order-dependence it fixes and for determinism
- **v5 pass:** `RuleArea` + one shared clearance resolver in `pcb-core`
  (router grid, fanout, organic and DRC all read it); pairwise per-net
  clearance in the search (a net now honours its neighbour's stricter
  class, and a rule area overrides both); `rule-area` /
  `rule-area-around` / `list-rule-areas` / `rule-area-remove` /
  `fab-rules jlcpcb-2l|jlcpcb-4l` script verbs; `RuleBelowFabLimit`
  warning; a via-in-pad must fit inside its pad; fanout barrels satisfy
  the fab's annular ring
- **v4 pass:** true dogbones (copper stub laid with the via, depth+stub
  chosen together); rank-staggered fanout along each package side; no body
  keep-out under fine-pitch SMD (< 0.5 mm pitch); board-truthful route/drc
  text (final copper counts, per-net failures, hints, budget flag); no
  panic when the budget expires before the first pass; regression tests
  (`pcb-router/tests/fine_pitch_fanout.rs`)
