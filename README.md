# fragua

[![CI](https://github.com/mentasystems/fragua/actions/workflows/ci.yml/badge.svg)](https://github.com/mentasystems/fragua/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-d6905b.svg)](LICENSE)
[![Landing](https://img.shields.io/badge/landing-mentasystems.com%2Ffragua-d6905b)](https://mentasystems.com/fragua)
[![Release](https://img.shields.io/github/v/release/mentasystems/fragua?color=d6905b)](https://github.com/mentasystems/fragua/releases/latest)

AI-native PCB design tool. The agent does the work, the human watches and steers.

## Go rewrite (primary going forward)

The product is being rewritten in **Go** (see [`PORT_GO.md`](PORT_GO.md)): single static binary,
local HTTP API, browser UI — no Tauri/WebKit host. Rust crates remain as a temporary oracle.

```sh
go test ./...
go build -o fragua ./cmd/fragua
./fragua run path/to/board.fragua
# API http://127.0.0.1:7878  (FRAGUA_API_ADDR, FRAGUA_NO_BROWSER)
```

Cross-compile: `GOOS=linux GOARCH=amd64 go build -o fragua-linux ./cmd/fragua`.

Packages: `cmd/fragua`, `internal/{core,drc,erc,gerber,fab,placer,router,render,script,host,odb}`.


- 🌐 Landing: <https://mentasystems.com/fragua>
- 🧭 [VISION.md](VISION.md) — what we are building and why
- 🏗️ [ARCHITECTURE.md](ARCHITECTURE.md) — the stack and crate layout
- 🤝 [CONTRIBUTING.md](CONTRIBUTING.md) — how to help

## Status

**v1.1.x** — end-to-end agent loop, schematic → board → fab-ready zip (and ODB++ from the UI):

- `pcb-core`: project model (schematic, board, library, pours, rule areas,
  stackup, cutouts, NPTH holes), nm fixed-point geometry, tokio broadcast
  event bus, JSON persistence (`.fragua`; legacy `.json` still loads).
- `pcb-script`: line-oriented agent DSL — `lib`, `sym`, `net`, `class`,
  `palette`, `place`, `auto-place`, `edge-mount`, `edge-place`, `edge-plan`,
  `elevated`, `route`, `compact`, `rule-area`, `fab-rules`, `erc`, `drc`,
  `auto-pour`, `pack`, plus polygonal outlines / milled cutouts / mount holes.
  Full reference at app launch and `GET /` / `GET /help`.
- `pcb-router`: two engines. Default: Theta\* any-angle search on a multi-layer
  grid + rip-up-and-reroute + optional PathFinder negotiation + Steiner-ish
  multi-source, with fine-pitch escape-slot matching, reachability-proved
  fanout, and an organic post-pass (string-pull + arc fillets). Experimental
  (`route engine=topo`): topological homotopy A\* over a Delaunay dual.
  Honours per-net `NetClass` and design-rule areas for width / clearance.
- `pcb-placer`: two stages. Electrostatic global placement (ePlace-style) then
  simulated-annealing legalisation (hand-solder gap floor + congestion proxy +
  bundle crossings). Edge planning for edge-mounted parts, decoupling-ring
  seating for passives, elevated-body awareness. Deterministic for a fixed seed.
- `pcb-drc` / `pcb-erc`: geometric DRC (clearance, drill, edge, body-off-board,
  elevated overlap, cutout/hole voids, castellated pads) and schematic ERC
  (floating pins, drivers, power rails) plus heuristics (decoupling caps, I²C
  pull-ups).
- `pcb-fab` + `pcb-gerber` + `pcb-odb`: JLCPCB / PCBWay / Generic pack (Gerber +
  Excellon + BOM/CPL zip) and ODB++ `.tgz` (UI export; industry interchange).
- `pcb-render`: Board + schematic → SVG/PNG (substrate, copper, silk, DRC
  markers; poly outlines and cutouts).
- `pcb-router-tune`: GA / random search over router genes (cell, via cost,
  clearance, rotations) — CLI + in-app Auto Routing.
- `src-tauri` + `frontend`: Tauri 2 shell. Local HTTP API on `127.0.0.1:7878`
  (override with `FRAGUA_API_ADDR`). Frontend pans/zooms the live SVG and
  surfaces the activity log.

`cargo test --workspace` is green. Stress campaign notes: [`stress/`](stress/).

## Install

One-liner (macOS arm64/x64, Linux x64):

```sh
curl -fsSL https://raw.githubusercontent.com/mentasystems/fragua/master/scripts/install.sh | sh
```

Drops the `fragua` binary in `/usr/local/bin` (or `~/.local/bin` if it
can't write there). Windows users: grab `fragua-<ver>-windows-x64.zip`
from the [releases page](https://github.com/mentasystems/fragua/releases/latest).

Then tell your AI to design the hardware with the `fragua` CLI — it launches
the window, exposes the HTTP script API, and the agent drives the rest.

## Run it

The launch subcommand is **`run`**. Bare `fragua` (or `fragua help`) prints
the usage + full script reference and exits — so agents can discover the
surface before starting the server.

```sh
# Build the frontend bundle once (release build embeds it).
npm --prefix frontend install
npm --prefix frontend run build

# Launch empty in-memory project.
cargo run --release --bin fragua -- run

# …or open an existing project (autosave bound to that path):
cargo run --release --bin fragua -- run /path/to/project.fragua

# Installed binary:
fragua run
fragua run /path/to/project.fragua
```

The window opens and the local HTTP API starts on `127.0.0.1:7878`
(override: `FRAGUA_API_ADDR`).

## Drive it from an agent

Stateless HTTP — every request is independent. From any tool that can
make HTTP calls (Claude Code, GPT, a shell loop):

```sh
# Discover the full action surface (usage + script reference).
curl -s http://127.0.0.1:7878/
# same: curl -s http://127.0.0.1:7878/help

# Liveness.
curl -s http://127.0.0.1:7878/health

# Run a multi-line script.
curl -s http://127.0.0.1:7878/script \
  -H 'content-type: application/json' \
  -d '{"script": "outline 80 30 radius=2\nstatus"}'

# Headless PNG of the live board (or schematic).
curl -s 'http://127.0.0.1:7878/screenshot?view=board&width=1600' -o board.png

# Persist when launched without a file argument.
curl -s http://127.0.0.1:7878/save \
  -H 'content-type: application/json' \
  -d '{"path": "/tmp/board.fragua"}'
```

| Method | Path | Purpose |
|--------|------|---------|
| `GET` | `/`, `/help` | Usage + full script reference |
| `GET` | `/health` | `ok` |
| `GET` | `/screenshot` | PNG (`view=board\|schematic`, `width=`) |
| `POST` | `/script` | Multi-line script body `{"script":"..."}` |
| `POST` | `/save` | Atomic write + bind autosave `{"path":"..."}` |

Replies are `text/plain`: per-line outcomes in the form
`[L<n> ok|FAIL <tool>] <text>`, plus a warning when the session is
memory-only.

## End-to-end recipe

```text
class ground pour=both
class power width=0.4

sym U1 ic key=esp32_s3_zero
  pin 1 L 3V3 role=power_in
  pin 2 L GND role=power_in
  ...
sym C1 capacitor key=c_0603 lcsc=C14663
sym R1 resistor key=r_0603 lcsc=C25804

net GND  U1.GND C1.2 R1.2 class=ground
net +3V3 U1.3V3 C1.1 class=power

erc

palette U1 esp32_s3_zero
palette C1 c_0603 value=100nF
palette R1 r_0603 value=10k
place U1 25 15
place C1 35 15
place R1 35 25

auto-place R1 C1 seed=42
route
# optional once clean: compact allow_failed=0 route_seconds=90
pack fab=jlcpcb out=/tmp
```

The final line writes `/tmp/<project>-jlcpcb.zip` ready to upload.
Recommended pipeline: ERC → power planes / classes → place → auto-place →
route → (compact) → pack.
