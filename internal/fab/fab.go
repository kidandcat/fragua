// Package fab packs manufacturing outputs for JLCPCB / PCBWay / Generic.
package fab

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/mentasystems/fragua/internal/core"
	"github.com/mentasystems/fragua/internal/drc"
	"github.com/mentasystems/fragua/internal/erc"
	"github.com/mentasystems/fragua/internal/gerber"
)

// Provider names.
const (
	Jlcpcb  = "jlcpcb"
	Pcbway  = "pcbway"
	Generic = "generic"
)

// Profile is a fab capability floor.
type Profile struct {
	Name               string
	MinTraceWidthMM    float64
	MinClearanceMM     float64
	MinDrillMM         float64
	MinAnnularRingMM   float64
	MinViaDiameterMM   float64
	MinEdgeClearanceMM float64
	MaxBoardSizeMM     [2]float64
}

// ProfileByName returns a built-in profile.
func ProfileByName(name string) (Profile, error) {
	switch strings.ToLower(name) {
	case Jlcpcb, "jlcpcb-2l", "jlc":
		return Profile{
			Name: "jlcpcb", MinTraceWidthMM: 0.127, MinClearanceMM: 0.127,
			MinDrillMM: 0.2, MinAnnularRingMM: 0.13, MinViaDiameterMM: 0.45,
			MinEdgeClearanceMM: 0.3, MaxBoardSizeMM: [2]float64{500, 500},
		}, nil
	case Pcbway:
		return Profile{
			Name: "pcbway", MinTraceWidthMM: 0.1, MinClearanceMM: 0.1,
			MinDrillMM: 0.2, MinAnnularRingMM: 0.1, MinViaDiameterMM: 0.4,
			MinEdgeClearanceMM: 0.25, MaxBoardSizeMM: [2]float64{500, 500},
		}, nil
	case Generic, "":
		return Profile{
			Name: "generic", MinTraceWidthMM: 0.15, MinClearanceMM: 0.15,
			MinDrillMM: 0.3, MinAnnularRingMM: 0.15, MinViaDiameterMM: 0.6,
			MinEdgeClearanceMM: 0.3, MaxBoardSizeMM: [2]float64{500, 500},
		}, nil
	default:
		return Profile{}, fmt.Errorf("unknown fab profile %q", name)
	}
}

// ToHandle converts to core.FabProfileHandle.
func (p Profile) ToHandle() *core.FabProfileHandle {
	return &core.FabProfileHandle{
		Name: p.Name, MinTraceWidthMM: p.MinTraceWidthMM, MinClearanceMM: p.MinClearanceMM,
		MinDrillMM: p.MinDrillMM, MinAnnularRingMM: p.MinAnnularRingMM,
		MinViaDiameterMM: p.MinViaDiameterMM, MinEdgeClearanceMM: p.MinEdgeClearanceMM,
		MaxBoardSizeMM: p.MaxBoardSizeMM,
	}
}

// PackResult is the outcome of Pack.
type PackResult struct {
	ZipPath    string
	Files      []string
	DRCErrors  int
	ERCErrors  int
	SkippedDRC bool
}

// Pack runs ERC+DRC (warnings ok) and writes a zip of the fab pack.
func Pack(p *core.Project, provider, outDir string) (*PackResult, error) {
	prof, err := ProfileByName(provider)
	if err != nil {
		return nil, err
	}
	p.SetFabProfile(prof.ToHandle())

	p.RLock()
	board := p.Board()
	sch := p.Schematic()
	name := p.Name()
	if name == "" {
		name = "board"
	}
	name = sanitize(name)
	ercRep := erc.Check(sch, board, erc.DefaultOptions())
	drcOpts := drc.DefaultOptions()
	drcOpts.FabProfile = prof.ToHandle()
	if fab := core.ActiveFabRules(board); fab.MinClearanceMM > 0 {
		drcOpts.MinClearance = core.FromMM(fab.MinClearanceMM)
		if fab.MinTraceWidthMM > 0 {
			drcOpts.MinTraceWidth = core.FromMM(fab.MinTraceWidthMM)
		}
		if fab.MinViaDrillMM > 0 {
			drcOpts.MinDrill = core.FromMM(fab.MinViaDrillMM)
		}
		if fab.MinEdgeClearanceMM > 0 {
			drcOpts.EdgeClearance = core.FromMM(fab.MinEdgeClearanceMM)
		}
	}
	drcRep := drc.Check(board, sch, drcOpts)
	p.RUnlock()

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	work := filepath.Join(outDir, name+"-fab")
	_ = os.RemoveAll(work)
	files, err := gerber.WriteFabPack(board, name, work)
	if err != nil {
		return nil, err
	}
	// README
	readme := fmt.Sprintf("Fragua fab pack\nprovider=%s\nerc_errors=%d drc_errors=%d\n",
		prof.Name, ercRep.Errors, drcRep.Errors)
	rp := filepath.Join(work, "README.txt")
	_ = os.WriteFile(rp, []byte(readme), 0o644)
	files = append(files, rp)

	zipPath := filepath.Join(outDir, name+"-"+prof.Name+".zip")
	if err := zipDir(work, zipPath); err != nil {
		return nil, err
	}
	return &PackResult{
		ZipPath:   zipPath,
		Files:     files,
		DRCErrors: drcRep.Errors,
		ERCErrors: ercRep.Errors,
	}, nil
}

func sanitize(s string) string {
	s = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, s)
	if s == "" {
		return "board"
	}
	return s
}

func zipDir(dir, zipPath string) error {
	f, err := os.Create(zipPath)
	if err != nil {
		return err
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	defer zw.Close()
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		w, err := zw.Create(rel)
		if err != nil {
			return err
		}
		r, err := os.Open(path)
		if err != nil {
			return err
		}
		defer r.Close()
		_, err = io.Copy(w, r)
		return err
	})
}
