//go:build ignore

package main

import (
	"fmt"
	"os"

	"github.com/mentasystems/fragua/internal/core"
	"github.com/mentasystems/fragua/internal/drc"
	"github.com/mentasystems/fragua/internal/script"
)

func main() {
	raw, err := os.ReadFile("bench/boost-max17220-fair/script.txt")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	p := core.NewProject("boost-wlp-fair")
	for _, r := range script.RunScript(p, string(raw)) {
		fmt.Printf("%s %s: %s\n", ok(r.OK), r.Tool, r.Result)
		if !r.OK {
			os.Exit(1)
		}
	}
	p.RLock()
	defer p.RUnlock()
	b := p.Board()
	for _, fp := range b.Footprints {
		fmt.Printf("place %s %.2f,%.2f r=%.0f\n", fp.Reference, fp.Position.X.ToMM(), fp.Position.Y.ToMM(), fp.Rotation)
	}
	fmt.Println(drc.Check(b, p.Schematic(), drc.DefaultOptions()).Summary())
}

func ok(v bool) string {
	if v {
		return "ok"
	}
	return "error"
}
