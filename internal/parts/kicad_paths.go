package parts

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// KicadLibsEnv is the search-path override (OS path-list separated).
const KicadLibsEnv = "FRAGUA_KICAD_LIBS"

// KicadFootprintRoots returns the directories searched for .pretty libraries:
// FRAGUA_KICAD_LIBS first, then the stock KiCad 8/9/10 install locations.
func KicadFootprintRoots() []string {
	return kicadRoots("footprints")
}

// KicadSymbolRoots returns the directories searched for .kicad_sym libraries.
func KicadSymbolRoots() []string {
	return kicadRoots("symbols")
}

func kicadRoots(kind string) []string {
	var roots []string
	seen := map[string]bool{}
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		roots = append(roots, p)
	}
	for _, p := range filepath.SplitList(os.Getenv(KicadLibsEnv)) {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		add(p)
		// Tolerate a KiCad share root: FRAGUA_KICAD_LIBS=/usr/share/kicad.
		add(filepath.Join(p, kind))
	}
	for _, p := range kicadInstallRoots() {
		add(filepath.Join(p, kind))
	}
	return roots
}

// kicadInstallRoots lists stock KiCad SharedSupport/share directories, newest
// major version first.
func kicadInstallRoots() []string {
	versions := []string{"10.0", "9.0", "8.0"}
	switch runtime.GOOS {
	case "darwin":
		return []string{"/Applications/KiCad/KiCad.app/Contents/SharedSupport"}
	case "windows":
		var out []string
		for _, base := range []string{`C:\Program Files\KiCad`, `C:\Program Files (x86)\KiCad`} {
			for _, v := range versions {
				out = append(out, filepath.Join(base, v, "share", "kicad"))
			}
		}
		return out
	default:
		out := []string{"/usr/share/kicad", "/usr/local/share/kicad"}
		for _, v := range versions {
			out = append(out, filepath.Join("/opt/kicad", v, "share", "kicad"))
		}
		// Flatpak / snap installs.
		if home, err := os.UserHomeDir(); err == nil {
			out = append(out, filepath.Join(home, ".var/app/org.kicad.KiCad/data/kicad"))
		}
		return out
	}
}

// ResolveKicadFootprint finds Library/Footprint across the search roots.
// Library may be given with or without the .pretty suffix; an empty library
// searches every .pretty directory (first match wins, roots in order).
func ResolveKicadFootprint(library, footprint string) (string, error) {
	name := strings.TrimSuffix(footprint, ".kicad_mod") + ".kicad_mod"
	roots := KicadFootprintRoots()
	if library != "" {
		lib := strings.TrimSuffix(library, ".pretty") + ".pretty"
		for _, root := range roots {
			p := filepath.Join(root, lib, name)
			if fileExists(p) {
				return p, nil
			}
		}
		return "", fmt.Errorf("kicad: %s/%s not found in %s", lib, name, describeRoots(roots))
	}
	for _, root := range roots {
		dirs, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		names := make([]string, 0, len(dirs))
		for _, d := range dirs {
			if d.IsDir() && strings.HasSuffix(d.Name(), ".pretty") {
				names = append(names, d.Name())
			}
		}
		sort.Strings(names)
		for _, d := range names {
			p := filepath.Join(root, d, name)
			if fileExists(p) {
				return p, nil
			}
		}
	}
	return "", fmt.Errorf("kicad: footprint %s not found in %s", name, describeRoots(roots))
}

// ResolveKicadSymbolFile finds Library.kicad_sym across the symbol roots.
// KiCad 10 splits libraries into Library.kicad_symdir/Symbol.kicad_sym, so that
// layout is searched too.
func ResolveKicadSymbolFile(library, symbol string) (string, error) {
	roots := KicadSymbolRoots()
	base := strings.TrimSuffix(library, ".kicad_sym")
	for _, root := range roots {
		if p := filepath.Join(root, base+".kicad_sym"); fileExists(p) {
			return p, nil
		}
		if symbol != "" {
			if p := filepath.Join(root, base+".kicad_symdir", symbol+".kicad_sym"); fileExists(p) {
				return p, nil
			}
		}
	}
	return "", fmt.Errorf("kicad: symbol library %s not found in %s", base, describeRoots(roots))
}

func describeRoots(roots []string) string {
	if len(roots) == 0 {
		return "(no KiCad libraries found; set " + KicadLibsEnv + ")"
	}
	return strings.Join(roots, ", ")
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// CollectKicadMods lists the .kicad_mod files under path. A file yields itself;
// a directory is scanned one level deep plus any .pretty subdirectories.
func CollectKicadMods(path string) ([]string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return []string{path}, nil
	}
	var out []string
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		p := filepath.Join(path, e.Name())
		if e.IsDir() {
			if !strings.HasSuffix(e.Name(), ".pretty") {
				continue
			}
			sub, err := os.ReadDir(p)
			if err != nil {
				continue
			}
			for _, s := range sub {
				if !s.IsDir() && strings.HasSuffix(s.Name(), ".kicad_mod") {
					out = append(out, filepath.Join(p, s.Name()))
				}
			}
			continue
		}
		if strings.HasSuffix(e.Name(), ".kicad_mod") {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out, nil
}
