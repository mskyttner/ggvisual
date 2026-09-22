// Package chartconv converts a rendered PNG chart (or a Vega-Lite spec, for
// the HTML-embedding formats) into duckpipe's various terminal/HTML output
// formats. Every exported function is pure — it takes bytes in and returns
// bytes out — so it's usable both by the duckpipe CLI (which prints the
// result to stdout) and by the ggvisual HTTP service (which writes it to a
// response body). No os.Exit, no direct stdout/stderr writes.
package chartconv

import (
	"bytes"
	"compress/flate"
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"regexp"
	"sort"
	"strings"

	"github.com/eliukblau/pixterm/pkg/ansimage"
	"github.com/lucasb-eyer/go-colorful"
	"github.com/wader/ansisvg/ansitosvg"
	"github.com/wader/ansisvg/svgscreen/xydim"
)

const vegaEditorURL = "https://vega.github.io/editor/#/url/vega-lite/"

// Vendored vega/vega-lite/vega-embed for the html-page-offline format's
// self-contained export — see vendor/README.md for exact versions and
// provenance. Embedded (not fetched from CDN at render or view time) so the
// resulting HTML needs no network access at all. Shared by both the
// duckpipe CLI and the ggvisual HTTP service rather than duplicated.
//
//go:embed vendor/vega.min.js
var vegaJS string

//go:embed vendor/vega-lite.min.js
var vegaLiteJS string

//go:embed vendor/vega-embed.min.js
var vegaEmbedJS string

// OfflineHTMLPage returns a complete standalone HTML page embedding spec
// with vega/vega-lite/vega-embed inlined (not loaded from CDN) — the
// html-page-offline format.
func OfflineHTMLPage(spec []byte) ([]byte, error) {
	snippet, err := HTMLBodySnippet(spec)
	if err != nil {
		return nil, err
	}
	head := "<script>" + vegaJS + "</script>\n" +
		"<script>" + vegaLiteJS + "</script>\n" +
		"<script>" + vegaEmbedJS + "</script>"
	return []byte(WrapHTMLPage(head, snippet)), nil
}

// RenderAnsi decodes a PNG and renders it as ANSI true-colour block characters
// (▄ lower-half-block) at the given rows×cols.
func RenderAnsi(pngBytes []byte, rows, cols int) ([]byte, error) {
	img, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		return nil, fmt.Errorf("decode PNG: %w", err)
	}
	ai, err := ansimage.NewScaledFromImage(img, rows, cols, color.Transparent,
		ansimage.ScaleModeFit, ansimage.NoDithering)
	if err != nil {
		return nil, fmt.Errorf("render ANSI image: %w", err)
	}
	return []byte(ai.Render()), nil
}

// RenderBraille decodes a PNG and renders it as coloured Unicode braille
// characters. If width/height are 0, defaults to img_width/2 × img_height/4
// (exact 2×4-pixel-per-cell mapping with no information loss).
func RenderBraille(pngBytes []byte, width, height int) ([]byte, error) {
	img, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		return nil, fmt.Errorf("decode PNG: %w", err)
	}
	b := img.Bounds()
	if width == 0 {
		width = b.Dx() / 2
	}
	if height == 0 {
		height = b.Dy() / 4
	}
	return []byte(renderBrailleANSI(img, width, height)), nil
}

// RenderSVG decodes a PNG, renders it as ANSI half-block characters, then
// converts that ANSI stream to SVG. Default cols = 300 (matches the CLI's
// prior stdout-only default) if width==0.
func RenderSVG(pngBytes []byte, width, height int) ([]byte, error) {
	img, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		return nil, fmt.Errorf("decode PNG: %w", err)
	}
	b := img.Bounds()
	cols := 300
	if width > 0 {
		cols = width
	}
	rows := b.Dy()
	if height > 0 {
		rows = height
	}
	ai, err := ansimage.NewScaledFromImage(img, rows, cols, color.Transparent,
		ansimage.ScaleModeFit, ansimage.NoDithering)
	if err != nil {
		return nil, fmt.Errorf("render ANSI image: %w", err)
	}
	return writeAnsiSVG(ai.Render(), xydim.XyDimInt{X: 1, Y: 2})
}

// RenderBrailleSVG renders a PNG as braille ANSI text and converts it to SVG.
// Default dimensions are img_width/2 × img_height/4 if width/height are 0.
func RenderBrailleSVG(pngBytes []byte, width, height int) ([]byte, error) {
	img, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		return nil, fmt.Errorf("decode PNG: %w", err)
	}
	b := img.Bounds()
	if width == 0 {
		width = 200
	}
	if height == 0 {
		height = (b.Dy() * width * 2) / (b.Dx() * 4)
		if height < 1 {
			height = 1
		}
	}
	ansiText := renderBrailleANSI(img, width, height)
	return writeAnsiSVG(ansiText, xydim.XyDimInt{X: 2, Y: 4})
}

// RenderText decodes a PNG and returns its ANSI half-block rendering with
// colour codes stripped — a plain monochrome character grid.
func RenderText(pngBytes []byte, width, height int) ([]byte, error) {
	img, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		return nil, fmt.Errorf("decode PNG: %w", err)
	}
	b := img.Bounds()
	cols := 300
	if width > 0 {
		cols = width
	}
	rows := (b.Dy() * cols) / (b.Dx() * 2)
	if rows < 1 {
		rows = 1
	}
	if height > 0 {
		rows = height
	}
	background := modeColorIn(img, b.Min.X, b.Max.X, b.Min.Y, b.Max.Y)
	return []byte(renderMonoBlocks(img, cols, rows, background)), nil
}

// RenderBrailleText decodes a PNG and returns its braille rendering with
// colour codes stripped.
func RenderBrailleText(pngBytes []byte, width, height int) ([]byte, error) {
	img, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		return nil, fmt.Errorf("decode PNG: %w", err)
	}
	b := img.Bounds()
	if width == 0 {
		width = 200
	}
	if height == 0 {
		height = (b.Dy() * width * 2) / (b.Dx() * 4)
		if height < 1 {
			height = 1
		}
	}
	return []byte(stripANSI(renderBrailleANSI(img, width, height))), nil
}

// RenderXterm renders the PNG as ANSI half-block characters and returns an
// xterm.js HTML <div>+<script> snippet. Requires Terminal to be loaded
// globally by the host page.
func RenderXterm(pngBytes []byte, width, height int) ([]byte, error) {
	img, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		return nil, fmt.Errorf("decode PNG: %w", err)
	}
	b := img.Bounds()
	cols := width
	if cols == 0 {
		cols = 300
	}
	pixRows := height
	if pixRows == 0 {
		pixRows = b.Dy()
	}
	ai, err := ansimage.NewScaledFromImage(img, pixRows, cols, color.Transparent,
		ansimage.ScaleModeFit, ansimage.NoDithering)
	if err != nil {
		return nil, fmt.Errorf("render ANSI image: %w", err)
	}
	ansiText := ai.Render()
	charRows := strings.Count(ansiText, "\n")
	if charRows < 1 {
		charRows = 1
	}
	background := modeColorIn(img, b.Min.X, b.Max.X, b.Min.Y, b.Max.Y)
	plainText := renderMonoBlocks(img, cols, charRows, background)
	snippet, err := xtermSnippet(ansiText, cols, charRows, "canvas", plainText)
	if err != nil {
		return nil, fmt.Errorf("build xterm snippet: %w", err)
	}
	return []byte(snippet), nil
}

// RenderXtermPage is like RenderXterm but returns a complete standalone HTML
// page (with xterm.js loaded from CDN).
func RenderXtermPage(pngBytes []byte, width, height int) ([]byte, error) {
	snippet, err := RenderXterm(pngBytes, width, height)
	if err != nil {
		return nil, err
	}
	head := `<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/xterm@4/css/xterm.css">` +
		"\n" + `<script src="https://cdn.jsdelivr.net/npm/xterm@4/lib/xterm.js"></script>`
	return []byte(WrapHTMLPage(head, string(snippet))), nil
}

// RenderBrailleXterm renders the PNG as coloured braille and returns an
// xterm.js HTML snippet.
func RenderBrailleXterm(pngBytes []byte, width, height int) ([]byte, error) {
	return brailleXtermSnippet(pngBytes, width, height)
}

// RenderBrailleXtermPage is like RenderBrailleXterm but returns a complete
// standalone HTML page (with xterm.js loaded from CDN).
func RenderBrailleXtermPage(pngBytes []byte, width, height int) ([]byte, error) {
	snippet, err := brailleXtermSnippet(pngBytes, width, height)
	if err != nil {
		return nil, err
	}
	head := `<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/xterm@4/css/xterm.css">` +
		"\n" + `<script src="https://cdn.jsdelivr.net/npm/xterm@4/lib/xterm.js"></script>`
	return []byte(WrapHTMLPage(head, string(snippet))), nil
}

// brailleXtermSnippet decodes the PNG, renders it as coloured braille, and
// returns the xterm.js <div>+<script> snippet. Default cols = 200 (matches
// RenderBrailleSVG).
func brailleXtermSnippet(pngBytes []byte, width, height int) ([]byte, error) {
	img, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		return nil, fmt.Errorf("decode PNG: %w", err)
	}
	b := img.Bounds()
	cols := width
	if cols == 0 {
		cols = 200
	}
	rows := height
	if rows == 0 {
		rows = (b.Dy() * cols * 2) / (b.Dx() * 4)
		if rows < 1 {
			rows = 1
		}
	}

	ansiText := renderBrailleANSI(img, cols, rows)
	snippet, err := xtermSnippet(ansiText, cols, rows, "dom", "")
	if err != nil {
		return nil, fmt.Errorf("build xterm snippet: %w", err)
	}
	return []byte(snippet), nil
}

// RenderCast decodes a PNG, renders it as ANSI half-block art (same pipeline
// as RenderXterm), and returns it as an asciicast v3 recording.
func RenderCast(pngBytes []byte, width, height int) ([]byte, error) {
	return buildCast(pngBytes, width, height)
}

// RenderCastPage is like RenderCast but returns a complete standalone HTML
// page (with asciinema-player loaded from CDN), the recording embedded as a
// base64 data: URL.
func RenderCastPage(pngBytes []byte, width, height int) ([]byte, error) {
	cast, err := buildCast(pngBytes, width, height)
	if err != nil {
		return nil, err
	}
	b64 := base64.StdEncoding.EncodeToString(cast)
	head := `<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/asciinema-player@3/dist/bundle/asciinema-player.css">` +
		"\n" + `<script src="https://cdn.jsdelivr.net/npm/asciinema-player@3/dist/bundle/asciinema-player.min.js"></script>`
	body := fmt.Sprintf(`<div id="cast"></div>
<script>
AsciinemaPlayer.create('data:text/plain;base64,%s', document.getElementById('cast'), {fit: 'width'});
</script>`, b64)
	return []byte(WrapHTMLPage(head, body)), nil
}

// buildCast renders pngBytes as ANSI half-block art and returns it as
// asciicast v3 NDJSON (https://docs.asciinema.org/manual/asciicast/v3/).
func buildCast(pngBytes []byte, width, height int) ([]byte, error) {
	img, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		return nil, fmt.Errorf("decode PNG: %w", err)
	}
	b := img.Bounds()
	cols := width
	if cols == 0 {
		cols = 300
	}
	pixRows := height
	if pixRows == 0 {
		pixRows = b.Dy()
	}
	ai, err := ansimage.NewScaledFromImage(img, pixRows, cols, color.Transparent,
		ansimage.ScaleModeFit, ansimage.NoDithering)
	if err != nil {
		return nil, fmt.Errorf("render ANSI image: %w", err)
	}
	ansiText := ai.Render()
	charRows := strings.Count(ansiText, "\n")
	if charRows < 1 {
		charRows = 1
	}

	ansiText = strings.TrimRight(ansiText, "\n")
	ansiText = strings.ReplaceAll(ansiText, "\r\n", "\n")
	ansiText = strings.ReplaceAll(ansiText, "\n", "\r\n")

	eventData, err := json.Marshal(ansiText)
	if err != nil {
		return nil, fmt.Errorf("encode cast event: %w", err)
	}
	header := fmt.Sprintf(`{"version":3,"term":{"cols":%d,"rows":%d}}`, cols, charRows)
	event := fmt.Sprintf(`[0,"o",%s]`, eventData)
	return []byte(header + "\n" + event + "\n"), nil
}

// WrapHTMLPage wraps an HTML body snippet in a minimal standalone HTML
// document. extraHead is inserted verbatim into <head> (may be empty).
func WrapHTMLPage(extraHead, body string) string {
	return `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
` + extraHead + `
</head>
<body>
` + body + `
</body>
</html>
`
}

// HTMLSnippet returns a self-contained HTML fragment that renders the
// Vega-Lite spec using the ESM build of vega-embed loaded from CDN.
func HTMLSnippet(spec []byte) (string, error) {
	id, err := randomID("vg-")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`<div id="%s"></div>
<script type="module">
  import vegaEmbed from 'https://cdn.jsdelivr.net/npm/vega-embed@7/+esm';
  vegaEmbed('#%s', %s);
</script>`, id, id, spec), nil
}

// HTMLBodySnippet is like HTMLSnippet but emits a plain <script> with no
// import statement — for use when vega-embed is already loaded by the host
// page.
func HTMLBodySnippet(spec []byte) (string, error) {
	id, err := randomID("vg-")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`<div id="%s" style="width:100%%;min-width:0"></div>
<script>
(function() {
  var spec = %s;
  if (spec.height === 'container') { spec.height = 400; }
  vegaEmbed('#%s', spec);
})();
</script>`, id, spec, id), nil
}

// VegaLiteEditorURL encodes a Vega-Lite JSON spec as a shareable editor URL.
func VegaLiteEditorURL(spec []byte) (string, error) {
	var buf bytes.Buffer
	w, err := flate.NewWriter(&buf, flate.BestCompression)
	if err != nil {
		return "", err
	}
	if _, err := w.Write(spec); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	return vegaEditorURL + base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

func randomID(prefix string) (string, error) {
	var raw [4]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(raw[:]), nil
}

// ansiSGRRe matches ANSI SGR escape sequences (e.g. "\x1b[38;2;255;0;0m").
var ansiSGRRe = regexp.MustCompile("\x1b\\[[0-9;]*m")

func stripANSI(s string) string {
	return ansiSGRRe.ReplaceAllString(s, "")
}

// renderMonoBlocks divides img into cols×rows character cells and renders it
// as a monochrome half-block picture. See the original duckpipe CLI's
// design comment history for the reasoning behind the "ink" test used here.
func renderMonoBlocks(img image.Image, cols, rows int, background color.RGBA) string {
	b := img.Bounds()
	srcW, srcH := b.Dx(), b.Dy()
	var sb strings.Builder
	for r := 0; r < rows; r++ {
		y0 := b.Min.Y + r*srcH/rows
		y1 := b.Min.Y + (r+1)*srcH/rows
		if y1 <= y0 {
			y1 = y0 + 1
		}
		yMid := (y0 + y1) / 2
		if yMid <= y0 {
			yMid = y0 + 1
		}
		for c := 0; c < cols; c++ {
			x0 := b.Min.X + c*srcW/cols
			x1 := b.Min.X + (c+1)*srcW/cols
			if x1 <= x0 {
				x1 = x0 + 1
			}
			topInk := isInk(modeColorIn(img, x0, x1, y0, yMid), background)
			botInk := isInk(modeColorIn(img, x0, x1, yMid, y1), background)
			switch {
			case topInk && botInk:
				sb.WriteRune('█')
			case topInk:
				sb.WriteRune('▀')
			case botInk:
				sb.WriteRune('▄')
			default:
				sb.WriteRune(' ')
			}
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

func isInk(c, background color.RGBA) bool {
	cLab, _ := colorful.MakeColor(c)
	bgLab, _ := colorful.MakeColor(background)
	return cLab.DistanceLab(bgLab) > 0.08
}

var brailleDots = [4][2]int{
	{0x1, 0x8},
	{0x2, 0x10},
	{0x4, 0x20},
	{0x40, 0x80},
}

const brailleQuantizeColors = 16

func renderBrailleANSI(img image.Image, cols, rows int) string {
	img = quantizeImage(img, brailleNeutrals, dominantChromaColors(img, brailleQuantizeColors-len(brailleNeutrals)))
	b := img.Bounds()
	background := modeColorIn(img, b.Min.X, b.Max.X, b.Min.Y, b.Max.Y)
	srcW, srcH := b.Dx(), b.Dy()
	var sb strings.Builder
	for r := 0; r < rows; r++ {
		y0 := b.Min.Y + r*srcH/rows
		y1 := b.Min.Y + (r+1)*srcH/rows
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for c := 0; c < cols; c++ {
			x0 := b.Min.X + c*srcW/cols
			x1 := b.Min.X + (c+1)*srcW/cols
			if x1 <= x0 {
				x1 = x0 + 1
			}
			ch, col := brailleCell(img, x0, x1, y0, y1, background)
			fmt.Fprintf(&sb, "\x1b[48;2;%d;%d;%dm\x1b[38;2;%d;%d;%dm%c",
				background.R, background.G, background.B, col.R, col.G, col.B, ch)
		}
		sb.WriteString("\x1b[0m\n")
	}
	return sb.String()
}

func brailleCell(img image.Image, x0, x1, y0, y1 int, background color.RGBA) (rune, color.RGBA) {
	var subColor [8]color.RGBA
	n := 0
	for i := 0; i < 4; i++ {
		sy0 := y0 + (y1-y0)*i/4
		sy1 := y0 + (y1-y0)*(i+1)/4
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		for j := 0; j < 2; j++ {
			sx0 := x0 + (x1-x0)*j/2
			sx1 := x0 + (x1-x0)*(j+1)/2
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}
			subColor[n] = modeColorIn(img, sx0, sx1, sy0, sy1)
			n++
		}
	}

	dots := 0x2800
	contentCounts := make(map[color.RGBA]int)
	var contentBest color.RGBA
	contentBestN := 0
	idx := 0
	for i := 0; i < 4; i++ {
		for j := 0; j < 2; j++ {
			c := subColor[idx]
			idx++
			if c == background {
				continue
			}
			dots |= brailleDots[i][j]
			contentCounts[c]++
			if contentCounts[c] > contentBestN {
				contentBest, contentBestN = c, contentCounts[c]
			}
		}
	}
	if contentBestN == 0 {
		return rune(dots), background
	}
	return rune(dots), contentBest
}

func modeColorIn(img image.Image, x0, x1, y0, y1 int) color.RGBA {
	counts := make(map[color.RGBA]int)
	var best color.RGBA
	bestN := 0
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			c := color.RGBA{R: uint8(r / 257), G: uint8(g / 257), B: uint8(b / 257), A: 255}
			counts[c]++
			if counts[c] > bestN {
				best, bestN = c, counts[c]
			}
		}
	}
	return best
}

var brailleNeutrals = []color.RGBA{
	{R: 0, G: 0, B: 0, A: 255},
	{R: 128, G: 128, B: 128, A: 255},
	{R: 255, G: 255, B: 255, A: 255},
}

func dominantChromaColors(img image.Image, n int) []color.RGBA {
	b := img.Bounds()
	counts := make(map[color.RGBA]int)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			c := color.RGBA{R: uint8(r / 257), G: uint8(g / 257), B: uint8(bl / 257), A: 255}
			if isNeutral(c) {
				continue
			}
			counts[c]++
		}
	}
	type entry struct {
		c color.RGBA
		n int
	}
	entries := make([]entry, 0, len(counts))
	for c, cnt := range counts {
		entries = append(entries, entry{c, cnt})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].n > entries[j].n })
	if len(entries) > n {
		entries = entries[:n]
	}
	chroma := make([]color.RGBA, len(entries))
	for i, e := range entries {
		chroma[i] = e.c
	}
	return chroma
}

func isNeutral(c color.RGBA) bool {
	max8 := func(a, b, c uint8) uint8 {
		if a < b {
			a = b
		}
		if a < c {
			a = c
		}
		return a
	}
	min8 := func(a, b, c uint8) uint8 {
		if a > b {
			a = b
		}
		if a > c {
			a = c
		}
		return a
	}
	return int(max8(c.R, c.G, c.B))-int(min8(c.R, c.G, c.B)) < 48
}

func quantizeImage(img image.Image, neutrals, chroma []color.RGBA) *image.RGBA {
	full := append(append([]color.RGBA{}, neutrals...), chroma...)
	b := img.Bounds()
	out := image.NewRGBA(b)
	cache := make(map[color.RGBA]color.RGBA)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			c := color.RGBA{R: uint8(r / 257), G: uint8(g / 257), B: uint8(bl / 257), A: 255}
			nearest, ok := cache[c]
			if !ok {
				if isNeutral(c) {
					nearest = nearestColor(c, neutrals)
				} else {
					nearest = nearestColor(c, full)
				}
				cache[c] = nearest
			}
			out.SetRGBA(x, y, nearest)
		}
	}
	return out
}

func nearestColor(c color.RGBA, palette []color.RGBA) color.RGBA {
	cLab, _ := colorful.MakeColor(c)
	best := palette[0]
	bestLab, _ := colorful.MakeColor(best)
	bestD := cLab.DistanceLab(bestLab)
	for _, p := range palette[1:] {
		pLab, _ := colorful.MakeColor(p)
		if d := cLab.DistanceLab(pLab); d < bestD {
			best, bestD = p, d
		}
	}
	return best
}

const actionsMenuHTML = `<details id="%[1]s" style="position:relative;display:inline-block;margin-bottom:4px;">
<summary style="list-style:none;cursor:pointer;display:inline-flex;align-items:center;justify-content:center;width:28px;height:22px;border:1px solid #ddd;border-radius:4px;background:#fff;color:#434a56;" title="Actions"><svg viewBox="0 0 16 16" fill="currentColor" stroke="none" stroke-width="1" stroke-linecap="round" stroke-linejoin="round" width="14" height="14"><circle r="2" cy="8" cx="2"></circle><circle r="2" cy="8" cx="8"></circle><circle r="2" cy="8" cx="14"></circle></svg></summary>
<div style="position:absolute;z-index:1000;background:#fff;box-shadow:0 1px 4px rgba(0,0,0,.2);border-radius:4px;padding:6px 0;margin-top:4px;min-width:140px;"><a href="#" id="%[2]s" style="display:block;padding:4px 12px;color:#434a56;text-decoration:none;white-space:nowrap;font:13px sans-serif;">Copy as text</a></div>
</details>
<style>#%[1]s summary::-webkit-details-marker{display:none}</style>`

func xtermSnippet(ansiText string, cols, rows int, rendererType, plainText string) (string, error) {
	ansiText = strings.TrimRight(ansiText, "\n")
	ansiText = strings.ReplaceAll(ansiText, "\r\n", "\n")
	ansiText = strings.ReplaceAll(ansiText, "\n", "\r\n")

	suffix, err := randomID("")
	if err != nil {
		return "", err
	}
	wrapID := "xt-wrap-" + suffix
	id := "xt-" + suffix
	detailsID := "xt-actions-" + suffix
	copyID := "xt-copy-" + suffix
	b64 := base64.StdEncoding.EncodeToString([]byte(ansiText))
	plainB64 := base64.StdEncoding.EncodeToString([]byte(plainText))
	containerH := rows * 17
	return fmt.Sprintf(actionsMenuHTML, detailsID, copyID) + fmt.Sprintf(`
<div id="%s" style="width:100%%;overflow:hidden"><div id="%s" style="height:%dpx"></div></div>
<script>
(function(){
  var b='%s';
  var plainB64='%s';
  var t;
  function b64ToBytes(b64){
    var bin=atob(b64);
    var bytes=new Uint8Array(bin.length);
    for(var i=0;i<bin.length;i++){bytes[i]=bin.charCodeAt(i);}
    return bytes;
  }
  var copyBtn=document.getElementById('%s');
  var detailsEl=document.getElementById('%s');
  if(copyBtn){
    copyBtn.addEventListener('click', function(e){
      e.preventDefault();
      var text;
      if(plainB64){
        text=new TextDecoder().decode(b64ToBytes(plainB64));
      }else{
        if(!t){return;}
        t.selectAll();
        text=t.getSelection();
        t.clearSelection();
      }
      if(detailsEl){detailsEl.open=false;}
      if(!(navigator.clipboard&&navigator.clipboard.writeText)){return;}
      navigator.clipboard.writeText(text).then(function(){
        var orig=copyBtn.textContent;
        copyBtn.textContent='Copied!';
        setTimeout(function(){copyBtn.textContent=orig;},1500);
      });
    });
  }
  function init(done){
    var wrap=document.getElementById('%s');
    var el=document.getElementById('%s');
    t=new Terminal({cols:%d,rows:%d,scrollback:0,disableStdin:true,rendererType:'%s'});
    t.open(el);
    var vp=el.querySelector('.xterm-viewport');
    if(vp){vp.style.backgroundColor='transparent';}
    function fit(){
      var natural=el.scrollWidth||el.getBoundingClientRect().width;
      var naturalH=el.scrollHeight||el.getBoundingClientRect().height;
      var avail=wrap.clientWidth;
      var scale=(avail>0&&natural>0)?Math.min(1,avail/natural):1;
      el.style.transformOrigin='top left';
      el.style.transform='scale('+scale+')';
      wrap.style.height=(naturalH*scale)+'px';
    }
    t.write(b64ToBytes(b), function(){
      fit();
      if(window.ResizeObserver){
        new ResizeObserver(fit).observe(el);
      }
      window.addEventListener('resize', fit);
      done();
    });
  }
  window.__xtermQueue=window.__xtermQueue||[];
  window.__xtermQueue.push(init);
  function pump(){
    if(window.__xtermRunning||window.__xtermQueue.length===0){return;}
    window.__xtermRunning=true;
    var next=window.__xtermQueue.shift();
    next(function(){window.__xtermRunning=false;pump();});
  }
  pump();
})();
</script>`, wrapID, id, containerH, b64, plainB64, copyID, detailsID, wrapID, id, cols, rows, rendererType), nil
}

// writeAnsiSVG converts an ANSI escape-code stream to SVG bytes.
func writeAnsiSVG(ansiText string, charBox xydim.XyDimInt) ([]byte, error) {
	opts := ansitosvg.DefaultOptions
	opts.Transparent = true
	opts.CharBoxSize = charBox
	opts.FillOnly = true
	opts.GridMode = true
	if charBox.Y > 0 && charBox.Y < opts.FontSize {
		opts.FontSize = charBox.Y
	}
	var buf bytes.Buffer
	if err := ansitosvg.Convert(strings.NewReader(ansiText), &buf, opts); err != nil {
		return nil, fmt.Errorf("render SVG: %w", err)
	}
	return buf.Bytes(), nil
}
