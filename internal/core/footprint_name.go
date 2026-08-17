package core

import (
	"regexp"
	"strings"
)

// Imperial EIA size → IPC-7351 metric body (mm tenths).
// 0603 (0.06"×0.03") = 1608 (1.6×0.8 mm).
var imperialToMetric = map[string]string{
	"01005": "0402",
	"0201":  "0603",
	"0402":  "1005",
	"0603":  "1608",
	"0805":  "2012",
	"1206":  "3216",
	"1210":  "3225",
	"1806":  "4516",
	"1812":  "4532",
	"2010":  "5025",
	"2512":  "6332",
}

// passiveKeyRE matches c_0603, R0603, L-0805, LED_1206, FB_0603, C_0603_1608Metric.
var passiveKeyRE = regexp.MustCompile(`(?i)^(c|r|l|led|fb)[-_]?(\d{4,5})(?:[-_]\d{4})?(?:metric)?$`)

// FootprintPackageName is the BOM/CPL package string an EE / JLCPCB SMT
// importer will accept.
//
//   - strips a `library:` prefix (and any `LibName:` KiCad-style prefix)
//   - maps passive size keys to IPC-7351 metric names:
//     c_0603 → C_0603_1608Metric, r_0805 → R_0805_2012Metric
//   - leaves unknown names unchanged (after prefix strip)
//
// LCSC / MPN are never invented here.
func FootprintPackageName(fp *Footprint) string {
	if fp == nil {
		return ""
	}
	raw := fp.Library
	if raw == "" {
		raw = fp.Key
	}
	return PackageNameFromLibrary(raw)
}

// PackageNameFromLibrary maps a library key or `library:key` to a BOM footprint.
func PackageNameFromLibrary(lib string) string {
	name := strings.TrimSpace(lib)
	name = strings.TrimPrefix(name, "library:")
	if i := strings.LastIndex(name, ":"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if strings.HasSuffix(strings.ToLower(name), "metric") && strings.Contains(name, "_") {
		return name
	}
	key := strings.ReplaceAll(name, "-", "_")
	m := passiveKeyRE.FindStringSubmatch(key)
	if m == nil {
		return name
	}
	kind := strings.ToUpper(m[1])
	size := m[2]
	metric, ok := imperialToMetric[size]
	if !ok {
		return kind + "_" + size
	}
	return kind + "_" + size + "_" + metric + "Metric"
}
