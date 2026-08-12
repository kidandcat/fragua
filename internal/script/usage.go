package script

// Usage returns the CLI / GET / help text (script reference grows as verbs land).
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
  lib KEY … + indented pad NUMBER X Y W H
  sym REF KIND … + indented pin for generic_ic
  net NAME REF.PIN …
  palette REF KEY | palette list
  list-lib | lib-list
  place REF x y [rot=DEG]
  auto-place [seed=N]
  route [max_seconds=N]
  clear-route | delete-trace ID
  trace NET x1 y1 x2 y2 [layer=Top|Bottom] [width=0.15]
  via NET x y [drill=0.3] [dia=0.6]
  rule-area NAME x1 y1 x2 y2 [clearance=N] …
  fab-rules jlcpcb|jlcpcb-4l|clear|list
  layer list|add|remove|rename
  drc / erc / pack [fab=…]
  compact (stub)
  screenshot PATH
  save [PATH] | view | status | help

See PORT_GO.md for the Go port plan. Full verb parity is in progress.
`
}
