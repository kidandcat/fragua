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

// Run starts the HTTP API, optionally loads path, opens browser, blocks until signal.
func Run(projectPath string) error {
	var (
		p   *core.Project
		err error
	)
	if projectPath != "" {
		p, err = core.LoadFromPath(projectPath)
		if err != nil {
			return fmt.Errorf("load %s: %w", projectPath, err)
		}
	} else {
		p = core.NewProject("untitled")
	}

	addr := os.Getenv("FRAGUA_API_ADDR")
	if addr == "" {
		addr = "127.0.0.1:7878"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/help" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
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
		ch := p.Events().Subscribe(32)
		defer p.Events().Unsubscribe(ch)
		// initial ping
		fmt.Fprintf(w, "data: {\"kind\":\"hello\"}\n\n")
		flusher.Flush()
		for {
			select {
			case <-r.Context().Done():
				return
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
		p.RLock()
		svg := render.BoardSVG(p.Board())
		p.RUnlock()
		// Prefer SVG until PNG pipeline is wired; agents accept either.
		w.Header().Set("Content-Type", "image/svg+xml")
		_, _ = io.WriteString(w, svg)
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
	// Static UI: on-disk internal/host/ui for live edit, else embed.
	uiDir := findUIDir()
	if uiDir != "" {
		mux.Handle("/ui/", http.StripPrefix("/ui/", http.FileServer(http.Dir(uiDir))))
	} else if sub, err := fs.Sub(embeddedUI, "ui"); err == nil {
		mux.Handle("/ui/", http.StripPrefix("/ui/", http.FileServer(http.FS(sub))))
	} else {
		mux.HandleFunc("/ui/", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "UI not found", 404)
		})
	}
	mux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusFound)
	})

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: withCORS(mux), ReadHeaderTimeout: 10 * time.Second}

	fmt.Printf("Fragua (Go) — API on http://%s  ·  GET /help for the script reference\n", addr)
	if projectPath != "" {
		fmt.Printf("Opened %s\n", projectPath)
	}

	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "http: %v\n", err)
		}
	}()

	if os.Getenv("FRAGUA_NO_BROWSER") == "" {
		openBrowser("http://" + addr + "/ui/")
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return srv.Shutdown(ctx)
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
	_ = cmd.Start()
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
