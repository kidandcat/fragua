---
name: fragua
description: Design printed circuit boards with Fragua — schematic, footprint placement, auto-routing, DRC/ERC and JLCPCB/PCBWay manufacturing files. Use whenever the task involves a PCB, a schematic, a board layout, footprints, nets, traces, copper pours, Gerbers, a BOM/CPL, or ordering a board from a fab. Also use for .fragua project files.
---

# Fragua

An AI-native PCB design tool: one Go binary that hosts a script API for you and a live
browser UI for the human. You drive the board end to end; they watch and steer.

## 1. Launch

Check it is installed (`fragua help`). If not:

```sh
curl -fsSL https://raw.githubusercontent.com/mentasystems/fragua/master/scripts/install.sh | sh
```

Then start it. **Bare `fragua` only prints help and exits** — you need a subcommand:

```sh
fragua mcp path/to/board.fragua    # MCP over stdio + HTTP API + UI
fragua run path/to/board.fragua    # HTTP API + UI only
```

If the `fragua` MCP server is configured in `.mcp.json`, it is already running — use the
`fragua_*` tools directly. Otherwise start `fragua run` in the background and use HTTP:

```sh
curl -s http://127.0.0.1:7878/script -H 'content-type: application/json' \
  -d '{"script":"outline 30 20\nstatus"}'
```

Tell the human the UI is at <http://127.0.0.1:7878/ui/> so they can watch.

## 2. Discover the verbs — do not guess

The verb set is small but specific. Argument names are not guessable.

- `fragua_help` (no argument) or `GET /help` — full reference plus a "First 10 minutes" recipe.
- `fragua_help` with `verb: "route"`, or `fragua help route`, or `GET /help?verb=route` —
  one verb's usage, aliases, description and examples.
- `list-lib` — the 70+ footprints that already exist. **Always look before authoring one.**

## 3. The script language

One verb per line; indented lines continue an open `sym`/`lib` block; `#` comments.
Send related lines together in one `fragua_script` call.

**Execution stops at the first failing line.** Every line after an error did not run.

```text
ok <verb>: <result>                  succeeded
error line <n> <verb>: <reason>      failed; the script stopped here
```

## 4. The recipe

Follow this order. Each step depends on the one before it.

```text
# --- board + rules ---
outline 30 20 radius=2
fab-rules jlcpcb                 # fab minimums become the DRC floor; set BEFORE routing
class ground pour=both
class power width=0.4

# --- schematic ---  kinds: ic, resistor, capacitor, inductor, led, diode
sym U1 ic key=esp32_s3_zero
  pin 1 L 5V  role=power_in
  pin 2 L GND role=power_in
  pin 3 L 3V3 role=power_out
  pin 4 R IO1 role=bidir
sym R1 resistor key=r_0603
sym C1 capacitor key=c_0603

net GND  U1.GND C1.2 R1.2 class=ground
net +3V3 U1.3V3 C1.1     class=power
net SIG  U1.IO1 R1.1

nc U1.IO9 U1.IO10                # unused pins, or ERC calls them floating
erc                              # iterate until 0 errors

# --- footprints: bind, THEN anchor the ones whose position matters ---
palette U1 esp32_s3_zero
palette R1 r_0603 value=10k
palette C1 c_0603 value=100nF

place U1 15 10                   # anchor the parts whose position matters
edge-place J1 bottom             # connectors: never hand-compute the edge maths
                                 # R1 and C1 need no `place`: auto-place seats them

# --- refine, route, pour ---
auto-place seed=42               # seats the unplaced, arranges them; anchors stay put
route max_seconds=120
auto-pour
stitch

# --- verify and ship ---
drc
pack fab=jlcpcb out=/tmp         # /tmp/<project>-jlcpcb.zip, ready to upload
save /abs/path/board.fragua
```

## 5. Reading results

```text
ok erc: erc: 0 errors, 4 warnings (4 findings)
ok auto-place: place: HPWL 49.94 → 17.38 mm, moved 2 parts, 8000 iters
ok route: route: 3/3 nets ok, 3 traces, 0 vias, 14.1 mm copper, 0 ms
ok drc: drc: 1 errors, 2 warnings (3 findings)
ok pack: packed /tmp/untitled-jlcpcb.zip (erc_err=0 drc_err=1)
```

- `route: N/M nets ok` — if `N < M`, just run `route` again; it only attacks open nets.
  If `N` stops improving, more seconds will not help: the geometry is blocking. Give the
  board more area, move the offending part, or add a layer (`layer add`).
- `erc`/`drc` give **counts, not locations**. Errors must be 0; warnings are usually
  acceptable. To see where a violation is, look at the UI or `fragua_screenshot`.
- **`pack` only refuses on ERC errors.** It packs a DRC-dirty board and reports
  `drc_err=N`. Never hand the human a zip with `drc_err>0` without saying so.
- `status` after every step is cheap. `palette=0` is normal; placed parts are `footprints=N`.

## 6. Budgets

| Knob | Value |
|------|-------|
| `route max_seconds=N` | default 600, max 3600 — start at 90–180 |
| `auto-place seed=N` | always set it, so runs are reproducible |
| `compact route_seconds=N` | 20–90; optional and slow, only on a clean board |

## 7. Common mistakes

- Running bare `fragua` and wondering why nothing listens. Use `run` / `mcp`.
- Thinking `palette` places the part. It only binds the footprint — `place` the ones whose
  position matters; `auto-place` seats the rest itself (`seated N new`).
- Letting `auto-place` run before the ICs are anchored, so decoupling scatters.
- Authoring a footprint with `lib` that `list-lib` already has.
- Inventing a symbol kind. Only `ic`, `resistor`, `capacitor`, `inductor`, `led`, `diode`
  exist — model crystals, switches and connectors as `ic` with explicit pin roles.
- Setting `fab-rules` after routing instead of before.
- Forgetting `nc` on unused MCU pins, then chasing ERC warnings that are not bugs.
- Re-routing without `clear-pour`, leaving stale copper. Order: `clear-pour` → `route` →
  `auto-pour` → `stitch`.
- Running a whole session in memory and losing it. `save /abs/path.fragua` early.

## 8. Working with the human

Fragua exists because a human is watching. When the router plateaus, a part will not fit,
or a design decision is genuinely ambiguous (connector choice, target voltage, dimensions),
stop and ask — do not silently pick. Point them at <http://127.0.0.1:7878/ui/>, say what
you tried, and let them drag a part or relax a constraint.
