# fragua — Architecture

This document maps [VISION.md](VISION.md) onto the Go tree and the
in-process data flow (v1.2+).

## Process model

One process. One static binary (`cmd/fragua`). Inside it:

- A **shared in-memory project** (`internal/core.Project`) — single source of
  truth for the schematic, board, nets, design rules, stackup, and routing
  state.
- A **local HTTP API** on `127.0.0.1:7878` (override: `FRAGUA_API_ADDR`) —
  agents and humans run script verbs through `POST /script`. Also: `GET /`
  and `GET /help` (usage + script reference), `GET /health`, `GET /screenshot`,
  `GET /ui/`, `POST /save`.
- An **embedded browser UI** (`internal/host/ui`) — SVG of the live board,
  script box, DRC/route buttons. The agent and the human edit the same
  `Project`. Every mutation emits a change event consumed by the UI.

CLI: **`fragua run [file.fragua]`** starts the server and opens the browser.
Bare `fragua` (or `fragua help`) prints usage + the full script reference
and exits so agents can discover the surface without starting the server.

## Layout

```
pcb/   (repo: mentasystems/fragua)
├── go.mod
├── cmd/
│   ├── fragua/            HTTP host + embedded UI
│   ├── board-regen/       keep-place, Go-route, JLCPCB pack
│   ├── drc-print/
│   ├── fab-dump/
│   └── fecha-regen/
├── internal/
│   ├── core/              project, board, schematic, library, geometry (nm)
│   ├── script/            DSL parse + tool dispatch + help text
│   ├── router/            Theta*, fanout/slots, RR&R, negotiate, organic, stitch
│   ├── placer/            SA legalisation + decoupling ring + edge snap
│   ├── drc/  erc/
│   ├── fab/  gerber/  odb/
│   ├── render/            board SVG
│   └── host/              HTTP API + UI
├── scripts/install.sh
├── stress/                RP2040 open-hardware stress campaign
├── docs/                  GitHub Pages landing
├── VISION.md
├── ARCHITECTURE.md
└── README.md
```

## Package responsibilities

### `internal/core`
The model. Units are `Length(i64)` nanometres. Owns `Project`, `Board`,
`Schematic`, library, rule areas, fab floors (`FabRules` — JLCPCB 2L/4L),
stackup (`Default2Layer` / `Default4Layer`: F / GND / +3V3 / B), pours,
and the event bus. File I/O is JSON (`.fragua` / legacy `.json`).

### `internal/router`
Auto-routing. Stages: fab ceiling → QFN escape-slot matching + leftover
far-ring → Prim/Theta* tree → RR&R (both-or-neither) → negotiate leftovers
→ organic string-pull → pour stitch. Via-in-pad is an explicit exception
(`escape via-in-pad REF.PAD`), not the default.

### `internal/placer`
Simulated annealing legalisation, decoupling-ring seating for passives,
edge-mount snap. Deterministic for a fixed seed.

### `internal/drc` / `internal/erc`
Geometric DRC (clearance, drill, edge, net split, unconnected pads) and
schematic ERC (floating pins, drivers, power rails) plus heuristics.

### `internal/fab` + `internal/gerber` + `internal/odb`
`pack fab=jlcpcb` writes Gerber + Excellon + BOM/CPL zip. DRC for the pack
uses the board's `FabRules` (JLCPCB mins). ODB++ is a subset writer.

### `internal/script`
One verb per line. `internal/script/usage.go` is the agent-facing
reference (also `GET /` and `fragua help`).

### `internal/host`
`cmd/fragua` embeds `ui/` and serves it at `/ui/`. Loopback only.

## Data flow

```
agent  --POST /script-->  script.RunScript  -->  core.Project
human  --browser /ui/-->  same
                              |
                              +--> router / placer / drc / fab
                              +--> events --> UI refresh
                              +--> gerber.WriteFabPack --> zip
```

One writer process per board file. No database.
