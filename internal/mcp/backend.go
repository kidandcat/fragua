// Package mcp serves the fragua script API to AI agents over MCP (stdio).
//
// The same verbs the HTTP API exposes become MCP tools, so an agent can drive
// a board while the human watches the browser UI on the same live project.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mentasystems/fragua/internal/core"
	"github.com/mentasystems/fragua/internal/render"
	"github.com/mentasystems/fragua/internal/script"
)

// Backend is the project the MCP tools act on: either the in-process host or
// another fragua already listening on the API address.
type Backend interface {
	// Script runs a multi-line script and returns the per-line text results.
	Script(ctx context.Context, src string) (string, error)
	// State returns the JSON project snapshot.
	State(ctx context.Context) ([]byte, error)
	// ScreenshotSVG returns the current board rendering as SVG.
	ScreenshotSVG(ctx context.Context) (string, error)
	// Save writes the project; an empty path reuses the bound autosave path.
	Save(ctx context.Context, path string) (string, error)
	// Addr is the HTTP API / UI address backing this session.
	Addr() string
	// Mode is "in-process" or "proxy", for the status text.
	Mode() string
}

// Local drives an in-process project — the one the browser UI is showing.
type Local struct {
	Project *core.Project
	APIAddr string
}

func (l *Local) Addr() string { return l.APIAddr }
func (l *Local) Mode() string { return "in-process" }

func (l *Local) Script(_ context.Context, src string) (string, error) {
	return script.FormatResults(script.RunScript(l.Project, src)), nil
}

func (l *Local) State(_ context.Context) ([]byte, error) {
	return json.MarshalIndent(l.Project.SnapshotFile(), "", "  ")
}

func (l *Local) ScreenshotSVG(_ context.Context) (string, error) {
	l.Project.RLock()
	defer l.Project.RUnlock()
	return render.BoardSVG(l.Project.Board()), nil
}

func (l *Local) Save(_ context.Context, path string) (string, error) {
	if path == "" {
		path = l.Project.SavePath()
	}
	if path == "" {
		return "", fmt.Errorf("no path: this session is memory-only, pass a path to save to")
	}
	if err := l.Project.SaveToPath(path); err != nil {
		return "", err
	}
	return "saved " + path, nil
}

// Proxy forwards to a fragua HTTP API that already owns the address.
type Proxy struct {
	APIAddr string
	Client  *http.Client
}

// NewProxy targets an existing fragua host at addr.
func NewProxy(addr string) *Proxy {
	// Routing can legitimately run for max_seconds=3600; do not cut it short.
	return &Proxy{APIAddr: addr, Client: &http.Client{Timeout: 65 * time.Minute}}
}

func (p *Proxy) Addr() string { return p.APIAddr }
func (p *Proxy) Mode() string { return "proxy" }
func (p *Proxy) url(path string) string {
	return "http://" + p.APIAddr + path
}

func (p *Proxy) Script(ctx context.Context, src string) (string, error) {
	body, _ := json.Marshal(map[string]string{"script": src})
	b, err := p.do(ctx, http.MethodPost, "/script", body)
	return string(b), err
}

func (p *Proxy) State(ctx context.Context) ([]byte, error) {
	return p.do(ctx, http.MethodGet, "/state", nil)
}

func (p *Proxy) ScreenshotSVG(ctx context.Context) (string, error) {
	b, err := p.do(ctx, http.MethodGet, "/screenshot", nil)
	return string(b), err
}

func (p *Proxy) Save(ctx context.Context, path string) (string, error) {
	body, _ := json.Marshal(map[string]string{"path": path})
	b, err := p.do(ctx, http.MethodPost, "/save", body)
	return strings.TrimSpace(string(b)), err
}

func (p *Proxy) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.url(path), rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fragua at %s: %w", p.APIAddr, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(out)))
	}
	return out, nil
}
