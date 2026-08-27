# TODO

## Signal-integrity campaign (`si-check` verb)

Physics-aware verification beyond geometric DRC, staged so the cheap 80%
lands first and full-wave simulation stays an external tool.

**v1 (native Go) — SHIPPED** (internal/si + `si-check` verb): impedance
audit per net class (coalesced per net/layer/width), return-path check
against plane layers or — on planeless 2L stacks — pours as de-facto
reference, diff-pair skew, via budget, `not_routed`/`unknown_net` guards.
Shipped alongside: router resolves per-net trace width
(impedance-derived > class TraceWidthMM > default; internal/router/width.go),
compact's probe uses the same widths, and layer-name resolution is
stackup-aware (pour/auto-pour/trace landed on wrong inner layers of 4L+).

Validated against fecha-gateway-v3 (2L: return path fully covered by the
GND pours; 123 Ω on 0.25 mm traces is expected 2L physics, harmless at
SPI/UART speeds) and a 4L ESP32-S3 USB board (caught router ignoring
impedance widths, missing inner-plane pours, diff-pair skew).

**Open (router):** impedance-width nets (e.g. USB at 0.414 mm on the
default 4L stack) can become unroutable through fine-pitch congestion the
0.089 mm default squeezed through. Needs impedance-aware necking (allow
short necked escapes, keep nominal width elsewhere) or a routability
fallback with an explicit report line. Also: `impedance.LineParams`
refuses the asymmetric default 4L stack on inner layers, so impedance
nets are only width-controlled on F/B there.

**v2 (external, later):** full-wave FDTD for the rare GHz/antenna cases via
gerber2ems (Antmicro) + openEMS over the fab pack Fragua already emits —
shell-out only, GPL stays out of the binary. First real target: the LoRa
868 MHz antenna feedline on fecha-gateway-v3.

**Explicit non-goal:** reimplementing an FDTD kernel in Go. Only revisit if
`si-check` becomes a product differentiator and the shell-out falls short
(and then from the literature, not from openEMS sources).

## PCB compaction campaign (fecha-gateway-v3)

**Shipped:** the `compact` script verb (feasibility-gated outline shrink +
re-place / re-route gates). Used successfully on the RP2040 stress board
(see `stress/` v8–v9).

**Still open:** apply that pipeline to the real order board and ship a
measurably smaller fab pack.

Goal: compact fecha-gateway-v3 as much as possible while keeping DRC at 0
(courtyard clearances, routability, edge clearance). The board is on hold
and will **not** be sent to JLCPCB until this campaign succeeds — it is the
real-world test case for production-module layouts (elevated modem on
headers, poly/module footprints), not bare QFNs.

Test board (fecha-gateway-v3):

- Design (Fragua JSON, current state = v3: MOSFET modem power switch + U2
  chirality fix, silk "v3 fragua", auto-routed, DRC/ERC clean):
  `~/fecha/firmware/sf7/yellow/fecha-gateway-v2/fecha-gateway-v2.json`
- Reference fab export of that state (do not regenerate over it):
  `~/fecha/firmware/sf7/yellow/fecha-gateway-v2/fab/fecha-gateway-v3-*`
  (+ `preview-v3.png`)
- Work on a copy — that JSON is the source of truth for the v3 order.

Success criteria: measurably smaller board area than the current v3 outline,
auto-route still completes (0 failed nets, or an explicit
`allow_failed` budget you accept), DRC 0 / ERC 0, and the JLCPCB export
stays valid.
