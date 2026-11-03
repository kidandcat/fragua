# PCBGolf bench

Measures placement and two route passes of the comma.ai PCBGolf board
(245 parts, 191 nets) and prints the numbers.

https://github.com/commaai/PCBGolf publishes no license. The generated
netlist is a derivative of that board, so it is not committed here.
Rebuild it locally and pass the path:

```sh
git clone --depth 1 https://github.com/commaai/PCBGolf.git /tmp/PCBGolf
python3 bench/pcbgolf/kicad2fragua.py /tmp/PCBGolf/pcbgolf.kicad_pcb > /tmp/netlist.fs
go run ./bench/pcbgolf -netlist /tmp/netlist.fs -outline 140x110 -iters 8000 -route 60
```

`kicad2fragua.py` reads a KiCad `.kicad_pcb` and writes a fragua script
(`lib`, `sym`, `palette`, `net`). It does not contain the board.

The same flags are the before/after measurement: outline, placer
iterations, route budget, seed. A second `route` pass is always run.
`-route 0` stops after placement.

What the lines mean:

- `place` is the auto-place summary. A fit failure names the parts and
  the outline instead of leaving them stacked.
- `courtyard` is `courtyard_overlap` errors under the design-rule DRC.
- `unconnected_pads` is pads with a net and no copper, over pads with a net.
- `route-pass2` must not report fewer routed nets than `route-pass1`.
