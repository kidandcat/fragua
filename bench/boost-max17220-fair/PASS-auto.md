# PASS auto — fair MAX17220 WLP-6 boost island

Pure script run (`bench/boost-max17220-fair/script.txt`). **No hand place/trace.**
Density **N** courtyards (not L).

- ERC errors: **0** | DRC errors: **0** | Routed: **6/6**
- Route: 207 traces, 4 vias, 48.6 mm copper, 17 ms (1 necked at escapes)
- After pour/stitch: 8 vias | DRC 0 errors, 0 warnings
- Placement (`auto-place seed=42`, U3 anchored at 9,7):
  - L2 10.80,7.20 r=90 (LX/VSTOR face)
  - C8 10.77,10.82 · C9 5.67,10.82 · C11 8.80,11.82
  - R15 8.80,3.68 · R16 10.77,3.68 (off L2 body)

Repro: `go test ./internal/script -run TestFairMAX17220WLPBoostRoutes`
or `go run bench/boost-max17220-fair/dump.go`
