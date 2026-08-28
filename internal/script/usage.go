package script

// Usage returns the CLI / GET / help text.
func Usage() string {
	return `fragua — AI-native PCB design tool

Usage:
  fragua                 print this help and exit
  fragua help            same
  fragua run [file]      start local HTTP API (default 127.0.0.1:7878)

Environment:
  FRAGUA_API_ADDR        listen address (default 127.0.0.1:7878)
  FRAGUA_NO_BROWSER      if set, do not open a browser window

HTTP API:
  GET  /  /help          this reference
  GET  /health           ok
  GET  /screenshot       PNG/SVG of current board
  GET  /events           SSE project change stream
  GET  /state            JSON project snapshot (UI)
  POST /script           run multi-line script (text/plain body or JSON {"script":"..."})
  POST /save             save project {"path":"..."} optional

Script verbs (line-oriented, agent-first):
  outline W H [radius=R]
  outline-poly x1 y1 x2 y2 ...
  cutout x1 y1 ... [label=NAME] | clear-cutouts
  hole X Y D [label=NAME] | clear-holes
  keepout X1 Y1 X2 Y2 [no_copper=true] [no_place=true]
  lib KEY … + indented pad NUMBER X Y W H
  sym REF KIND … + indented pin for generic_ic
  net NAME REF.PIN …
  class NAME [clearance=N] [width=N] [impedance=Z]
  net-class NET CLASS
  palette REF KEY | palette list
  list-lib
  place REF x y [rot=DEG]
  place-legal REF [tries=N] [rot=DEG]
  edge-place REF left|right|top|bottom [along=N]
  edge-plan REF [REF...]
  move REF X Y | rotate REF DEG
  unplace REF | delete REF | clear-board
  auto-place [REF...] [seed=N] [iters=N]
  route [max_seconds=N] [organic=true] [teardrop=true]   (N: default 90, max 600)
  clear-route | clear-net NET | delete-trace ID | delete-via ID
  trace NET x1 y1 x2 y2 [layer=Top] [width=0.15]
  via NET x y [drill=0.3] [dia=0.6]
  pour NET [layer=Top] [relief=spokes4|solid] [stitch=true] [pitch=N]
  auto-pour [NET...]          (default GND, both layers)
  clear-pour [NET]
  stitch                      (grid + pad vias that tie pour islands)
  nc REF.PIN [REF.PIN...]     (mark unused MCU pins; no floating_pin)
  fiducial X Y [ref=FID1]
  diff NETA NETB              (diff-pair data; single-ended Z only)
  impedance [NET]             (closed-form microstrip/stripline; not FEM)
  si-check [NET...] [tol=0.10] [max_vias=N]
                              (impedance, return path, diff skew, via budget)
  teardrop on|off             (copper fillets at pad/via junctions)
  silk-line X1 Y1 X2 Y2 | silk-text X Y TEXT [size=1]
  rule-area NAME x1 y1 x2 y2 [clearance=N] …
  fab-rules jlcpcb|jlcpcb-2l-via02|jlcpcb-4l|clear|list
  escape via-in-pad REF.PAD | via-in-pad-stranded [on|off] | list
  layer list|add|remove|rename|dielectric
  drc / erc
  compact [step=1] [seed=N] [allow_failed=0] [route_seconds=20] [max_seconds=600] [aspect=keep|free]
  pack [fab=jlcpcb] [out=DIR] [teardrop=true] | export DIR   (fails on ERC errors)
  screenshot PATH
  save [PATH] | view | status | reset | help

An agent can take a board from 0 to a JLCPCB pack with the verbs above.
Commercial floor: the same agent loop used on shipped boards:
auto-place (SA + decouple + edge snap) → route (Theta* + fanout + stitch)
→ pour/stitch → drc/erc → pack.
`
}
