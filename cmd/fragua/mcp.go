package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mentasystems/fragua/internal/host"
	"github.com/mentasystems/fragua/internal/mcp"
)

// runMCP serves the script API over MCP on stdio.
//
// With --connect ADDR it attaches to a fragua the human already runs
// (`fragua run --control PORT`) and fails if nothing answers there. Without
// it, it hosts the HTTP API and browser UI itself on --control/--addr (or
// FRAGUA_API_ADDR); if that address already belongs to another fragua it
// drives that one instead of failing.
//
// stdout is the MCP protocol channel: every log line goes to stderr.
func runMCP(f hostFlags) error {
	backend, cleanup, err := mcpBackend(f)
	if err != nil {
		return err
	}
	defer cleanup()

	fmt.Fprintln(os.Stderr, "MCP server ready on stdio.")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return mcp.Serve(ctx, backend)
}

func mcpBackend(f hostFlags) (mcp.Backend, func(), error) {
	noop := func() {}
	if f.connect != "" {
		if !host.Alive(f.connect) {
			return nil, noop, fmt.Errorf("no fragua host answers at %s — start one with `fragua run --control PORT` and connect to that port", f.connect)
		}
		fmt.Fprintf(os.Stderr, "Fragua (Go) — connected to the host at http://%s  ·  UI http://%s/ui/\n", f.connect, f.connect)
		return mcp.NewProxy(f.connect), noop, nil
	}

	addr := f.addr
	if addr == "" {
		addr = host.Addr()
	}
	srv, err := host.StartAt(f.file, addr, os.Stderr)
	switch {
	case err == nil:
		return &mcp.Local{Project: srv.Project, APIAddr: srv.Addr}, func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx)
		}, nil
	case host.Alive(addr):
		fmt.Fprintf(os.Stderr, "Fragua (Go) — another fragua owns http://%s, proxying to it  ·  UI http://%s/ui/\n", addr, addr)
		if f.file != "" {
			fmt.Fprintf(os.Stderr, "note: %s not loaded; the running host keeps its own project\n", f.file)
		}
		return mcp.NewProxy(addr), noop, nil
	default:
		return nil, noop, fmt.Errorf("listen %s: %w", addr, err)
	}
}
