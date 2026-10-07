package chartconv

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"regexp"
	"strings"
	"testing"
)

var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// testPNG returns a white w×h PNG with a dark frame and a filled block, so
// every edge has non-background content.
func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := color.RGBA{255, 255, 255, 255}
			if x < 4 || y < 4 || x >= w-4 || y >= h-4 || (x > w/4 && x < w/2 && y > h/4) {
				c = color.RGBA{20, 20, 200, 255}
			}
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// grid strips ANSI colour codes and returns the output's row count and the
// widest row in runes.
func grid(out []byte) (rows, cols int) {
	s := strings.TrimRight(ansiEscape.ReplaceAllString(string(out), ""), "\n")
	for _, line := range strings.Split(s, "\n") {
		rows++
		if n := len([]rune(line)); n > cols {
			cols = n
		}
	}
	return rows, cols
}

func TestRenderDimensions(t *testing.T) {
	src := testPNG(t, 240, 160)
	sizes := []struct{ w, h int }{{40, 10}, {100, 30}}

	renderers := map[string]func([]byte, int, int) ([]byte, error){
		"text":         RenderText,
		"braille":      RenderBraille,
		"braille-text": RenderBrailleText,
	}
	for name, render := range renderers {
		for _, s := range sizes {
			out, err := render(src, s.w, s.h)
			if err != nil {
				t.Fatalf("%s %dx%d: %v", name, s.w, s.h, err)
			}
			rows, cols := grid(out)
			if rows != s.h || cols != s.w {
				t.Errorf("%s width=%d height=%d: got %d cols x %d rows", name, s.w, s.h, cols, rows)
			}
		}
	}
}

func TestRenderBrailleDefaultSize(t *testing.T) {
	// 0×0 means exact 2×4 pixels per braille cell.
	out, err := RenderBraille(testPNG(t, 240, 160), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if rows, cols := grid(out); rows != 40 || cols != 120 {
		t.Errorf("got %d cols x %d rows, want 120 x 40", cols, rows)
	}
}

// RenderAnsi takes (rows, cols) — the reverse of every other renderer's
// (width, height) — and ScaleModeFit may use less than the box to keep the
// aspect ratio, so assert it stays inside the box and that the box's rows
// and cols aren't transposed.
func TestRenderAnsiRowsCols(t *testing.T) {
	src := testPNG(t, 240, 80) // wide image: width is the limiting side
	out, err := RenderAnsi(src, 20, 60)
	if err != nil {
		t.Fatal(err)
	}
	rows, cols := grid(out)
	if cols > 60 || rows > 20 {
		t.Errorf("RenderAnsi(rows=20, cols=60) overflowed: %d cols x %d rows", cols, rows)
	}
	if cols != 60 {
		t.Errorf("wide image should fill all 60 cols, got %d (rows/cols transposed?)", cols)
	}
}

func TestRenderInvalidPNG(t *testing.T) {
	bad := []byte("not a png")
	for name, render := range map[string]func([]byte, int, int) ([]byte, error){
		"text": RenderText, "braille": RenderBraille, "svg": RenderSVG,
	} {
		if _, err := render(bad, 10, 10); err == nil {
			t.Errorf("%s: expected decode error", name)
		}
	}
}
