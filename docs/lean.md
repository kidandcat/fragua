# Lean in fragua

Fragua is a Go program today. An earlier Rust core was already ported; the
language it moves to is Lean 4, not a return to Rust. The end state is one
Lean executable and no Go: `cmd/fragua`, the router, the placer, the
renderer, and the HTTP host included. Lean arrives as modules that replace
one package at a time so the Go tree keeps building while a package is in
transit. The hypothesis behind the port is performance. Lean compiles to C
with reference counting and no tracing garbage collector, so a hot loop that
lives in Lean may be faster and allocate less than the same loop in Go.
That is a hypothesis. Phase 0 measures it on the geometry kernel; later
phases re-measure on their own workload. The phase-0 numbers do not show a
speedup (see below).

The first module is exact integer geometry. It is called from trace/trace
clearance in `internal/drc` when the binary is built with `-tags lean`.

## Strangler pattern

Each replacement is a **module** with these rules:

- It replaces one Go package, behind a pure interface the differential test
  can run against the Go code that is about to leave. The Go type that owns
  the board stays in Go only until the packages that import it have moved.
- Its interface is pure where the Go package is pure. Coordinates are
  integers. A package that does I/O (`host`, `gerber`, `cmd`) gets an
  interface that is still deterministic on its bytes: same board in, same
  bytes out.
- Coordinates are nanometres, the unit of `core.Length` (`int64`). Lean computes
  them as `Int`, which is unbounded, so a difference or a product of two
  `int64` endpoints cannot wrap.
- The module has an empty Lake dependency set. No Mathlib. The toolchain is
  pinned in `lean/lean-toolchain` (currently Lean 4.35.0).
- Until a package moves, Go keeps the implementation `go test ./...` runs.
  `-tags lean` swaps in the proved code where a bridge exists. A differential
  test, or a byte-identical golden file, has to agree before the Go package
  is deleted.
- Go for a package is deleted when three things are true: no remaining Go
  package imports it, the differential or golden check against the last Go
  behavior is green on `bench/` and `stress/`, and this document has that
  phase's time and RSS numbers. `internal/core` is imported by almost
  everything, so it is deleted in the last phase, with the Go module.
  `cmd/*` becomes the Lean `main` in that same phase. After it, `scripts/install.sh`
  ships one Lean binary and the Go release matrix is gone.

The tag is the seam while Go is still the process. Default builds do not
look for `lake`, `elan`, or `fragua-geom`.

## What a module is allowed to see

Integer nanometres in, an integer, a reduced rational, or a boolean out.
Squared distance from a point to a segment is a rational even when every
coordinate is an integer (`(0,1)` to the segment `(0,0)–(1,1)` is `1/2`), so
the interface returns a numerator and a denominator rather than a rounded
length. The clearance predicate compares that rational with a clearance in
nanometres using only integer arithmetic:

```text
separated(num, den, c)  iff  den > 0 and num ≥ 0 and num ≥ c² · den
```

with a negative clearance treated as zero. Nothing in the predicate goes
through `float64`.

The one place a float still appears is the millimetre string DRC already
prints. Both builds convert the same rational with the same `big.Float`
square root in `internal/leanbridge`, so the printed report does not depend
on which side produced the rational.

## Bridge

Two ways to call Lean from Go:

### (a) Lean compiled to a C static library, linked with cgo

Lean can emit C, but an exported function is not a freestanding C function.
It needs the Lean runtime. The release workflow
(`.github/workflows/release.yml`) builds with `CGO_ENABLED=0` and
cross-compiles four targets from Ubuntu: `darwin/arm64`, `darwin/amd64`,
`linux/amd64`, `windows/amd64`. A static Lean library would need a Lean
runtime (and its allocator) built for each of those targets, plus a C
cross-toolchain, and cgo cannot be disabled. `scripts/install.sh` then
installs one archive per target and expects a single static binary with no
shared-library dependency. Linking the runtime into every shipped `fragua`
would make the default install depend on a C toolchain at build time and
would ship the runtime to users who never opted into Lean.

It is not on the path to a Lean executable. A cgo `fragua` would still be a
Go program, which is the thing this port deletes, and it still fights the
`CGO_ENABLED=0` release. It was not measured.

### (b) A small Lean executable, length-prefixed JSON on stdin/stdout

`lake build` in `lean/` produces `fragua-geom`. The Go side, only when built
with `-tags lean`, starts that process once and keeps it. Each request and
response is a little-endian `uint32` length followed by one JSON object.
Integers are decimal strings, because a squared distance of two `int64`
endpoints needs more than the 53 bits of a JSON number.

This is the bridge this repository uses.

- **Build and CI cost.** `go test`, `go vet`, and `go build` on the existing
  jobs do not install Lean. A separate job installs elan, builds the pinned
  toolchain, checks the proofs, and runs the differential tests.
- **Cross-compilation.** `fragua` itself still cross-compiles with
  `CGO_ENABLED=0`. The helper is built per host by someone who has elan.
  It is not an input to `scripts/install.sh`.
- **The single static binary.** The install script is unchanged. The binary
  it downloads is the pure-Go release. Opting into Lean is a second artifact
  (`fragua-geom`) next to a tag-built `fragua`, not a change to the curl
  install.
- **No toolchain at runtime.** Running the tagged binary needs the helper
  executable, not `lake` or `elan`. Running the default binary needs neither.
- **Cost.** A call crosses a pipe. On this kernel the pipe is about 20 µs
  and the geometry itself is about 3.5 µs, so a per-call bridge spends most
  of its time in the frame. A loop that stays inside the helper pays the
  frame once. The final binary pays it never: there is no Go process left
  to talk to. Measure which of those three you built. The numbers are below.

The helper is discovered via `FRAGUA_GEOM_BIN`, then by walking up from the
working directory or the executable to `lean/.lake/build/bin/fragua-geom`.
A missing helper under `-tags lean` is a deployment failure: the call panics
rather than silently using the Go fallback, so a tagged build cannot pretend
it checked the proved code.

## Roadmap

Every package moves. The order is the import order: a package is ported
after the packages it imports, and its Go sources are deleted only when
nothing left in Go still imports them. The bridge in every phase until the
last is (b), the length-prefixed helper. The granularity rule from the
phase-0 measurement applies the whole way: one request per board operation,
with the hot loop inside Lean. A request per primitive is how phase 0 lost
to Go, and it is not repeated for the router or the placer.

`internal/llms` and `agent/` (the docs `fragua init` writes) move with
`script` and `host`. They are part of the same final binary.

| Phase | Packages | Depends on | Bridge | Go deleted when | Re-measure, against the Go that phase replaces |
| --- | --- | --- | --- | --- | --- |
| 0 (this change) | `lean/`, `internal/leanbridge`; one call from `internal/drc` | nothing | (b). DRC is one frame per trace pair. `seg_sum` is the same loop in one frame, for the measurement only | nothing yet | The three rates and the RSS block below. Repeat them if the geometry or the toolchain changes |
| 1 | `internal/core` (model, units, polygon, JSON) | phase 0 distances | (b), one request per load or query, not per `Min` | last phase. Almost every Go package imports `core` | Load time and RSS for `stress/rp2040-minimal.fragua` and `bench/boards/*` vs `core.LoadFromPath`. Round-trip bytes identical |
| 2 | `internal/impedance` | core stackup, `Length` | (b), one sweep per board | `drc`, `si`, `router`, `script` no longer import the Go package | Width and impedance vs the Go oracle; time and RSS of a full-board sweep |
| 3 | `internal/parts`, `internal/kicad` | core library types | (b), one request per footprint or library file | `render`, `fab`, `script` have dropped the Go imports | Parse time and RSS on `internal/parts/testdata`; golden footprint JSON |
| 4 | `internal/erc` | core schematic | (b), one `erc.Check` per board | `script`, `host`, `fab`, `cmd/drcdump`, `cmd/*-regen` have moved | ERC text byte-identical on `bench/` and `stress/`; time and RSS of `erc.Check` |
| 5 | `internal/drc` (the rest: pairs, edge, drill, net connectivity, `net_split`) | phase 0, core, impedance | (b), one `drc.Check` per board. The pair loops stay inside Lean. The per-pair call from phase 0 is removed here | `script`, `host`, `fab`, `bench`, `cmd` have moved | `drc-print` byte-identical on `bench/` and `stress/`. Time and RSS of `drc.Check` on `stress/rp2040-minimal.fragua`. The per-pair tagged build is the slow baseline to beat |
| 6 | `internal/si` | core, impedance | (b), one `si.Check` per board | `script` has moved | SI report on a routed stress board; time and RSS vs Go |
| 7 | `internal/placer` | core; clearance from phase 5 | (b), one `auto-place` per board. ePlace and SA run inside the helper | `script`, `bench`, `cmd/fecha-regen` have moved | Fixed seed on the placer tests and `bench/boards`: HPWL, seconds, peak RSS vs Go. This is the first workload with dense float grids and many small nodes. Phase 0's tie does not predict it |
| 8 | `internal/router` | core, impedance, clearance from phase 5 | (b), one `route` per board. A*, Theta*, RR&R stay inside the helper. A frame per expansion is forbidden; that is the phase-0 failure mode | `script`, `bench`, `cmd/board-regen`, `cmd/fecha-regen` have moved | `stress/rp2040-minimal` and `bench/boards` at a fixed budget: routed nets, trace count, seconds, peak RSS vs the Go router. Where the search is deterministic, DRC text of the result matches. Where it is not, compare completion and `drc_err`, not the path bytes |
| 9 | `internal/gerber`, `internal/odb`, `internal/fab` | core; fab also drc, erc, gerber, kicad | (b), one `pack` per board | `script` and the `cmd/*` dumpers have moved. `odb` goes as soon as nothing imports it | Golden Gerber/zip bytes on the stress boards; time and RSS of `pack` |
| 10 | `internal/render` | core, parts | (b), one SVG or PNG per frame, drawn inside Lean | `host`, `mcp`, `script`, `cmd` have moved | Screenshot SVG byte-identical, and the PNG pixel-identical, on `bench/boards`; time and RSS vs `internal/render` |
| 11 | `internal/script`, `internal/llms` | the verbs from phases 4–10 | direct Lean calls once `script` itself is Lean. Until then, (b) per verb | `host`, `mcp`, `bench`, `cmd/fragua`, `cmd/gen-llms` have moved | `internal/script` tests' wall time. A scripted place + route + drc on the two-resistor board: seconds and RSS |
| 12 | `internal/host`, `internal/mcp`, `agent/` | script, render, drc, erc | none inside the process. HTTP is Lean. The sidecar is gone for code that has moved | `cmd/fragua` has moved | Latency and RSS of `POST /script`, `GET /drc`, `GET /screenshot` on `stress/rp2040` against `fragua run` |
| 13 | `internal/bench`, `cmd/fragua`, `cmd/board-regen`, `cmd/drc-print`, `cmd/drcdump`, `cmd/fab-dump`, `cmd/fecha-regen`, `cmd/gen-llms` | whatever those commands import, already Lean | none. This phase is the Lean `main` | the Go module, including `internal/core` and `internal/leanbridge`. `scripts/install.sh` ships the Lean binary for the same four targets the Go release builds today (linux/amd64, darwin/arm64, darwin/amd64, windows/amd64) | End-to-end wall time and peak RSS of that binary on `bench/boards` and `stress/rp2040` (load, drc, and route where the board is routed) against the last Go release. That is the measurement that accepts or rejects the reference-counting hypothesis for the product |

Theorems that belong to the early phases, carried along with the code:

- Phase 0, proved in this change: non-negativity, symmetry, zero iff endpoints coincide, zero iff a point is on the segment, segment distance zero iff the segments touch, segment distance equals the four-endpoint minimum when there is no proper crossing, clearance matches `num ≥ c²·den` and is monotone in the clearance.
- Phase 1: `Abs` idempotent and non-negative; `Min`/`Max` commutative, associative, idempotent; integer-millimetre round trip where the value fits.
- Phase 5: same-net copper is not a clearance error; disjoint layers are ignored; tightening a clearance only adds violations; union-find `find` is idempotent; `net_split` iff the touch relation has more than one component.
- Later phases are held to golden bytes and to the timing row above. Proofs are added where the statement is an equality on integers (connectivity, clearance). The placer and the router still move, proof or not: their check is the re-measure plus DRC legality, not an exemption from the port.

## This module

`lean/FraguaGeom/Basic.lean` defines and proves:

- `dist2_nonneg`, `dist2_comm`, `dist2_eq_zero_iff`
- `len2_nonneg`, `len2_eq_zero_iff`, `len2_eq_dist2`
- `lagrange` (the plane identity `|ap|²|ab|² − (ap·ab)² = (ap×ab)²`) and
  `altNum_eq_cross_sq`
- `pointSegDist2_den_pos`, `pointSegDist2_num_nonneg`
- `pointSegDist2_eq_zero_iff_onSegment`
- `pointSegDist2_le_left`, `pointSegDist2_le_right` (the segment is no farther
  than either endpoint)
- `pointSegDist2_degenerate`, `pointSegDist2_left_endpoint`,
  `pointSegDist2_right_endpoint`
- `properCross_symm`
- `segSegDist2_spec` (no proper crossing → minimum of the four endpoint
  distances), `segSegDist2_cross` (a proper crossing → 0)
- `segSegDist2_den_pos`, `segSegDist2_num_nonneg`, `segSegDist2_symm`
- `segSegDist2_eq_zero_iff_touches`
- `separated_spec`, `separated_invalid`, `separated_neg_clearance`,
  `separated_mono`, `separated_dist2`

Proofs are kernel-checked. The audit in `lean/AxiomAudit.lean` rejects
anything outside `propext`, `Classical.choice`, and `Quot.sound` (187
constants on the current tree). There is no `sorry` and no `axiom`
declaration. `grind` closes the polynomial steps; it does not add a project
axiom.

`lean/Main.lean` is the helper. Operations: `ping`, `dist2`, `point_seg`,
`seg_seg`, `separated`, `norm`, `batch`, and `seg_sum` (the hot loop used
only for the measurement below).

`internal/leanbridge` is the Go side. `TestDifferential` (build tag `lean`)
runs both implementations on the edge set plus 4000 random segments: small
coordinates, board-scale nanometres, and values at the `int64` rails,
including zero-length segments, collinear overlaps, shared endpoints, proper
crossings, and T junctions. `TestComparatorRejectsMutation` and
`TestDifferentialRejectsMutation` feed the comparator a value shifted by one
and require it to reject that value. A one-off check that the whole table
fails was done by adding 1 inside `pureDist2` and running:

```sh
go test -tags lean -count=1 -run 'TestDifferential$' ./internal/leanbridge/
```

The first case failed with `0/1 != 1/1`, and the edit was reverted.

Trace/trace clearance calls `leanbridge.SegmentSegmentMM`. On
`stress/rp2040-minimal.fragua` that is 19898 segment pairs. `cmd/drc-print`
and `cmd/drcdump` output on `bench/boards/*.fragua` and `stress/*.fragua` is
byte-identical with and without the tag.

### Measured cost

Linux x86_64, Lean 4.35.0, the same `segSegDist2` the proofs cover.
`go test -tags lean -bench=Benchmark -benchtime=1s -count=1 ./internal/leanbridge/`:

```text
BenchmarkPureSegSeg-4        429225      2692 ns/op    2184 B/op      99 allocs/op
BenchmarkPureHotLoop-4           86  13724355 ns/op 13895326 B/op  500568 allocs/op
BenchmarkLeanSegSeg-4         60279     19555 ns/op    1104 B/op      32 allocs/op
BenchmarkLeanSegSegBatch-4      501   2320738 ns/op  259834 B/op    6413 allocs/op
BenchmarkLeanHotLoop-4           82  14672530 ns/op     994 B/op      20 allocs/op
```

`PureHotLoop` and `LeanHotLoop` are 4096 varying segment distances per
operation (`pureSegSum` / `seg_sum`). Per query:

| Granularity | ns/query | queries/sec | Go heap per query |
| --- | --- | --- | --- |
| Go, one query in-process | 2692 | 3.71×10⁵ | 2184 B, 99 allocs |
| Go, 4096-query loop in-process | 3351 | 2.98×10⁵ | 3392 B, 122 allocs |
| Lean, one JSON frame per query | 19555 | 5.11×10⁴ | 1104 B, 32 allocs |
| Lean, 256 queries in one JSON batch, answers returned | 9065 | 1.10×10⁵ | 1015 B, 25 allocs |
| Lean, 4096-query loop inside the helper, one frame, sum returned | 3582 | 2.79×10⁵ | 0.24 B, 0.005 allocs |

Resident set for a fixed loop of 20 000 queries (`TestHotLoopPerf`):

| Run | Time | Queries/sec | Memory |
| --- | --- | --- | --- |
| Go loop | 68.8 ms | 2.91×10⁵ | Process RSS 9.7 → 9.9 MiB (HWM 10.0 MiB). Heap 0.27 → 3.7 MiB live, 68 MiB allocated and collected |
| Loop inside Lean | 73.2 ms | 2.73×10⁵ | Helper RSS flat at 62.2 MiB before and after (HWM unchanged). Go RSS stays 9.7 MiB |
| One frame per query | 491 ms | 4.07×10⁴ | Helper RSS 62.2 → 64.2 MiB. Go RSS stays ~9.7 MiB |

`TestSegSumAgrees` checks the sum for n = 0, 1, 7, 100, 1000 and 20000, and
checks that 20 000 iterations take more than 100× one iteration (measured
643×). A hoisted loop would fail that.

What the numbers say:

- A frame per call is about 7× slower than the Go loop. Roughly 20 µs of
  the 24 µs is the bridge. At that granularity Lean cannot win, whatever
  the collector does.
- A loop that stays inside Lean ties the Go loop (Go is about 6% faster)
  and drops the Go-side allocations from ~100 per query to a handful per
  thousands of queries. The helper does not grow its RSS across the loop;
  reference counting is reaping the temporaries. It pays for that with a
  62 MiB runtime sitting there the whole time, against a Go process of
  about 10 MiB.
- So the speedup is not in this module. The granularity that stops losing
  is "the whole hot loop in one Lean call". The granularity that could
  still beat Go is a subsystem that allocates a lot of short-lived search
  or grid state (phases 7 and 8, placer and router). Phase 0 is the
  control: arbitrary-precision geometry, both sides, no win for Lean.
  Phase 13 is the product measurement. Do not treat a per-call bridge
  number as evidence about either.

DRC still uses one frame per trace pair so a missing helper fails on the
first pair. On `stress/rp2040-minimal.fragua` that is 19898 pairs and about
half a second, which is the slow row of the table. Phase 5 replaces it
with one `drc.Check`.

## Build

```sh
# Default. No Lean toolchain.
go test ./...
go build -o fragua ./cmd/fragua

# Proved geometry.
curl -fsSL https://raw.githubusercontent.com/leanprover/elan/master/elan-init.sh | sh -s -- -y --default-toolchain none
export PATH="$HOME/.elan/bin:$PATH"
( cd lean && lake build )
export FRAGUA_GEOM_BIN="$PWD/lean/.lake/build/bin/fragua-geom"
go test -tags lean ./internal/leanbridge/ ./internal/drc/
go build -tags lean -o fragua ./cmd/fragua
```

`lake build` also elaborates `lean/AxiomAudit.lean`, which fails the build if
a `FraguaGeom` constant depends on an unexpected axiom.
