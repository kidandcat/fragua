// Command fragua is the AI-native PCB design tool (Go host).
//
//	fragua              print usage + script reference
//	fragua help         same
//	fragua run [file]   start HTTP API + open browser
package main

import (
	"fmt"
	"os"

	"github.com/mentasystems/fragua/internal/host"
	"github.com/mentasystems/fragua/internal/script"
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(script.Usage())
		return
	}
	if args[0] != "run" {
		fmt.Fprintf(os.Stderr, "unknown command %q — try `fragua help` or `fragua run [file.fragua]`\n", args[0])
		os.Exit(2)
	}
	path := ""
	if len(args) > 1 {
		path = args[1]
	}
	if err := host.Run(path); err != nil {
		fmt.Fprintf(os.Stderr, "fragua: %v\n", err)
		os.Exit(1)
	}
}
