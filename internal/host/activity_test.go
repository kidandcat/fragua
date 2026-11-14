package host

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mentasystems/fragua/internal/core"
	"github.com/mentasystems/fragua/internal/script"
)

// nextActivity returns the next activity event on ch, or false after a short
// wait. Change events (board_changed …) are skipped.
func nextActivity(t *testing.T, ch chan core.Event) (core.Event, bool) {
	t.Helper()
	deadline := time.After(300 * time.Millisecond)
	for {
		select {
		case ev := <-ch:
			if ev.Kind == core.EventActivity {
				return ev, true
			}
		case <-deadline:
			return core.Event{}, false
		}
	}
}

func postScript(t *testing.T, srv *httptest.Server, client, body string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/script", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "text/plain")
	if client != "" {
		req.Header.Set(script.ClientHeader, client)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
}

// Script lines from anyone but the UI reach the human's log as activity
// events; the UI's own lines do not come back to it.
func TestScriptPublishesAgentActivity(t *testing.T) {
	p := core.NewProject("activity")
	srv := httptest.NewServer(Handler(p))
	t.Cleanup(srv.Close)
	ch := p.Events().Subscribe(64)
	defer p.Events().Unsubscribe(ch)

	postScript(t, srv, "", "outline 30 20")
	ev, ok := nextActivity(t, ch)
	if !ok {
		t.Fatal("POST /script without a client tag published no activity")
	}
	if ev.Source != "http" || ev.Level != core.ActivitySuccess || ev.Message != "outline 30 20" || !strings.HasPrefix(ev.Detail, "ok outline:") {
		t.Fatalf("activity = %+v", ev)
	}

	postScript(t, srv, "MCP", "outline 31 20\nbogus-verb")
	ev, ok = nextActivity(t, ch)
	if !ok || ev.Source != "mcp" || ev.Level != core.ActivityError || !strings.Contains(ev.Detail, "error") {
		t.Fatalf("mcp activity = %+v (ok=%v)", ev, ok)
	}

	postScript(t, srv, "ui", "outline 32 20")
	if ev, ok := nextActivity(t, ch); ok {
		t.Fatalf("the UI's own script came back as activity: %+v", ev)
	}
}

func TestActivityClipsLongScripts(t *testing.T) {
	p := core.NewProject("clip")
	ch := p.Events().Subscribe(8)
	defer p.Events().Unsubscribe(ch)
	src := strings.Repeat("status\n", 2000)
	script.PublishActivity(p, "mcp", src, script.RunScript(p, src))
	ev, ok := nextActivity(t, ch)
	if !ok {
		t.Fatal("no activity")
	}
	if len(ev.Message) > 4200 || !strings.Contains(ev.Message, "more lines)") {
		t.Fatalf("message not clipped: %d bytes", len(ev.Message))
	}
}

func TestControlAddr(t *testing.T) {
	for in, want := range map[string]string{
		"7878":           "127.0.0.1:7878",
		":9000":          ":9000",
		"0.0.0.0:7000":   "0.0.0.0:7000",
		"localhost:8080": "localhost:8080",
	} {
		got, err := ControlAddr(in)
		if err != nil || got != want {
			t.Fatalf("ControlAddr(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "0", "70000", "abc", "host:"} {
		if got, err := ControlAddr(bad); err == nil {
			t.Fatalf("ControlAddr(%q) = %q, want an error", bad, got)
		}
	}
}
