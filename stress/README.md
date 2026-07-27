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
| DRC | 3E | 2E | 7E (marginal clearance at the QFN, 0.125–0.175 vs 0.2 mm) |

**Plateau evidence:** `max_seconds=600` returns the exact same 18 failed nets as
180 s — the remaining wall is algorithmic (escape congestion around U1), not
compute time. **4-layer probe:** adding In1/In2 currently *regresses* to 1/39
and overruns the budget (fine-escape path + 4-layer grid starve the search) —
the 4L path needs its own fix before it can be the answer.

### What works

- End-to-end agent loop: libs → confirm → schematic → place → route → screenshot
- Complex multi-pin ICs and many passives
- GND pour + net classes
- `edge-place REF side`
- **`route max_seconds=N`** returns on time; progress in the activity log
- **2-layer dogbone fanout** for pads too small for via-in-pad (QFN 0.4 mm)

### Still hard

1. **QFN-56 full connectivity on 2 layers** — dogbone stubs + no fine-pitch
   body moat took us 5→21/39, but 18 nets (QSPI, USB, SWD, XIN/RUN, some
   GPIOs, +1V1, VBUS) plateau regardless of budget. Next levers: reduced
   clearance class around fine-pitch, smarter rip-up of the +3V3 ring that
   hogs the escape corridors, more dogbone depth slots.
2. **4-layer path** — `layer add` works, but routing on 4L currently
   regresses badly (1/39) and blows the time budget; fine-escape + 4-layer
   grid interaction needs work before 4L can rescue fine-pitch.
3. **Passive clustering** — auto-place pulls 2–4 pad parts toward fixed IC
   pads; still not as tight as a human decoupling ring.

## Code changes from this stress pass

- `confirm-lib` / `list-pending` / `discard-pending`
- `list-lib` + placement FAIL text
- `edge-place REF left|right|top|bottom [along=N]`
- **Router:** 2-layer dogbone fanout; `max_seconds` + progress; scaled A*
  pop cap; skip organic when budget hit
- **Placer:** pull passives toward net anchors after SA
- **v4 pass:** true dogbones (copper stub laid with the via, depth+stub
  chosen together); rank-staggered fanout along each package side; no body
  keep-out under fine-pitch SMD (< 0.5 mm pitch); board-truthful route/drc
  text (final copper counts, per-net failures, hints, budget flag); no
  panic when the budget expires before the first pass; regression tests
  (`pcb-router/tests/fine_pitch_fanout.rs`)
