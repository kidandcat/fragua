# Fragua stress: RP2040 Minimal (open hardware)

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

| Stage | v1 (first pass) | v3 (after router/placer fixes) |
|-------|-----------------|--------------------------------|
| Library | OK via `confirm-lib` | same |
| Schematic | ERC 0 | same |
| Placement | 36 fps, `edge-place` | same + passive pull-to-anchor |
| Route wall | **~6 min**, no budget | **~90 s** with `max_seconds=90` |
| Copper | 62 traces / 3 vias | **402 traces / 41 vias** (dogbone fanout) |
| Fully connected nets | ~6/39 | ~5/39 (still the QFN wall) |
| DRC | 3E | 2E |

### What works

- End-to-end agent loop: libs → confirm → schematic → place → route → screenshot
- Complex multi-pin ICs and many passives
- GND pour + net classes
- `edge-place REF side`
- **`route max_seconds=N`** returns on time; progress in the activity log
- **2-layer dogbone fanout** for pads too small for via-in-pad (QFN 0.4 mm)

### Still hard

1. **QFN-56 full connectivity on 2 layers** — dogbones open Bottom, but
   escape density + body keep-out still leave QSPI / USB / many GPIOs
   unrouted. Next levers: thinner body keep-out moat, 4-layer default for
   fine-pitch, or module-style RP2040 footprint.
2. **Passive clustering** — auto-place pulls 2–4 pad parts toward fixed IC
   pads; still not as tight as a human decoupling ring.

## Code changes from this stress pass

- `confirm-lib` / `list-pending` / `discard-pending`
- `list-lib` + placement FAIL text
- `edge-place REF left|right|top|bottom [along=N]`
- **Router:** 2-layer dogbone fanout; `max_seconds` + progress; scaled A*
  pop cap; skip organic when budget hit
- **Placer:** pull passives toward net anchors after SA
