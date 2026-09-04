package script

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mentasystems/fragua/internal/core"
	"github.com/mentasystems/fragua/internal/kicad"
)

// IsKiCadProvider reports the `pack fab=kicad` spelling.
func IsKiCadProvider(name string) bool {
	switch strings.ToLower(name) {
	case "kicad", "kicad_pcb", "kicad9":
		return true
	}
	return false
}

// packKiCad handles `pack fab=kicad [out=PATH] [zones=filled|outline] [grid=MM]`.
// out may be a directory or a .kicad_pcb file; a directory gets <name>.kicad_pcb.
func packKiCad(p *core.Project, args string) (string, error) {
	opts := kicad.DefaultOptions()
	out := ""
	for _, f := range strings.Fields(args) {
		switch {
		case strings.HasPrefix(f, "fab="):
		case strings.HasPrefix(f, "out="):
			out = strings.TrimPrefix(f, "out=")
		case strings.HasPrefix(f, "zones="):
			opts.FillZones = strings.TrimPrefix(f, "zones=") != "outline"
		case strings.HasPrefix(f, "grid="):
			v, err := strconv.ParseFloat(strings.TrimPrefix(f, "grid="), 64)
			if err != nil || v <= 0 {
				return "", fmt.Errorf("pack: grid= needs a positive millimetre value")
			}
			opts.FillGridMM = v
		case !strings.Contains(f, "=") && out == "":
			out = f
		}
	}
	p.RLock()
	name := p.Name()
	savePath := p.SavePath()
	p.RUnlock()
	if name == "" {
		name = "board"
	}
	opts.Name = name

	if out == "" {
		dir := os.TempDir()
		if savePath != "" {
			dir = filepath.Dir(savePath)
		}
		out = filepath.Join(dir, sanitizeFileName(name)+".kicad_pcb")
	}
	if !strings.EqualFold(filepath.Ext(out), ".kicad_pcb") {
		if err := os.MkdirAll(out, 0o755); err != nil {
			return "", err
		}
		out = filepath.Join(out, sanitizeFileName(name)+".kicad_pcb")
	} else if dir := filepath.Dir(out); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}

	p.RLock()
	board := p.Board()
	text, err := kicad.Export(board, opts)
	nFP, nTrace, nVia, nZone := len(board.Footprints), len(board.Traces), len(board.Vias), len(board.Pours)
	p.RUnlock()
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(out, []byte(text), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %s (KiCad %s: %d footprints, %d segments, %d vias, %d zones)",
		out, kicad.FormatVersion, nFP, nTrace, nVia, nZone), nil
}

func sanitizeFileName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "board"
	}
	return b.String()
}
