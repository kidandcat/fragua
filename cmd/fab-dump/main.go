// Command fab-dump writes a Gerber/Excellon/BOM pack (Go engine).
package main

import (
	"fmt"
	"os"

	"github.com/mentasystems/fragua/internal/core"
	"github.com/mentasystems/fragua/internal/gerber"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: fab-dump <project.fragua> <out_dir> [stem]")
		os.Exit(2)
	}
	p, err := core.LoadFromPath(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	stem := p.Name()
	if stem == "" {
		stem = "demo"
	}
	if len(os.Args) > 3 {
		stem = os.Args[3]
	}
	paths, err := gerber.WriteFabPack(p.Board(), stem, os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %d files to %s\n", len(paths), os.Args[2])
}
