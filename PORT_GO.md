# Fragua → Go: algorithmic parity plan

## Contract (authoritative)

**Not required:** 1:1 source translation of Rust code, same file layout, same type names, or same internal data structures.

**Required for cut-over:** **1:1 parity of algorithms, processes, and outputs.**

That means:

| Layer | Meaning |
|-------|---------|
| **Algorithm** | Same mathematical / graph / search procedure (same stages, same decisions at each stage for fixed inputs + seeds + budgets). Implementation language/style free. |
| **Process** | Same end-to-end pipeline for each script verb / HTTP call: same order of checks, same options defaults, same side-effects on `Project`, same report shape. |
| **Output** | Same observable results: DRC/ERC findings (kind + severity + involved nets/refs; positions within 0.01 mm), connectivity, footprint positions (nm), Gerber/Excellon after normalisation, copper after deterministic route (hash or segment-equal under fixed seed/budget). |

If two engines run the same script on the same project with the same seed and wall-clock budget is not the limiter, they must produce **equivalent** boards and reports. “Looks similar / usually works” is **not** parity.

Rust remains the **oracle** until Go matches. Then Rust is deleted.

---

## What “done” looks like

```text
./scripts/algo-parity.sh stress/rp2040-minimal.fragua   → PASS
./scripts/algo-parity.sh <synthetic suite>              → PASS
go test ./...                                           → PASS
# plus route parity on fixed boards with seed+budget
```

Cut-over only when:

1. **Geometry / load** — same footprint count, positions (nm), stackup, nets after load+round-trip of stable fields.
2. **ERC** — same multiset of `(kind, severity, involved)` (message text may differ until messages are locked).
3. **DRC** — same multiset of `(kind, severity, net/involved, x_mm±0.01, y_mm±0.01)` under default options **and** under fab-rules presets.
4. **Route process** — same stages as Rust default engine: fanout → grid search (Theta*/A*) → RR&R → optional negotiate → organic post-pass → stitch; same default `RouteOptions` (cell 0.25 mm, clearance 0.40 mm, via_cost 8, max_seconds 90).
5. **Route output** — under `max_seconds` large enough that budget is not the limiter, same failed/ok nets and same copper hash (canonical sort of traces/vias).
6. **Place process** — same two stages (global + SA legalisation), same seed derivation, same solder gap floor.
7. **Fab pack** — same file set; Gerber/Excellon equivalent after number normalisation and stable sort.
8. **Script surface** — every verb that exists in Rust `script_reference()` behaves with the same success/failure semantics and project mutations.

---

## Explicitly out of scope for parity

- Tauri window / native menus (Go = HTTP + browser).
- PNG pixel-identical screenshots (SVG/PNG structural content yes; raster engine may differ).
- Wall-clock runtime equality.
- Identical log wording (until we freeze agent-facing strings as part of the process contract).

---

## Measurement (oracle)

| Tool | Role |
|------|------|
| `cargo run -p pcb-oracle --release -- <file.fragua>` | Rust dump: geometry, DRC, ERC, copper_hash |
| `go run ./cmd/parity-dump -- <file.fragua>` | Go dump, **same JSON schema** |
| `./scripts/algo-parity.sh [file]` | Diff dumps; exit 1 on mismatch |

Extend dumps with `--route` for clear+route process compare.

**Current baseline (skeleton Go vs Rust):** expected **FAIL** on DRC/ERC kind counts and copper — that is the gap to close, not a surprise.

---

## Implementation strategy (not 1:1 code)

For each subsystem:

1. **Extract the process** from Rust (stage list, defaults, ordering, seed rules) into a short English SPEC section in this file or `docs/parity/<pkg>.md`.
2. **Reimplement the algorithm in idiomatic Go** against that SPEC (not a line translation).
3. **Lock with oracle tests**: fixture boards → dump must match Rust.
4. Only then move to the next subsystem.

Recommended order (dependency + measurability):

| Order | Subsystem | Parity signal |
|------:|-----------|---------------|
| 1 | `core` load/geometry/rules | geometry + round-trip |
| 2 | `erc` | `by_kind` + findings multiset |
| 3 | `drc` | full `run()` process order as in Rust `pcb_drc::run` |
| 4 | `gerber`/`fab` | normalised file compare |
| 5 | `placer` | seed-fixed positions |
| 6 | `router` default engine | copper hash + ok/failed nets |
| 7 | `script` verbs | process side-effects |
| 8 | organic / fanout / compact / topo | as in Rust default/experimental flags |

---

## Architecture (Go product shape)

Unchanged from product intent: one process, HTTP `127.0.0.1:7878`, browser UI, no Tauri.

```
cmd/fragua          — product binary
cmd/parity-dump     — Go oracle side
crates/pcb-oracle   — Rust oracle side
internal/{core,drc,erc,gerber,fab,placer,router,render,script,host,odb}
```

---

## Status

| Item | State |
|------|--------|
| Skeleton Go runtime | exists (`go test ./...` green) |
| Algorithmic parity | **not achieved** — skeleton ≠ oracle |
| Oracle harness | `pcb-oracle` + `parity-dump` + `algo-parity.sh` |
| Cut-over | blocked until harness PASS on stress + synthetic suite |

### Skeleton defaults to fix early (known process mismatches)

| Setting | Rust | Go skeleton (fix toward) |
|---------|------|---------------------------|
| Router `clearance` | 0.40 mm | was 0.20 — match 0.40 |
| Router `cell` | 0.25 mm | match |
| Router `via_cost` | 8 | match |
| Router `max_seconds` | 90 | match |
| DRC `min_clearance` | 0.20 mm | match |
| Route process | fanout + Theta* + RR&R + organic | simplified A* only — **full process required** |

---

## Agents / workstreams

Parallel work is OK **only with oracle gates**:

- Stream A: ERC process + findings parity  
- Stream B: DRC `run()` stage list parity  
- Stream C: Router default engine process (largest)  
- Stream D: Script verb process parity  

No stream claims “done” without `algo-parity` (or package-level dump compare) green for its fixtures.
