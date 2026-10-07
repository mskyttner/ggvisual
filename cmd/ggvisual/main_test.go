package main

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"ggsql-tools/internal/ggexec"
)

func TestQueryInt(t *testing.T) {
	tests := []struct {
		query string
		want  int
	}{
		{"width=80", 80},
		{"", 7},          // missing -> default
		{"width=abc", 7}, // non-integer -> default, not an error
		{"width=", 7},
	}
	for _, tt := range tests {
		r := httptest.NewRequest("POST", "/render?"+tt.query, nil)
		if got := queryInt(r, "width", 7); got != tt.want {
			t.Errorf("%q: got %d, want %d", tt.query, got, tt.want)
		}
	}
}

func TestWriteErrorStatus(t *testing.T) {
	tests := []struct {
		err  error
		want int
		cat  string
	}{
		{&ggexec.Error{Category: ggexec.CategoryBadGgsql, Message: "m"}, http.StatusBadRequest, "bad_ggsql"},
		{&ggexec.Error{Category: ggexec.CategoryBadSQL, Message: "m"}, http.StatusBadRequest, "bad_sql"},
		{&ggexec.Error{Category: ggexec.CategoryTimeout, Message: "m"}, http.StatusGatewayTimeout, "timeout"},
		{&ggexec.Error{Category: ggexec.CategoryInternal, Message: "m"}, http.StatusInternalServerError, "internal"},
		{http.ErrAbortHandler, http.StatusInternalServerError, "internal"}, // plain error -> internal
	}
	for _, tt := range tests {
		w := httptest.NewRecorder()
		writeError(w, tt.err)
		var body errorResponse
		if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if w.Code != tt.want || body.Error.Category != tt.cat {
			t.Errorf("%v: got %d/%s, want %d/%s", tt.err, w.Code, body.Error.Category, tt.want, tt.cat)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("%v: Content-Type %q, want application/json", tt.err, ct)
		}
	}
}

func TestMissingVisualIs400(t *testing.T) {
	s := &server{}
	for name, h := range map[string]http.HandlerFunc{"render": s.handleRender, "validate": s.handleValidate} {
		w := httptest.NewRecorder()
		h(w, httptest.NewRequest("POST", "/"+name+"?format=png", nil))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", name, w.Code)
		}
	}
}

// Every format advertised by /formats must be handled: raster formats by
// convertPNG, the rest by convertSpec. Guards against adding a format to the
// list (or to the dispatch) without the other.
func TestFormatsAreDispatched(t *testing.T) {
	w := httptest.NewRecorder()
	(&server{}).handleFormats(w, httptest.NewRequest("GET", "/formats", nil))
	var advertised []string
	if err := json.NewDecoder(w.Body).Decode(&advertised); err != nil {
		t.Fatal(err)
	}

	raster := []string{
		"png", "ansi", "braille", "svg", "braille-svg",
		"xterm", "xterm-page", "braille-xterm", "braille-xterm-page",
		"text", "braille-text", "cast", "cast-page",
	}
	for _, f := range raster {
		if !slices.Contains(advertised, f) {
			t.Errorf("raster format %q missing from /formats", f)
		}
	}

	img := image.NewRGBA(image.Rect(0, 0, 80, 40))
	for i := range img.Pix {
		img.Pix[i] = 200
	}
	img.Set(10, 10, color.Black)
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, img); err != nil {
		t.Fatal(err)
	}
	spec := []byte(`{"mark": "bar"}`)

	for _, f := range advertised {
		var (
			out []byte
			err error
		)
		if slices.Contains(raster, f) {
			out, err = convertPNG(f, pngBuf.Bytes(), 20, 6)
		} else {
			out, err = convertSpec(f, spec)
		}
		if err != nil || len(out) == 0 {
			t.Errorf("format %q: len=%d err=%v", f, len(out), err)
		}
	}
	if _, err := convertPNG("nope", pngBuf.Bytes(), 20, 6); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Errorf("unknown raster format: got %v, want unsupported error", err)
	}
}
