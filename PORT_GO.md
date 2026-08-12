# Fragua → Go port plan

**Goal:** replace the Rust/Tauri stack with a pure Go binary + local HTTP API + browser UI.  
**Why:** one language (Go), trivial cross-compile distribution, no Tauri/WebKit host, same agent-first product.

**Oracle:** keep the Rust crates in-tree until Go is green and stress parity is measured; then delete.

---

## Target architecture

```
fragua (Go module: github.com/mentasystems/fragua)
├── cmd/fragua/              CLI: help | run [file.fragua]
├── internal/
│   ├── core/                Project, Board, Schematic, Library, units (nm i64), geometry
│   ├── drc/                 Geometric design rule check
│   ├── erc/                 Electrical rule check
│   ├── gerber/              RS-274X + Excellon + BOM/CPL
│   ├── fab/                 JLCPCB / PCBWay / Generic pack
│   ├── odb/                 ODB++ .tgz (optional phase)
│   ├── placer/              ePlace + SA + edge-plan
│   ├── router/              grid A*/Theta* + RR&R + fanout + organic (+ topo later)
│   ├── routertune/          GA/random search (later)
│   ├── render/              Board/schematic → SVG (+ PNG via oksvg/resvg or go-cairo)
│   ├── script/              Line-oriented DSL + dispatch
│   └── host/                HTTP API + static frontend + event stream
└── frontend/                Existing Vite TS UI; Tauri invoke → HTTP/WS
```

**Process model (unchanged product-wise):**

1. One process, one in-memory `Project` (`sync.RWMutex`).
2. HTTP on `127.0.0.1:7878` (`FRAGUA_API_ADDR`): `GET /`, `/help`, `/health`, `/screenshot`, `POST /script`, `POST /save`, plus `GET /events` (SSE) for UI.
3. Browser opens automatically on `run`; no Tauri.

---

## Phases & parity gates

| Phase | Packages | Gate (must be green) |
|-------|----------|----------------------|
| **0** | Scaffold + host stub | `go build ./...`, `fragua help` prints usage |
| **1** | `core` | Unit tests for Length/Point/Rect; load `stress/rp2040-minimal.fragua`; round-trip save JSON keys |
| **2** | `drc` + `erc` | Ported `pcb-drc` / `pcb-erc` tests; same error/warning counts on synthetic boards |
| **3** | `gerber` + `fab` | Fab pack file list + well-formed headers; zip pack |
| **4** | `render` | SVG board render non-empty; screenshot endpoint returns PNG or SVG fallback |
| **5** | `placer` | HPWL reduction tests with fixed seed; edge-plan tests |
| **6** | `router` | Simple route tests; multi-layer; per-net clearance; 0 failed on trivial boards |
| **7** | `script` | Verb surface: lib/sym/net/place/route/drc/erc/pack/outline…; layer round-trip byte-stable |
| **8** | `host` + frontend | Full HTTP API; open browser; agent script E2E on RP2040 stress |
| **9** | Parity harness | Diff Rust oracle vs Go on stress scripts (levels A–D below) |
| **10** | Cut over | Go is default install; Rust crates archived/removed; CI `go test ./...` |

### Parity levels (empirical)

| Level | Compare | Required for cut-over |
|-------|---------|------------------------|
| A | ERC/DRC counts + kind codes | yes |
| B | Net connectivity (split / fully connected) | yes |
| C | Outline, stackup, footprint refs + pos (nm) | yes |
| D | Gerber/Excellon normalized (stable sort) | yes |
| E | Copper geometry (grid hash / tolerance) | nice-to-have |
| F | Byte-identical traces vs Rust | **not required** |

Determinism in Go: never iterate `map` for ordered output; use sorted keys / `slices.Sort`. Placer seeds must match Rust semantics where tests pin them.

---

## Mapping Rust → Go

| Rust crate | Go package | Priority |
|------------|------------|----------|
| `pcb-core` | `internal/core` | P0 |
| `pcb-drc` | `internal/drc` | P0 |
| `pcb-erc` | `internal/erc` | P0 |
| `pcb-gerber` | `internal/gerber` | P0 |
| `pcb-fab` | `internal/fab` | P0 |
| `pcb-render` | `internal/render` | P1 |
| `pcb-placer` | `internal/placer` | P1 |
| `pcb-router` | `internal/router` | P1 |
| `pcb-script` | `internal/script` | P1 |
| `src-tauri` HTTP | `internal/host` + `cmd/fragua` | P1 |
| `pcb-odb` | `internal/odb` | P2 |
| `pcb-router-tune` | `internal/routertune` | P2 |
| Tauri UI shell | drop | — |
| `frontend/` | keep; replace `@tauri-apps` with fetch/SSE | P1 |

---

## Non-goals (v1 Go)

- Pixel-perfect PNG vs resvg.
- Topological router engine (`engine=topo`) — stub or port later.
- Byte-identical copper vs Rust.
- MCP server (already dropped in Rust).

---

## Distribution

```sh
GOOS=darwin GOARCH=arm64 go build -o fragua ./cmd/fragua
GOOS=linux  GOARCH=amd64 go build -o fragua-linux ./cmd/fragua
# embed frontend dist via //go:embed
```

Install script points at Go release artifacts (same one-liner UX).

---

## Execution order for agents

1. **Scaffold** (orchestrator): `go.mod`, package stubs, `PORT_GO.md`, host skeleton.
2. **Agent Core**: port units/geometry/board/schematic/project/library/rules.
3. **Agent Checks**: drc + erc (depends on core types).
4. **Agent Fab**: gerber + fab.
5. **Agent PlaceRoute**: placer + router (largest).
6. **Agent ScriptHost**: script DSL + HTTP host + wire frontend.
7. **Orchestrator**: integrate, fix compile, run tests, parity harness, install binary.

All code/comments/commits in **English**. Product docs can stay bilingual where they already are.

---

## Status (2026-08-12)

**Phase 0–8 skeleton is live.** `go test ./...` green. Binary: `go build -o fragua ./cmd/fragua`.

| Area | State |
|------|--------|
| `internal/core` | units, geometry, board, schematic, project load/save `.fragua`, library disk store, events |
| `internal/drc` | copper-graph connectivity, clearances, edge, body-off-board |
| `internal/erc` | floating/empty/roles/heuristics |
| `internal/gerber` + `fab` | RS-274X pack + zip providers |
| `internal/placer` | force + SA with seed |
| `internal/router` | multi-source grid A*, vias, adaptive cell, own-net walkable |
| `internal/script` | outline/sym/lib/palette/place/route/drc/erc/pack/trace/via/rule-area/layer/… |
| `internal/host` | HTTP API + SSE + browser UI (`frontend/dist`) |
| `internal/render` | board SVG |
| `internal/odb` | minimal tgz skeleton |
| Rust crates | still in tree as oracle; not removed yet |

### Still open (parity levels E / full Rust depth)

- Fine-pitch fanout / organic post-pass / topo engine
- Full compact verb
- PNG screenshot (SVG works)
- Frontend: replace remaining Tauri `invoke` with HTTP (minimal Go UI is at `frontend/dist/index.html`)
- Byte-identical copper vs Rust (**not a cut-over requirement**)
- Differential harness Rust↔Go automated in CI

Smoke: `./scripts/parity-smoke.sh`
