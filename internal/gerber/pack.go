// Package gerber writes RS-274X Gerbers, Excellon drills, BOM and CPL.
// Output follows the same algorithmic process as crates/pcb-gerber.
package gerber

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mentasystems/fragua/internal/core"
)

// WriteFabPack writes the full manufacturing pack into outDir.
// Returns absolute paths of written files in emission order (matches Rust).
func WriteFabPack(board *core.Board, name, outDir string) ([]string, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	stem := sanitize(name)
	stackup := board.StackupOrDefault()

	var paths []string
	write := func(filename, body string) error {
		p := filepath.Join(outDir, filename)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			return err
		}
		paths = append(paths, p)
		return nil
	}

	// Copper: one file per stackup copper layer (F.Cu / B.Cu / InN.Cu …).
	for idx, spec := range stackup.Layers {
		if spec.Kind == core.LayerKindDielectric {
			continue
		}
		layerName := spec.Name
		if layerName == "" {
			layerName = defaultLayerName(idx, len(stackup.Layers))
		}
		sanitized := sanitizeLayerName(layerName)
		layer := core.Layer{Index: uint8(idx)}
		if err := write(stem+"-"+sanitized+".gbr", writeCopperLayer(board, layer, layerName)); err != nil {
			return paths, err
		}
	}
	// Fallback if stackup has no copper entries (should not happen).
	if len(paths) == 0 {
		if err := write(stem+"-F_Cu.gbr", writeCopperLayer(board, core.LayerTop, "F.Cu")); err != nil {
			return paths, err
		}
		if err := write(stem+"-B_Cu.gbr", writeCopperLayer(board, core.LayerBottom, "B.Cu")); err != nil {
			return paths, err
		}
	}

	if err := write(stem+"-F_Mask.gbr", writeMask(board, sideTop)); err != nil {
		return paths, err
	}
	if err := write(stem+"-B_Mask.gbr", writeMask(board, sideBottom)); err != nil {
		return paths, err
	}
	if err := write(stem+"-F_SilkS.gbr", writeSilk(board, sideTop)); err != nil {
		return paths, err
	}
	if err := write(stem+"-B_SilkS.gbr", writeSilk(board, sideBottom)); err != nil {
		return paths, err
	}
	if err := write(stem+"-F_Paste.gbr", writePaste(board, sideTop)); err != nil {
		return paths, err
	}
	if err := write(stem+"-B_Paste.gbr", writePaste(board, sideBottom)); err != nil {
		return paths, err
	}
	if err := write(stem+"-Edge_Cuts.gbr", writeEdgeCuts(board)); err != nil {
		return paths, err
	}
	if err := write(stem+"-PTH.drl", writeExcellon(board, true)); err != nil {
		return paths, err
	}
	if err := write(stem+"-NPTH.drl", writeExcellon(board, false)); err != nil {
		return paths, err
	}
	if err := write(stem+"-bom.csv", writeBOM(board)); err != nil {
		return paths, err
	}
	if err := write(stem+"-pos.csv", writePos(board)); err != nil {
		return paths, err
	}
	if err := write(stem+"-netlist.txt", writeNetlist(board)); err != nil {
		return paths, err
	}
	if err := write(stem+"-ipc-d-356.ipc", writeIPCD356(board, stem)); err != nil {
		return paths, err
	}
	if err := write("README.txt", writeREADME(board, stem)); err != nil {
		return paths, err
	}
	return paths, nil
}

// writeREADME is the fab-pack layer map an EE / JLCPCB CAM operator reads.
func writeREADME(board *core.Board, stem string) string {
	var b strings.Builder
	fab := core.ActiveFabRules(board)
	stack := board.StackupOrDefault()
	fmt.Fprintf(&b, "Fragua manufacturing pack\n")
	fmt.Fprintf(&b, "Software: Fragua %s\n", core.Version)
	fmt.Fprintf(&b, "Units: millimetres\n")
	fmt.Fprintf(&b, "Coordinate origin: board lower-left (X right, Y up)\n")
	fmt.Fprintf(&b, "CPL rotation: degrees counterclockwise.\n")
	fmt.Fprintf(&b, "  Bottom-side rotation is (180 - placement) mod 360,\n")
	fmt.Fprintf(&b, "  matching the KiCad JLCPCB plugin coordinate-file convention.\n")
	fmt.Fprintf(&b, "\nLayer map (JLCPCB):\n")
	for idx, spec := range stack.Layers {
		if spec.Kind == core.LayerKindDielectric {
			continue
		}
		name := spec.Name
		if name == "" {
			name = defaultLayerName(idx, len(stack.Layers))
		}
		role := "Inner copper"
		fn := fileFunction(name, idx, stack.CopperCount())
		switch {
		case name == "F.Cu" || idx == 0:
			role = "Top copper"
		case name == "B.Cu" || idx == stack.CopperCount()-1:
			role = "Bottom copper"
		default:
			role = fmt.Sprintf("Inner copper L%d", idx+1)
		}
		if spec.AssignedNet != "" {
			role += " (plane net " + spec.AssignedNet + ")"
		}
		fmt.Fprintf(&b, "  %s-%s.gbr    %s  [%s]\n", stem, sanitizeLayerName(name), role, fn)
	}
	fmt.Fprintf(&b, "  %s-F_Mask.gbr     Top soldermask\n", stem)
	fmt.Fprintf(&b, "  %s-B_Mask.gbr     Bottom soldermask\n", stem)
	fmt.Fprintf(&b, "  %s-F_SilkS.gbr    Top silkscreen\n", stem)
	fmt.Fprintf(&b, "  %s-B_SilkS.gbr    Bottom silkscreen\n", stem)
	fmt.Fprintf(&b, "  %s-F_Paste.gbr    Top solder paste (SMD pads only; no PTH/vias)\n", stem)
	fmt.Fprintf(&b, "  %s-B_Paste.gbr    Bottom solder paste (SMD pads only; no PTH/vias)\n", stem)
	fmt.Fprintf(&b, "  %s-Edge_Cuts.gbr  Board outline\n", stem)
	fmt.Fprintf(&b, "  %s-PTH.drl       Plated holes (vias + PTH pads)\n", stem)
	fmt.Fprintf(&b, "  %s-NPTH.drl      Non-plated holes (mounting)\n", stem)
	fmt.Fprintf(&b, "  %s-bom.csv       BOM (Comment, Designator, Footprint, LCSC Part #, Manufacturer, MPN, Quantity)\n", stem)
	fmt.Fprintf(&b, "  %s-pos.csv       Pick-and-place / JLC coordinate file\n", stem)
	fmt.Fprintf(&b, "  %s-netlist.txt   Net → pads (simple netlist)\n", stem)
	fmt.Fprintf(&b, "  %s-ipc-d-356.ipc IPC-D-356A electrical test netlist (CUST 0 = 0.0001 in)\n", stem)
	fmt.Fprintf(&b, "\nFab rules (%s):\n", fab.Preset)
	fmt.Fprintf(&b, "  min trace/space: %.3f mm\n", fab.MinTraceWidthMM)
	fmt.Fprintf(&b, "  min via drill: %.3f mm\n", fab.MinViaDrillMM)
	fmt.Fprintf(&b, "  min via diameter: %.3f mm\n", fab.MinViaDiameterMM)
	fmt.Fprintf(&b, "  min annular ring: %.3f mm\n", fab.MinAnnularRingMM)
	fmt.Fprintf(&b, "  min edge clearance: %.3f mm\n", fab.MinEdgeClearanceMM)
	fmt.Fprintf(&b, "\nFootprint names: a library: prefix is stripped. Passive size keys\n")
	fmt.Fprintf(&b, "  (c_0603, r_0603, …) are emitted as IPC-7351 metric names\n")
	fmt.Fprintf(&b, "  (C_0603_1608Metric, R_0603_1608Metric).\n")
	fmt.Fprintf(&b, "LCSC / MPN / Manufacturer are copied from the part when present;\n")
	fmt.Fprintf(&b, "empty cells mean the part has no number — they are never invented.\n")
	return b.String()
}

func defaultLayerName(idx, n int) string {
	if idx == 0 {
		return "F.Cu"
	}
	if idx == n-1 {
		return "B.Cu"
	}
	return fmt.Sprintf("In%d.Cu", idx)
}

// sanitize replaces non [A-Za-z0-9._-] with '_'.
func sanitize(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, c := range name {
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '.' || c == '_' || c == '-' {
			b.WriteByte(byte(c))
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "untitled"
	}
	return b.String()
}

// sanitizeLayerName: alnum and '-' kept; everything else → '_' (F.Cu → F_Cu).
func sanitizeLayerName(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, c := range name {
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			b.WriteByte(byte(c))
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "Layer"
	}
	return b.String()
}
