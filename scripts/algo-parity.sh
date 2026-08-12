#!/usr/bin/env bash
# Differential algorithmic parity: Rust oracle vs Go dump.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
PROJ="${1:-stress/rp2040-minimal.fragua}"
OUTDIR="${TMPDIR:-/tmp}/fragua-parity-$$"
mkdir -p "$OUTDIR"

echo "== building oracles =="
cargo build -p pcb-oracle --release 2>&1 | tail -5
go build -o "$OUTDIR/parity-dump" ./cmd/parity-dump

echo "== rust dump =="
./target/release/pcb-oracle "$PROJ" --out "$OUTDIR/rust.json"
echo "== go dump =="
"$OUTDIR/parity-dump" "$PROJ" --out "$OUTDIR/go.json"

python3 - <<'PY' "$OUTDIR/rust.json" "$OUTDIR/go.json"
import json, sys
from pathlib import Path
r=json.loads(Path(sys.argv[1]).read_text())
g=json.loads(Path(sys.argv[2]).read_text())

def cmp_section(name, a, b):
    print(f"\n## {name}")
    ok=True
    for k in ("errors","warnings"):
        ra, ga = a.get(k), b.get(k)
        mark = "OK" if ra==ga else "DIFF"
        if ra!=ga: ok=False
        print(f"  {k}: rust={ra} go={ga} [{mark}]")
    rb, gb = a.get("by_kind",{}), b.get("by_kind",{})
    keys=sorted(set(rb)|set(gb))
    for k in keys:
        rv, gv = rb.get(k,0), gb.get(k,0)
        if rv!=gv:
            ok=False
            print(f"  kind {k}: rust={rv} go={gv} [DIFF]")
    if ok:
        print("  by_kind: match")
    # geometry
    return ok

print("geometry rust", r["geometry"])
print("geometry go  ", g["geometry"])
geo_ok = r["geometry"]==g["geometry"]
print("geometry", "OK" if geo_ok else "DIFF")

drc_ok = cmp_section("DRC", r["drc"], g["drc"])
erc_ok = cmp_section("ERC", r["erc"], g["erc"])
ch_ok = r.get("copper_hash")==g.get("copper_hash")
print(f"\ncopper_hash match: {ch_ok}")
if not ch_ok:
    print("  rust", (r.get("copper_hash") or "")[:16]+"...")
    print("  go  ", (g.get("copper_hash") or "")[:16]+"...")

all_ok = geo_ok and drc_ok and erc_ok and ch_ok
print("\n== RESULT:", "PASS" if all_ok else "FAIL (expected until algorithms match)", "==")
sys.exit(0 if all_ok else 1)
PY
