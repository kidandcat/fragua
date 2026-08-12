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
	return paths, nil
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
