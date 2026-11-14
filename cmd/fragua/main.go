// Command fragua is the AI-native PCB design tool (Go host).
//
//	fragua               print usage + script reference
//	fragua help [verb]   same, or help for one script verb
//	fragua run [file] [--control PORT]   start HTTP API + open browser
//	fragua mcp [file] [--control PORT | --connect ADDR]
//	                     MCP server on stdio; hosts the API, or attaches to one
//	fragua init [dir]    write agent onboarding files into a directory
//	fragua bench [dir]   run the reference bench suite (place → route → drc)
//	fragua render FILE   3D product-shot PNG (--3d, -o, --width)
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
		if len(args) > 1 {
			out, ok := script.VerbUsage(args[1])
			fmt.Print(out)
			if !ok {
				os.Exit(2)
			}
			return
		}
		fmt.Print(script.Usage())
		return
	}

	var err error
	switch args[0] {
	case "run":
		var f hostFlags
		if f, err = parseHostFlags("run", args[1:], false); err == nil {
			err = host.Run(f.file, f.addr)
		}
	case "mcp":
		var f hostFlags
		if f, err = parseHostFlags("mcp", args[1:], true); err == nil {
			err = runMCP(f)
		}
	case "init":
		err = runInit(args[1:])
	case "bench":
		err = runBench(args[1:])
	case "render", "render3d":
		err = runRender(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q — try `fragua help` or `fragua run [file.fragua]`\n", args[0])
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "fragua: %v\n", err)
		os.Exit(1)
	}
}
