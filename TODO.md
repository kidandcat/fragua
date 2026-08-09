# TODO

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
