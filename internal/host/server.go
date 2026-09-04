package host

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/mentasystems/fragua/internal/core"
	"github.com/mentasystems/fragua/internal/render"
	"github.com/mentasystems/fragua/internal/script"
)

//go:embed ui/*
var embeddedUI embed.FS

// Addr returns the API listen address (FRAGUA_API_ADDR, else the default).
func Addr() string {
	if a := os.Getenv("FRAGUA_API_ADDR"); a != "" {
		return a
	}
	return "127.0.0.1:7878"
}

// Server is a started host: the project it mutates plus its shutdown hook.
type Server struct {
	Project  *core.Project
	Addr     string
	Shutdown func(context.Context) error
}

// Start loads path, serves the API in the background and opens the browser.
// Progress notes go to logw (stderr when stdout is a protocol channel).
// A failure to bind usually means another fragua already owns the address.
func Start(projectPath string, logw io.Writer) (*Server, error) {
	p, err := loadProject(projectPath)
	if err != nil {
		return nil, err
	}
	addr := Addr()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Handler: Handler(p), ReadHeaderTimeout: 10 * time.Second}

	fmt.Fprintf(logw, "Fragua (Go) — API on http://%s  ·  UI http://%s/ui/  ·  GET /help for the script reference\n", addr, addr)
	if projectPath != "" {
		fmt.Fprintf(logw, "Opened %s\n", projectPath)
	}

	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "http: %v\n", err)
		}
	}()

	if os.Getenv("FRAGUA_NO_BROWSER") == "" {
		openBrowser("http://" + addr + "/ui/")
	}
	return &Server{Project: p, Addr: addr, Shutdown: srv.Shutdown}, nil
}

// Run starts the HTTP API, optionally loads path, opens browser, blocks until signal.
func Run(projectPath string) error {
	s, err := Start(projectPath, os.Stdout)
	if err != nil {
		return err
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return s.Shutdown(ctx)
}

// Alive reports whether a fragua host already answers /health at addr.
func Alive(addr string) bool {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://" + addr + "/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	return resp.StatusCode == http.StatusOK && strings.TrimSpace(string(body)) == "ok"
}

func loadProject(projectPath string) (*core.Project, error) {
	if projectPath == "" {
		return core.NewProject("untitled"), nil
	}
	p, err := core.LoadFromPath(projectPath)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", projectPath, err)
	}
	return p, nil
}

// Handler serves the agent HTTP API and the observer UI for p.
func Handler(p *core.Project) http.Handler {
	if p == nil {
		p = core.NewProject("untitled")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/help" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if verb := r.URL.Query().Get("verb"); verb != "" {
			out, ok := script.VerbUsage(verb)
			if !ok {
				w.WriteHeader(http.StatusNotFound)
			}
			_, _ = io.WriteString(w, out)
			return
		}
		_, _ = io.WriteString(w, script.Usage())
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/state", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(p.SnapshotFile())
	})
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "SSE unsupported", 500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		ch := p.Events().Subscribe(32)
		defer p.Events().Unsubscribe(ch)
		fmt.Fprintf(w, "data: {\"kind\":\"hello\"}\n\n")
		flusher.Flush()
		keepalive := time.NewTicker(15 * time.Second)
		defer keepalive.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-keepalive.C:
				fmt.Fprintf(w, ": keepalive\n\n")
				flusher.Flush()
			case ev, ok := <-ch:
				if !ok {
					return
				}
				b, _ := json.Marshal(ev)
				fmt.Fprintf(w, "data: %s\n\n", b)
				flusher.Flush()
			}
		}
	})
	mux.HandleFunc("/screenshot", func(w http.ResponseWriter, r *http.Request) {
		var opts render.Options
		// drc=1 costs a full check, so it is opt-in: the UI normally draws
		// the markers itself from GET /drc into the same `drc` group.
		if r.URL.Query().Get("drc") == "1" {
			opts.Markers = markersFor(runDRC(p))
		}
		p.RLock()
		svg := render.BoardSVGWith(p.Board(), opts)
		p.RUnlock()
		w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
		_, _ = io.WriteString(w, svg)
	})
	mux.HandleFunc("/schematic", func(w http.ResponseWriter, _ *http.Request) {
		p.RLock()
		svg := render.SchematicSVG(p.Schematic())
		p.RUnlock()
		w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
		_, _ = io.WriteString(w, svg)
	})
	mux.HandleFunc("/drc", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, runDRC(p))
	})
	mux.HandleFunc("/erc", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, runERC(p))
	})
	mux.HandleFunc("/summary", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, summarize(p))
	})
	mux.HandleFunc("/cancel", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		op := p.Ops().Cancel()
		writeJSON(w, map[string]any{"cancelled": op != "", "op": op})
	})
	mux.HandleFunc("/script", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		src := string(body)
		ct := r.Header.Get("Content-Type")
		if strings.Contains(ct, "json") || strings.HasPrefix(strings.TrimSpace(src), "{") {
			var payload struct {
				Script string `json:"script"`
			}
			if err := json.Unmarshal(body, &payload); err == nil && payload.Script != "" {
				src = payload.Script
			}
		}
		rs := script.RunScript(p, src)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, script.FormatResults(rs))
	})
	mux.HandleFunc("/save", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		path := p.SavePath()
		var payload struct {
			Path string `json:"path"`
		}
		if json.Unmarshal(body, &payload) == nil && payload.Path != "" {
			path = payload.Path
		}
		if path == "" {
			http.Error(w, "path required", 400)
			return
		}
		if err := p.SaveToPath(path); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "saved %s\n", path)
	})
	mountUI(mux)
	mux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusFound)
	})
	return withCORS(mux)
}

func mountUI(mux *http.ServeMux) {
	uiDir := findUIDir()
	if uiDir != "" {
		mux.Handle("/ui/", http.StripPrefix("/ui/", http.FileServer(http.Dir(uiDir))))
		return
	}
	if sub, err := fs.Sub(embeddedUI, "ui"); err == nil {
		mux.Handle("/ui/", http.StripPrefix("/ui/", http.FileServer(http.FS(sub))))
		return
	}
	mux.HandleFunc("/ui/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "UI not found", 404)
	})
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(204)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return
	}
	if err := cmd.Start(); err != nil {
		return
	}
	// Reap it: the helper exits immediately once the browser is up, and an
	// unwaited child stays a zombie for the life of the server.
	go func() { _ = cmd.Wait() }()
}

func findUIDir() string {
	candidates := []string{
		filepath.Join("internal", "host", "ui"),
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates, filepath.Join(dir, "ui"))
	}
	if wd, err := os.Getwd(); err == nil {
		d := wd
		for i := 0; i < 6; i++ {
			candidates = append(candidates, filepath.Join(d, "internal", "host", "ui"))
			candidates = append(candidates, filepath.Join(d, "ui"))
			parent := filepath.Dir(d)
			if parent == d {
				break
			}
			d = parent
		}
	}
	for _, c := range candidates {
		if st, err := os.Stat(filepath.Join(c, "index.html")); err == nil && !st.IsDir() {
			return c
		}
	}
	return ""
}
