// Package bench runs the reference boards under bench/ through the same loop
// an agent runs — auto-place → route → drc — and reports one row per board.
// Numbers are measured, never estimated: a board that fails to route says so.
package bench

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/mentasystems/fragua/internal/core"
	"github.com/mentasystems/fragua/internal/drc"
	"github.com/mentasystems/fragua/internal/placer"
	"github.com/mentasystems/fragua/internal/router"
	"github.com/mentasystems/fragua/internal/script"
)

// Options controls one bench run.
type Options struct {
	Seed          uint64  // placer seed; the same seed gives the same placement
	BudgetSeconds float64 // per-board router wall budget
}

// DefaultOptions is the published bench configuration.
func DefaultOptions() Options {
	return Options{Seed: 42, BudgetSeconds: 60}
}

// Result is one bench row.
type Result struct {
	Name        string  `json:"name"`
	File        string  `json:"file"`
	Layers      int     `json:"layers"`
	Parts       int     `json:"parts"`
	Nets        int     `json:"nets"`
	RoutedNets  int     `json:"routed_nets"`
	Traces      int     `json:"traces"`
	Vias        int     `json:"vias"`
	LengthMM    float64 `json:"trace_length_mm"`
	DRCErrors   int     `json:"drc_errors"`
	DRCWarnings int     `json:"drc_warnings"`
	// DRCKinds counts error violations by kind, so a DRC number in the table
	// can always be traced back to what actually failed.
	DRCKinds   map[string]int `json:"drc_error_kinds,omitempty"`
	AutoPlaced bool           `json:"auto_placed"`
	WallMS     int64          `json:"wall_ms"`
	Error      string         `json:"error,omitempty"`
}

// topKind is the violation kind that dominates a board's DRC errors.
func (r Result) topKind() (string, int) {
	best, n := "", 0
	for k, c := range r.DRCKinds {
		if c > n || (c == n && k < best) {
			best, n = k, c
		}
	}
	return best, n
}

// Meta records what produced a run so a table can be reproduced.
type Meta struct {
	FraguaVersion string  `json:"fragua_version"`
	Go            string  `json:"go"`
	OS            string  `json:"os"`
	Arch          string  `json:"arch"`
	CPUs          int     `json:"cpus"`
	Seed          uint64  `json:"seed"`
	BudgetSeconds float64 `json:"budget_seconds"`
	GeneratedAt   string  `json:"generated_at"`
}

// Run is the whole suite: every board file in dir, in name order.
type Run struct {
	Meta    Meta     `json:"meta"`
	Results []Result `json:"results"`
}

// Discover lists the board files in dir (*.fragua projects and *.txt scripts).
// A path to a single board file is accepted too, so one board can be iterated
// on without waiting for the suite.
func Discover(dir string) ([]string, error) {
	if fi, err := os.Stat(dir); err == nil && !fi.IsDir() {
		return []string{dir}, nil
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext != ".fragua" && ext != ".txt" {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	sort.Strings(out)
	return out, nil
}

// RunDir benches every board in dir.
func RunDir(dir string, opts Options) (Run, error) {
	files, err := Discover(dir)
	if err != nil {
		return Run{}, err
	}
	if len(files) == 0 {
		return Run{}, fmt.Errorf("bench: no board files in %s", dir)
	}
	run := Run{Meta: Meta{
		FraguaVersion: core.Version,
		Go:            runtime.Version(),
		OS:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		CPUs:          runtime.NumCPU(),
		Seed:          opts.Seed,
		BudgetSeconds: opts.BudgetSeconds,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
	}}
	for _, f := range files {
		run.Results = append(run.Results, RunFile(f, opts))
	}
	return run, nil
}

// RunFile loads one board and takes it through place → route → drc.
func RunFile(path string, opts Options) Result {
	start := time.Now()
	res := Result{File: filepath.Base(path), Name: boardName(path)}
	p, autoPlace, err := load(path)
	if err != nil {
		res.Error = err.Error()
		res.WallMS = time.Since(start).Milliseconds()
		return res
	}
	if n := p.Name(); n != "" {
		res.Name = n
	}

	p.MutateBoard(func(b *core.Board) { b.ClearRoute() })
	if autoPlace {
		popts := placer.DefaultOptions()
		popts.Seed = opts.Seed
		var perr error
		p.MutateBoard(func(b *core.Board) { _, perr = placer.Place(b, nil, popts) })
		if perr != nil {
			res.Error = "auto-place: " + perr.Error()
			res.WallMS = time.Since(start).Milliseconds()
			return res
		}
		res.AutoPlaced = true
	}

	ropts := router.DefaultOptions()
	ropts.MaxSeconds = opts.BudgetSeconds
	var rep router.Report
	p.MutateBoard(func(b *core.Board) {
		ropts.Schematic = p.Schematic()
		rep = router.Route(b, ropts)
	})

	p.RLock()
	board := p.Board()
	stack := board.StackupOrDefault()
	res.Layers = stack.CopperCount()
	res.Parts = len(board.Footprints)
	res.Nets = len(rep.PerNet)
	for _, n := range rep.PerNet {
		if n.Outcome.Status == "ok" {
			res.RoutedNets++
		}
	}
	res.Traces = rep.TraceCount
	res.Vias = rep.ViaCount
	res.LengthMM = rep.TotalLengthMM
	drcRep := drc.Check(board, p.Schematic(), drcOptions(board))
	p.RUnlock()
	res.DRCErrors = drcRep.Errors
	res.DRCWarnings = drcRep.Warnings
	for _, v := range drcRep.Violations {
		if v.Severity != drc.SeverityError {
			continue
		}
		if res.DRCKinds == nil {
			res.DRCKinds = map[string]int{}
		}
		res.DRCKinds[string(v.Kind)]++
	}
	res.WallMS = time.Since(start).Milliseconds()
	return res
}

// load reads a board file. A JSON project is taken as placed; a script is run
// and then auto-placed unless it placed the parts itself.
func load(path string) (p *core.Project, autoPlace bool, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false, err
	}
	text := string(raw)
	if strings.HasPrefix(strings.TrimSpace(text), "{") {
		p, err = core.LoadFromPath(path)
		if err != nil {
			return nil, false, err
		}
		// A saved project has no comments, so it carries the directive in a
		// "bench" key the project loader ignores.
		var meta struct {
			Bench string `json:"bench"`
		}
		_ = json.Unmarshal(raw, &meta)
		return p, placeDirective(meta.Bench, false), nil
	}
	p = core.NewProject(boardName(path))
	for _, r := range script.RunScript(p, text) {
		if !r.OK {
			return nil, false, fmt.Errorf("line %d %s: %s", r.Line, r.Tool, r.Result)
		}
	}
	return p, placeDirective(text, !scriptPlaces(text)), nil
}

// placeDirective honours `# bench: place=auto|manual` in the board file.
func placeDirective(text string, def bool) bool {
	for _, line := range strings.Split(text, "\n") {
		l := strings.TrimSpace(line)
		if !strings.HasPrefix(l, "# bench:") {
			continue
		}
		switch {
		case strings.Contains(l, "place=auto"):
			return true
		case strings.Contains(l, "place=manual"):
			return false
		}
	}
	return def
}

// scriptPlaces reports whether the script already ran the placer itself. A
// plain `place` does not count: a script has to drop its parts on the board
// before anything can move them, and optimising that drop is the measurement.
func scriptPlaces(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue
		}
		verb, _, _ := strings.Cut(strings.TrimSpace(line), " ")
		switch strings.ToLower(verb) {
		case "auto-place", "auto_place", "compact":
			return true
		}
	}
	return false
}

// drcOptions mirrors the pack path: the board's fab rules are the floor.
func drcOptions(board *core.Board) drc.Options {
	o := drc.DefaultOptions()
	fab := core.ActiveFabRules(board)
	if fab.MinClearanceMM > 0 {
		o.MinClearance = core.FromMM(fab.MinClearanceMM)
	}
	if fab.MinTraceWidthMM > 0 {
		o.MinTraceWidth = core.FromMM(fab.MinTraceWidthMM)
	}
	if fab.MinViaDrillMM > 0 {
		o.MinDrill = core.FromMM(fab.MinViaDrillMM)
	}
	if fab.MinAnnularRingMM > 0 {
		o.MinAnnularRing = core.FromMM(fab.MinAnnularRingMM)
	}
	if fab.MinEdgeClearanceMM > 0 {
		o.EdgeClearance = core.FromMM(fab.MinEdgeClearanceMM)
	}
	if fab.MinHoleToHoleMM > 0 {
		o.MinHoleToHole = core.FromMM(fab.MinHoleToHoleMM)
	}
	if fab.MinSliverMM > 0 {
		o.MinSliver = core.FromMM(fab.MinSliverMM)
	}
	return o
}

func boardName(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// Markdown renders the published table.
func (r Run) Markdown() string {
	var b strings.Builder
	b.WriteString("| board | layers | place | parts | nets | routed | DRC err | vias | trace mm | wall s |\n")
	b.WriteString("|---|--:|:--|--:|--:|--:|--:|--:|--:|--:|\n")
	var totWall int64
	for _, x := range r.Results {
		totWall += x.WallMS
		if x.Error != "" {
			fmt.Fprintf(&b, "| %s | — | — | — | — | — | — | — | — | %.1f |\n", x.Name, float64(x.WallMS)/1000)
			continue
		}
		place := "given"
		if x.AutoPlaced {
			place = "auto"
		}
		fmt.Fprintf(&b, "| %s | %d | %s | %d | %d | %d/%d | %d | %d | %.0f | %.1f |\n",
			x.Name, x.Layers, place, x.Parts, x.Nets, x.RoutedNets, x.Nets,
			x.DRCErrors, x.Vias, x.LengthMM, float64(x.WallMS)/1000)
	}
	fmt.Fprintf(&b, "\nTotal wall time: %.1f s — seed %d, per-board router budget %.0f s, Fragua %s, %s %s/%s, %d CPUs, generated %s.\n",
		float64(totWall)/1000, r.Meta.Seed, r.Meta.BudgetSeconds, r.Meta.FraguaVersion,
		r.Meta.Go, r.Meta.OS, r.Meta.Arch, r.Meta.CPUs, r.Meta.GeneratedAt)
	b.WriteString("Placement is deterministic for a given seed. Routing is not: the router " +
		"spends a wall-clock slice per net, so an idle machine searches further than a busy one " +
		"and the routed / vias / copper columns move a little between runs.\n")
	for _, x := range r.Results {
		switch {
		case x.Error != "":
			fmt.Fprintf(&b, "\n%s FAILED: %s\n", x.Name, x.Error)
		case x.DRCErrors > 0:
			kind, n := x.topKind()
			fmt.Fprintf(&b, "\n%s: %d DRC errors, %d of them %s.\n", x.Name, x.DRCErrors, n, kind)
		}
	}
	return b.String()
}

// JSON is the machine-readable form of the same run.
func (r Run) JSON() ([]byte, error) { return json.MarshalIndent(r, "", "  ") }

// Failures counts boards that did not run at all (bad script, load error).
func (r Run) Failures() int {
	n := 0
	for _, x := range r.Results {
		if x.Error != "" {
			n++
		}
	}
	return n
}

// Unclean counts boards that ran but did not fully route or have DRC errors.
// Not every bench board is meant to be clean — rp2040-minimal is the known
// hard case — so this is a `--strict` signal, not a failure by itself.
func (r Run) Unclean() int {
	n := 0
	for _, x := range r.Results {
		if x.Error == "" && (x.DRCErrors > 0 || x.RoutedNets != x.Nets) {
			n++
		}
	}
	return n
}
