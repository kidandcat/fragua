package script

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mentasystems/fragua/internal/core"
	"github.com/mentasystems/fragua/internal/parts"
)

// Real parts, zero friction: `part`, `lib-gen`, `lib-import`, `list-parts`.
// Every one of them lands a core.LibraryEntry (footprint + pins) in the
// on-disk library, and — when a reference is implied — spawns the matching
// schematic symbol and palette footprint, so the very next line can `place`
// and `net`.

// OfflineEnv forces cache-only resolution of `part LCSC:…`.
const OfflineEnv = "FRAGUA_OFFLINE"

// partsFetcher is the EasyEDA fetcher; swapped out in tests.
var partsFetcher parts.Fetcher = parts.HTTPFetcher{Timeout: 30 * time.Second}

// partsVerb handles the parts verbs. ok=false means "not mine" so dispatch can
// carry on with its own switch (keeps the dispatch.go hook to a single line).
func partsVerb(p *core.Project, tool, args string) (msg string, err error, ok bool) {
	switch tool {
	case "part":
		msg, err = cmdPart(p, args)
	case "lib-gen", "lib_gen", "libgen":
		msg, err = cmdLibGen(p, args)
	case "lib-import", "lib_import", "libimport":
		msg, err = cmdLibImport(p, args)
	case "list-parts", "list_parts", "parts":
		msg, err = cmdListParts(p, args)
	default:
		return "", nil, false
	}
	return msg, err, true
}

// ─── part ────────────────────────────────────────────────────────────

// cmdPart resolves one component and spawns it.
//
//	part LCSC:C2040 [as=REF] [key=KEY] [value=V] [rot=DEG] [refresh=true]
//	part C2040                                   (the LCSC: prefix is optional)
//	part kicad:Package_TO_SOT_SMD:SOT-23 [as=Q1] [sym=Lib:Symbol]
func cmdPart(p *core.Project, args string) (string, error) {
	fields := tokenize(args)
	if len(fields) == 0 {
		return "", fmt.Errorf("part LCSC:C2040 [as=REF] | part kicad:Library:Footprint [as=REF]")
	}
	spec := fields[0]
	opt := parseKV(fields[1:])

	var (
		part   *parts.Part
		origin string
		err    error
	)
	switch {
	case strings.HasPrefix(strings.ToLower(spec), "kicad:"):
		part, origin, err = resolveKicadPart(spec, opt)
	default:
		part, origin, err = resolveLCSCPart(p, spec, opt)
	}
	if err != nil {
		return "", err
	}
	if v, has := opt["value"]; has {
		part.Entry.DefaultValue = v
	}
	if r, has := opt["rot"]; has {
		part.Entry.DefaultRotationDeg = float32(parseFloatArg(r))
	}
	if _, err := p.PutLibrary(part.Entry); err != nil {
		return "", err
	}

	ref := opt["as"]
	if ref == "" {
		ref = nextReference(p, part.RefPrefix)
	}
	if err := spawnPart(p, part, ref, opt); err != nil {
		return "", err
	}
	return fmt.Sprintf("part %s → %s key=%s pads=%d pins=%d src=%s%s",
		spec, ref, part.Entry.Key, len(part.Entry.Pads), len(part.Pins), part.Entry.Source,
		originSuffix(origin, part.Entry)), nil
}

func originSuffix(origin string, e core.LibraryEntry) string {
	var b strings.Builder
	if origin != "" {
		fmt.Fprintf(&b, " (%s)", origin)
	}
	if e.JLCClass != nil {
		fmt.Fprintf(&b, " %s", strings.ToLower(strings.TrimSuffix(*e.JLCClass, " Part")))
	}
	return b.String()
}

// resolveLCSCPart returns the part from the on-disk cache when possible, and
// only reaches for the network otherwise. FRAGUA_OFFLINE=1 makes the cache the
// only source.
func resolveLCSCPart(p *core.Project, spec string, opt map[string]string) (*parts.Part, string, error) {
	lcsc, ok := parts.NormaliseLCSC(spec)
	if !ok {
		return nil, "", fmt.Errorf("part: %q is neither an LCSC id (C2040) nor kicad:Library:Footprint", spec)
	}
	key := opt["key"]
	if key == "" {
		key = lcsc
	}
	refresh := isTrue(opt["refresh"])
	if !refresh {
		if e, found := findCachedLCSC(p, lcsc, key); found {
			return partFromEntry(e), "cached", nil
		}
	}
	if isTrue(os.Getenv(OfflineEnv)) {
		return nil, "", fmt.Errorf("part %s: not in the library and %s=1 (fetch it once online first)", lcsc, OfflineEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	raw, err := partsFetcher.Fetch(ctx, lcsc)
	if err != nil {
		return nil, "", err
	}
	part, err := parts.ParseEasyEDA(raw, key)
	if err != nil {
		return nil, "", err
	}
	return part, "fetched", nil
}

// findCachedLCSC looks the part up by key first, then by stored LCSC id.
func findCachedLCSC(p *core.Project, lcsc, key string) (core.LibraryEntry, bool) {
	// A hand-authored entry that merely carries the LCSC id (pads, no pins) is
	// not a cache hit: `part` promises named pins, so only an lcsc-sourced
	// entry with a pin list satisfies the lookup; anything else is refetched.
	usable := func(e core.LibraryEntry) bool {
		return e.Source == parts.SourceLCSC && len(e.Pads) > 0 && len(e.Pins) > 0
	}
	if e, ok := p.FindLibrary(key); ok && usable(e) {
		return e, true
	}
	for _, e := range p.Library().ListEntries() {
		if e.LcscID != nil && strings.EqualFold(*e.LcscID, lcsc) && usable(e) {
			return e, true
		}
	}
	return core.LibraryEntry{}, false
}

// resolveKicadPart handles `kicad:Library:Footprint` and `kicad:Footprint`,
// optionally merging pin names from `sym=Library:Symbol`.
func resolveKicadPart(spec string, opt map[string]string) (*parts.Part, string, error) {
	rest := spec[len("kicad:"):]
	library, footprint := "", rest
	if i := strings.LastIndex(rest, ":"); i >= 0 {
		library, footprint = rest[:i], rest[i+1:]
	}
	if footprint == "" {
		return nil, "", fmt.Errorf("part kicad:Library:Footprint — footprint name missing")
	}
	path, err := parts.ResolveKicadFootprint(library, footprint)
	if err != nil {
		return nil, "", err
	}
	part, err := parts.LoadKicadMod(path, opt["key"])
	if err != nil {
		return nil, "", err
	}
	if symSpec := opt["sym"]; symSpec != "" {
		if n, err := mergeKicadSymbol(part, symSpec); err != nil {
			return nil, "", err
		} else if n > 0 {
			part.Pins = part.Entry.Pins
			part.Kind = "generic_ic"
		}
	}
	return part, filepath.Base(filepath.Dir(path)), nil
}

// mergeKicadSymbol pulls pin names/roles from `Library:Symbol` (or a path).
func mergeKicadSymbol(part *parts.Part, spec string) (int, error) {
	library, symbol := spec, ""
	if i := strings.LastIndex(spec, ":"); i > 1 { // >1 keeps C:\… paths intact
		library, symbol = spec[:i], spec[i+1:]
	}
	path := library
	if !strings.HasSuffix(strings.ToLower(path), ".kicad_sym") {
		var err error
		if path, err = parts.ResolveKicadSymbolFile(library, symbol); err != nil {
			return 0, err
		}
	}
	syms, err := parts.LoadKicadSym(path)
	if err != nil {
		return 0, err
	}
	for _, s := range syms {
		if symbol == "" || strings.EqualFold(s.Name, symbol) {
			return parts.MergeSymbolPins(&part.Entry, s), nil
		}
	}
	return 0, fmt.Errorf("kicad: symbol %q not found in %s", symbol, filepath.Base(path))
}

// ─── lib-gen ─────────────────────────────────────────────────────────

// cmdLibGen generates an IPC land pattern.
//
//	lib-gen NAME family=chip size=0603 [density=N] [as=REF]
//	lib-gen NAME family=qfn pins=32 pitch=0.5 body=5 ep=3.2
func cmdLibGen(p *core.Project, args string) (string, error) {
	fields := tokenize(args)
	if len(fields) == 0 {
		return "", fmt.Errorf("lib-gen NAME family=%s …", strings.Join(parts.Families(), "|"))
	}
	opt := parseKV(fields[1:])
	spec := parts.GenSpec{
		Key:     fields[0],
		Family:  opt["family"],
		Size:    opt["size"],
		Pins:    int(parseFloatArg(opt["pins"])),
		Pitch:   parseFloatArg(opt["pitch"]),
		Body:    parseFloatArg(opt["body"]),
		BodyLen: parseFloatArg(firstSet(opt, "body_len", "bodylen", "length")),
		EPX:     parseFloatArg(firstSet(opt, "ep", "ep_x", "epx")),
		EPY:     parseFloatArg(firstSet(opt, "ep_y", "epy")),
		Rows:    int(parseFloatArg(opt["rows"])),
		Cols:    int(parseFloatArg(firstSet(opt, "cols", "columns"))),
		Spacing: parseFloatArg(firstSet(opt, "spacing", "row", "row_spacing")),
		Pad:     parseFloatArg(firstSet(opt, "pad", "pad_size", "bump")),
	}
	if spec.Family == "" {
		return "", fmt.Errorf("lib-gen: family= required (%s)", strings.Join(parts.Families(), " "))
	}
	d, err := parts.ParseDensity(opt["density"])
	if err != nil {
		return "", err
	}
	spec.Density = d
	if k := opt["kind"]; k != "" {
		kind, err := expandKind(k)
		if err != nil {
			return "", err
		}
		spec.Kind = kind
	}
	part, err := parts.Generate(spec)
	if err != nil {
		return "", err
	}
	if v, has := opt["value"]; has {
		part.Entry.DefaultValue = v
	}
	if r, has := opt["rot"]; has {
		part.Entry.DefaultRotationDeg = float32(parseFloatArg(r))
	}
	if _, err := p.PutLibrary(part.Entry); err != nil {
		return "", err
	}
	out := fmt.Sprintf("lib-gen %s family=%s pads=%d density=%s src=ipc",
		part.Entry.Key, spec.Family, len(part.Entry.Pads), spec.Density)
	if ref := opt["as"]; ref != "" {
		if err := spawnPart(p, part, ref, opt); err != nil {
			return "", err
		}
		out += " → " + ref
	}
	return out, nil
}

// ─── lib-import ──────────────────────────────────────────────────────

// cmdLibImport imports KiCad libraries from disk. As everywhere else, key=
// names the library entry and as= the reference to spawn.
//
//	lib-import kicad FILE.kicad_mod [key=KEY] [as=REF]
//	lib-import kicad DIR                      (every .kicad_mod, .pretty included)
//	lib-import kicad FILE.kicad_sym [key=KEY] (merge pin names into an entry)
func cmdLibImport(p *core.Project, args string) (string, error) {
	fields := tokenize(args)
	if len(fields) < 2 || !strings.EqualFold(fields[0], "kicad") {
		return "", fmt.Errorf("lib-import kicad <file-or-dir> [key=KEY] [as=REF]")
	}
	path := expandHome(fields[1])
	opt := parseKV(fields[2:])

	if strings.HasSuffix(strings.ToLower(path), ".kicad_sym") {
		return importKicadSym(p, path, opt)
	}
	files, err := parts.CollectKicadMods(path)
	if err != nil {
		return "", fmt.Errorf("lib-import: %w", err)
	}
	if len(files) == 0 {
		return "", fmt.Errorf("lib-import: no .kicad_mod under %s", path)
	}
	key := opt["key"]
	if key != "" && len(files) > 1 {
		return "", fmt.Errorf("lib-import: key=%s only makes sense for a single footprint (%d found)", key, len(files))
	}
	var imported, failed int
	var last *parts.Part
	var firstErr error
	for _, f := range files {
		part, err := parts.LoadKicadMod(f, key)
		if err != nil {
			failed++
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if _, err := p.PutLibrary(part.Entry); err != nil {
			return "", err
		}
		imported++
		last = part
	}
	if imported == 0 {
		return "", fmt.Errorf("lib-import: nothing imported (%w)", firstErr)
	}
	out := fmt.Sprintf("lib-import kicad %d footprint(s) src=kicad", imported)
	if failed > 0 {
		out += fmt.Sprintf(" (%d skipped: %v)", failed, firstErr)
	}
	if imported == 1 && last != nil {
		out += fmt.Sprintf(" key=%s pads=%d", last.Entry.Key, len(last.Entry.Pads))
		if ref := opt["as"]; ref != "" {
			if err := spawnPart(p, last, ref, opt); err != nil {
				return "", err
			}
			out += " → " + ref
		}
	} else if opt["as"] != "" {
		return "", fmt.Errorf("lib-import: as=%s needs a single footprint (%d imported)", opt["as"], imported)
	}
	return out, nil
}

// importKicadSym merges symbol pin names onto library entries whose pad numbers
// line up — by key= when given, otherwise by matching symbol name to entry key.
func importKicadSym(p *core.Project, path string, opt map[string]string) (string, error) {
	syms, err := parts.LoadKicadSym(path)
	if err != nil {
		return "", fmt.Errorf("lib-import: %w", err)
	}
	target := opt["key"]
	merged, touched := 0, 0
	for _, s := range syms {
		key := target
		if key == "" {
			key = s.Name
		}
		entry, ok := p.FindLibrary(key)
		if !ok {
			continue
		}
		n := parts.MergeSymbolPins(&entry, s)
		if n == 0 {
			continue
		}
		if _, err := p.PutLibrary(entry); err != nil {
			return "", err
		}
		merged += n
		touched++
		if target != "" {
			break
		}
	}
	if touched == 0 {
		return "", fmt.Errorf("lib-import: %d symbol(s) in %s matched no library entry (import the footprint first, or pass key=KEY)",
			len(syms), filepath.Base(path))
	}
	return fmt.Sprintf("lib-import kicad %d symbol(s) → %d entr(ies), %d pins named", len(syms), touched, merged), nil
}

// ─── list-parts ──────────────────────────────────────────────────────

// cmdListParts prints one library entry per line with its source and pin count.
// An optional argument filters by source (lcsc / kicad / ipc) or key substring.
func cmdListParts(p *core.Project, args string) (string, error) {
	filter := strings.ToLower(strings.TrimSpace(args))
	entries := p.Library().ListEntries()
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })

	var b strings.Builder
	n := 0
	for _, e := range entries {
		src := e.Source
		if src == "" {
			src = "manual"
		}
		if filter != "" && !strings.EqualFold(src, filter) && !strings.Contains(strings.ToLower(e.Key), filter) {
			continue
		}
		n++
		fmt.Fprintf(&b, "\n%s src=%s pads=%d pins=%d", e.Key, src, len(e.Pads), len(e.Pins))
		if e.LcscID != nil {
			fmt.Fprintf(&b, " lcsc=%s", *e.LcscID)
		}
		if e.MPN != nil {
			fmt.Fprintf(&b, " mpn=%s", *e.MPN)
		}
		if e.JLCClass != nil {
			fmt.Fprintf(&b, " jlc=%s", strings.ToLower(strings.TrimSuffix(*e.JLCClass, " Part")))
		}
		if e.Datasheet != nil {
			fmt.Fprintf(&b, " datasheet=%s", *e.Datasheet)
		}
	}
	if n == 0 {
		if filter != "" {
			return fmt.Sprintf("0 parts matching %q", filter), nil
		}
		return "0 parts in library", nil
	}
	return fmt.Sprintf("%d part(s)%s", n, b.String()), nil
}

// ─── spawn ───────────────────────────────────────────────────────────

// spawnPart creates the schematic symbol and palette footprint for ref, so the
// next line can `place ref x y` and `net NAME ref.PIN`.
func spawnPart(p *core.Project, part *parts.Part, ref string, opt map[string]string) error {
	symArgs := []string{ref, symKindToken(part.Kind), "key=" + part.Entry.Key}
	if part.Entry.DefaultValue != "" {
		symArgs = append(symArgs, "value="+quoteArg(part.Entry.DefaultValue))
	}
	if part.Entry.Description != "" {
		symArgs = append(symArgs, "desc="+quoteArg(part.Entry.Description))
	}
	if part.Entry.LcscID != nil {
		symArgs = append(symArgs, "lcsc="+*part.Entry.LcscID)
	}
	if part.Entry.MPN != nil {
		symArgs = append(symArgs, "mpn="+quoteArg(*part.Entry.MPN))
	}
	if part.Entry.Manufacturer != nil {
		symArgs = append(symArgs, "mfr="+quoteArg(*part.Entry.Manufacturer))
	}
	pins := part.Pins
	if part.Kind != "generic_ic" {
		pins = nil // discrete kinds carry their own pin pair
	}
	if _, err := addSymbol(p, strings.Join(symArgs, " "), pins); err != nil {
		return err
	}
	palArgs := []string{ref, part.Entry.Key}
	if r, has := opt["rot"]; has {
		palArgs = append(palArgs, "rot="+r)
	}
	if l, has := opt["layer"]; has {
		palArgs = append(palArgs, "layer="+l)
	}
	_, err := paletteCmd(p, strings.Join(palArgs, " "))
	return err
}

// symKindToken maps a parts kind onto the `sym` verb's kind token.
func symKindToken(kind string) string {
	switch kind {
	case "resistor":
		return "r"
	case "capacitor":
		return "c"
	case "inductor":
		return "l"
	case "led":
		return "led"
	case "diode":
		return "d"
	default:
		return "ic"
	}
}

// partFromEntry rebuilds a Part from a cached library entry.
func partFromEntry(e core.LibraryEntry) *parts.Part {
	kind := e.SymbolKindName
	if kind == "" {
		kind = "generic_ic"
	}
	p := &parts.Part{Entry: e, Pins: e.Pins, Kind: kind}
	if len(p.Pins) == 0 {
		if sk, ok := e.SymbolKindFor(); ok {
			p.Pins = sk.Pins()
		}
	}
	p.RefPrefix = parts.RefPrefixFromEasyEDA(refPrefixFallback(kind))
	return p
}

func refPrefixFallback(kind string) string {
	switch kind {
	case "resistor":
		return "R"
	case "capacitor":
		return "C"
	case "inductor":
		return "L"
	case "led", "diode":
		return "D"
	default:
		return "U"
	}
}

// resolvePinAlias turns a pin *name* into its number so `net GND U1.GND` works
// alongside `net GND U1.57`. Numbers always win; an unknown token is returned
// unchanged so the caller still reports it as such.
func resolvePinAlias(sch *core.Schematic, board *core.Board, ref, pin string) string {
	var sym *core.Symbol
	for _, s := range sch.Symbols {
		if s != nil && s.Reference == ref {
			sym = s
			break
		}
	}
	if sym != nil {
		pins := sym.Kind.Pins()
		for _, sp := range pins {
			if sp.Number == pin {
				return pin
			}
		}
		for _, sp := range pins {
			if sp.Name != "" && strings.EqualFold(sp.Name, pin) {
				return sp.Number
			}
		}
	}
	if fp := board.FootprintByRef(ref); fp != nil {
		for i := range fp.Pads {
			if fp.Pads[i].Number == pin {
				return pin
			}
		}
		for i := range fp.Pads {
			if fp.Pads[i].Name != "" && strings.EqualFold(fp.Pads[i].Name, pin) {
				return fp.Pads[i].Number
			}
		}
	}
	return pin
}

// nextReference returns the first free PREFIX+N reference in the schematic.
func nextReference(p *core.Project, prefix string) string {
	if prefix == "" {
		prefix = "U"
	}
	p.RLock()
	used := map[string]bool{}
	for _, s := range p.Schematic().Symbols {
		if s != nil {
			used[strings.ToUpper(s.Reference)] = true
		}
	}
	for _, fp := range p.Board().Footprints {
		used[strings.ToUpper(fp.Reference)] = true
	}
	for _, fp := range p.Palette() {
		used[strings.ToUpper(fp.Reference)] = true
	}
	p.RUnlock()
	for i := 1; ; i++ {
		ref := prefix + strconv.Itoa(i)
		if !used[strings.ToUpper(ref)] {
			return ref
		}
	}
}

// ─── small helpers ───────────────────────────────────────────────────

// parseKV collects key=value tokens; bare tokens are ignored.
func parseKV(fields []string) map[string]string {
	out := make(map[string]string, len(fields))
	for _, f := range fields {
		if i := strings.Index(f, "="); i > 0 {
			out[strings.ToLower(f[:i])] = f[i+1:]
		}
	}
	return out
}

func firstSet(kv map[string]string, keys ...string) string {
	for _, k := range keys {
		if v, ok := kv[k]; ok {
			return v
		}
	}
	return ""
}

func parseFloatArg(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return v
}

func isTrue(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// quoteArg re-quotes a value with spaces so tokenize() keeps it whole.
func quoteArg(s string) string {
	if strings.ContainsAny(s, " \t") {
		return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return s
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}
