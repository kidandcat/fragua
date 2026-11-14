package script

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mentasystems/fragua/internal/core"
	"github.com/mentasystems/fragua/internal/dxf"
)

// IsDXFProvider reports whether a pack fab= value means the mechanical DXF.
func IsDXFProvider(name string) bool {
	return strings.EqualFold(name, "dxf")
}

// packDXF handles `pack fab=dxf [out=PATH]`: the outline, cutouts, holes and
// component envelopes as an R12 DXF for enclosure CAD. out may be a directory
// or a .dxf file; a directory gets <name>.dxf. Unlike the fab bundle it does
// not need a clean ERC: the enclosure is often drawn before the schematic is
// done.
func packDXF(p *core.Project, args string) (string, error) {
	out := ""
	for _, f := range strings.Fields(args) {
		switch {
		case strings.HasPrefix(f, "fab="):
		case strings.HasPrefix(f, "out="):
			out = strings.TrimPrefix(f, "out=")
		case strings.Contains(f, "="):
			return "", fmt.Errorf("pack fab=dxf: unknown option %q (only out=)", f)
		case out == "":
			out = f
		}
	}
	p.RLock()
	name := p.Name()
	savePath := p.SavePath()
	board := p.Board()
	hasOutline := board.Outline != nil || len(board.OutlinePoly) >= 3
	var text string
	var st dxf.Stats
	if hasOutline {
		text, st = dxf.Write(board)
	}
	p.RUnlock()
	if !hasOutline {
		return "", fmt.Errorf("pack fab=dxf: the board has no outline yet (run `outline W H` first)")
	}
	if name == "" {
		name = "board"
	}
	if out == "" {
		dir := os.TempDir()
		if savePath != "" {
			dir = filepath.Dir(savePath)
		}
		out = filepath.Join(dir, sanitizeFileName(name)+".dxf")
	}
	if !strings.EqualFold(filepath.Ext(out), ".dxf") {
		if err := os.MkdirAll(out, 0o755); err != nil {
			return "", err
		}
		out = filepath.Join(out, sanitizeFileName(name)+".dxf")
	} else if dir := filepath.Dir(out); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(out, []byte(text), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %s (DXF mm: outline %.2fx%.2f, %d cutouts, %d npth, %d pth, %d top / %d bottom parts)",
		out, st.OutlineMM[0], st.OutlineMM[1], st.Cutouts, st.NPTH, st.PTH, st.CompTop, st.CompBottom), nil
}
