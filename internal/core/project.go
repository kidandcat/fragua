package core

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ProjectFile is the on-disk .fragua JSON shape.
type ProjectFile struct {
	Name      string      `json:"name"`
	Board     *Board      `json:"board"`
	Schematic *Schematic  `json:"schematic"`
	Palette   []Footprint `json:"palette"`
}

// Project is the live shared project state.
type Project struct {
	mu        sync.RWMutex
	name      string
	board     *Board
	schematic *Schematic
	palette   []Footprint
	// Disk-backed component library (~/.pcb-library/). Never nil after NewProject.
	library  *Library
	savePath string
	bus      *EventBus
	// ops tracks the long operation in flight (route / auto-place / compact).
	// A value, not a pointer: Ops() must never take p.mu, or POST /cancel
	// would deadlock behind the very route it is trying to stop.
	ops OpTracker
	// Fab profile (memory-only, re-adopted per session).
	fabProfile *FabProfileHandle
}

// FabProfileHandle is a named fab capability profile.
type FabProfileHandle struct {
	Name               string
	MinTraceWidthMM    float64
	MinClearanceMM     float64
	MinDrillMM         float64
	MinAnnularRingMM   float64
	MinViaDiameterMM   float64
	MinEdgeClearanceMM float64
	MinHoleToHoleMM    float64
	MinSliverMM        float64
	MaxBoardSizeMM     [2]float64
}

// NewProject creates an empty in-memory project with the default library open.
func NewProject(name string) *Project {
	return &Project{
		name:      name,
		board:     NewBoard(),
		schematic: NewSchematic(),
		library:   openLibraryBestEffort(),
		bus:       NewEventBus(),
	}
}

// Name returns the project name.
func (p *Project) Name() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.name
}

// Board returns the live board pointer. Callers must hold RLock/Lock
// themselves when concurrent access is possible. Does not lock.
func (p *Project) Board() *Board {
	return p.board
}

// Schematic returns the live schematic pointer. Does not lock — pair with RLock/Lock.
func (p *Project) Schematic() *Schematic {
	return p.schematic
}

// Lock acquires the write lock (host/script mutators).
func (p *Project) Lock() { p.mu.Lock() }

// Unlock releases the write lock.
func (p *Project) Unlock() { p.mu.Unlock() }

// RLock acquires the read lock.
func (p *Project) RLock() { p.mu.RLock() }

// RUnlock releases the read lock.
func (p *Project) RUnlock() { p.mu.RUnlock() }

// Events returns the event bus.
func (p *Project) Events() *EventBus { return p.bus }

// Library returns the disk-backed component library (never nil after NewProject/LoadFromPath).
func (p *Project) Library() *Library {
	if p.library == nil {
		p.library = openLibraryBestEffort()
	}
	return p.library
}

// SetLibrary replaces the project library (tests / alternate roots).
func (p *Project) SetLibrary(lib *Library) {
	p.mu.Lock()
	p.library = lib
	p.mu.Unlock()
}

// SavePath returns the bound autosave path (may be empty).
func (p *Project) SavePath() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.savePath
}

// SetSavePath binds autosave target.
func (p *Project) SetSavePath(path string) {
	p.mu.Lock()
	p.savePath = path
	p.mu.Unlock()
}

// SnapshotFile builds a ProjectFile for serialization.
func (p *Project) SnapshotFile() ProjectFile {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return ProjectFile{
		Name:      p.name,
		Board:     p.board,
		Schematic: p.schematic,
		Palette:   p.palette,
	}
}

// LoadFromPath loads a .fragua / .json project file.
func LoadFromPath(path string) (*Project, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var pf ProjectFile
	if err := json.Unmarshal(data, &pf); err != nil {
		return nil, fmt.Errorf("parse project: %w", err)
	}
	p := NewProject(pf.Name)
	if pf.Board != nil {
		p.board = pf.Board
		if p.board.Footprints == nil {
			p.board.Footprints = make(map[string]*Footprint)
		}
		if p.board.Stackup == nil || len(p.board.Stackup.Layers) == 0 {
			s := Default2Layer()
			p.board.Stackup = &s
		}
	}
	if pf.Schematic != nil {
		p.schematic = pf.Schematic
	}
	p.palette = pf.Palette
	p.savePath = path
	p.bus.Publish(Event{Kind: EventProjectChanged})
	return p, nil
}

// SaveToPath writes the project JSON atomically.
func (p *Project) SaveToPath(path string) error {
	pf := p.SnapshotFile()
	data, err := json.MarshalIndent(pf, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	p.SetSavePath(path)
	p.bus.Publish(Event{Kind: EventSaved, Path: path})
	return nil
}

// SetFabProfile sets the session fab profile.
func (p *Project) SetFabProfile(h *FabProfileHandle) {
	p.mu.Lock()
	p.fabProfile = h
	p.mu.Unlock()
}

// FabProfile returns the current fab profile (may be nil).
func (p *Project) FabProfile() *FabProfileHandle {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.fabProfile
}

// Reset clears board, schematic and palette (script `reset`).
func (p *Project) Reset() {
	p.mu.Lock()
	p.board = NewBoard()
	p.schematic = NewSchematic()
	p.palette = nil
	p.mu.Unlock()
	p.bus.Publish(Event{Kind: EventProjectChanged})
}

// MutateBoard runs fn with exclusive access to the board.
func (p *Project) MutateBoard(fn func(b *Board)) {
	p.mu.Lock()
	fn(p.board)
	p.mu.Unlock()
	p.bus.Publish(Event{Kind: EventBoardChanged})
}

// MutateSchematic runs fn with exclusive access to the schematic.
func (p *Project) MutateSchematic(fn func(s *Schematic)) {
	p.mu.Lock()
	fn(p.schematic)
	p.mu.Unlock()
	p.bus.Publish(Event{Kind: EventSchematicChanged})
}

// PutLibrary inserts or replaces a library entry on disk (via Library.Put).
func (p *Project) PutLibrary(e LibraryEntry) (LibraryEntry, error) {
	return p.Library().Put(e)
}

// FindLibrary returns a copy of the library entry by key, or false if missing.
func (p *Project) FindLibrary(key string) (LibraryEntry, bool) {
	return p.Library().Get(key)
}

// Palette returns palette items (caller should hold RLock/Lock). Does not lock.
func (p *Project) Palette() []Footprint {
	return p.palette
}

// PaletteAdd appends a footprint to the palette.
func (p *Project) PaletteAdd(fp Footprint) {
	p.mu.Lock()
	p.palette = append(p.palette, fp)
	p.mu.Unlock()
	p.bus.Publish(Event{Kind: EventBoardChanged})
}

// PaletteTake removes and returns the palette item with the given reference.
func (p *Project) PaletteTake(ref string) (*Footprint, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.palette {
		if p.palette[i].Reference == ref {
			fp := p.palette[i]
			p.palette = append(p.palette[:i], p.palette[i+1:]...)
			return &fp, true
		}
	}
	return nil, false
}

// Stackup helpers on LayerStackup ─────────────────────────────────────

// FindLayerByName returns the copper layer with the given stackup name.
func (s *LayerStackup) FindLayerByName(name string) (Layer, bool) {
	for i, ls := range s.Layers {
		if ls.Name == name {
			return Layer{Index: uint8(i)}, true
		}
	}
	return Layer{}, false
}

// PushLayer inserts a copper layer just above the physical bottom (last
// entry). Appending after B.Cu used to leave the file named B.Cu as an
// inner layer and the real bottom named In2.Cu.
func (s *LayerStackup) PushLayer(spec LayerSpec, slab Dielectric) {
	n := len(s.Layers)
	if n < 2 {
		s.Layers = append(s.Layers, spec)
		s.Dielectrics = append(s.Dielectrics, slab)
		return
	}
	last := s.Layers[n-1]
	s.Layers = append(s.Layers[:n-1], spec, last)
	s.Dielectrics = append(s.Dielectrics, slab)
}

// RemoveNamed removes a copper layer by name. Returns false if not found or would leave <2 layers.
func (s *LayerStackup) RemoveNamed(name string) bool {
	if len(s.Layers) <= 2 {
		return false
	}
	idx := -1
	for i, ls := range s.Layers {
		if ls.Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false
	}
	s.Layers = append(s.Layers[:idx], s.Layers[idx+1:]...)
	if len(s.Dielectrics) > 0 {
		slabIdx := idx
		if slabIdx >= len(s.Dielectrics) {
			slabIdx = len(s.Dielectrics) - 1
		}
		s.Dielectrics = append(s.Dielectrics[:slabIdx], s.Dielectrics[slabIdx+1:]...)
	}
	return true
}

// FabRulesPreset returns a built-in fab floor.
//
// jlcpcb / jlcpcb-2l is JLCPCB *standard* 2-layer (via 0.30 / 0.60).
// jlcpcb-2l-via02 is the 0.20 mm via capability (extra, not the default).
// jlcpcb-4l uses the same standard via; jlcpcb-4l-via02 is the 4L opt-in.
func FabRulesPreset(name string) *FabRules {
	sz := [2]float64{500, 500}
	switch strings.ToLower(name) {
	case "jlcpcb-2l", "jlcpcb_2l", "jlcpcb-2", "jlcpcb", "jlc":
		return &FabRules{
			Preset: "jlcpcb-2l", MinTraceWidthMM: 0.127, MinClearanceMM: 0.127,
			MinViaDrillMM: 0.30, MinViaDiameterMM: 0.60, MinAnnularRingMM: 0.15,
			MinEdgeClearanceMM: 0.30, MinHoleToHoleMM: 0.50, MinSliverMM: 0.15,
			MaxBoardSizeMM: &sz,
		}
	// The via02 presets' annular floor is 0.125 mm, not 0.13: a 0.20 mm drill
	// in the 0.45 mm pad the same preset allows leaves exactly 0.125 mm of
	// ring, so a 0.13 rule failed every via the preset's own minimum produces.
	case "jlcpcb-2l-via02", "jlcpcb-2l-via0.2", "jlcpcb_2l_via02":
		return &FabRules{
			Preset: "jlcpcb-2l-via02", MinTraceWidthMM: 0.127, MinClearanceMM: 0.127,
			MinViaDrillMM: 0.20, MinViaDiameterMM: 0.45, MinAnnularRingMM: 0.125,
			MinEdgeClearanceMM: 0.30, MinHoleToHoleMM: 0.50, MinSliverMM: 0.15,
			MaxBoardSizeMM: &sz,
		}
	case "jlcpcb-4l", "jlcpcb_4l", "jlcpcb-4":
		return &FabRules{
			Preset: "jlcpcb-4l", MinTraceWidthMM: 0.0889, MinClearanceMM: 0.0889,
			MinViaDrillMM: 0.30, MinViaDiameterMM: 0.60, MinAnnularRingMM: 0.15,
			MinEdgeClearanceMM: 0.30, MinHoleToHoleMM: 0.50, MinSliverMM: 0.15,
			MaxBoardSizeMM: &sz,
		}
	case "jlcpcb-4l-via02", "jlcpcb-4l-via0.2", "jlcpcb_4l_via02":
		return &FabRules{
			Preset: "jlcpcb-4l-via02", MinTraceWidthMM: 0.0889, MinClearanceMM: 0.0889,
			MinViaDrillMM: 0.20, MinViaDiameterMM: 0.45, MinAnnularRingMM: 0.125,
			MinEdgeClearanceMM: 0.30, MinHoleToHoleMM: 0.50, MinSliverMM: 0.15,
			MaxBoardSizeMM: &sz,
		}
	default:
		return nil
	}
}
