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

// runMCP serves the script API over MCP on stdio, hosting the same HTTP API and
// browser UI as `fragua run` so the human can watch. If the API address is
// already taken by another fragua, we drive that one instead of failing.
//
// stdout is the MCP protocol channel: every log line goes to stderr.
func runMCP(projectPath string) error {
	var backend mcp.Backend

	srv, err := host.Start(projectPath, os.Stderr)
	switch {
	case err == nil:
		backend = &mcp.Local{Project: srv.Project, APIAddr: srv.Addr}
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx)
		}()
	case host.Alive(host.Addr()):
		addr := host.Addr()
		fmt.Fprintf(os.Stderr, "Fragua (Go) — another fragua owns http://%s, proxying to it  ·  UI http://%s/ui/\n", addr, addr)
		if projectPath != "" {
			fmt.Fprintf(os.Stderr, "note: %s not loaded; the running host keeps its own project\n", projectPath)
		}
		backend = mcp.NewProxy(addr)
	default:
		return fmt.Errorf("listen %s: %w", host.Addr(), err)
	}

	fmt.Fprintln(os.Stderr, "MCP server ready on stdio.")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return mcp.Serve(ctx, backend)
}
