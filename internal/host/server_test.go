package host

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mentasystems/fragua/internal/core"
)

func TestObserverMux(t *testing.T) {
	p := core.NewProject("ui-test")
	srv := httptest.NewServer(Handler(p))
	t.Cleanup(srv.Close)

	ui := get(t, srv, "/ui/")
	if ui.status != 200 {
		t.Fatalf("/ui/ status %d", ui.status)
	}
	if !strings.Contains(ui.contentType, "text/html") {
		t.Fatalf("/ui/ content-type %q", ui.contentType)
	}
	if !bytes.Contains(ui.body, []byte("<!DOCTYPE html>")) || !bytes.Contains(ui.body, []byte("FRAGUA")) {
		t.Fatalf("/ui/ is not the observer HTML")
	}
	if !bytes.Contains(ui.body, []byte(`id="board"`)) {
		t.Fatalf("/ui/ missing board image")
	}

	empty := get(t, srv, "/screenshot")
	if empty.status != 200 {
		t.Fatalf("/screenshot status %d", empty.status)
	}
	if !strings.Contains(empty.contentType, "image/svg+xml") {
		t.Fatalf("/screenshot content-type %q", empty.contentType)
	}
	assertPlausibleSVG(t, empty.body)
	if !bytes.Contains(empty.body, []byte("empty board")) {
		t.Fatalf("empty project screenshot should label the empty canvas")
	}

	st := post(t, srv, "/script", "text/plain", "status")
	if st.status != 200 {
		t.Fatalf("POST /script status HTTP %d", st.status)
	}
	if !bytes.Contains(st.body, []byte("ok status:")) {
		t.Fatalf("POST /script status body: %s", st.body)
	}

	ol := post(t, srv, "/script", "text/plain", "outline 40 30")
	if ol.status != 200 {
		t.Fatalf("POST /script outline HTTP %d", ol.status)
	}
	if !bytes.Contains(ol.body, []byte("ok outline:")) {
		t.Fatalf("POST /script outline body: %s", ol.body)
	}

	outlined := get(t, srv, "/screenshot")
	assertPlausibleSVG(t, outlined.body)
	if !bytes.Contains(outlined.body, []byte("40.0 mm")) || !bytes.Contains(outlined.body, []byte("30.0 mm")) {
		t.Fatalf("outlined screenshot missing dimension labels: %s", truncate(outlined.body, 200))
	}
	if bytes.Equal(empty.body, outlined.body) {
		t.Fatal("screenshot did not change after outline")
	}
}

func TestEventsHello(t *testing.T) {
	p := core.NewProject("events")
	srv := httptest.NewServer(Handler(p))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if !strings.Contains(res.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("events content-type %q", res.Header.Get("Content-Type"))
	}
	sc := bufio.NewScanner(res.Body)
	for sc.Scan() {
		if strings.Contains(sc.Text(), "hello") {
			cancel()
			return
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		t.Fatal(err)
	}
	t.Fatal("GET /events never flushed a hello event")
}

func TestHealthAndRootStayAgentAPI(t *testing.T) {
	srv := httptest.NewServer(Handler(core.NewProject("api")))
	t.Cleanup(srv.Close)

	h := get(t, srv, "/health")
	if h.status != 200 || !bytes.Contains(h.body, []byte("ok")) {
		t.Fatalf("/health: %d %s", h.status, h.body)
	}
	root := get(t, srv, "/")
	if root.status != 200 || !strings.Contains(root.contentType, "text/plain") {
		t.Fatalf("/ should stay the text script reference, got %d %q", root.status, root.contentType)
	}
	st := get(t, srv, "/state")
	if st.status != 200 || !strings.Contains(st.contentType, "application/json") {
		t.Fatalf("/state: %d %q", st.status, st.contentType)
	}
}

type httpResult struct {
	status      int
	contentType string
	body        []byte
}

func get(t *testing.T, srv *httptest.Server, path string) httpResult {
	t.Helper()
	res, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return httpResult{status: res.StatusCode, contentType: res.Header.Get("Content-Type"), body: body}
}

func post(t *testing.T, srv *httptest.Server, path, ct, body string) httpResult {
	t.Helper()
	res, err := http.Post(srv.URL+path, ct, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return httpResult{status: res.StatusCode, contentType: res.Header.Get("Content-Type"), body: b}
}

func assertPlausibleSVG(t *testing.T, body []byte) {
	t.Helper()
	if !bytes.Contains(body, []byte(`<svg`)) || !bytes.Contains(body, []byte(`xmlns="http://www.w3.org/2000/svg"`)) {
		t.Fatalf("not an SVG: %s", truncate(body, 160))
	}
	if !bytes.Contains(body, []byte("viewBox=")) {
		t.Fatalf("SVG missing viewBox: %s", truncate(body, 160))
	}
	if bytes.Contains(body, []byte(`width="100%"`)) || bytes.Contains(body, []byte(`height="100%"`)) {
		t.Fatalf("SVG still uses percentage size (collapses in <img>/<object>): %s", truncate(body, 160))
	}
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
