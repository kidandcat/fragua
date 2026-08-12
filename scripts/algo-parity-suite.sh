#!/usr/bin/env bash
# Run Rust↔Go oracle dumps on every board in parity-boards.txt (or args).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
LIST="${PARITY_BOARDS:-$ROOT/scripts/parity-boards.txt}"
OUT="${PARITY_OUT:-/tmp/fragua-board-parity}"
mkdir -p "$OUT"

test -x target/release/pcb-oracle || cargo build -p pcb-oracle --release
go build -o "$OUT/parity-dump" ./cmd/parity-dump

boards=()
if [[ $# -gt 0 ]]; then
  boards=("$@")
else
  while IFS= read -r line || [[ -n "$line" ]]; do
    [[ -z "$line" || "$line" =~ ^# ]] && continue
    boards+=("$line")
  done < "$LIST"
fi

echo "board|geom|drc_e(r/g)|drc_w(r/g)|erc_e(r/g)|erc_w(r/g)|copper|status" | tee "$OUT/summary.tsv"

pass=0
fail=0
for b in "${boards[@]}"; do
  name=$(basename "$b")
  if [[ ! -f "$b" ]]; then
    echo "$name|MISSING||||||FAIL" | tee -a "$OUT/summary.tsv"
    fail=$((fail+1))
    continue
  fi
  rj="$OUT/${name}.rust.json"
  gj="$OUT/${name}.go.json"
  if ! ./target/release/pcb-oracle "$b" --out "$rj" 2>"$OUT/${name}.rust.err"; then
    echo "$name|RUST_FAIL||||||FAIL" | tee -a "$OUT/summary.tsv"
    fail=$((fail+1))
    continue
  fi
  if ! "$OUT/parity-dump" "$b" --out "$gj" 2>"$OUT/${name}.go.err"; then
    echo "$name|GO_FAIL||||||FAIL" | tee -a "$OUT/summary.tsv"
    fail=$((fail+1))
    continue
  fi
  python3 - "$name" "$rj" "$gj" "$OUT/summary.tsv" <<'PY'
import json, sys
name, rj, gj, sumf = sys.argv[1:5]
r, g = json.load(open(rj)), json.load(open(gj))

def geo(x):
    o = x.get("outline_mm")
    if o:
        o = (round(float(o[0]), 3), round(float(o[1]), 3))
    return (x["footprints"], x["traces"], x["vias"], x["pours"], x["nets"], o)

geom_ok = geo(r["geometry"]) == geo(g["geometry"])
drc_ok = r["drc"]["by_kind"] == g["drc"]["by_kind"] and r["drc"]["errors"] == g["drc"]["errors"] and r["drc"]["warnings"] == g["drc"]["warnings"]
erc_ok = r["erc"]["by_kind"] == g["erc"]["by_kind"] and r["erc"]["errors"] == g["erc"]["errors"] and r["erc"]["warnings"] == g["erc"]["warnings"]
cu_ok = r.get("copper_hash") == g.get("copper_hash")
status = "PASS" if (geom_ok and drc_ok and erc_ok and cu_ok) else "FAIL"
line = (
    f"{name}|{'OK' if geom_ok else 'DIFF'}"
    f"|{r['drc']['errors']}/{g['drc']['errors']}"
    f"|{r['drc']['warnings']}/{g['drc']['warnings']}"
    f"|{r['erc']['errors']}/{g['erc']['errors']}"
    f"|{r['erc']['warnings']}/{g['erc']['warnings']}"
    f"|{'OK' if cu_ok else 'DIFF'}|{status}"
)
print(line)
open(sumf, "a").write(line + "\n")
sys.exit(0 if status == "PASS" else 1)
PY
  if [[ $? -eq 0 ]]; then pass=$((pass+1)); else fail=$((fail+1)); fi
done

echo
echo "PASS=$pass FAIL=$fail  dumps=$OUT"
[[ "$fail" -eq 0 ]]
