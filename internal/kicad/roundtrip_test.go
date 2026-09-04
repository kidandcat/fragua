package kicad

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/mentasystems/fragua/internal/core"
)

// kicadPlace is KiCad's own placement of a footprint child: the local point is
// rotated by the footprint orientation with RotatePoint's Y-down matrix and
// added to the footprint position. If Fragua's flip or angle sign were wrong,
// this would land somewhere else than PadWorldCenter.
func kicadPlace(fx, fy, angleDeg, lx, ly float64) (float64, float64) {
	r := angleDeg * math.Pi / 180
	c, s := math.Cos(r), math.Sin(r)
	return fx + lx*c + ly*s, fy - lx*s + ly*c
}

func atOf(n *node) (x, y, angle float64) {
	at := n.first("at")
	x, _ = strconv.ParseFloat(at.arg(0), 64)
	y, _ = strconv.ParseFloat(at.arg(1), 64)
	angle, _ = strconv.ParseFloat(at.arg(2), 64)
	return
}

// TestPadWorldRoundTrip re-derives every pad position from the exported file
// exactly as KiCad will, and compares it to Fragua's own world geometry.
func TestPadWorldRoundTrip(t *testing.T) {
	boards := map[string]*core.Board{"synthetic": smallBoard()}
	for _, dir := range []string{"../../bench/boards"} {
		ents, _ := os.ReadDir(dir)
		for _, ent := range ents {
			if ent.IsDir() || filepath.Ext(ent.Name()) != ".fragua" {
				continue
			}
			p, err := core.LoadFromPath(filepath.Join(dir, ent.Name()))
			if err != nil {
				t.Fatalf("%s: %v", ent.Name(), err)
			}
			boards[ent.Name()] = p.Board()
		}
	}

	for name, board := range boards {
		t.Run(name, func(t *testing.T) {
			s, err := Export(board, DefaultOptions())
			if err != nil {
				t.Fatal(err)
			}
			root, err := parseSexpr(s)
			if err != nil {
				t.Fatal(err)
			}
			// The flip origin is the top of the board in Fragua coordinates.
			e := &exporter{board: board, stack: board.StackupOrDefault()}
			flip := e.originY().ToMM()

			want := map[string][2]float64{}
			for _, id := range board.FootprintOrder {
				fp := board.Footprints[id]
				if fp == nil {
					continue
				}
				for i := range fp.Pads {
					c := core.PadWorldCenter(fp, &fp.Pads[i])
					want[fp.Reference+"."+fp.Pads[i].Number] = [2]float64{c.X.ToMM(), flip - c.Y.ToMM()}
				}
			}

			checked := 0
			for _, fpNode := range root.find("footprint") {
				ref := ""
				for _, p := range fpNode.find("property") {
					if p.arg(0) == "Reference" {
						ref = p.arg(1)
					}
				}
				fx, fy, ang := atOf(fpNode)
				for _, pad := range fpNode.find("pad") {
					lx, ly, _ := atOf(pad)
					gx, gy := kicadPlace(fx, fy, ang, lx, ly)
					exp, ok := want[ref+"."+pad.arg(0)]
					if !ok {
						continue // NPTH mounting-hole footprint
					}
					if math.Abs(gx-exp[0]) > 1e-3 || math.Abs(gy-exp[1]) > 1e-3 {
						t.Fatalf("%s.%s lands at (%.4f, %.4f), Fragua says (%.4f, %.4f)",
							ref, pad.arg(0), gx, gy, exp[0], exp[1])
					}
					checked++
				}
			}
			if checked == 0 {
				t.Fatal("no pads checked")
			}
			t.Logf("%d pad positions round-tripped", checked)
		})
	}
}
