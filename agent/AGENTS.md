## Fragua

[Fragua](https://fragua.cloud) is an AI-native PCB design tool. You design the board; the
human watches it happen in a browser and steers. One static Go binary — no KiCad, no
FreeRouting, no external CAD.

### Launch

```sh
fragua mcp board.fragua     # MCP server on stdio + HTTP API + UI on 127.0.0.1:7878
fragua run board.fragua     # HTTP API + UI only, no MCP
```

Both host the *same* live project, so the human always sees what you are doing at
<http://127.0.0.1:7878/ui/>. Omit the file to start in memory — then `fragua_save` with an
absolute path early, or the work is lost on exit.

Env: `FRAGUA_API_ADDR` (default `127.0.0.1:7878`), `FRAGUA_NO_BROWSER=1`.

### Two ways to drive it

**MCP tools** (preferred — configured in `.mcp.json`):

| Tool | Use it for |
|------|-----------|
| `fragua_script` | Everything. Runs script lines; this is the real API. |
| `fragua_help` | The verb reference. Pass `verb` for one verb's usage + examples. |
| `fragua_status` | Cheap one-line summary. Call it after every step. |
| `fragua_state` | JSON state; pass `section` (`board`/`schematic`/`palette`) to keep it small. |
| `fragua_drc` | Runs `erc` then `drc`. |
| `fragua_route` | `route` with typed options. |
| `fragua_screenshot` | Board as SVG source. |
| `fragua_save` | Write the `.fragua` file. |

**Plain HTTP** (any tool that can curl):

```sh
curl -s http://127.0.0.1:7878/help                      # full reference
curl -s 'http://127.0.0.1:7878/help?verb=route'         # one verb
curl -s http://127.0.0.1:7878/script \
  -H 'content-type: application/json' \
  -d '{"script":"outline 30 20\nstatus"}'
curl -s http://127.0.0.1:7878/screenshot -o board.svg
```

### The script language

Line-oriented, one verb per line. Indented lines continue an open `sym`/`lib` block.
`#` comments. **Execution stops at the first failing line** — everything after it did not
run, so read every result line.

```text
ok <verb>: <result>                  succeeded
error line <n> <verb>: <reason>      failed; the script stopped here
```

Run `fragua_help` (or `fragua help`, or `GET /help`) for the full verb list. Do not guess
argument names — ask for the verb's help.

### End-to-end recipe

```text
# 1. Board and design rules first.
outline 30 20 radius=2
fab-rules jlcpcb                 # fab minimums become the DRC floor
class ground pour=both
class power width=0.4

# 2. Schematic. Kinds: ic, resistor, capacitor, inductor, led, diode.
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

nc U1.IO9 U1.IO10                # silence "floating pin" on unused GPIOs
erc                              # must reach 0 errors

# 3. Footprints: bind, then place. Both steps are required.
list-lib                         # 70+ footprints already exist — look before you author
palette U1 esp32_s3_zero
palette R1 r_0603 value=10k
palette C1 c_0603 value=100nF

place U1 15 10                   # anchor what matters (ICs, connectors)
edge-place J1 bottom             # connectors: let the verb do the edge maths
place R1 22 6                    # everything else still needs an initial position
place C1 22 14

# 4. Refine, route, pour.
auto-place R1 C1 seed=42         # only moves already-placed parts; anchors stay put
route max_seconds=120
auto-pour                        # GND on both layers
stitch                           # vias tying the pour islands together

# 5. Verify, then ship.
drc
pack fab=jlcpcb out=/tmp         # writes /tmp/<project>-jlcpcb.zip
save /abs/path/board.fragua
```

### Reading results

```text
ok erc: erc: 0 errors, 4 warnings (4 findings)
ok auto-place: place: HPWL 49.94 → 17.38 mm, moved 2 parts, 8000 iters
ok route: route: 3/3 nets ok, 3 traces, 0 vias, 14.1 mm copper, 0 ms
ok stitch: stitched 78 vias
ok drc: drc: 1 errors, 2 warnings (3 findings)
ok pack: packed /tmp/untitled-jlcpcb.zip (erc_err=0 drc_err=1)
ok status: name="untitled" footprints=3 traces=3 vias=78 nets=3 symbols=3 palette=0 outline=30.00x20.00mm
```

- **`route: N/M nets ok`** — `N < M` means nets are still open. Re-run `route`; it only
  attacks what is unrouted, so repeated bounded calls are cheap. If `N` stops improving,
  the geometry is the problem: give the board more room, move the blocking part, or add a
  layer (`layer add`) — not more seconds.
- **`erc`/`drc` report counts, not locations.** Errors must be zero; warnings are usually
  fine (unused pins, conservative power checks). To see *where* a violation is, look at the
  UI canvas or `fragua_screenshot` — it highlights them.
- **`pack` only refuses on ERC errors.** It will happily pack a board with DRC errors and
  tell you so in `drc_err=N`. Never ship a board with `drc_err>0`: check it yourself.
- **`status`** is the cheapest sanity check. `palette=0` is normal — that counts palette
  entries, not placed parts; placed parts are `footprints=N`.

### Budgets

| Knob | Use |
|------|-----|
| `route max_seconds=N` | Default 600, max 3600. Start at 90–180. Always set one on a dense board. |
| `auto-place seed=N` | Fixes the RNG. Always pass it so a run is reproducible. |
| `compact route_seconds=N` | 20–90. Optional, slow; only on an already-clean board. |

### Common mistakes

- **`fragua` with no subcommand just prints help and exits.** Use `fragua run` / `fragua mcp`.
- **`palette` does not place.** `palette REF KEY` binds a footprint; the part still needs
  `place`, `place-legal` or `edge-place`. `auto-place` on an unplaced part fails with
  `no movable footprints`.
- **`auto-place` only refines what is already placed.** Anchor the ICs and connectors first,
  then let it move the passives — otherwise decoupling scatters away from its IC.
- **Authoring a footprint you already have.** Run `list-lib` first; 70+ parts ship with the
  tool. Only reach for `lib` when nothing matches.
- **Only six symbol kinds exist** — `ic`, `resistor`, `capacitor`, `inductor`, `led`, `diode`.
  Model a crystal, switch or connector as an `ic` with explicit pin roles.
- **Pin roles matter to ERC.** `power_in`, `power_out`, `input`, `output`, `bidir`, `passive`,
  `nc`. A rail with no `power_out` anywhere warns.
- **Unused pins warn until you say so.** `nc U1.IO9 U1.IO10 …`.
- **Set `fab-rules` before routing**, not after — they are the floor the router respects.
- **Pours go after routing.** `auto-pour` then `stitch`. Re-routing means `clear-pour`,
  `route`, `auto-pour`, `stitch` again.
- **A memory-only session loses everything.** `save /abs/path.fragua` early.

### When you are stuck

Ask the human. Fragua is built for a human watching the UI: if the router plateaus or a part
will not fit, say what you tried and what the board looks like, and let them drag something
or change a constraint. The UI is at <http://127.0.0.1:7878/ui/>.
