#!/usr/bin/env bash
# Smoke parity: load stress board in Go host, report status/drc/erc.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
BIN="${FRAGUA_BIN:-$ROOT/fragua}"
if [[ ! -x "$BIN" ]]; then
  go build -o "$BIN" ./cmd/fragua
fi
ADDR="${FRAGUA_API_ADDR:-127.0.0.1:17999}"
export FRAGUA_API_ADDR="$ADDR"
export FRAGUA_NO_BROWSER=1
"$BIN" run "$ROOT/stress/rp2040-minimal.fragua" &
PID=$!
cleanup() { kill "$PID" 2>/dev/null || true; }
trap cleanup EXIT
for i in $(seq 1 50); do
  if curl -sf "http://$ADDR/health" >/dev/null; then break; fi
  sleep 0.1
done
echo "=== status ==="
curl -s -X POST "http://$ADDR/script" --data 'status'
echo "=== erc ==="
curl -s -X POST "http://$ADDR/script" --data 'erc'
echo "=== drc ==="
curl -s -X POST "http://$ADDR/script" --data 'drc'
