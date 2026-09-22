// duckpipe — pipe CSV from duckdb through ggsql and get a shareable Vega-Lite URL.
//
// Usage:
//
//	duckdb mydb.db -csv -header "SELECT x, y FROM t" \
//	  | duckpipe --visual "VISUALISE x AS x, y AS y DRAW point LABEL title => 'My Chart'"
//
// The --visual flag accepts everything after the SQL SELECT in a ggsql query.
// The tool writes stdin CSV to a temp file, runs ggsql in a podman container,
// and encodes the resulting Vega-Lite JSON as a shareable editor URL.
//
// The pure PNG/spec → output-format conversion logic lives in
// internal/chartconv, shared with the ggvisual HTTP service — see
// /persistent-service.md for why: this CLI keeps its podman-per-request
// invocation unchanged, while ggvisual runs ggsql/vl-convert as local
// subprocesses inside the merged image.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"ggsql-tools/internal/chartconv"

	"golang.org/x/term"
)

const defaultImage = "localhost/ggsql:latest"

func main() {
	visual := flag.String("visual", "", "ggsql VISUALISE clause (everything after the SELECT)")
	image := flag.String("image", defaultImage, "container image for ggsql")
	format := flag.String("format", "url", "output format: url, vegalite, html, html-page, html-page-offline, html-body, png, ansi, braille, svg, braille-svg, xterm, xterm-page, braille-xterm, braille-xterm-page, text, braille-text, cast, cast-page")
	width := flag.Int("width", 0, "output width in chars (braille/ansi); 0 = auto from image")
	height := flag.Int("height", 0, "output height in chars (braille/ansi); 0 = auto from image")
	pngWidth := flag.Int("png-width", 600, `PNG width for raster formats; replaces "container" in the Vega-Lite spec`)
	pngHeight := flag.Int("png-height", 400, `PNG height for raster formats; replaces "container" in the Vega-Lite spec`)
	flag.Parse()

	if *visual == "" {
		fmt.Fprintln(os.Stderr, "duckpipe: --visual is required")
		fmt.Fprintln(os.Stderr, `  example: --visual "VISUALISE x AS x, y AS y DRAW point"`)
		os.Exit(1)
	}

	if fi, _ := os.Stdin.Stat(); fi.Mode()&os.ModeCharDevice != 0 {
		fmt.Fprintln(os.Stderr, "duckpipe: no data on stdin — pipe CSV (with header) from duckdb")
		os.Exit(1)
	}

	tmpCSV, err := os.CreateTemp("", "duckpipe-*.csv")
	if err != nil {
		fatal("create temp file", err)
	}
	defer os.Remove(tmpCSV.Name())

	if _, err := io.Copy(tmpCSV, os.Stdin); err != nil {
		fatal("read stdin", err)
	}
	tmpCSV.Close()

	// The query reads the mounted CSV from /data/input.csv inside the container.
	query := fmt.Sprintf(
		"SELECT * FROM read_csv('/data/input.csv', header=true)\n%s",
		strings.TrimSpace(*visual),
	)
	mount := tmpCSV.Name() + ":/data/input.csv:ro"

	switch *format {
	case "png", "ansi", "braille", "svg", "braille-svg", "xterm", "xterm-page", "braille-xterm", "braille-xterm-page", "text", "braille-text", "cast", "cast-page":
		pngBytes := containerPNG(query, mount, *image, *pngWidth, *pngHeight)
		writeRaster(*format, pngBytes, *width, *height)
		return
	}

	args := []string{
		"run", "--rm",
		"-v", mount,
		*image,
		"exec", query,
		"--reader", "duckdb://memory",
	}

	out, err := exec.Command("podman", args...).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			fmt.Fprintf(os.Stderr, "ggsql: %s\n", ee.Stderr)
		}
		fatal("run ggsql", err)
	}

	spec := bytes.TrimSpace(out)
	switch *format {
	case "vegalite":
		os.Stdout.Write(spec)
		fmt.Println()
	case "html":
		snippet, err := chartconv.HTMLSnippet(spec)
		if err != nil {
			fatal("encode HTML", err)
		}
		fmt.Println(snippet)
	case "html-page":
		snippet, err := chartconv.HTMLBodySnippet(spec)
		if err != nil {
			fatal("encode HTML", err)
		}
		head := `<script src="https://cdn.jsdelivr.net/npm/vega@6"></script>` + "\n" +
			`<script src="https://cdn.jsdelivr.net/npm/vega-lite@6"></script>` + "\n" +
			`<script src="https://cdn.jsdelivr.net/npm/vega-embed@7"></script>`
		fmt.Println(chartconv.WrapHTMLPage(head, snippet))
	case "html-page-offline":
		page, err := chartconv.OfflineHTMLPage(spec)
		if err != nil {
			fatal("encode HTML", err)
		}
		os.Stdout.Write(page)
		fmt.Println()
	case "html-body":
		snippet, err := chartconv.HTMLBodySnippet(spec)
		if err != nil {
			fatal("encode HTML", err)
		}
		fmt.Println(snippet)
	default:
		u, err := chartconv.VegaLiteEditorURL(spec)
		if err != nil {
			fatal("encode URL", err)
		}
		fmt.Println(u)
	}
}

// writeRaster dispatches a rendered PNG to the chartconv format converters
// and prints the result to stdout, matching each function's former
// print-to-stdout behavior (fatal() on error, same as every other CLI path).
func writeRaster(format string, pngBytes []byte, width, height int) {
	if format == "png" {
		os.Stdout.Write(pngBytes)
		return
	}

	var (
		out []byte
		err error
	)
	switch format {
	case "ansi":
		rows, cols := termSize()
		if width > 0 {
			cols = width
		}
		if height > 0 {
			rows = height
		}
		out, err = chartconv.RenderAnsi(pngBytes, rows, cols)
	case "braille":
		out, err = chartconv.RenderBraille(pngBytes, width, height)
	case "svg":
		out, err = chartconv.RenderSVG(pngBytes, width, height)
	case "braille-svg":
		out, err = chartconv.RenderBrailleSVG(pngBytes, width, height)
	case "xterm":
		out, err = chartconv.RenderXterm(pngBytes, width, height)
	case "xterm-page":
		out, err = chartconv.RenderXtermPage(pngBytes, width, height)
	case "braille-xterm":
		out, err = chartconv.RenderBrailleXterm(pngBytes, width, height)
	case "braille-xterm-page":
		out, err = chartconv.RenderBrailleXtermPage(pngBytes, width, height)
	case "text":
		out, err = chartconv.RenderText(pngBytes, width, height)
	case "braille-text":
		out, err = chartconv.RenderBrailleText(pngBytes, width, height)
	case "cast":
		out, err = chartconv.RenderCast(pngBytes, width, height)
	case "cast-page":
		out, err = chartconv.RenderCastPage(pngBytes, width, height)
	}
	if err != nil {
		fatal("render "+format, err)
	}
	os.Stdout.Write(out)
}

// containerPNG runs ggsql + vl-convert in the container and returns PNG bytes.
// pngWidth/pngHeight replace any "container" values in the Vega-Lite spec so
// vl-convert produces a PNG with the correct dimensions (default 600×400).
func containerPNG(query, mount, image string, pngWidth, pngHeight int) []byte {
	// sed replaces "container" dimension values with explicit pixel sizes so that
	// vl-convert (which has no HTML context) renders at a meaningful aspect ratio.
	patch := fmt.Sprintf(
		`sed 's/"width": "container"/"width": %d/; s/"height": "container"/"height": %d/'`,
		pngWidth, pngHeight,
	)
	shell := fmt.Sprintf(
		"ggsql exec %s --reader duckdb://memory | %s > /tmp/spec.json"+
			" && vl-convert vl2png -i /tmp/spec.json -o /tmp/out.png"+
			" && cat /tmp/out.png",
		shellQuote(query), patch,
	)
	args := []string{
		"run", "--rm",
		"--entrypoint", "/bin/sh",
		"-v", mount,
		image,
		"-c", shell,
	}
	out, err := exec.Command("podman", args...).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			fmt.Fprintf(os.Stderr, "ggsql|vl-convert: %s\n", ee.Stderr)
		}
		fatal("render PNG", err)
	}
	return out
}

// termSize returns the current terminal dimensions, falling back to 40×80.
func termSize() (rows, cols int) {
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return 40, 80
	}
	return h, w
}

// shellQuote wraps s in single quotes, escaping any single quotes within.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func fatal(msg string, err error) {
	fmt.Fprintf(os.Stderr, "duckpipe: %s: %v\n", msg, err)
	os.Exit(1)
}
