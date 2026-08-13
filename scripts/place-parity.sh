#!/usr/bin/env bash
# Compare Rust vs Go SA-only place (seed=42, no global / edge / decouple).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
BOARD="${1:-$ROOT/stress/two-resistors.fragua}"
OUT="${PARITY_OUT:-/tmp/fragua-place-parity}"
mkdir -p "$OUT"
test -x target/release/pcb-oracle || cargo build -p pcb-oracle --release
go build -o "$OUT/parity-dump" ./cmd/parity-dump
name=$(basename "$BOARD")
./target/release/pcb-oracle "$BOARD" --place --out "$OUT/${name}.rust.json"
"$OUT/parity-dump" "$BOARD" --place --out "$OUT/${name}.go.json"
python3 - "$OUT/${name}.rust.json" "$OUT/${name}.go.json" <<'PY'
import json, sys
r, g = json.load(open(sys.argv[1])), json.load(open(sys.argv[2]))
rp, gp = r.get("place") or {}, g.get("place") or {}
print(f"HPWL rust {rp.get('initial_hpwl_mm'):.4f} → {rp.get('final_hpwl_mm'):.4f}")
print(f"     go   {gp.get('initial_hpwl_mm'):.4f} → {gp.get('final_hpwl_mm'):.4f}")
rt, gt = rp.get("positions") or {}, gp.get("positions") or {}
refs = sorted(set(rt) | set(gt))
nm_ok = True
max_nm = 0
for ref in refs:
    a, b = rt.get(ref), gt.get(ref)
    if a is None or b is None:
        print(f"  {ref}: MISSING rust={a} go={b}")
        nm_ok = False
        continue
    dx = abs(a[0] - b[0]) * 1e6
    dy = abs(a[1] - b[1]) * 1e6
    dr = abs(a[2] - b[2])
    max_nm = max(max_nm, dx, dy)
    mark = "OK" if dx < 1 and dy < 1 and dr < 1e-6 else "DIFF"
    if mark != "OK":
        nm_ok = False
    print(f"  {ref}: rust ({a[0]:.6f},{a[1]:.6f},{a[2]:.1f}) go ({b[0]:.6f},{b[1]:.6f},{b[2]:.1f}) Δnm=({dx:.0f},{dy:.0f}) {mark}")
print(f"max |Δ| nm: {max_nm:.0f}")
init_ok = abs((rp.get("initial_hpwl_mm") or 0) - (gp.get("initial_hpwl_mm") or 0)) < 1e-6
print("RESULT:", "PASS" if nm_ok and init_ok else "PARTIAL" if init_ok else "FAIL")
sys.exit(0 if nm_ok else 1)
PY
