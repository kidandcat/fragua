package script

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mentasystems/fragua/internal/core"
	"github.com/mentasystems/fragua/internal/drc"
	"github.com/mentasystems/fragua/internal/erc"
	"github.com/mentasystems/fragua/internal/fab"
	"github.com/mentasystems/fragua/internal/placer"
	"github.com/mentasystems/fragua/internal/render"
	"github.com/mentasystems/fragua/internal/router"
)

// Result is one script line outcome.
type Result struct {
	Line   int    `json:"line"`
	Tool   string `json:"tool"`
	OK     bool   `json:"ok"`
	Result string `json:"result"`
}

// openBlock accumulates indented pin/pad lines under sym/lib.
type openBlock struct {
	verb   string
	line   int
	args   string
	pins   []core.SchPin
	pads   []core.LibraryPad
}

// RunScript executes a multi-line script against project.
func RunScript(p *core.Project, script string) []Result {
	var results []Result
	sc := bufio.NewScanner(strings.NewReader(script))
	lineNo := 0
	var block *openBlock

	flushBlock := func() bool {
		if block == nil {
			return true
		}
		b := block
		block = nil
		msg, err := finishBlock(p, b)
		r := Result{Line: b.line, Tool: b.verb, OK: err == nil, Result: msg}
		if err != nil {
			r.Result = err.Error()
		}
		results = append(results, r)
		return err == nil
	}

	for sc.Scan() {
		lineNo++
		raw := sc.Text()
		// strip trailing whitespace; keep leading for indent detect
		raw = strings.TrimRight(raw, " \t\r")
		if strings.TrimSpace(raw) == "" {
			continue
		}
		trim := strings.TrimSpace(raw)
		if strings.HasPrefix(trim, "#") || strings.HasPrefix(trim, "//") {
			continue
		}
		// strip inline comments
		if i := strings.Index(trim, " #"); i >= 0 {
			trim = strings.TrimSpace(trim[:i])
		}
		indented := strings.HasPrefix(raw, " ") || strings.HasPrefix(raw, "\t")
		if indented {
			if block == nil {
				results = append(results, Result{
					Line: lineNo, Tool: "?", OK: false,
					Result: "indented line without open sym/lib block",
				})
				return results
			}
			if err := absorbContinuation(block, lineNo, trim); err != nil {
				results = append(results, Result{
					Line: lineNo, Tool: block.verb, OK: false, Result: err.Error(),
				})
				return results
			}
			continue
		}
		// non-indented: finish any open block first
		if !flushBlock() {
			return results
		}
		tool, args := splitVerb(trim)
		if tool == "sym" || tool == "lib" {
			block = &openBlock{verb: tool, line: lineNo, args: args}
			continue
		}
		msg, err := dispatch(p, tool, args)
		r := Result{Line: lineNo, Tool: tool, OK: err == nil, Result: msg}
		if err != nil {
			r.Result = err.Error()
		}
		results = append(results, r)
		if err != nil {
			return results
		}
	}
	_ = flushBlock()
	return results
}

// FormatResults renders results as text/plain for agents.
func FormatResults(rs []Result) string {
	var b strings.Builder
	for _, r := range rs {
		if r.OK {
			fmt.Fprintf(&b, "ok %s: %s\n", r.Tool, r.Result)
		} else {
			fmt.Fprintf(&b, "error line %d %s: %s\n", r.Line, r.Tool, r.Result)
		}
	}
	return b.String()
}

func splitVerb(line string) (tool string, args string) {
	parts := strings.SplitN(line, " ", 2)
	tool = strings.ToLower(parts[0])
	if len(parts) > 1 {
		args = strings.TrimSpace(parts[1])
	}
	return tool, args
}

func absorbContinuation(b *openBlock, line int, body string) error {
	fields := tokenize(body)
	if len(fields) == 0 {
		return nil
	}
	sub := strings.ToLower(fields[0])
	switch {
	case b.verb == "sym" && sub == "pin":
		// pin NUMBER SIDE [NAME] [name=] [role=]
		if len(fields) < 3 {
			return fmt.Errorf("pin needs: pin NUMBER SIDE [NAME] [role=ROLE]")
		}
		pin := core.SchPin{
			Number: fields[1],
			Side:   expandSide(fields[2]),
			Role:   core.PinPassive,
		}
		for _, t := range fields[3:] {
			if strings.HasPrefix(t, "name=") {
				pin.Name = strings.TrimPrefix(t, "name=")
			} else if strings.HasPrefix(t, "role=") {
				pin.Role = expandRole(strings.TrimPrefix(t, "role="))
			} else if !strings.Contains(t, "=") {
				pin.Name = t
			}
		}
		b.pins = append(b.pins, pin)
		return nil
	case b.verb == "lib" && sub == "pad":
		// pad NUMBER X Y W H [name=NAME] [drill=MM]
		if len(fields) < 6 {
			return fmt.Errorf("pad needs: pad NUMBER X Y W H [name=NAME] [drill=MM]")
		}
		x, err1 := strconv.ParseFloat(fields[2], 64)
		y, err2 := strconv.ParseFloat(fields[3], 64)
		w, err3 := strconv.ParseFloat(fields[4], 64)
		h, err4 := strconv.ParseFloat(fields[5], 64)
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
			return fmt.Errorf("pad: bad numbers")
		}
		pad := core.LibraryPad{Number: fields[1], XMM: x, YMM: y, WMM: w, HMM: h}
		for _, t := range fields[6:] {
			if strings.HasPrefix(t, "name=") {
				pad.Name = strings.TrimPrefix(t, "name=")
			} else if strings.HasPrefix(t, "drill=") {
				d, err := strconv.ParseFloat(strings.TrimPrefix(t, "drill="), 64)
				if err != nil {
					return fmt.Errorf("pad drill: %w", err)
				}
				pad.DrillMM = &d
			}
		}
		b.pads = append(b.pads, pad)
		return nil
	default:
		return fmt.Errorf("`%s` block can't contain `%s`", b.verb, sub)
	}
}

func finishBlock(p *core.Project, b *openBlock) (string, error) {
	switch b.verb {
	case "sym":
		return addSymbol(p, b.args, b.pins)
	case "lib":
		return addLibrary(p, b.args, b.pads)
	default:
		return "", fmt.Errorf("unknown block %q", b.verb)
	}
}

func dispatch(p *core.Project, tool, args string) (string, error) {
	switch tool {
	case "status", "view":
		return status(p), nil
	case "clear-route", "clear_route":
		p.MutateBoard(func(b *core.Board) { b.ClearRoute() })
		return "cleared traces and vias", nil
	case "drc":
		p.RLock()
		rep := drc.Check(p.Board(), p.Schematic(), drc.DefaultOptions())
		p.RUnlock()
		return rep.Summary(), nil
	case "erc":
		p.RLock()
		rep := erc.Check(p.Schematic(), p.Board(), erc.DefaultOptions())
		p.RUnlock()
		return rep.Summary(), nil
	case "route":
		opts := router.DefaultOptions()
		opts = router.ParseOptions(opts, args)
		var rep router.Report
		p.MutateBoard(func(b *core.Board) {
			rep = router.Route(b, opts)
		})
		return rep.Summary(), nil
	case "auto-place", "auto_place":
		opts := placer.DefaultOptions()
		opts = placer.ParseOptions(opts, args)
		var rep placer.Report
		var err error
		p.MutateBoard(func(b *core.Board) {
			rep, err = placer.Place(b, nil, opts)
		})
		if err != nil {
			return "", err
		}
		return rep.Summary(), nil
	case "outline":
		return setOutline(p, args)
	case "place":
		return placeOne(p, args)
	case "pack":
		return packBoard(p, args)
	case "net":
		return addNet(p, args)
	case "delete-trace", "delete_trace":
		return deleteTrace(p, args)
	case "save":
		path := strings.TrimSpace(args)
		if path == "" {
			path = p.SavePath()
		}
		if path == "" {
			return "", fmt.Errorf("save: path required")
		}
		if err := p.SaveToPath(path); err != nil {
			return "", err
		}
		return "saved " + path, nil
	case "help":
		return "see GET /help", nil
	case "sym":
		return addSymbol(p, args, nil)
	case "lib":
		return addLibrary(p, args, nil)
	case "palette":
		return paletteCmd(p, args)
	case "trace":
		return addTrace(p, args)
	case "via":
		return addVia(p, args)
	case "rule-area", "rule_area":
		return addRuleArea(p, args)
	case "fab-rules", "fab_rules":
		return setFabRules(p, args)
	case "layer":
		return layerCmd(p, args)
	case "compact":
		return "", fmt.Errorf("compact: not yet implemented in Go port (use auto-place + outline manually)")
	case "screenshot":
		return screenshot(p, args)
	case "list-lib", "lib-list", "list_lib", "lib_list":
		return listLib(p), nil
	default:
		return "", fmt.Errorf("unknown verb %q (Go port in progress — see PORT_GO.md)", tool)
	}
}

// listLib prints one library entry per line (agent-parseable, like Rust list-lib).
func listLib(p *core.Project) string {
	lib := p.Library()
	keys := lib.List()
	if len(keys) == 0 {
		return "0 entries in library"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d entries in library\n", len(keys))
	for _, key := range keys {
		e, ok := lib.Get(key)
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "%s pads=%d", e.Key, len(e.Pads))
		if e.EdgeMounted {
			b.WriteString(" edge")
			if e.EdgeSide != nil {
				fmt.Fprintf(&b, "=%s", *e.EdgeSide)
			}
		}
		if e.Elevated {
			b.WriteString(" elevated")
		}
		if e.BodyRect != nil {
			b.WriteString(" body")
		}
		if e.Description != "" {
			fmt.Fprintf(&b, " %q", e.Description)
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

func status(p *core.Project) string {
	// Name() takes its own RLock — fetch before holding project lock.
	name := p.Name()
	p.RLock()
	defer p.RUnlock()
	b := p.Board()
	sch := p.Schematic()
	ow, oh := 0.0, 0.0
	if b.Outline != nil {
		ow = b.Outline.Width().ToMM()
		oh = b.Outline.Height().ToMM()
	}
	return fmt.Sprintf(
		"name=%q footprints=%d traces=%d vias=%d nets=%d symbols=%d palette=%d outline=%.2fx%.2fmm",
		name, len(b.Footprints), len(b.Traces), len(b.Vias), len(sch.Nets),
		len(sch.Symbols), len(p.Palette()), ow, oh,
	)
}

func setOutline(p *core.Project, args string) (string, error) {
	fields := strings.Fields(args)
	if len(fields) < 2 {
		return "", fmt.Errorf("outline W H [radius=R] expected")
	}
	var w, h, rad float64
	if _, err := fmt.Sscanf(fields[0], "%f", &w); err != nil {
		return "", fmt.Errorf("outline width: %w", err)
	}
	if _, err := fmt.Sscanf(fields[1], "%f", &h); err != nil {
		return "", fmt.Errorf("outline height: %w", err)
	}
	for _, f := range fields[2:] {
		if strings.HasPrefix(f, "radius=") {
			fmt.Sscanf(f, "radius=%f", &rad)
		}
	}
	p.MutateBoard(func(b *core.Board) {
		r := core.RectFromCorners(core.Origin, core.NewPoint(core.FromMM(w), core.FromMM(h)))
		b.Outline = &r
		if rad > 0 {
			b.OutlineCornerRadius = core.FromMM(rad)
		}
	})
	return fmt.Sprintf("outline %.2f x %.2f mm", w, h), nil
}

func placeOne(p *core.Project, args string) (string, error) {
	// place REF x y [rot=DEG] or [ROT_DEG]
	fields := strings.Fields(args)
	if len(fields) < 3 {
		return "", fmt.Errorf("place REF x y [rot=DEG]")
	}
	ref := fields[0]
	x, err1 := strconv.ParseFloat(fields[1], 64)
	y, err2 := strconv.ParseFloat(fields[2], 64)
	if err1 != nil || err2 != nil {
		return "", fmt.Errorf("place: bad coordinates")
	}
	rotSet := false
	rot := 0.0
	for _, f := range fields[3:] {
		if strings.HasPrefix(f, "rot=") {
			fmt.Sscanf(f, "rot=%f", &rot)
			rotSet = true
		} else if !strings.Contains(f, "=") {
			if v, err := strconv.ParseFloat(f, 64); err == nil {
				rot = v
				rotSet = true
			}
		}
	}
	// Prefer re-placing an existing board footprint.
	var found bool
	p.MutateBoard(func(b *core.Board) {
		fp := b.FootprintByRef(ref)
		if fp == nil {
			return
		}
		fp.Position = core.NewPoint(core.FromMM(x), core.FromMM(y))
		if rotSet {
			fp.Rotation = rot
		}
		found = true
	})
	if found {
		return fmt.Sprintf("placed %s at %.2f,%.2f rot=%.0f", ref, x, y, rot), nil
	}
	// Else take from palette.
	fp, ok := p.PaletteTake(ref)
	if !ok {
		return "", fmt.Errorf("place: unknown ref %q (not on board or palette)", ref)
	}
	fp.Position = core.NewPoint(core.FromMM(x), core.FromMM(y))
	if rotSet {
		fp.Rotation = rot
	}
	p.MutateBoard(func(b *core.Board) {
		b.AddFootprint(fp)
	})
	return fmt.Sprintf("placed %s at %.2f,%.2f rot=%.0f", ref, x, y, fp.Rotation), nil
}

func packBoard(p *core.Project, args string) (string, error) {
	provider := "jlcpcb"
	out := ""
	for _, f := range strings.Fields(args) {
		if strings.HasPrefix(f, "fab=") {
			provider = strings.TrimPrefix(f, "fab=")
		} else if strings.HasPrefix(f, "out=") {
			out = strings.TrimPrefix(f, "out=")
		} else if !strings.Contains(f, "=") && out == "" {
			out = f
		}
	}
	if out == "" {
		if sp := p.SavePath(); sp != "" {
			out = filepath.Join(filepath.Dir(sp), "fab")
		} else {
			out = filepath.Join(os.TempDir(), "fragua-fab")
		}
	}
	res, err := fab.Pack(p, provider, out)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("packed %s (erc_err=%d drc_err=%d)", res.ZipPath, res.ERCErrors, res.DRCErrors), nil
}

func addNet(p *core.Project, args string) (string, error) {
	// net NAME REF.PIN REF.PIN ...
	fields := strings.Fields(args)
	if len(fields) < 2 {
		return "", fmt.Errorf("net NAME REF.PIN ...")
	}
	name := fields[0]
	var conns []core.NetConnection
	p.Lock()
	defer p.Unlock()
	sch := p.Schematic()
	board := p.Board()
	for _, tok := range fields[1:] {
		if strings.HasPrefix(tok, "class=") {
			continue
		}
		parts := strings.SplitN(tok, ".", 2)
		if len(parts) != 2 {
			return "", fmt.Errorf("net: bad pin %q", tok)
		}
		ref, pin := parts[0], parts[1]
		var sid core.ID
		for _, s := range sch.Symbols {
			if s != nil && s.Reference == ref {
				sid = s.ID
				break
			}
		}
		if sid.IsZero() {
			if fp := board.FootprintByRef(ref); fp != nil {
				for i := range fp.Pads {
					if fp.Pads[i].Number == pin {
						n := name
						fp.Pads[i].Net = &n
					}
				}
				continue
			}
			return "", fmt.Errorf("net: unknown symbol %q", ref)
		}
		conns = append(conns, core.NetConnection{SymbolID: sid, PinNumber: pin})
		if fp := board.FootprintByRef(ref); fp != nil {
			for i := range fp.Pads {
				if fp.Pads[i].Number == pin {
					n := name
					fp.Pads[i].Net = &n
				}
			}
		}
	}
	if sch.Nets == nil {
		sch.Nets = make(map[string]*core.Net)
	}
	sch.Nets[name] = &core.Net{Name: name, Connections: conns}
	p.Events().Publish(core.Event{Kind: core.EventSchematicChanged})
	return fmt.Sprintf("net %s (%d pins)", name, len(conns)), nil
}

func deleteTrace(p *core.Project, args string) (string, error) {
	id := strings.TrimSpace(args)
	if id == "" {
		return "", fmt.Errorf("delete-trace ID")
	}
	var n int
	p.MutateBoard(func(b *core.Board) {
		out := b.Traces[:0]
		for _, tr := range b.Traces {
			if tr.ID.String() != id {
				out = append(out, tr)
			} else {
				n++
			}
		}
		b.Traces = out
	})
	return fmt.Sprintf("deleted %d traces", n), nil
}

// ─── sym / lib ───────────────────────────────────────────────────────

func addSymbol(p *core.Project, args string, pins []core.SchPin) (string, error) {
	// sym REF KIND [key=K] [value=V] [rot=DEG] [x=N] [y=N] [desc=...]
	fields := tokenize(args)
	if len(fields) < 2 {
		return "", fmt.Errorf("sym needs: sym REF KIND [...key=value...]")
	}
	ref := fields[0]
	kind, err := expandKind(fields[1])
	if err != nil {
		return "", err
	}
	var key, value, desc string
	var rot, xMM, yMM float64
	var hasPos bool
	for _, t := range fields[2:] {
		switch {
		case strings.HasPrefix(t, "key="):
			key = strings.TrimPrefix(t, "key=")
		case strings.HasPrefix(t, "value="):
			value = strings.TrimPrefix(t, "value=")
		case strings.HasPrefix(t, "rot="):
			fmt.Sscanf(t, "rot=%f", &rot)
		case strings.HasPrefix(t, "rotation="):
			fmt.Sscanf(t, "rotation=%f", &rot)
		case strings.HasPrefix(t, "x="):
			fmt.Sscanf(t, "x=%f", &xMM)
			hasPos = true
		case strings.HasPrefix(t, "y="):
			fmt.Sscanf(t, "y=%f", &yMM)
			hasPos = true
		case strings.HasPrefix(t, "desc="):
			desc = strings.TrimPrefix(t, "desc=")
		case strings.HasPrefix(t, "description="):
			desc = strings.TrimPrefix(t, "description=")
		}
	}
	if kind == "generic_ic" && len(pins) == 0 {
		return "", fmt.Errorf("sym: kind=generic_ic requires pin lines")
	}
	sk := core.SymbolKind{Kind: kind}
	if kind == "generic_ic" {
		sk.ICPins = pins
	}
	id := core.NewID()
	p.MutateSchematic(func(s *core.Schematic) {
		if s.Symbols == nil {
			s.Symbols = make(map[string]*core.Symbol)
		}
		// auto grid if no position
		n := float64(len(s.Symbols))
		if !hasPos {
			row := float64(int(n) / 6)
			col := n - row*6
			xMM = 15 + col*25
			yMM = 15 + row*20
		}
		sym := &core.Symbol{
			ID: id, Reference: ref, Value: value, Kind: sk,
			Position: core.NewPoint(core.FromMM(xMM), core.FromMM(yMM)),
			Rotation: rot, Key: key, Description: desc,
		}
		// replace existing by reference
		for k, old := range s.Symbols {
			if old != nil && old.Reference == ref {
				delete(s.Symbols, k)
				break
			}
		}
		s.Symbols[id.String()] = sym
		// keep order unique by ref
		out := s.SymbolOrder[:0]
		for _, oid := range s.SymbolOrder {
			if old := s.Symbols[oid]; old != nil && old.Reference != ref {
				out = append(out, oid)
			}
		}
		s.SymbolOrder = append(out, id.String())
	})
	return fmt.Sprintf("Added %s (%s)", ref, id.String()), nil
}

func addLibrary(p *core.Project, args string, pads []core.LibraryPad) (string, error) {
	// lib KEY [value=V] [rot=DEG] [edge=true|false] [elevated=true] [desc=...]
	fields := tokenize(args)
	if len(fields) < 1 {
		return "", fmt.Errorf("lib needs: lib KEY [...]")
	}
	key := fields[0]
	if len(pads) == 0 {
		return "", fmt.Errorf("lib %s needs at least one indented `pad ...` line", key)
	}
	entry := core.LibraryEntry{
		Key:         key,
		Pads:        pads,
		Attachments: []core.Attachment{},
		Silk:        []core.LibrarySilk{},
	}
	for _, t := range fields[1:] {
		switch {
		case strings.HasPrefix(t, "value="):
			entry.DefaultValue = strings.TrimPrefix(t, "value=")
		case strings.HasPrefix(t, "default_value="):
			entry.DefaultValue = strings.TrimPrefix(t, "default_value=")
		case strings.HasPrefix(t, "rot="):
			var r float64
			fmt.Sscanf(t, "rot=%f", &r)
			entry.DefaultRotationDeg = float32(r)
		case strings.HasPrefix(t, "edge="):
			entry.EdgeMounted = strings.EqualFold(strings.TrimPrefix(t, "edge="), "true")
		case strings.HasPrefix(t, "elevated="):
			entry.Elevated = strings.EqualFold(strings.TrimPrefix(t, "elevated="), "true")
		case strings.HasPrefix(t, "desc="):
			entry.Description = strings.TrimPrefix(t, "desc=")
		case strings.HasPrefix(t, "description="):
			entry.Description = strings.TrimPrefix(t, "description=")
		case strings.HasPrefix(t, "lcsc="):
			s := strings.TrimPrefix(t, "lcsc=")
			entry.LcscID = &s
		case strings.HasPrefix(t, "mpn="):
			s := strings.TrimPrefix(t, "mpn=")
			entry.MPN = &s
		}
	}
	if _, err := p.PutLibrary(entry); err != nil {
		return "", err
	}
	return fmt.Sprintf("lib %s (%d pads)", key, len(pads)), nil
}

// ─── palette ─────────────────────────────────────────────────────────

func paletteCmd(p *core.Project, args string) (string, error) {
	fields := tokenize(args)
	if len(fields) == 0 {
		return "", fmt.Errorf("palette REF KEY | palette list")
	}
	if strings.EqualFold(fields[0], "list") {
		p.RLock()
		defer p.RUnlock()
		items := p.Palette()
		if len(items) == 0 {
			return "0 palette item(s)", nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%d palette item(s):", len(items))
		for _, fp := range items {
			fmt.Fprintf(&b, "\n  %s key=%s pads=%d", fp.Reference, fp.Key, len(fp.Pads))
		}
		return b.String(), nil
	}
	// palette REF KEY [rot=] [value=] [layer=top|bottom]
	if len(fields) < 2 {
		return "", fmt.Errorf("palette REF KEY [rot=...] [value=...]")
	}
	ref, key := fields[0], fields[1]
	var value string
	rot := 0.0
	layer := core.LayerTop
	for _, t := range fields[2:] {
		switch {
		case strings.HasPrefix(t, "rot="):
			fmt.Sscanf(t, "rot=%f", &rot)
		case strings.HasPrefix(t, "rotation="):
			fmt.Sscanf(t, "rotation=%f", &rot)
		case strings.HasPrefix(t, "value="):
			value = strings.TrimPrefix(t, "value=")
		case strings.HasPrefix(t, "layer="):
			layer = parseLayerToken(strings.TrimPrefix(t, "layer="))
		}
	}

	p.Lock()
	entry, found := p.FindLibrary(key)
	sch := p.Schematic()
	var sym *core.Symbol
	for _, s := range sch.Symbols {
		if s != nil && s.Reference == ref {
			sym = s
			break
		}
	}
	// freeform: no library entry → build pads from symbol kind / pin count
	if !found {
		if strings.EqualFold(key, "freeform") || key == "" {
			var pads []core.LibraryPad
			if sym != nil {
				switch strings.ToLower(sym.Kind.Kind) {
				case "resistor", "capacitor", "inductor", "led", "diode":
					pads = core.ResistorCapPads()
				default:
					n := len(sym.Kind.Pins())
					if n == 0 {
						n = 2
					}
					pads = core.FreeformPads(n)
					// match pin numbers from schematic when possible
					pins := sym.Kind.Pins()
					for i := range pads {
						if i < len(pins) {
							pads[i].Number = pins[i].Number
							pads[i].Name = pins[i].Name
						}
					}
				}
			} else {
				pads = core.ResistorCapPads()
			}
			fk := key
			if fk == "" || strings.EqualFold(fk, "freeform") {
				fk = "freeform"
			}
			entry = core.LibraryEntry{Key: fk, Pads: pads}
		} else {
			p.Unlock()
			return "", fmt.Errorf("palette: no library entry with key %s", key)
		}
	}
	// stamp nets from schematic
	if value == "" && sym != nil {
		value = sym.Value
	}
	if rot == 0 && entry.DefaultRotationDeg != 0 {
		rot = float64(entry.DefaultRotationDeg)
	}
	desc := entry.Description
	if sym != nil && sym.Description != "" {
		desc = sym.Description
	}
	// net map for pins
	netForPin := func(pin string) *string {
		if sym == nil {
			return nil
		}
		for _, net := range sch.Nets {
			if net == nil {
				continue
			}
			for _, c := range net.Connections {
				if c.SymbolID == sym.ID && c.PinNumber == pin {
					n := net.Name
					return &n
				}
			}
		}
		return nil
	}
	p.Unlock()

	fp := entry.ToFootprint(ref, value, layer, rot)
	fp.Description = desc
	for i := range fp.Pads {
		if n := netForPin(fp.Pads[i].Number); n != nil {
			fp.Pads[i].Net = n
		} else if fp.Pads[i].Name != "" {
			if n := netForPin(fp.Pads[i].Name); n != nil {
				fp.Pads[i].Net = n
			}
		}
	}
	p.PaletteAdd(*fp)
	return fmt.Sprintf("Spawned %s from %s", ref, key), nil
}

// ─── trace / via ─────────────────────────────────────────────────────

func addTrace(p *core.Project, args string) (string, error) {
	// Supported:
	//   trace NET x1 y1 x2 y2 [layer=Top|Bottom] [width=0.15]
	//   trace LAYER NET x1 y1 x2 y2 [width=N]   (Rust form)
	fields := tokenize(args)
	if len(fields) < 5 {
		return "", fmt.Errorf("trace NET x1 y1 x2 y2 [layer=...] [width=...]")
	}
	layer := core.LayerTop
	width := 0.15
	var net string
	var x1, y1, x2, y2 float64
	var err error

	// Detect Rust form: first token is layer name and second is net
	if isLayerToken(fields[0]) && len(fields) >= 6 {
		layer = parseLayerToken(fields[0])
		net = fields[1]
		x1, err = strconv.ParseFloat(fields[2], 64)
		if err != nil {
			return "", fmt.Errorf("trace x1: %w", err)
		}
		y1, err = strconv.ParseFloat(fields[3], 64)
		if err != nil {
			return "", fmt.Errorf("trace y1: %w", err)
		}
		x2, err = strconv.ParseFloat(fields[4], 64)
		if err != nil {
			return "", fmt.Errorf("trace x2: %w", err)
		}
		y2, err = strconv.ParseFloat(fields[5], 64)
		if err != nil {
			return "", fmt.Errorf("trace y2: %w", err)
		}
		width = 0.25
		for _, t := range fields[6:] {
			if strings.HasPrefix(t, "width=") {
				fmt.Sscanf(t, "width=%f", &width)
			}
		}
	} else {
		net = fields[0]
		x1, err = strconv.ParseFloat(fields[1], 64)
		if err != nil {
			return "", fmt.Errorf("trace x1: %w", err)
		}
		y1, err = strconv.ParseFloat(fields[2], 64)
		if err != nil {
			return "", fmt.Errorf("trace y1: %w", err)
		}
		x2, err = strconv.ParseFloat(fields[3], 64)
		if err != nil {
			return "", fmt.Errorf("trace x2: %w", err)
		}
		y2, err = strconv.ParseFloat(fields[4], 64)
		if err != nil {
			return "", fmt.Errorf("trace y2: %w", err)
		}
		for _, t := range fields[5:] {
			if strings.HasPrefix(t, "layer=") {
				layer = parseLayerToken(strings.TrimPrefix(t, "layer="))
			} else if strings.HasPrefix(t, "width=") {
				fmt.Sscanf(t, "width=%f", &width)
			}
		}
	}
	id := core.NewID()
	p.MutateBoard(func(b *core.Board) {
		b.Traces = append(b.Traces, core.Trace{
			ID: id, Layer: layer, Net: net, Width: core.FromMM(width),
			Start: core.NewPoint(core.FromMM(x1), core.FromMM(y1)),
			End:   core.NewPoint(core.FromMM(x2), core.FromMM(y2)),
		})
	})
	return fmt.Sprintf("trace %s on %s (%s)", id.String(), layer.LegacyName(), net), nil
}

func addVia(p *core.Project, args string) (string, error) {
	// via NET x y [drill=0.3] [dia=0.6] [diameter=0.6]
	fields := tokenize(args)
	if len(fields) < 3 {
		return "", fmt.Errorf("via NET x y [drill=0.3] [dia=0.6]")
	}
	net := fields[0]
	x, err1 := strconv.ParseFloat(fields[1], 64)
	y, err2 := strconv.ParseFloat(fields[2], 64)
	if err1 != nil || err2 != nil {
		return "", fmt.Errorf("via: bad coordinates")
	}
	drill, dia := 0.3, 0.6
	for _, t := range fields[3:] {
		switch {
		case strings.HasPrefix(t, "drill="):
			fmt.Sscanf(t, "drill=%f", &drill)
		case strings.HasPrefix(t, "dia="):
			fmt.Sscanf(t, "dia=%f", &dia)
		case strings.HasPrefix(t, "diameter="):
			fmt.Sscanf(t, "diameter=%f", &dia)
		}
	}
	id := core.NewID()
	p.MutateBoard(func(b *core.Board) {
		b.Vias = append(b.Vias, core.Via{
			ID: id, Net: net,
			Position: core.NewPoint(core.FromMM(x), core.FromMM(y)),
			Drill:    core.FromMM(drill),
			Diameter: core.FromMM(dia),
		})
	})
	return fmt.Sprintf("via %s (%s)", id.String(), net), nil
}

// ─── rule-area / fab-rules ───────────────────────────────────────────

func addRuleArea(p *core.Project, args string) (string, error) {
	// rule-area NAME X1 Y1 X2 Y2 [clearance=N] [width=N] [via_drill=N] [via_dia=N] [priority=N]
	fields := tokenize(args)
	if len(fields) < 5 {
		return "", fmt.Errorf("rule-area NAME X1 Y1 X2 Y2 [clearance=N] ...")
	}
	name := fields[0]
	x1, e1 := strconv.ParseFloat(fields[1], 64)
	y1, e2 := strconv.ParseFloat(fields[2], 64)
	x2, e3 := strconv.ParseFloat(fields[3], 64)
	y2, e4 := strconv.ParseFloat(fields[4], 64)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil {
		return "", fmt.Errorf("rule-area: bad coordinates")
	}
	area := core.RuleArea{
		ID:   core.NewID(),
		Name: name,
		Rect: core.RectFromCorners(
			core.NewPoint(core.FromMM(x1), core.FromMM(y1)),
			core.NewPoint(core.FromMM(x2), core.FromMM(y2)),
		),
	}
	for _, t := range fields[5:] {
		switch {
		case strings.HasPrefix(t, "clearance="):
			var v float64
			fmt.Sscanf(t, "clearance=%f", &v)
			area.ClearanceMM = &v
		case strings.HasPrefix(t, "width="):
			var v float64
			fmt.Sscanf(t, "width=%f", &v)
			area.TraceWidthMM = &v
		case strings.HasPrefix(t, "via_drill="):
			var v float64
			fmt.Sscanf(t, "via_drill=%f", &v)
			area.ViaDrillMM = &v
		case strings.HasPrefix(t, "via_dia="):
			var v float64
			fmt.Sscanf(t, "via_dia=%f", &v)
			area.ViaDiameterMM = &v
		case strings.HasPrefix(t, "priority="):
			fmt.Sscanf(t, "priority=%d", &area.Priority)
		}
	}
	if area.ClearanceMM == nil && area.TraceWidthMM == nil &&
		area.ViaDrillMM == nil && area.ViaDiameterMM == nil {
		return "", fmt.Errorf("rule-area: set at least one of clearance/width/via_drill/via_dia")
	}
	// replace same name
	p.MutateBoard(func(b *core.Board) {
		out := b.RuleAreas[:0]
		for _, a := range b.RuleAreas {
			if a.Name != name {
				out = append(out, a)
			}
		}
		b.RuleAreas = append(out, area)
	})
	cl := "—"
	if area.ClearanceMM != nil {
		cl = fmt.Sprintf("%.3f", *area.ClearanceMM)
	}
	return fmt.Sprintf("rule-area %s clearance=%s mm", name, cl), nil
}

func setFabRules(p *core.Project, args string) (string, error) {
	// fab-rules PRESET | clear | list
	fields := strings.Fields(args)
	if len(fields) < 1 {
		return "", fmt.Errorf("fab-rules jlcpcb|jlcpcb-4l|clear|list")
	}
	key := strings.ToLower(fields[0])
	switch key {
	case "clear", "none":
		p.MutateBoard(func(b *core.Board) { b.FabRules = nil })
		return "fab rules cleared", nil
	case "list":
		return "fab-rules presets: jlcpcb-2l, jlcpcb-4l", nil
	}
	rules := core.FabRulesPreset(key)
	if rules == nil {
		return "", fmt.Errorf("fab-rules: unknown preset %q (have jlcpcb-2l, jlcpcb-4l)", fields[0])
	}
	p.MutateBoard(func(b *core.Board) { b.FabRules = rules })
	// also set session profile for pack/drc
	p.SetFabProfile(&core.FabProfileHandle{
		Name: rules.Preset, MinTraceWidthMM: rules.MinTraceWidthMM,
		MinClearanceMM: rules.MinClearanceMM, MinDrillMM: rules.MinDrillMM,
		MinAnnularRingMM: rules.MinAnnularRingMM, MinViaDiameterMM: rules.MinViaDiameterMM,
		MinEdgeClearanceMM: rules.MinEdgeClearanceMM,
		MaxBoardSizeMM:     [2]float64{rules.MaxBoardWidthMM, rules.MaxBoardHeightMM},
	})
	return fmt.Sprintf(
		"fab rules `%s`: trace %.3f mm, space %.3f mm, via drill %.3f mm, via dia %.3f mm",
		rules.Preset, rules.MinTraceWidthMM, rules.MinClearanceMM, rules.MinDrillMM, rules.MinViaDiameterMM,
	), nil
}

// ─── layer ───────────────────────────────────────────────────────────

func layerCmd(p *core.Project, args string) (string, error) {
	fields := tokenize(args)
	if len(fields) < 1 {
		return "", fmt.Errorf("layer list|add|remove|rename ...")
	}
	switch strings.ToLower(fields[0]) {
	case "list":
		p.RLock()
		defer p.RUnlock()
		s := p.Board().StackupOrDefault()
		var b strings.Builder
		fmt.Fprintf(&b, "%d copper layer(s):", len(s.Layers))
		for i, ls := range s.Layers {
			kind := string(ls.Kind)
			if kind == "" {
				kind = "signal"
			}
			name := ls.Name
			if name == "" {
				name = core.Layer{Index: uint8(i)}.LegacyName()
			}
			fmt.Fprintf(&b, "\n  [%d] %s (%s)", i, name, kind)
		}
		return b.String(), nil
	case "add":
		// layer add NAME (signal|plane|mixed) [thickness=N]
		if len(fields) < 3 {
			return "", fmt.Errorf("layer add NAME (signal|plane|mixed) [thickness=N]")
		}
		name, kindStr := fields[1], strings.ToLower(fields[2])
		kind := core.LayerKindSignal
		switch kindStr {
		case "signal":
			kind = core.LayerKindSignal
		case "plane", "power":
			kind = core.LayerKindPower
		case "mixed":
			kind = core.LayerKindMixed
		default:
			return "", fmt.Errorf("layer add: unknown kind %q", fields[2])
		}
		thickness := 0.035
		for _, t := range fields[3:] {
			if strings.HasPrefix(t, "thickness=") {
				fmt.Sscanf(t, "thickness=%f", &thickness)
			}
		}
		p.MutateBoard(func(b *core.Board) {
			s := b.StackupOrDefault()
			slab := core.Dielectric{ThicknessMM: 1.5, Er: 4.5}
			if len(s.Dielectrics) > 0 {
				var sumT, sumE float64
				for _, d := range s.Dielectrics {
					sumT += d.ThicknessMM
					sumE += d.Er
				}
				n := float64(len(s.Dielectrics))
				slab = core.Dielectric{ThicknessMM: sumT / n, Er: sumE / n}
			}
			oz := thickness / 0.035
			if oz <= 0 {
				oz = 1
			}
			s.PushLayer(core.LayerSpec{
				Name: name, Kind: kind, CopperWeightOz: oz, ThicknessUM: thickness * 1000,
			}, slab)
			b.Stackup = &s
		})
		return fmt.Sprintf("added layer `%s`", name), nil
	case "remove":
		if len(fields) < 2 {
			return "", fmt.Errorf("layer remove NAME")
		}
		name := fields[1]
		var err error
		p.MutateBoard(func(b *core.Board) {
			s := b.StackupOrDefault()
			target, ok := s.FindLayerByName(name)
			if !ok {
				err = fmt.Errorf("layer.remove: no layer named `%s`", name)
				return
			}
			uses := 0
			for _, fp := range b.Footprints {
				if fp == nil {
					continue
				}
				if fp.Layer == target {
					uses++
				}
				for _, pad := range fp.Pads {
					if pad.Layer == target {
						uses++
					}
				}
			}
			for _, tr := range b.Traces {
				if tr.Layer == target {
					uses++
				}
			}
			for _, pour := range b.Pours {
				if pour.Layer == target {
					uses++
				}
			}
			if uses > 0 {
				err = fmt.Errorf("layer.remove: %d item(s) still on layer `%s`", uses, name)
				return
			}
			if !s.RemoveNamed(name) {
				err = fmt.Errorf("layer.remove: refused to remove `%s` (would leave fewer than 2 layers)", name)
				return
			}
			b.Stackup = &s
		})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("removed layer `%s`", name), nil
	case "rename":
		if len(fields) < 3 {
			return "", fmt.Errorf("layer rename OLD NEW")
		}
		oldN, newN := fields[1], fields[2]
		var err error
		p.MutateBoard(func(b *core.Board) {
			s := b.StackupOrDefault()
			found := false
			for i := range s.Layers {
				if s.Layers[i].Name == oldN {
					s.Layers[i].Name = newN
					found = true
					break
				}
			}
			if !found {
				err = fmt.Errorf("layer.rename: no layer named `%s`", oldN)
				return
			}
			b.Stackup = &s
		})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("renamed layer `%s` → `%s`", oldN, newN), nil
	default:
		return "", fmt.Errorf("layer: unknown subcommand `%s` (list|add|remove|rename)", fields[0])
	}
}

// ─── screenshot ──────────────────────────────────────────────────────

func screenshot(p *core.Project, args string) (string, error) {
	// screenshot PATH [view=board|schematic] [width=PX]
	fields := tokenize(args)
	if len(fields) < 1 {
		return "", fmt.Errorf("screenshot PATH [view=board|schematic]")
	}
	path := fields[0]
	view := "board"
	for _, t := range fields[1:] {
		if strings.HasPrefix(t, "view=") {
			view = strings.ToLower(strings.TrimPrefix(t, "view="))
		} else if strings.HasPrefix(t, "path=") {
			path = strings.TrimPrefix(t, "path=")
		}
	}
	if path == "" {
		return "", fmt.Errorf("screenshot: path is empty")
	}
	p.RLock()
	var content string
	switch view {
	case "board", "":
		content = render.BoardSVG(p.Board())
	case "schematic":
		// schematic SVG not yet; fall back to board
		content = render.BoardSVG(p.Board())
		view = "board"
	default:
		p.RUnlock()
		return "", fmt.Errorf("screenshot: unknown view %q (use board or schematic)", view)
	}
	p.RUnlock()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil && filepath.Dir(path) != "." {
		return "", fmt.Errorf("screenshot: mkdir: %w", err)
	}
	// Write SVG; if path ends with .png still write SVG bytes with a note
	// (PNG pipeline not wired yet). Agents can use SVG.
	outPath := path
	if strings.HasSuffix(strings.ToLower(path), ".png") {
		outPath = strings.TrimSuffix(path, filepath.Ext(path)) + ".svg"
	}
	if err := os.WriteFile(outPath, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("screenshot: write %s: %w", outPath, err)
	}
	return fmt.Sprintf("screenshot: wrote %s (%d bytes, view=%s)", outPath, len(content), view), nil
}

// ─── helpers ─────────────────────────────────────────────────────────

func tokenize(s string) []string {
	// split on whitespace; support key="quoted value"
	var out []string
	s = strings.TrimSpace(s)
	for len(s) > 0 {
		if s[0] == '"' {
			end := 1
			for end < len(s) && s[end] != '"' {
				if s[end] == '\\' && end+1 < len(s) {
					end += 2
					continue
				}
				end++
			}
			if end >= len(s) {
				out = append(out, s[1:])
				break
			}
			out = append(out, s[1:end])
			s = strings.TrimSpace(s[end+1:])
			continue
		}
		// token until space; if key="..." keep value
		i := 0
		for i < len(s) && s[i] != ' ' && s[i] != '\t' {
			if s[i] == '=' && i+1 < len(s) && s[i+1] == '"' {
				// key="..."
				j := i + 2
				for j < len(s) && s[j] != '"' {
					if s[j] == '\\' && j+1 < len(s) {
						j += 2
						continue
					}
					j++
				}
				if j < len(s) {
					// unquote into token
					key := s[:i]
					val := s[i+2 : j]
					out = append(out, key+"="+val)
					s = strings.TrimSpace(s[j+1:])
					i = -1
					break
				}
			}
			i++
		}
		if i < 0 {
			continue
		}
		out = append(out, s[:i])
		s = strings.TrimSpace(s[i:])
	}
	return out
}

func expandKind(s string) (string, error) {
	switch strings.ToLower(s) {
	case "ic", "generic_ic", "genericic":
		return "generic_ic", nil
	case "r", "resistor":
		return "resistor", nil
	case "c", "capacitor":
		return "capacitor", nil
	case "l", "inductor":
		return "inductor", nil
	case "led":
		return "led", nil
	case "d", "diode":
		return "diode", nil
	default:
		return "", fmt.Errorf("kind: expected ic/r/c/l/led/d (or full names), got %q", s)
	}
}

func expandSide(s string) core.PinSide {
	switch strings.ToUpper(s) {
	case "L", "LEFT":
		return core.PinLeft
	case "R", "RIGHT":
		return core.PinRight
	case "T", "TOP":
		return core.PinTop
	case "B", "BOTTOM":
		return core.PinBottom
	default:
		return core.PinLeft
	}
}

func expandRole(s string) core.PinRole {
	switch strings.ToLower(s) {
	case "passive":
		return core.PinPassive
	case "input", "in":
		return core.PinInput
	case "output", "out":
		return core.PinOutput
	case "bidir", "io":
		return core.PinBidir
	case "power_out", "power-out":
		return core.PinPowerOut
	case "power_in", "power-in", "power":
		return core.PinPowerIn
	default:
		return core.PinPassive
	}
}

func isLayerToken(s string) bool {
	switch strings.ToLower(s) {
	case "top", "bottom", "f.cu", "b.cu":
		return true
	default:
		return strings.HasPrefix(strings.ToLower(s), "in")
	}
}

func parseLayerToken(s string) core.Layer {
	switch strings.ToLower(s) {
	case "top", "f.cu":
		return core.LayerTop
	case "bottom", "b.cu":
		return core.LayerBottom
	default:
		if strings.HasPrefix(strings.ToLower(s), "in") {
			n, err := strconv.Atoi(s[2:])
			if err == nil {
				return core.Layer{Index: uint8(n + 1)}
			}
		}
		return core.LayerTop
	}
}
