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
| `rp2040-minimal.fragua` | Live project (schematic + placement + partial route) |
| `01_schematic.fragua.txt` | Script that builds libs + schematic |
| `rp2040-minimal-v2.png` | Latest board screenshot |
| `0*_*.txt` | API session logs |

## How to drive

```sh
# From repo root (release binary)
./target/release/fragua run stress/rp2040-minimal.fragua

# Agent loop
curl -s http://127.0.0.1:7878/script \
  -H 'content-type: application/json' \
  -d '{"script":"status\nview\nroute"}'
```

## Results (2026-07-27 stress session)

| Stage | Result |
|-------|--------|
| Library (9 new footprints) | OK via `lib` + **`confirm-lib`** |
| Schematic | 36 symbols, 39 nets, **ERC 0 errors** |
| Placement | 36 footprints; **`edge-place`** fixed USB/headers |
| Route (default, ~6 min wall) | **62 traces / 3 vias**, only **~6/39 nets fully connected** |
| DRC after route | **3 errors**, 107 warnings |

### What works

- End-to-end agent loop: libs → confirm → schematic → place → route → screenshot
- Complex multi-pin ICs and many passives on one board
- GND pour + net classes
- Edge connectors with the new `edge-place REF side` verb

### Gaps exposed (to improve next)

1. **Fine-pitch QFN fanout (0.4 mm)** — 2-layer router rarely escapes RP2040
   pads; QSPI / USB / crystal nets often fail. Escape pre-pass is a no-op on
   2-layer boards (`pcb-router` escape module).
2. **Route wall-clock** — full RR&R on this board can take **5–10+ minutes**
   with no agent-visible progress or time budget; HTTP client timeouts leave
   the server still burning CPU.
3. **`list-lib` / placement failures** — text/plain API previously hid keys and
   place errors (fixed this session).
4. **Pending library confirm** — was UI-only; agents could not finish a new
   footprint without a human click (fixed: `confirm-lib` / `list-pending`).
5. **Edge placement math** — hand-tuned `place X Y` for edge-mounted parts
   routinely failed bbox/side checks (fixed: `edge-place`).

## Fragua code changes from this stress pass

- `confirm-lib KEY`, `list-pending`, `discard-pending KEY`
- `list-lib` lists keys in the text reply
- `placement.batch` prints per-item FAIL reasons in text
- `edge-place REF left|right|top|bottom [along=N]`
