package main

import (
	"fmt"
	"strings"

	"github.com/mentasystems/fragua/internal/host"
)

// hostFlags are the options `fragua run` and `fragua mcp` share.
type hostFlags struct {
	file    string // project to open (first positional argument)
	addr    string // --control PORT / --addr HOST:PORT; empty = FRAGUA_API_ADDR or default
	connect string // mcp only: --connect ADDR, attach to a running host
}

// parseHostFlags reads `[file] [--control PORT] [--addr HOST:PORT]
// [--connect ADDR]` in any order; --flag=value works too. allowConnect is
// false for `run`, which always hosts.
func parseHostFlags(cmd string, args []string, allowConnect bool) (hostFlags, error) {
	var f hostFlags
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			if f.file != "" {
				return f, fmt.Errorf("%s: unexpected argument %q (one project file at most)", cmd, a)
			}
			f.file = a
			continue
		}
		name, val, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
		switch name {
		case "control", "addr", "connect":
		default:
			return f, fmt.Errorf("%s: unknown flag %q", cmd, a)
		}
		if name == "connect" && !allowConnect {
			return f, fmt.Errorf("%s: --connect is for `fragua mcp`; `fragua run` always hosts", cmd)
		}
		if !hasVal {
			if i+1 >= len(args) {
				return f, fmt.Errorf("%s: --%s needs a value", cmd, name)
			}
			i++
			val = args[i]
		}
		addr, err := host.ControlAddr(val)
		if err != nil {
			return f, fmt.Errorf("%s: --%s: %w", cmd, name, err)
		}
		if name == "connect" {
			f.connect = addr
		} else {
			f.addr = addr
		}
	}
	if f.connect != "" && f.addr != "" {
		return f, fmt.Errorf("%s: --connect attaches to a running host; it cannot be combined with --control/--addr", cmd)
	}
	if f.connect != "" && f.file != "" {
		return f, fmt.Errorf("%s: --connect drives the host's own project; open %s in `fragua run` instead", cmd, f.file)
	}
	return f, nil
}
