# NOTES-auto — pure auto MAX17220 boost (master after PRs #33–#37)

**Hand traces:** none (script verbs only: `place U3`, `auto-place seed=42`, `route`, `auto-pour`, `stitch`).

## Perfect checklist

| # | Criterion | Result |
|---|-----------|--------|
| 1 | Courtyard visual ≥0.3 mm (density N) | **PASS** min gap **0.323 mm** (L2-Rsel) |
| 2 | LX: 0 vias, short top path | **PASS** LX vias=**0**, pad LX→L = **2.433 mm** (Top-only; courtyard-limited vs ~2 mm ideal) |
| 3 | L–IN direct, Cin in loop, Rsel not in power | **PASS** IN Top-routed; Cin on IN/GND; Rsel on SEL/EN |
| 4 | HF Cout closer than bulk | **N/A** single Cout on OUT/GND (pad **2.126 mm**) |
| 5 | GND vias ≤~1.5 mm of U3.GND / Cin / Cout | **PASS** U3 **0.700**, Cin **0.000**, Cout **0.000** |
| 6 | No foreign net under inductor body | **PASS** |
| 7 | ERC 0 errors, DRC 0 errors | **PASS** (ERC warnings only: unpowered IN) |
| 8 | Script verbs only | **PASS** |

## Counts
- Routed **6/6**, traces 19, vias 8 (GND stitch 4, no LX vias)
- Outline 22×16 mm, fab-rules jlcpcb-2l

## Placement (mm)
| Ref | Pos | Rot |
|-----|-----|-----|
| U3 | 11.0, 8.0 | 0 |
| L2 | 14.403, 8.348 | 270 |
| Cin | 14.95, 5.485 | 0 |
| Cout | 7.05, 8.95 | 180 |
| Rsel | 14.95, 11.13 | 0 |
| Cen | 7.05, 5.485 | 180 |

## Artifacts
- `/Users/jairo/pcb/bench/boost-max17220/boost-auto.fragua`
- `/Users/jairo/pcb/bench/boost-max17220/fragua-route-auto.png`
- `/Users/jairo/pcb/bench/boost-max17220/fragua-route-auto.svg`
- `/Users/jairo/pcb/bench/boost-max17220/NOTES-auto.md`
- `/Users/jairo/pcb/bench/boost-max17220/PASS-auto.md`

## Product fixes that got us here
- #33 park SA passives + LX/IN inductor bridge seat
- #34 island seat ignores 2 mm SA solder floor
- #35 0.55 mm seat floor + outward stitch dogbone
- #36/#37 LX-tight experiment reverted (kept courtyard)
