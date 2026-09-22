// Package ggexec runs ggsql and vl-convert as local subprocesses (no
// container spin-up — ggvisual runs inside the same image as the ggsql/
// vl-convert binaries) and classifies their failures into the categories
// scoped in persistent-service.md's "Structured error envelope" section.
package ggexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
)

// Category classifies a render failure so a caller can map it to the right
// HTTP status without pattern-matching stderr text itself.
type Category string

const (
	CategoryBadGgsql Category = "bad_ggsql"
	CategoryBadSQL   Category = "bad_sql"
	CategoryTimeout  Category = "timeout"
	CategoryInternal Category = "internal"
)

// Error wraps a subprocess failure with its category and the (possibly
// trimmed) stderr text.
type Error struct {
	Category Category
	Message  string
}

func (e *Error) Error() string { return string(e.Category) + ": " + e.Message }

// DefaultMemoryLimit is the RLIMIT_AS passed to prlimit for each ggsql/
// vl-convert invocation. Verified in "Resource limits per subprocess":
// values much below ~1GB break ordinary small queries because DuckDB
// reserves address space well beyond what a query actually touches, so this
// is a backstop against a genuinely runaway render, not a tight budget.
const DefaultMemoryLimit = 2 << 30 // 2GiB

// Runner executes ggsql/vl-convert as local subprocesses.
type Runner struct {
	// MemoryLimit is the RLIMIT_AS (bytes) applied to every subprocess via
	// prlimit. Zero uses DefaultMemoryLimit.
	MemoryLimit int64
}

func (r *Runner) memLimit() int64 {
	if r.MemoryLimit > 0 {
		return r.MemoryLimit
	}
	return DefaultMemoryLimit
}

// wrapAS prefixes argv with a prlimit invocation bounding virtual address
// space (RLIMIT_AS) — verified present in the target image (util-linux) and
// effective for ggsql without needing cgroup delegation.
func (r *Runner) wrapAS(argv ...string) []string {
	return append([]string{fmt.Sprintf("--as=%d", r.memLimit()), "--"}, argv...)
}

// wrapData prefixes argv with a prlimit invocation bounding the heap
// (RLIMIT_DATA) instead of total virtual address space. vl-convert embeds a
// V8-based engine that reserves a huge virtual address space cage
// (Oilpan/CagedHeap) regardless of actual usage — verified directly:
// RLIMIT_AS up to 32GiB still made it abort with "Fatal process out of
// memory: Oilpan: CagedHeap reservation" against a trivial 76MB-RSS render,
// while RLIMIT_DATA at the same 2GiB default works cleanly and is still a
// real, enforced constraint (verified separately: a 1MB RLIMIT_DATA reliably
// makes it fail). ggsql doesn't have this problem (DuckDB, no V8), so it
// keeps using wrapAS.
func (r *Runner) wrapData(argv ...string) []string {
	return append([]string{fmt.Sprintf("--data=%d", r.memLimit()), "--"}, argv...)
}

// Exec runs `ggsql exec <query> --reader duckdb://memory` and returns the
// resulting Vega-Lite JSON (or other writer output) bytes.
func (r *Runner) Exec(ctx context.Context, query string) ([]byte, error) {
	argv := r.wrapAS("ggsql", "exec", query, "--reader", "duckdb://memory")
	out, err := run(ctx, "prlimit", argv, nil)
	if err != nil {
		return nil, classifyGgsql(ctx, err)
	}
	return bytes.TrimSpace(out), nil
}

// Validate runs `ggsql validate` against the query — a cheap syntax check
// with no data pipeline / render step, per ggsql-endpoint.md's "validate-only
// mode" request.
func (r *Runner) Validate(ctx context.Context, query string) error {
	argv := r.wrapAS("ggsql", "validate", query)
	_, err := run(ctx, "prlimit", argv, nil)
	if err != nil {
		return classifyGgsql(ctx, err)
	}
	return nil
}

// RenderPNG patches "container" width/height in specJSON to pngWidth/
// pngHeight and runs vl-convert over it via /dev/stdin/-o /dev/stdout — no
// temp file, per "stdin vs temp-file for vl-convert" scoping.
func (r *Runner) RenderPNG(ctx context.Context, specJSON []byte, pngWidth, pngHeight int) ([]byte, error) {
	patched := bytes.ReplaceAll(specJSON,
		[]byte(`"width": "container"`), fmt.Appendf(nil, `"width": %d`, pngWidth))
	patched = bytes.ReplaceAll(patched,
		[]byte(`"height": "container"`), fmt.Appendf(nil, `"height": %d`, pngHeight))

	argv := r.wrapData("vl-convert", "vl2png", "-i", "/dev/stdin", "-o", "/dev/stdout")
	out, err := run(ctx, "prlimit", argv, patched)
	if err != nil {
		return nil, classifyVlConvert(err)
	}
	return out, nil
}

func run(ctx context.Context, name string, argv []string, stdin []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, argv...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return out, nil
}

// stderrOf extracts stderr text from an *exec.ExitError, if that's what err
// is.
func stderrOf(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return strings.TrimSpace(string(ee.Stderr))
	}
	return err.Error()
}

// signaled reports whether err represents a process killed by a signal
// (e.g. SIGABRT from a Rust allocator abort, SIGKILL from our own timeout)
// rather than a normal nonzero exit — verified in "Resource limits per
// subprocess" to matter: a signal-terminated process may have written no
// meaningful stderr at all, so this must be checked before stderr-prefix
// matching.
func signaled(err error) (syscall.Signal, bool) {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return 0, false
	}
	status, ok := ee.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return 0, false
	}
	return status.Signal(), true
}

// classifyGgsql categorizes a failed `ggsql exec`/`ggsql validate` call.
// Ordered check: timeout (structural, via ctx) wins over any stderr
// content, since a killed process's partial stderr isn't meaningful to
// pattern-match — verified in "Per-request timeout"/"Structured error
// envelope".
func classifyGgsql(ctx context.Context, err error) error {
	if ctx.Err() == context.DeadlineExceeded {
		return &Error{Category: CategoryTimeout, Message: "render exceeded the per-request timeout"}
	}
	if sig, ok := signaled(err); ok {
		return &Error{Category: CategoryInternal, Message: fmt.Sprintf("ggsql terminated by signal %v", sig)}
	}
	msg := stderrOf(err)
	switch {
	case strings.HasPrefix(msg, "Failed to execute query: Validation error:"),
		strings.HasPrefix(msg, "Failed to execute query: Parse error:"):
		return &Error{Category: CategoryBadGgsql, Message: msg}
	case strings.HasPrefix(msg, "Failed to execute query: Data source error: Failed to execute SQL:"):
		return &Error{Category: CategoryBadSQL, Message: msg}
	default:
		// Includes "Failed to create reader: ..." (a CLI-usage error that
		// can't be caller-triggered since --reader is always our own fixed
		// constant — see "Structured error envelope") and anything
		// unrecognized: fail safe as internal rather than guessing 400.
		return &Error{Category: CategoryInternal, Message: msg}
	}
}

// classifyVlConvert categorizes a failed vl-convert call. Per "Structured
// error envelope": vl-convert's input is this service's own generated JSON,
// never caller-controlled directly, so any failure here is internal, not
// the caller's fault.
func classifyVlConvert(err error) error {
	if sig, ok := signaled(err); ok {
		return &Error{Category: CategoryInternal, Message: fmt.Sprintf("vl-convert terminated by signal %v", sig)}
	}
	return &Error{Category: CategoryInternal, Message: stderrOf(err)}
}
