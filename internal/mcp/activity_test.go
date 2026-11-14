package mcp

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mentasystems/fragua/internal/core"
	"github.com/mentasystems/fragua/internal/host"
)

func waitActivity(t *testing.T, ch chan core.Event) core.Event {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Kind == core.EventActivity {
				return ev
			}
		case <-deadline:
			t.Fatal("no activity event")
		}
	}
}

// An agent's script shows up in the observer's log, whether the MCP server
// runs the project in-process or proxies to a host the human started.
func TestScriptPublishesActivity(t *testing.T) {
	t.Run("local", func(t *testing.T) {
		p := core.NewProject("local")
		ch := p.Events().Subscribe(32)
		defer p.Events().Unsubscribe(ch)
		if _, err := (&Local{Project: p}).Script(context.Background(), "outline 20 10"); err != nil {
			t.Fatal(err)
		}
		if ev := waitActivity(t, ch); ev.Source != "mcp" || ev.Message != "outline 20 10" {
			t.Fatalf("activity = %+v", ev)
		}
	})
	t.Run("proxy", func(t *testing.T) {
		p := core.NewProject("hosted")
		srv := httptest.NewServer(host.Handler(p))
		defer srv.Close()
		ch := p.Events().Subscribe(32)
		defer p.Events().Unsubscribe(ch)
		out, err := NewProxy(strings.TrimPrefix(srv.URL, "http://")).Script(context.Background(), "outline 20 10")
		if err != nil || !strings.Contains(out, "ok outline") {
			t.Fatalf("proxy script: %q, %v", out, err)
		}
		if ev := waitActivity(t, ch); ev.Source != "mcp" || ev.Message != "outline 20 10" {
			t.Fatalf("activity = %+v", ev)
		}
	})
}
