package main

import (
	"fmt"
	"os"

	"github.com/mentasystems/fragua/internal/core"
	"github.com/mentasystems/fragua/internal/drc"
)

func main() {
	p, err := core.LoadFromPath(os.Args[1])
	if err != nil {
		panic(err)
	}
	r := drc.Check(p.Board(), p.Schematic(), drc.DefaultOptions())
	fmt.Println(r.Summary())
	for _, v := range r.Violations {
		fmt.Printf("  %s %s net=%s @%.2f,%.2f  %s\n", v.Kind, v.Severity, v.Net, v.XMM, v.YMM, v.Message)
	}
}
