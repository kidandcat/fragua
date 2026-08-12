// Command parity-dump emits a JSON oracle dump from the Go engine,
// matching the schema of crates/pcb-oracle for differential compare.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/mentasystems/fragua/internal/core"
	"github.com/mentasystems/fragua/internal/drc"
	"github.com/mentasystems/fragua/internal/erc"
	"github.com/mentasystems/fragua/internal/router"
)

type dump struct {
	Engine     string       `json:"engine"`
	Path       string       `json:"path"`
	Geometry   geometry     `json:"geometry"`
	DRC        checkReport  `json:"drc"`
	ERC        checkReport  `json:"erc"`
	Route      *routeDump   `json:"route,omitempty"`
	CopperHash string       `json:"copper_hash,omitempty"`
}

type geometry struct {
	Footprints int        `json:"footprints"`
	Traces     int        `json:"traces"`
	Vias       int        `json:"vias"`
	Pours      int        `json:"pours"`
	Nets       int        `json:"nets"`
	OutlineMM  *[2]float64 `json:"outline_mm"`
}

type checkReport struct {
	Errors   int            `json:"errors"`
	Warnings int            `json:"warnings"`
	ByKind   map[string]int `json:"by_kind"`
	Findings []finding      `json:"findings"`
}

type finding struct {
	Kind     string   `json:"kind"`
	Severity string   `json:"severity"`
	Net      string   `json:"net"`
	XMM      float64  `json:"x_mm"`
	YMM      float64  `json:"y_mm"`
	Involved []string `json:"involved"`
}

type routeDump struct {
	Failed        int               `json:"failed"`
	OK            int               `json:"ok"`
	Skipped       int               `json:"skipped"`
	Traces        int               `json:"traces"`
	Vias          int               `json:"vias"`
	TotalLengthMM float64           `json:"total_length_mm"`
	Iterations    int               `json:"iterations"`
	PerNet        map[string]string `json:"per_net"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: parity-dump <project.fragua> [--route] [--out file.json]")
		os.Exit(2)
	}
	path := os.Args[1]
	doRoute := false
	out := ""
	for i := 2; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--route":
			doRoute = true
		case "--out":
			i++
			if i < len(os.Args) {
				out = os.Args[i]
			}
		}
	}

	p, err := core.LoadFromPath(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	b := p.Board()
	sch := p.Schematic()

	drcRep := drc.Check(b, sch, drc.DefaultOptions())
	ercRep := erc.Check(sch, b, erc.DefaultOptions())

	d := dump{
		Engine: "go",
		Path:   path,
		Geometry: geometry{
			Footprints: len(b.Footprints),
			Traces:     len(b.Traces),
			Vias:       len(b.Vias),
			Pours:      len(b.Pours),
			Nets:       len(sch.Nets),
		},
		DRC:        toCheckDRC(drcRep),
		ERC:        toCheckERC(ercRep),
		CopperHash: hashCopper(b),
	}
	if b.Outline != nil {
		d.Geometry.OutlineMM = &[2]float64{b.Outline.Width().ToMM(), b.Outline.Height().ToMM()}
	}

	if doRoute {
		b.ClearRoute()
		opts := router.DefaultOptions()
		opts.MaxSeconds = 30
		rep := router.Route(b, opts)
		rd := &routeDump{
			Traces:        len(b.Traces),
			Vias:          len(b.Vias),
			TotalLengthMM: rep.TotalLengthMM,
			Iterations:    rep.Iterations,
			PerNet:        map[string]string{},
		}
		for _, n := range rep.PerNet {
			rd.PerNet[n.Net] = n.Outcome.Status
			switch n.Outcome.Status {
			case "ok":
				rd.OK++
			case "failed":
				rd.Failed++
			default:
				rd.Skipped++
			}
		}
		d.Route = rd
		d.CopperHash = hashCopper(b)
	}

	raw, _ := json.MarshalIndent(d, "", "  ")
	raw = append(raw, '\n')
	if out != "" {
		if err := os.WriteFile(out, raw, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "wrote", out)
	} else {
		os.Stdout.Write(raw)
	}
}

func toCheckDRC(r drc.Report) checkReport {
	by := map[string]int{}
	var findings []finding
	for _, v := range r.Violations {
		// Rust Debug uses PascalCase; we emit snake for both sides via go kinds already snake
		k := string(v.Kind)
		by[pascalFromSnake(k)]++
		findings = append(findings, finding{
			Kind:     k,
			Severity: string(v.Severity),
			Net:      v.Net,
			XMM:      round2(v.XMM),
			YMM:      round2(v.YMM),
			Involved: nil,
		})
	}
	sort.Slice(findings, func(i, j int) bool {
		a, b := findings[i], findings[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Net != b.Net {
			return a.Net < b.Net
		}
		if a.XMM != b.XMM {
			return a.XMM < b.XMM
		}
		return a.YMM < b.YMM
	})
	return checkReport{Errors: r.Errors, Warnings: r.Warnings, ByKind: by, Findings: findings}
}

func toCheckERC(r erc.Report) checkReport {
	by := map[string]int{}
	var findings []finding
	for _, v := range r.Violations {
		k := string(v.Kind)
		by[pascalFromSnake(k)]++
		inv := []string{}
		if v.Symbol != "" {
			inv = append(inv, v.Symbol)
		}
		if v.Net != "" {
			inv = append(inv, v.Net)
		}
		findings = append(findings, finding{
			Kind: k, Severity: string(v.Severity), Net: v.Net,
			Involved: inv,
		})
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Kind != findings[j].Kind {
			return findings[i].Kind < findings[j].Kind
		}
		return findings[i].Net < findings[j].Net
	})
	return checkReport{Errors: r.Errors, Warnings: r.Warnings, ByKind: by, Findings: findings}
}

func pascalFromSnake(s string) string {
	// pad_pad_clearance → PadPadClearance for by_kind keys matching Rust Debug
	out := []byte{}
	up := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '_' {
			up = true
			continue
		}
		if up && c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
			up = false
		}
		out = append(out, c)
	}
	return string(out)
}

func round2(v float64) float64 {
	return float64(int(v*100+0.5)) / 100
}

// hashCopper matches crates/pcb-oracle (SHA-256 of little-endian fields, sorted).
func hashCopper(b *core.Board) string {
	h := sha256.New()
	type tk struct {
		net           string
		L             uint8
		x0, y0, x1, y1, w int64
	}
	var ts []tk
	for _, t := range b.Traces {
		ts = append(ts, tk{t.Net, t.Layer.Index, int64(t.Start.X), int64(t.Start.Y), int64(t.End.X), int64(t.End.Y), int64(t.Width)})
	}
	sort.Slice(ts, func(i, j int) bool {
		a, b := ts[i], ts[j]
		if a.net != b.net {
			return a.net < b.net
		}
		if a.L != b.L {
			return a.L < b.L
		}
		if a.x0 != b.x0 {
			return a.x0 < b.x0
		}
		if a.y0 != b.y0 {
			return a.y0 < b.y0
		}
		if a.x1 != b.x1 {
			return a.x1 < b.x1
		}
		if a.y1 != b.y1 {
			return a.y1 < b.y1
		}
		return a.w < b.w
	})
	putU8 := func(v uint8) { h.Write([]byte{v}) }
	putI64 := func(v int64) {
		var buf [8]byte
		u := uint64(v)
		for i := 0; i < 8; i++ {
			buf[i] = byte(u >> (8 * i))
		}
		h.Write(buf[:])
	}
	for _, t := range ts {
		h.Write([]byte(t.net))
		putU8(t.L)
		putI64(t.x0)
		putI64(t.y0)
		putI64(t.x1)
		putI64(t.y1)
		putI64(t.w)
	}
	type vk struct {
		net          string
		x, y, d, dia int64
	}
	var vs []vk
	for _, v := range b.Vias {
		vs = append(vs, vk{v.Net, int64(v.Position.X), int64(v.Position.Y), int64(v.Drill), int64(v.Diameter)})
	}
	sort.Slice(vs, func(i, j int) bool {
		a, b := vs[i], vs[j]
		if a.net != b.net {
			return a.net < b.net
		}
		if a.x != b.x {
			return a.x < b.x
		}
		if a.y != b.y {
			return a.y < b.y
		}
		return a.d < b.d
	})
	for _, v := range vs {
		h.Write([]byte(v.net))
		putI64(v.x)
		putI64(v.y)
		putI64(v.d)
		putI64(v.dia)
	}
	return hex.EncodeToString(h.Sum(nil))
}
