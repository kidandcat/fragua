# Fair MAX17220 boost island (Hand/Astra BOM)

WLP-6 U3, L2 inductor, C8 Cin, **C9+C11 dual Cout**, R15 EN pull-up, R16 SEL.
No test points. Density **N** courtyards (do not switch the inductor to L).

```
place U3 9 7
auto-place seed=42
route
auto-pour
stitch
drc
```

See `script.txt`. The integration test is `TestFairMAX17220WLPBoostRoutes`.

## What was failing

On master, `place U3` + `auto-place` + `route` left **VSTOR** and/or **SEL** open:

- Fanout required ≥16 pads, so WLP-6 never got dogbones.
- Fine-pitch width ceiling required ≥8 pads, so traces stayed 0.25 mm on 0.24 mm lands.
- `isPowerInNet` missed VSTOR/BATT, so the inductor never seated as an LX–IN bridge.
- Via-in-pad did not refuse a 0.30 mm drill in a 0.24 mm land (no copper).
- A SEL dogbone aimed at R16 landed on the inductor courtyard and **split** the net: the via sat nearer R16 than U3.SEL, so A* marked both pads reached without joining them. R16 could also sit under L2.

## Product fixes

- `lib-gen family=wlp` (0.4 mm pitch, pads ≤0.25 mm, JEDEC A1/A2/B1…).
- Fanout and fab-min traces for packages with ≥6 pads at <0.45 / <0.50 mm pitch.
- VSTOR/BATT seats the inductor; Rsel stays off the ferrite body.
- Via-in-pad only when the barrel fits the land.
- Two-pin signals with a same-layer partner inside 5 mm stay on one layer (no dogbone).
- `existingNetSources` no longer treats the far end of a dogbone stub as already on the tree.

## Result

See `PASS-auto.md`. `go run bench/boost-max17220-fair/dump.go`:

```
ok route: route: 6/6 nets ok (1 necked at escapes), 207 traces, 4 vias, 48.6 mm copper, 17 ms
ok drc: drc: 0 errors, 0 warnings (0 findings)
```

R16 at 10.77,3.68 stays off L2 (10.80,7.20). Courtyards are density N.
