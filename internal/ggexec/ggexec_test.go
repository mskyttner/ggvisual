package ggexec

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// failure runs a shell snippet and returns its *exec.ExitError (with Stderr
// populated, as exec.Cmd.Output does), standing in for a failed ggsql call.
func failure(t *testing.T, script string) error {
	t.Helper()
	_, err := exec.Command("sh", "-c", script).Output()
	if err == nil {
		t.Fatalf("%q unexpectedly succeeded", script)
	}
	return err
}

func TestClassifyGgsql(t *testing.T) {
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	tests := []struct {
		name string
		ctx  context.Context
		err  error
		want Category
	}{
		{"validation error", context.Background(),
			failure(t, "echo 'Failed to execute query: Validation error: bad aesthetic' >&2; exit 1"), CategoryBadGgsql},
		{"parse error", context.Background(),
			failure(t, "echo 'Failed to execute query: Parse error: unexpected token' >&2; exit 1"), CategoryBadGgsql},
		{"sql error", context.Background(),
			failure(t, "echo 'Failed to execute query: Data source error: Failed to execute SQL: no such table' >&2; exit 1"), CategoryBadSQL},
		{"unrecognized stderr", context.Background(),
			failure(t, "echo 'Failed to create reader: nope' >&2; exit 1"), CategoryInternal},
		{"killed by signal", context.Background(),
			failure(t, "kill -9 $$"), CategoryInternal},
		{"timeout wins over stderr", expired,
			failure(t, "echo 'Failed to execute query: Validation error: x' >&2; exit 1"), CategoryTimeout},
		{"non-exec error", context.Background(), errors.New("boom"), CategoryInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var e *Error
			if err := classifyGgsql(tt.ctx, tt.err); !errors.As(err, &e) || e.Category != tt.want {
				t.Errorf("got %v, want category %s", err, tt.want)
			}
		})
	}
}

func TestClassifyGgsqlKeepsStderr(t *testing.T) {
	err := classifyGgsql(context.Background(),
		failure(t, "echo '  Failed to execute query: Parse error: line 1  ' >&2; exit 1"))
	var e *Error
	if !errors.As(err, &e) || e.Message != "Failed to execute query: Parse error: line 1" {
		t.Errorf("got %v, want trimmed stderr as message", err)
	}
}

func TestClassifyVlConvertAlwaysInternal(t *testing.T) {
	for _, script := range []string{
		"echo 'Failed to execute query: Validation error: x' >&2; exit 1",
		"kill -9 $$",
	} {
		var e *Error
		if err := classifyVlConvert(failure(t, script)); !errors.As(err, &e) || e.Category != CategoryInternal {
			t.Errorf("%q: got %v, want internal", script, err)
		}
	}
}

// RenderPNG must replace "container" width/height with the requested pixel
// size before handing the spec to vl-convert. A stand-in vl-convert that
// echoes stdin lets us see exactly what it was given.
func TestRenderPNGPatchesContainerSize(t *testing.T) {
	if _, err := exec.LookPath("prlimit"); err != nil {
		t.Skip("prlimit not available")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "vl-convert"), []byte("#!/bin/sh\ncat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	spec := `{"width": "container", "height": "container", "mark": "bar"}`
	out, err := (&Runner{}).RenderPNG(context.Background(), []byte(spec), 800, 300)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"width": 800, "height": 300, "mark": "bar"}`
	if got := strings.TrimSpace(string(out)); got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}
