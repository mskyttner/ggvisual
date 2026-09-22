// ggvisual is the persistent HTTP service form of duckpipe's ggsql/
// vl-convert rendering pipeline — see /persistent-service.md in this repo
// for the design. It runs inside the merged ggsql+vl-convert image (built
// via `podman build --target service`) and talks to both binaries as local
// subprocesses instead of spawning a container per request.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"ggsql-tools/internal/chartconv"
	"ggsql-tools/internal/ggexec"
)

const (
	defaultRequestTimeout = 30 * time.Second
	defaultShutdownGrace  = 10 * time.Second
	tempSweepInterval     = 10 * time.Minute
	tempSweepAge          = 30 * time.Minute
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	requestTimeout := flag.Duration("request-timeout", defaultRequestTimeout, "per-request timeout for ggsql/vl-convert subprocess calls")
	shutdownGrace := flag.Duration("shutdown-grace", defaultShutdownGrace, "grace period to finish in-flight requests on SIGTERM/SIGINT")
	memLimit := flag.Int64("subprocess-memory-limit", ggexec.DefaultMemoryLimit, "RLIMIT_AS (bytes) applied to each ggsql/vl-convert subprocess")
	flag.Parse()

	runner := &ggexec.Runner{MemoryLimit: *memLimit}
	srv := &server{
		runner:         runner,
		requestTimeout: *requestTimeout,
	}

	sweepTempFiles() // startup sweep — see "Temp-file cleanup"
	go periodicSweep()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /render", srv.handleRender)
	mux.HandleFunc("POST /validate", srv.handleValidate)
	mux.HandleFunc("GET /formats", srv.handleFormats)
	mux.HandleFunc("GET /health", srv.handleHealth)
	mux.HandleFunc("GET /version", srv.handleVersion)

	httpSrv := &http.Server{Addr: *addr, Handler: mux}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("ggvisual listening on %s", *addr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("shutting down (grace period %s)", *shutdownGrace)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), *shutdownGrace)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v (in-flight requests may have been cut off)", err)
	}
}

type server struct {
	runner         *ggexec.Runner
	requestTimeout time.Duration
}

// errorResponse is the structured JSON error envelope from "Structured
// error envelope" — always JSON, regardless of the requested render format,
// per ggsql-endpoint.md.
type errorResponse struct {
	Error struct {
		Category string `json:"category"`
		Message  string `json:"message"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, err error) {
	category := string(ggexec.CategoryInternal)
	message := err.Error()
	if e, ok := err.(*ggexec.Error); ok {
		category = string(e.Category)
		message = e.Message
	}

	status := http.StatusInternalServerError
	switch ggexec.Category(category) {
	case ggexec.CategoryBadGgsql, ggexec.CategoryBadSQL:
		status = http.StatusBadRequest
	case ggexec.CategoryTimeout:
		status = http.StatusGatewayTimeout
	case ggexec.CategoryInternal:
		status = http.StatusInternalServerError
	}

	resp := errorResponse{}
	resp.Error.Category = category
	resp.Error.Message = message

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(resp)
}

// contentTypeFor mirrors the format-aware Content-Type table from
// ggsql-endpoint.md — raw bytes for image/terminal-art formats, never
// JSON-wrapped, since that breaks ANSI escape sequences.
func contentTypeFor(format string) string {
	switch format {
	case "vegalite", "":
		return "application/json"
	case "png":
		return "image/png"
	case "svg", "braille-svg":
		return "image/svg+xml"
	case "ansi", "braille", "text", "braille-text":
		return "text/plain; charset=utf-8"
	case "html", "html-page", "html-page-offline", "html-body",
		"xterm", "xterm-page", "braille-xterm", "braille-xterm-page", "cast-page":
		return "text/html; charset=utf-8"
	case "cast":
		return "text/plain; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

func (s *server) handleRender(w http.ResponseWriter, r *http.Request) {
	visual := r.URL.Query().Get("visual")
	format := r.URL.Query().Get("format")
	if visual == "" {
		writeError(w, &ggexec.Error{Category: ggexec.CategoryBadGgsql, Message: "missing required query param: visual"})
		return
	}
	pngWidth := queryInt(r, "png-width", 600)
	pngHeight := queryInt(r, "png-height", 400)
	width := queryInt(r, "width", 0)
	height := queryInt(r, "height", 0)

	tmpCSV, err := os.CreateTemp("", "ggvisual-*.csv")
	if err != nil {
		writeError(w, &ggexec.Error{Category: ggexec.CategoryInternal, Message: err.Error()})
		return
	}
	defer os.Remove(tmpCSV.Name())
	if _, err := io.Copy(tmpCSV, r.Body); err != nil {
		tmpCSV.Close()
		writeError(w, &ggexec.Error{Category: ggexec.CategoryInternal, Message: err.Error()})
		return
	}
	tmpCSV.Close()

	ctx, cancel := context.WithTimeout(r.Context(), s.requestTimeout)
	defer cancel()

	query := fmt.Sprintf("SELECT * FROM read_csv('%s', header=true)\n%s", tmpCSV.Name(), strings.TrimSpace(visual))

	needsPNG := map[string]bool{
		"png": true, "ansi": true, "braille": true, "svg": true, "braille-svg": true,
		"xterm": true, "xterm-page": true, "braille-xterm": true, "braille-xterm-page": true,
		"text": true, "braille-text": true, "cast": true, "cast-page": true,
	}

	w.Header().Set("Content-Type", contentTypeFor(format))

	if needsPNG[format] {
		spec, err := s.runner.Exec(ctx, query)
		if err != nil {
			writeError(w, err)
			return
		}
		png, err := s.runner.RenderPNG(ctx, spec, pngWidth, pngHeight)
		if err != nil {
			writeError(w, err)
			return
		}
		out, err := convertPNG(format, png, width, height)
		if err != nil {
			writeError(w, &ggexec.Error{Category: ggexec.CategoryInternal, Message: err.Error()})
			return
		}
		w.Write(out)
		return
	}

	spec, err := s.runner.Exec(ctx, query)
	if err != nil {
		writeError(w, err)
		return
	}

	out, err := convertSpec(format, spec)
	if err != nil {
		writeError(w, &ggexec.Error{Category: ggexec.CategoryInternal, Message: err.Error()})
		return
	}
	w.Write(out)
}

// convertPNG dispatches to chartconv for every raster-derived format.
func convertPNG(format string, png []byte, width, height int) ([]byte, error) {
	switch format {
	case "png":
		return png, nil
	case "ansi":
		rows, cols := height, width
		if rows == 0 {
			rows = 40
		}
		if cols == 0 {
			cols = 80
		}
		return chartconv.RenderAnsi(png, rows, cols)
	case "braille":
		return chartconv.RenderBraille(png, width, height)
	case "svg":
		return chartconv.RenderSVG(png, width, height)
	case "braille-svg":
		return chartconv.RenderBrailleSVG(png, width, height)
	case "xterm":
		return chartconv.RenderXterm(png, width, height)
	case "xterm-page":
		return chartconv.RenderXtermPage(png, width, height)
	case "braille-xterm":
		return chartconv.RenderBrailleXterm(png, width, height)
	case "braille-xterm-page":
		return chartconv.RenderBrailleXtermPage(png, width, height)
	case "text":
		return chartconv.RenderText(png, width, height)
	case "braille-text":
		return chartconv.RenderBrailleText(png, width, height)
	case "cast":
		return chartconv.RenderCast(png, width, height)
	case "cast-page":
		return chartconv.RenderCastPage(png, width, height)
	default:
		return nil, fmt.Errorf("unsupported raster format: %s", format)
	}
}

// convertSpec dispatches to chartconv for every Vega-Lite-spec-derived
// format (no PNG/vl-convert step needed).
func convertSpec(format string, spec []byte) ([]byte, error) {
	switch format {
	case "vegalite", "":
		return spec, nil
	case "html":
		s, err := chartconv.HTMLSnippet(spec)
		return []byte(s), err
	case "html-page":
		snippet, err := chartconv.HTMLBodySnippet(spec)
		if err != nil {
			return nil, err
		}
		head := `<script src="https://cdn.jsdelivr.net/npm/vega@6"></script>` + "\n" +
			`<script src="https://cdn.jsdelivr.net/npm/vega-lite@6"></script>` + "\n" +
			`<script src="https://cdn.jsdelivr.net/npm/vega-embed@7"></script>`
		return []byte(chartconv.WrapHTMLPage(head, snippet)), nil
	case "html-page-offline":
		return chartconv.OfflineHTMLPage(spec)
	case "html-body":
		s, err := chartconv.HTMLBodySnippet(spec)
		return []byte(s), err
	default:
		u, err := chartconv.VegaLiteEditorURL(spec)
		return []byte(u), err
	}
}

func (s *server) handleValidate(w http.ResponseWriter, r *http.Request) {
	visual := r.URL.Query().Get("visual")
	if visual == "" {
		writeError(w, &ggexec.Error{Category: ggexec.CategoryBadGgsql, Message: "missing required query param: visual"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.requestTimeout)
	defer cancel()
	// A minimal 1-column placeholder table is enough for a syntax-only
	// check — the caller's real CSV data doesn't matter for validation.
	query := fmt.Sprintf("SELECT 1 AS x\n%s", strings.TrimSpace(visual))
	if err := s.runner.Validate(ctx, query); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleFormats(w http.ResponseWriter, r *http.Request) {
	formats := []string{
		"url", "vegalite", "html", "html-page", "html-page-offline", "html-body",
		"png", "ansi", "braille", "svg", "braille-svg",
		"xterm", "xterm-page", "braille-xterm", "braille-xterm-page",
		"text", "braille-text", "cast", "cast-page",
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(formats)
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (s *server) handleVersion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"ggsql":      binaryVersion("ggsql", "--version"),
		"vl-convert": binaryVersion("vl-convert", "--version"),
	})
}

// binaryVersion runs `<name> <versionFlag>` and returns its trimmed stdout,
// or "unknown" if that fails — exposing real provenance per the
// extension-vs-CLI version skew already on record in persistent-service.md.
func binaryVersion(name, versionFlag string) string {
	out, err := exec.Command(name, versionFlag).Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

func queryInt(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// sweepTempFiles removes stale ggvisual-*.csv files left behind by a killed
// process (SIGKILL bypasses Go's deferred cleanup the same way os.Exit
// does) — see "Temp-file cleanup".
func sweepTempFiles() {
	matches, _ := filepath.Glob(filepath.Join(os.TempDir(), "ggvisual-*.csv"))
	for _, m := range matches {
		info, err := os.Stat(m)
		if err != nil {
			continue
		}
		if time.Since(info.ModTime()) > tempSweepAge {
			os.Remove(m)
		}
	}
}

func periodicSweep() {
	for range time.Tick(tempSweepInterval) {
		sweepTempFiles()
	}
}
