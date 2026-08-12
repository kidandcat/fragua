#!/usr/bin/env bash
# Compare Rust vs Go `route` process (per-net status + post-route DRC kinds).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
BOARD="${1:-/Users/jairo/drone/pcb/tof-card.fragua}"
OUT="${PARITY_OUT:-/tmp/fragua-route-parity}"
mkdir -p "$OUT"
test -x target/release/pcb-oracle || cargo build -p pcb-oracle --release
go build -o "$OUT/parity-dump" ./cmd/parity-dump
name=$(basename "$BOARD")
./target/release/pcb-oracle "$BOARD" --route --out "$OUT/${name}.rust.json"
"$OUT/parity-dump" "$BOARD" --route --out "$OUT/${name}.go.json"
python3 - "$OUT/${name}.rust.json" "$OUT/${name}.go.json" <<'PY'
import json, sys
r, g = json.load(open(sys.argv[1])), json.load(open(sys.argv[2]))
rr, gg = r["route"], g["route"]
st = rr["per_net"] == gg["per_net"]
print(f"per_net status: {'OK' if st else 'DIFF'}  rust ok/fail={rr['ok']}/{rr['failed']} go={gg['ok']}/{gg['failed']}")
print(f"traces/vias rust {rr['traces']}/{rr['vias']}  go {gg['traces']}/{gg['vias']}")
print(f"length_mm   rust {rr['total_length_mm']:.1f}  go {gg['total_length_mm']:.1f}")
print(f"post-DRC    rust {rr['drc_errors']}E/{rr['drc_warnings']}W {rr.get('drc_by_kind')}")
print(f"            go   {gg['drc_errors']}E/{gg['drc_warnings']}W {gg.get('drc_by_kind')}")
drc_ok = rr.get("drc_by_kind") == gg.get("drc_by_kind") and rr["drc_errors"] == gg["drc_errors"]
print("RESULT:", "PASS" if st and drc_ok else "PARTIAL" if st else "FAIL")
sys.exit(0 if st else 1)
PY
