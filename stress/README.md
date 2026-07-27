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
| `rp2040-minimal-v3.png` | Latest board screenshot |
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

### What works

- End-to-end agent loop: libs → confirm → schematic → place → route → screenshot
- Complex multi-pin ICs and many passives
- GND pour + net classes
- `edge-place REF side`
- **`route max_seconds=N`** returns on time; progress in the activity log
- **2-layer dogbone fanout** for pads too small for via-in-pad (QFN 0.4 mm)

### Still hard

1. **QFN-56 full connectivity on 2 layers** — dogbone stubs + no fine-pitch
   body moat took us 5→21/39, and it plateaus there regardless of budget.
   Rule areas (v5) made the escape *legal* but not *routable*: the wall is
   escape-corridor congestion, so the next levers are PathFinder-style
   negotiated congestion and rip-up that can evict the fat +3V3 ring, not
   another rule.
2. **4-layer path** — fixed (O8): 3L/4L now reach 22/39 inside budget, one
   net better than 2L. Extra layers cost ~1.5× per pass, so they buy far
   less than the fine-pitch wall costs.
3. **Passive clustering** — auto-place pulls 2–4 pad parts toward fixed IC
   pads; still not as tight as a human decoupling ring.

## Code changes from this stress pass

- `confirm-lib` / `list-pending` / `discard-pending`
- `list-lib` + placement FAIL text
- `edge-place REF left|right|top|bottom [along=N]`
- **Router:** 2-layer dogbone fanout; `max_seconds` + progress; scaled A*
  pop cap; skip organic when budget hit
- **Placer:** pull passives toward net anchors after SA
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
