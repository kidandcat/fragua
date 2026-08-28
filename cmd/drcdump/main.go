package main

import (
	"fmt"
	"os"
	"sort"

	"github.com/mentasystems/fragua/internal/core"
	"github.com/mentasystems/fragua/internal/drc"
	"github.com/mentasystems/fragua/internal/erc"
)

func main() {
	p, err := core.LoadFromPath(os.Args[1])
	if err != nil {
		panic(err)
	}
	dr := drc.Check(p.Board(), p.Schematic(), drc.DefaultOptions())
	er := erc.Check(p.Schematic(), p.Board(), erc.DefaultOptions())
	fmt.Println(dr.Summary())
	counts := map[string]int{}
	for _, v := range dr.Violations {
		counts[fmt.Sprintf("%v/%v", v.Severity, v.Kind)]++
	}
	keys := []string{}
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %-40s %d\n", k, counts[k])
	}
	fmt.Println("--- DRC errors ---")
	for _, v := range dr.Violations {
		if v.Severity == drc.SeverityError {
			fmt.Printf("  [%v] %s  @ %.2f,%.2f net=%s\n", v.Kind, v.Message, v.XMM, v.YMM, v.Net)
		}
	}
	fmt.Println("--- DRC warnings (first 60) ---")
	n := 0
	for _, v := range dr.Violations {
		if v.Severity != drc.SeverityError {
			if n < 60 {
				fmt.Printf("  [%v] %s  @ %.2f,%.2f net=%s\n", v.Kind, v.Message, v.XMM, v.YMM, v.Net)
			}
			n++
		}
	}
	fmt.Println(er.Summary())
	fmt.Println("--- ERC ---")
	for _, v := range er.Violations {
		fmt.Printf("  [%v/%v] %s\n", v.Severity, v.Kind, v.Message)
	}
}
