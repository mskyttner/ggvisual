# Scoping: duckpipe's rendering pipeline as a persistent HTTP service (`ggvisual`)

Follow-up to `../caddy-duckdb-module/ggsql-endpoint.md`, which asks for this repo's
`cmd/duckpipe` (currently a per-invocation CLI wrapping a per-request `podman run`) to become
a persistent sidecar service that module proxies HTTP requests to, instead of spawning a
container per chart.

## Current architecture (baseline)

`duckpipe` is a CLI: reads CSV on stdin, writes a temp file, and for every request —
`ansi`/`braille`/`svg`/`png` as much as `vegalite`/`html` — spawns a **fresh**
`podman run --rm -v <tmpcsv>:/data/input.csv:ro localhost/ggsql:latest exec ...`. Formats needing
a raster image additionally pipe through `vl-convert` inside that same container
(`containerPNG` in `main.go`). `ansi`/`braille`/`svg`/`text`/`xterm*` are **not** ggsql
writers at all — verified below — they're this repo's own Go post-processing
(`pixterm`/`ansisvg`/our custom braille+monochrome renderers) applied to the PNG bytes ggsql
returns.

Two costs this doc's target design removes: the per-request container spin-up, and the
podman-socket access `caddy-duckdb-module` would otherwise need to spawn sibling containers (a
real privilege-escalation surface, flagged in the sidecar doc).

## Verified facts that shape the design

- **`ggsql exec -w` (0.5.2 CLI) supports:** `vegalite`, `png`, `jpeg`, `tiff`, `webp`, `svg`,
  `pdf`, `hep`. **No `ansi`, `braille`, or `html`.** Confirms: this service still needs to own
  the Go-side rendering `duckpipe` already does — it can't be replaced by ggsql writer flags.
- **`ggsql exec` has no extension-loading mechanism** (no flag in `--help`, no doc hit for
  "extension"/"nanoarrow"). Its embedded DuckDB reader is sealed.
- **No ADBC support.** Checked explicitly: passing an unrecognized reader scheme returns
  `Unsupported connection string: ... Supported: duckdb://, sqlite://, odbc://` — no ADBC in that
  list — and `strings` on the binary turned up zero ADBC/Arrow-Flight references at all.
- **The `duckdb://` reader accepts a real database file path, not just `:memory:`.**
  Verified: `--reader duckdb:///tmp/x.duckdb` against a file with a pre-existing table reads it
  directly, full native type fidelity, no schema declaration or CSV/Arrow bridging needed.
- **CSV auto-detection only fails over a non-seekable pipe — reading a real file is fine.**
  Verified both ways: `read_csv('/dev/stdin')` on a piped stream misparses the header
  (`column0 not found`) unless given `auto_detect=false, columns={...}` explicitly; the exact
  same query against a real file on disk auto-detects correctly with zero schema declaration —
  which is what every `duckpipe` CLI invocation today already relies on (temp file, not a pipe).
- **Parquet via stdin is a hard no.** DuckDB refuses outright:
  `Reading parquet files from a FIFO stream is not supported... metadata is at the end`.
  Parquet needs random access to its footer — same underlying reason a `duckdb://` file reader
  needs a real file, not a pipe.
- **Arrow IPC (the *streaming* variant, not the File variant) works over a true pipe, and is
  self-describing.** Verified: `COPY tbl TO 'x.arrows' (FORMAT ARROW)`, piped through a real
  shell pipe into `duckdb -c "LOAD nanoarrow; SELECT ... FROM read_arrow('/dev/stdin')"` — schema
  (including correct numeric types, not just strings) came through with **no explicit columns
  needed**, unlike CSV-over-a-pipe. But `ggsql exec` itself cannot read Arrow directly —
  `read_arrow('/dev/stdin')` against `ggsql exec` fails with
  `Table Function with name read_arrow does not exist!`, consistent with the sealed-reader
  finding above.

## Data handoff: simpler than the previous pass in this doc concluded

An earlier version of this section reasoned from the pipe-only auto-detect failure to "Arrow IPC
as the wire format, with `ggvisual` bridging it to CSV before calling `ggsql`" — treating
that bridge (a new Arrow decoder, either shelling out to `duckdb`+`nanoarrow` or a native Go
Arrow reader) as necessary. It isn't. The auto-detect problem is specific to **non-seekable
pipes**; it doesn't apply to an ordinary file on disk, and the target architecture already
collapses `ggsql`/`vl-convert`/`duckpipe` into one container (see below) — so `ggvisual`
writing the HTTP request body straight to a small ephemeral **local** temp file (same container,
no cross-container bind-mount, cleaned up after the request) and pointing `ggsql exec` at
`read_csv('/tmp/xxx.csv')` has none of the problems a pipe would, and needs zero new code beyond
what `duckpipe` already does today (it already writes stdin to a temp CSV file per request — the
only change is that file no longer needs a `-v ...:/data/input.csv:ro` bind-mount into a
*separate* container, since `ggsql` is now a local subprocess in the same filesystem).

The `duckdb://<file>` reader finding is a genuine bonus worth keeping in mind (best type
fidelity, native types instead of CSV's stringify/reparse round-trip) but isn't required to hit
this doc's actual goal — CSV-to-local-temp-file already solves it with the code this repo
already has. Arrow IPC as the wire *format between* `caddy-duckdb-module` and this service is
still a reasonable, independent question (that module already speaks Arrow internally, per
2026-09-22 discussion) — but if adopted, the simplest shape is "decode it into that same local
temp CSV (or a local `.duckdb` file) server-side," not "invent a new way to feed `ggsql`
directly." Worth scoping as its own follow-up only if there's a concrete reason to prefer Arrow
over CSV on the wire (e.g. avoiding a stringify/reparse round-trip for large numeric columns) —
not assumed necessary by this pass.

## Concrete changes needed in this repo

1. **Extract the rendering path out of the CLI shape.** `render*` functions in `main.go`
   currently assume `os.Stdin`/flags/`fatal()`-and-exit. Need a pure function —
   `render(csv []byte, format string) ([]byte, contentType string, error)` — callable from both
   the existing CLI `main()` and a new HTTP handler.
2. **Replace `podman run --rm ...` with direct `exec.Command("ggsql", ...)` /
   `exec.Command("vl-convert", ...)`** — only valid once `ggvisual` runs *inside* the
   ggsql image itself (see Containerfile change below); no more nested containers.
3. **New `Containerfile`**: extend the existing one (currently: Ubuntu 24.04 +
   `ggsql` `.deb` + `vl-convert`) to also `COPY` in a compiled `duckpipe` binary, and change
   `ENTRYPOINT ["ggsql"]` → `ENTRYPOINT ["duckpipe", "serve"]`.
4. **New HTTP surface** (distinct from `caddy-duckdb-module`'s own `/duckdb/ggsql` route, which
   would proxy to this):
   - `POST /render` — CSV data (same as today's stdin input, now the HTTP body) +
     `visualise`/`format` fields; response `Content-Type` set per format, raw bytes, never
     JSON-wrapped (the sidecar doc's ANSI-escaping finding applies identically here).
   - `POST /validate` — trivial, `ggsql validate` is already a real subcommand.
   - `GET /formats`, `GET /health`, `GET /version` (ggsql + vl-convert + duckpipe versions, all
     three now meaningfully independent version numbers worth exposing given the
     extension-vs-CLI skew already on record).
5. **Per-request timeout** via `context.WithTimeout` around every subprocess call — currently
   unbounded.
6. **Structured JSON error envelope** with a category (`bad_ggsql`, `bad_sql`, `timeout`,
   `internal`) instead of today's `fatal()`-and-exit, so the caller can map to the right HTTP
   status without pattern-matching stderr text.

**Unchanged:** the existing CLI mode (`duckpipe --format ... --visual ...` piping CSV via
stdin) stays — `embed.qmd`, the `Makefile` demo target, and local dev workflows depend on it,
and nothing in the sidecar doc asks for its removal. HTTP service mode is additive.

## Concurrency model

Tested directly against the real `localhost/ggsql:latest` image: started one long-lived
container (`--entrypoint sleep infinity`, matching the target "one persistent container"
architecture) and ran concurrent `podman exec` calls into it, timing sequential vs. parallel and
diffing outputs.

- **`vl-convert vl2png`** (the actual GPU-rasterizing binary `duckpipe` depends on today — see
  above, `ggsql`'s own `png`/`jpeg`/`tiff`/`webp` writers aren't compiled into this build):
  8 sequential calls took 3.29s (~0.41s each, consistent with a single-call baseline of ~0.5s —
  full serialization, as expected for sequential runs). **8 concurrent calls took 0.78s total**,
  and **24 concurrent calls took 1.76s total** — clearly parallel, not serialized. Every output
  verified byte-identical via `md5sum` across all runs at both concurrency levels — no
  corruption, no cross-request state leaking into results.
- **The full pipeline `ggsql exec | vl-convert`** (matching `containerPNG` in `main.go`
  exactly): 12 concurrent end-to-end runs completed in 0.98s, all 12 outputs byte-identical.
- **No shared-state footprint found**: diffed the container's filesystem for files modified
  during a run (`find / -newer ...`) — nothing outside the explicit temp output files. No
  font-cache/shader-cache directory, no lock file, that concurrent requests could contend on.

**Real caveat, not just a footnote: this container has zero GPU device access.** The host has an
actual NVIDIA GPU (`/dev/dri/card1`, `card2`, `renderD128/129`, live NVRM driver on the host),
but `/dev/dri` doesn't exist at all *inside* the container (no `--device` passthrough in how it
was run, matching how `duckpipe`'s `containerPNG` runs it today) — so `vl-convert` is doing pure
CPU/software rasterization here, on a 24-core host. The clean parallel scaling above is ordinary
CPU work spreading across cores, not evidence about how a *GPU-accelerated* deployment would
behave under concurrent load — a real GPU adapter/context is a much more plausible place to find
genuine serialization (driver-level context limits, single command queue) than anything observed
in this CPU-only test. **If a future deployment adds `--device /dev/dri` for real GPU
acceleration, this test should be re-run before trusting these numbers.**

**Conclusion for the persistent-service design:** at CPU-rendering concurrency levels tested (up
to 24 concurrent), `ggvisual` does not need its own request queueing/backpressure to stay
correct — `exec.Command` calls can be issued concurrently as they arrive, relying on OS process
scheduling. Worth keeping a configurable concurrency cap (a buffered channel/semaphore around
the subprocess calls) as a simple safety valve against unbounded resource use under a request
storm, but that's a capacity-management decision, not a correctness requirement this testing
uncovered — nothing here found a reason `ggvisual` *must* serialize requests.

## Temp-file cleanup

`main.go` has exactly one `os.CreateTemp` site left (the per-request input CSV; the earlier PNG
temp-file usage was already refactored away to in-memory decoding). It does the right thing on
the surface — `defer os.Remove(tmpCSV.Name())` right after creation — but that defer is
undermined by a real, already-present bug, not a hypothetical one.

- **`fatal()` calls `os.Exit(1)` directly**, which skips every deferred function on the call
  stack — that's documented Go behavior, not a subtlety. There are **26 `fatal()` call sites** in
  `main.go`, and the overwhelming majority of them (`containerPNG`, every `render*` function, the
  direct `vegalite`/`html`/`html-page`/`html-body` path) run *after* the temp CSV is created.
- **Reproduced the leak directly, not just traced it in code**: ran `duckpipe` with a bad
  `--image` flag (forces `containerPNG`'s `fatal("render PNG", err)` after the temp file already
  exists) — the process exits, and `/tmp/duckpipe-<random>.csv` is left behind on disk. Confirmed
  via `ls` before manually cleaning it up.
- **This bug already exists today, in the CLI, right now** — it's not something the persistent-
  service refactor introduces. It's just low-impact today because CLI invocations are
  comparatively rare and human-triggered, and most systems clear `/tmp` periodically regardless
  (`systemd-tmpfiles`, reboot) — an implicit floor that happens to mask it.
- **That floor disappears under `ggvisual`.** A long-lived process handling far more
  requests than a human ever would, with no CLI-process-exit-triggered cleanup boundary, and — worse —
  the requests most likely to hit a `fatal()` path (malformed `visualise` clauses, bad SQL, ggsql
  syntax errors) are exactly the kind of input an HTTP endpoint sees *more* of over time, not
  less. Leak rate scales with `error rate × request volume` instead of being bounded by how often
  someone runs the CLI by hand.
- **`os.CreateTemp` itself is fine under concurrency** — worth stating plainly since it's the
  part that sounds most likely to be broken but isn't: it's documented/implemented to retry on
  name collision, safe for concurrent callers in the same process. Nothing to fix there.

**This is the same fix as item 1 in "Concrete changes needed" above, not a separate problem.**
`os.Exit(1)` inside a request handler wouldn't just leak a temp file — it would **kill the
entire server process**, taking down every other in-flight and future request with it. That's a
strictly bigger problem than the leak. Once `render*` is refactored to return `(bytes,
contentType, error)` instead of calling `fatal()`, the HTTP handler's own `defer
os.Remove(tmpFile)` runs correctly on every error path for free — there's no separate "fix temp
cleanup" work item, just "don't reintroduce `os.Exit` inside the new HTTP handler path" as a
constraint on doing item 1 correctly. The CLI's `main()` can keep calling something
`fatal()`-shaped (`os.Exit` on error is fine for a one-shot process the user is watching), but
the new pure `render()` function it wraps cannot.

Two smaller, genuinely independent hardening measures worth doing regardless:
- **Sweep stale `ggvisual-*.csv` files on `ggvisual` startup.** A SIGKILL (OOM, forced
  container stop) bypasses Go's deferred cleanup the same way `os.Exit` does — this isn't new to
  server mode (a killed CLI invocation leaks identically today), but a long-lived service
  restarting repeatedly over weeks is a more realistic place for these to actually accumulate
  than an interactively-run CLI.
- **A periodic background sweep** (e.g. remove `ggvisual-*.csv` older than N minutes) as
  defense-in-depth, independent of the `fatal()` fix — cheap insurance against whatever error
  path gets missed in the initial refactor.

## Network exposure

Scope: container-internal address only, reached by `caddy-duckdb-module` — matches
`ggsql-endpoint.md`'s explicit "no separate auth surface, trusted internally" stance, so this
is about *how* it stays internal, not whether.

- **`caddy-duckdb-module`'s own `docker-compose.yml` already has the exact precedent to follow**:
  the (currently commented-out) `fts-sidecar` service — `container_name: fts-sidecar`, no `ports:`
  entry at all, reached by the main `caddy-duckdb` service via
  `DUCKDB_FTS_SERVICE_URL=http://fts-sidecar:8701`, a plain env var pointing at the compose
  service name. Only `caddy-duckdb` itself publishes a host port
  (`"${DUCKDB_PORT:-8080}:${DUCKDB_PORT:-8080}"`). The `ggvisual` sidecar should follow this
  exactly: no `ports:` mapping, reached as `http://ggvisual:<port>` by service name.
- **Verified name-based reachability directly** (not just inferred from the fts-sidecar
  precedent): created a user-defined podman network, ran one container on it, and resolved its
  name from a second container — `socket.gethostbyname('svcb')` returned the container's
  network-internal IP with zero manual DNS config. This is the same mechanism
  podman-compose/docker-compose set up automatically for every service in one compose file (or
  services joined to the same named external network), confirming the fts-sidecar pattern above
  isn't relying on anything special-cased.
- **"Container-internal only" is a compose-level decision (omit `ports:`), not an
  application-level one.** `ggvisual` binding `0.0.0.0:<port>` *inside* its container is
  normal and fine — the isolation boundary is whether compose publishes that port to the host, not
  what address the Go server binds to internally. No bind-address logic needs to change in
  `duckpipe` itself for this.
- **One real placement decision, not yet made**: this repo's own `compose.yaml` currently only
  builds the `ggsql` image (`localhost/ggsql:latest`), it doesn't run it as a long-lived service on
  a shared network with `caddy-duckdb-module` — that module's `docker-compose.yml` lives in a
  sibling repo entirely. For name-based reachability to work the way `fts-sidecar` does, the
  running `ggvisual` container needs to end up in the **same compose network** as
  `caddy-duckdb`, which means either (a) defining it as a service directly in
  `caddy-duckdb-module/docker-compose.yml` (mirroring the `fts-sidecar` block, likely the simplest
  option and consistent with that file's existing pattern for optional internal services), or (b)
  running it from this repo's own `compose.yaml` and joining both compose projects to one shared
  external `docker network`/`podman network create` — more moving parts, no precedent in either
  repo today. Not this repo's call to make unilaterally since it changes `caddy-duckdb-module`'s
  compose file either way — flag to that repo's own scoping, but (a) is the recommended default
  given the exact precedent already sitting in that file.
- **`expose:` vs. nothing in this repo's `compose.yaml`**: once `ggvisual` is a real
  long-running process, `compose.yaml` needs *some* declaration for the port it listens on.
  `expose:` (documents the port to other services on the same network, publishes nothing to the
  host) is the correct primitive if this repo ends up defining the service itself — functionally
  redundant on a user-defined network (all container ports are already reachable to network peers
  regardless of `expose:`), but worth keeping as documentation of intent, matching why
  `fts-sidecar`'s block also states its port explicitly (`FTS_PORT=8701`) even without a `ports:`
  mapping.

## `exec.Command` replacement for `podman run`

Verified directly, not just planned: built the exact local-subprocess call shape both current
`podman run` sites (lines 131–145 and 197–225 of `main.go`) would become, and ran it end-to-end
outside any container — `ggsql` 0.5.2 is already installed locally (same version baked into the
image) and `vl-convert` 1.9.0 was extracted from `localhost/ggsql:latest` (`podman cp
$(podman create ...):/usr/local/bin/vl-convert`) to test on the host filesystem, standing in for
"local subprocess inside the merged container" without needing to rebuild that image first.

- **`ggsql exec` works as a direct local subprocess against a real file path** — no container, no
  bind mount. `ggsql exec "SELECT * FROM read_csv('/tmp/test.csv', header=true) VISUALISE ..."
  --reader duckdb://memory` produced the correct Vega-Lite spec. **This eliminates the
  `/data/input.csv` fixed mount path entirely**: the current code writes CSV to
  `os.CreateTemp` then references it via a bind-mounted alias inside the container
  (`mount := tmpCSV.Name() + ":/data/input.csv:ro"`, query hardcodes `/data/input.csv`). Once
  `ggsql` is a local subprocess in the same filesystem as `ggvisual`, the query can
  reference `tmpCSV.Name()` directly — no mount, no path translation, one fewer moving part.
- **The `vl-convert vl2png` step works the same way, called directly with argv (no shell)** —
  `vl-convert vl2png -i spec.json -o out.png` on a plain file produced a valid PNG
  (`662x493, 8-bit/color RGBA`, confirmed via `file`). No env vars needed beyond what
  `exec.Command` inherits by default (`os.Environ()` — nothing container-specific to set).
- **The "container"→pixel-size patch (currently a `sed` invocation inside the container's `sh -c`
  string) is trivially a Go `strings.Replace`, verified equivalent** — tested the substitution in
  place of `sed` before feeding `vl-convert`, same result. This means `containerPNG`'s entire
  `shell := fmt.Sprintf("ggsql exec %s ... | sed ... > /tmp/spec.json && vl-convert ... && cat
  ...", shellQuote(query), patch)` construction — a shell pipeline glued together as a string —
  goes away completely, replaced by three sequential Go statements: run `ggsql exec` via
  `exec.Command` → `.Output()`, patch the bytes in Go, write to a local temp file (or pipe directly
  into `vl-convert`'s stdin, since `vl2png` accepts `-i -`), run `vl-convert` via `exec.Command` →
  `.Output()`.
- **`shellQuote` (main.go:1215) becomes dead code.** It exists solely to safely embed `query`
  inside the `sh -c` shell string — the one place in this codebase doing manual shell escaping.
  Once there's no shell string to embed into (argv passed directly to `exec.Command` needs no
  quoting at all — Go passes each argument as a separate `argv[]` entry, bypassing shell parsing
  entirely), this whole function and its escaping surface disappear. Net security improvement,
  not just a simplification: today's shell-string construction is exactly the shape that shell-
  injection bugs come from if `shellQuote` ever has an edge case it misses; removing the shell
  entirely removes that risk class outright rather than trusting the escaping to be complete.
- **No `HOME`/cache-directory special-casing found to be needed.** Checked for files vl-convert
  might write on first run (font cache, etc.) via `find / -newer <marker>` after the render — the
  only fontconfig cache hit was pre-existing (unrelated, dated months earlier), no new
  `~/.cache/vl-convert` or similar appeared. Consistent with the "Concurrency model" section's
  earlier finding (no shared-state footprint inside the container across concurrent runs) — this
  local-host test corroborates it rather than introducing a new risk.

**Resulting shape of the two call sites** (matches item 2 in "Concrete changes needed" above,
now verified instead of assumed):
1. Plain-spec path (`vegalite`/`html`/`html-page`/…): `exec.Command("ggsql", "exec", query,
   "--reader", "duckdb://memory").Output()` — replaces the `exec.Command("podman", args...)` call,
   same error-handling shape (`*exec.ExitError` → stderr).
2. Raster path (`containerPNG`): becomes two sequential `exec.Command` calls (`ggsql`, then
   `vl-convert`) with a Go-side byte patch in between — replaces the single `sh -c` pipeline call.
   Both processes' stdin/stdout are handled by Go's `os/exec` directly (`cmd.Stdin =
   bytes.NewReader(...)` for piping into `vl-convert` if the stdin route is taken instead of a temp
   file), no shell involved anywhere in this path.

**Resolved (was deferred here originally): `vl-convert` reads the patched spec via `/dev/stdin`
and writes the PNG via `/dev/stdout` — no temp file at all for this step.** Scoped and verified
directly against the real binary, not assumed from `--help` text:

- **`-i -` (the conventional "stdin" sentinel) doesn't work** — `vl-convert` takes it literally as
  a filename and fails: `Failed to read input file: - No such file or directory`. Its `--help`
  only ever describes `--input`/`--output` as "Path to ... file," no stdin/stdout convention
  documented.
- **`-i /dev/stdin` / `-o /dev/stdout` do work, verified through a real anonymous pipe** (not just
  a shell `<` file redirection, which can silently be a reopened regular file rather than a true
  pipe) — `cat spec.json | vl-convert vl2png -i /dev/stdin -o /dev/stdout | file -` round-tripped a
  valid PNG with empty stderr. Confirms `vl-convert` doesn't need to `seek`/`fstat` its input for
  size the way the earlier CSV-over-a-pipe auto-detect problem required — it just reads to EOF and
  writes to completion, cleanly pipe-compatible on both ends.
- **`-o -` is a trap worth flagging so nobody reaches for it**: it silently "succeeds" (`exit=0`)
  while writing to a literal file named `-` in the working directory instead of stdout — the
  redirected stdout capture came back empty. Cosmetically similar failure shape to the `-i -`
  case, but *worse* because it doesn't error — a caller redirecting stdout and checking only the
  exit code would ship an empty response. `/dev/stdout` is the form to use; `-` is not a supported
  alias for it despite being a common convention elsewhere.
- **Verified from actual Go `exec.Command`, not just the shell**: `cmd.Stdin = bytes.NewReader(specBytes)`
  feeding `vl-convert vl2png -i /dev/stdin -o /dev/stdout`, captured via `cmd.Output()` — produced
  a byte-identical valid PNG (`31677` bytes, correct PNG magic header). Go's `os/exec` wires
  `cmd.Stdin` (an `io.Reader`, not an `*os.File`) through an internal `os.Pipe()` automatically, and
  that pipe is exactly what `/dev/stdin` resolves to inside the child — no special handling needed
  on the Go side beyond what `exec.Command`'s normal API already does.
- **Net effect on "Temp-file cleanup" above**: the service ends up with **exactly one** class of
  temp file across the entire request lifecycle — the input CSV `ggsql` reads (still required,
  per the "CSV auto-detection only fails over a non-seekable pipe" finding in "Verified facts").
  Neither the intermediate patched Vega-Lite spec nor the PNG output needs a file on disk at all;
  both stay as in-memory `[]byte` in the Go process, piped directly between `ggsql`'s stdout →
  Go's string patch → `vl-convert`'s stdin, and `vl-convert`'s stdout → Go's HTTP response body.
  This simplifies the sweep logic from "Temp-file cleanup" to a single glob pattern
  (`ggvisual-*.csv`) with no second pattern to add for spec/PNG artifacts, and removes an entire
  class of potential leak (a stray `/tmp/spec.json`/`/tmp/out.png` under concurrent requests,
  which today's shell-pipeline `containerPNG` implementation uses *fixed*, non-random filenames
  for — a real cross-request collision risk under concurrency that disappears entirely once
  there's no file at all, not just once the names are randomized).

## Merged `Containerfile`

Item 3 in "Concrete changes needed" originally said "extend the existing `Containerfile`... and
change `ENTRYPOINT ["ggsql"]` → `ENTRYPOINT ["duckpipe", "serve"]`" — **that direct mutation
contradicts the "Unchanged" note right below it**, which keeps the existing CLI mode
(`duckpipe` on the host, `podman run ... localhost/ggsql:latest exec ...`) working, and
`compose.yaml`'s own comment (`# Run one-shot via: podman-compose run --rm ggsql exec ...`) — both
depend on this image's `ENTRYPOINT` staying `ggsql`. Mutating it in place would silently break the
CLI mode the same pass is supposed to leave alone.

**Resolved by testing a multi-stage, multi-target `Containerfile`** instead of one linear file —
built and ran it for real, not just sketched:

```dockerfile
FROM golang:1.25-bookworm AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ggvisual ./cmd/ggvisual
COPY internal ./internal
RUN CGO_ENABLED=0 go build -o /ggvisual ./cmd/ggvisual

FROM ubuntu:24.04 AS base
RUN apt-get update && apt-get install -y --no-install-recommends ... \
 && wget ... ggsql.deb && dpkg -i ... \
 && wget ... vl-convert.zip && install ... \
 && apt-get remove -y wget unzip && rm -rf /var/lib/apt/lists/*
   # identical to today's Containerfile, unchanged

FROM base AS service
COPY --from=builder /ggvisual /usr/local/bin/ggvisual
ENTRYPOINT ["ggvisual"]

FROM base AS cli
ENTRYPOINT ["ggsql"]
```

(As actually implemented, the HTTP service is a separate binary, `ggvisual`, not a `duckpipe
serve` subcommand — see "Implementation" at the end of this doc for why. The shape above — one
shared `base` stage, two `ENTRYPOINT`s — is otherwise exactly what was built and tested.)

- **The service binary builds fully static, no CGO** — `CGO_ENABLED=0 go build` succeeded with
  zero changes to any dependency (`pixterm`, `go-colorful`, `ansisvg`, `x/term` are all pure Go);
  `ldd` confirms `not a dynamic executable`. This means the builder stage's base image (Debian
  vs. the final stage's Ubuntu) never has to glibc-match the runtime stage — the binary carries
  no dynamic dependency on either. Binary size: 11MB.
- **`podman build --target service` produces a working image**: `which duckpipe ggsql
  vl-convert` inside it resolved all three to real paths (`/usr/local/bin/duckpipe`,
  `/usr/bin/ggsql`, `/usr/local/bin/vl-convert`), and running `duckpipe` inside the built image
  hit the expected stdin-required error path, confirming the binary actually executes (not just
  present on disk) — no missing shared libs, no exec-format issues. Final image size: 324MB
  (vs. today's 313MB for the `cli`-only image — the 11MB static binary is the entire delta, no
  new apt packages needed for `service`).
- **`podman build --target cli` reproduces the existing image byte-for-byte**: same digest
  (`9c9a0d73989f`) as the already-built `localhost/ggsql:latest` — podman's build cache recognized
  the `base` stage as identical to what's on disk today and reused every layer, and building `cli`
  from it needed zero new layers. This confirms the multi-target restructuring is a pure
  superset of the current file — the `cli` target is exactly what exists today, unchanged in
  substance, just reachable via `--target cli` instead of being the file's only output.
- **Two image tags from one `Containerfile`, sharing every `apt-get`/download layer.** Build
  `localhost/ggsql:latest` (or keep the name, `--target cli`) for the CLI/one-shot path
  `compose.yaml` and the "Unchanged" CLI mode depend on, and a new
  `localhost/ggvisual:latest` (`--target service`) for `ggvisual`. No duplicated
  Ubuntu/`.deb`/`vl-convert` install steps across two separate files — that was the alternative
  worth ruling out (a wholly separate `Containerfile.service`), rejected because it would drift
  from the `cli` install steps over time (two places to bump the `ggsql`/`vl-convert` version
  pins instead of one).
- **`compose.yaml` needs a second service entry**, not a change to the existing `ggsql` one:
  add a `ggvisual` block building `--target service`, alongside the unchanged
  `ggsql` (`--target cli`, or no explicit target since `cli` — or keep the file's current implicit
  single-stage shape's name — would need to be made the default `FROM` target via a final
  unnamed stage if `docker build` without `--target` needs to keep resolving to the CLI image;
  compose's `build.target: cli` key makes this explicit either way). Ties directly into the
  "Network exposure" section above, which already identified that the *running* service container
  likely belongs in `caddy-duckdb-module`'s own compose file (mirroring `fts-sidecar`) rather than
  this repo's — this section is only about the image-build side, which stays in this repo either
  way (that module would reference `localhost/ggvisual:latest` as a prebuilt image, same as
  it presumably would for the `cli` one today).

**At the time this section was written**: `ggvisual` itself didn't exist yet — this section only
verified the *packaging* mechanics (a static Go binary layers cleanly onto the existing image,
`ENTRYPOINT` can differ per target without duplicating install steps) against a placeholder
binary. It's since been built for real — see "Implementation" at the end of this doc, which
confirms the actual `ggvisual` binary and HTTP handlers work end-to-end inside this same
multi-target `Containerfile` shape.

## HTTP request encoding

Resolved: **query params for `visual`/`format` (and other short text options), raw CSV as the
request body** — not multipart, not JSON-wrapped. Built and ran a minimal working prototype of
`POST /render` (plain `net/http`, reusing the exact `exec.Command("ggsql", "exec", ...)` shape
verified in the "`exec.Command` replacement" section) rather than deciding this on paper.

- **Direct analogue of the existing CLI shape**, which is why it was the first thing tried: CLI
  flags (`--visual`, `--format`) become query params, stdin (CSV) becomes the HTTP body. Minimal
  conceptual translation for anyone who already knows the CLI, and for `caddy-duckdb-module`
  specifically, which already runs its own SQL and just needs to hand off a CSV blob plus two
  short strings — no new mental model needed on the client side.
- **Verified the actual risk case, not just the happy path**: sent a `visual` value containing
  embedded double quotes, a comma, and a `LABEL title => '...'` clause with quoted text
  (`VISUALISE species AS x, n AS y, species AS color DRAW bar LABEL title => 'Penguins, by
  "Species"'`) — standard `curl` URL-encoding plus Go's `r.URL.Query().Get()` round-tripped it
  correctly with zero manual escaping on either side. This was the main reason multipart or a
  custom header scheme looked tempting (worry that a `visualise` clause's punctuation wouldn't
  survive query-string encoding) — it wasn't a real problem once actually tested.
- **Raw CSV as the POST body works with a plain `--data-binary` upload**, no `Content-Type`
  negotiation needed — `io.Copy(tmpFile, r.Body)` on the server side is exactly the same temp-file
  write the CLI already does with `os.Stdin`, just fed from `r.Body` instead.
- **Error path returns a real 4xx with ggsql's own message, not a crash or opaque 500**: fed a
  `visual` referencing a nonexistent column — `ggsql exec` failed as expected, the handler
  returned `HTTP 400` with body `ggsql: Failed to execute query: Validation error: Layer 1:
  aesthetic 'x' references non-existent column 'nonexistent_col'`. (Prototype used ggsql's raw
  stderr text directly for expediency — the real handler should map this into the structured
  category envelope from "Concrete changes needed" item 6, not ship raw stderr as the response
  body; that's the next item to scope, not resolved here.)
- **Why not multipart**: no actual need surfaced — CSV-as-raw-body plus two or three short query
  params covers every field `/render` needs (`visual`, `format`, and optionally `png-width`/
  `png-height` mirroring the CLI's existing flags). Multipart earns its complexity when there are
  multiple binary parts or the fields are large/structured; neither applies here. Query-string
  length is the one real constraint worth remembering if a `visualise` clause ever gets very long
  (many `FACET`/`SCALE` clauses chained) — Go's default `net/http` server has no meaningfully low
  URL-length limit of its own, but intermediate proxies commonly cap around 8KB. Not hit by
  anything tested here (149 characters encoded for a realistic clause); worth a real limit check
  only if a future `visualise` clause turns out to routinely approach that size.
- **Response side is unchanged from what "Desirable sidecar capabilities" in
  `ggsql-endpoint.md` already specifies** — this section only scoped the *request* shape; the
  format-aware `Content-Type` response table (`vegalite`→JSON, `png`→`image/png`, `ansi`/`braille`→
  raw `text/plain`, never JSON-wrapped) was already decided there and reconfirmed by this
  prototype's `vegalite` branch (`w.Header().Set("Content-Type", "application/json"); w.Write(out)`
  — no re-encoding of the ggsql output).

## Structured error envelope

Item 6 in "Concrete changes needed" wanted a category (`bad_ggsql`, `bad_sql`, `timeout`,
`internal`) derivable without pattern-matching arbitrary stderr text. Tested this directly by
provoking every realistic failure mode against the real `ggsql`/`vl-convert` binaries and reading
the actual error text and exit codes, rather than assuming a scheme would work.

- **Exit code is useless for categorization — verified, not assumed.** Every `ggsql exec` failure
  tested (bad aesthetic column, bad SQL syntax, missing table, missing file, malformed `VISUALISE`
  grammar, bad `--reader` argument) exits `1`. No distinction at the process level; categorization
  has to come from stderr content.
- **stderr has a reliable, three-way prefix structure** (sampled across 7 distinct real failure
  cases):
  - `Failed to execute query: Validation error: ...` — a ggsql-level semantic problem (e.g.
    `aesthetic 'x' references non-existent column 'nope'`, or `No data sources found` from a
    `VISUALISE` clause missing `DRAW`). → **`bad_ggsql`**.
  - `Failed to execute query: Parse error: ...` — a ggsql grammar problem (e.g. `DRAW
    nonsensemark`, an unrecognized mark). → **`bad_ggsql`**.
  - `Failed to execute query: Data source error: Failed to execute SQL: <DuckDB error class>: ...`
    — the `sql` half is broken, not the `visualise` half. Sampled three DuckDB error classes
    behind this same prefix (`Parser Error` for `SELEKT` typo, `Catalog Error` for a nonexistent
    table, `IO Error` for a missing CSV path) — all three share the identical
    `Data source error: Failed to execute SQL:` prefix regardless of which DuckDB-internal error
    class follows, so matching on that prefix alone (not the DuckDB sub-class) is sufficient and
    more future-proof than enumerating every DuckDB error class by name. → **`bad_sql`**.
  - `Failed to create reader: ...` (a *CLI-usage* error, distinct from a *query* error — e.g. bad
    `--reader` scheme) — **structurally cannot happen from real user input** in the target design,
    because `ggvisual` always passes the same fixed `--reader duckdb://memory` constant; it
    never echoes anything caller-supplied into that argument. If this class is ever seen in
    production it means `duckpipe`'s own code is passing a bad fixed argument, i.e. a programming
    bug, not a bad request. → **`internal`**, and worth a startup self-test (`ggsql exec "SELECT
    1" --reader duckdb://memory` at boot) if this class showing up ever needs to be caught before
    it reaches a real request.
- **`vl-convert` errors have their own, differently-shaped prefix**: `Error: Failed to parse
  input file as JSON: ...` (malformed spec) and `Error: Failed to read input file: ... No such
  file or directory` (missing temp file) — both sampled directly. Neither should be reachable from
  a caller-supplied `visual`/`format` value: the JSON spec being parsed is `ggsql`'s own output
  after this service's Go code already validated it round-trips as JSON (the `"container"` →
  pixel-size patch step). If `vl-convert` ever rejects it, that means *this service's own patch
  step* produced invalid JSON, or the temp file it expects got cleaned up out from under it (a
  concurrency bug in the cleanup sweep from "Temp-file cleanup") — either way, **`internal`**, not
  a caller-facing 400.
- **Timeout is detected structurally, not via stderr text — verified with a real
  `exec.CommandContext` kill**: a command exceeding `context.WithTimeout` reports `err ==
  "signal: killed"` from `cmd.Run()`/`cmd.Output()`, and separately, `ctx.Err() ==
  "context deadline exceeded"` — checking `ctx.Err() == context.DeadlineExceeded` after a
  non-nil `cmd` error is a clean, direct-vs-text-matching category source. → **`timeout`**,
  independent of whatever partial stderr the killed process happened to have written.

**Resulting design**: a small ordered prefix-match function per subprocess
(`categorizeGgsqlError(stderr string) category`, `categorizeVlConvertError(...)`), checked *after*
first checking `ctx.Err() == context.DeadlineExceeded` (timeout always wins, since a killed
process's partial stderr is not meaningful to pattern-match). Anything matching none of the known
prefixes falls through to `internal` by default — a closed allowlist of recognized prefixes rather
than an open list of "things that mean bad request," so an unrecognized future ggsql error message
fails safe (500, logged) instead of silently being categorized as the caller's fault (400).

**HTTP status mapping** (per `ggsql-endpoint.md`'s own request: "map to the right HTTP status"):
`bad_ggsql` → 400, `bad_sql` → 400, `timeout` → 504, `internal` → 500. Response body is the JSON
envelope `{"error": {"category": "...", "message": "..."}}` regardless of the requested render
`format` — this is the one place a response is always JSON even when `format=png`/`ansi` was
requested, consistent with `ggsql-endpoint.md`'s existing note that errors "return the module's
existing `ErrorResponse` JSON shape regardless of requested format."

**Not yet decided, deferred**: whether `message` in the envelope is the raw matched stderr text
verbatim (simplest, but leaks internal file paths like `/tmp/render-xxx.csv` into `bad_sql`/
`IO Error` messages — sampled case 5 above shows exactly this) or a cleaned/templated string with
paths stripped. Leaning toward stripping the temp-file path specifically (the one piece of
internal-filesystem detail that reliably appears in real messages, per the samples above) before
the `internal`-vs-caller-facing distinction matters much more than which of many possible DuckDB
error phrasings survives — worth a small `strings.ReplaceAll(msg, tmpPath, "<input>")` rather than
a broader sanitization pass, but this is a small polish item, not blocking the category scheme
above.

## Per-request timeout

Item 5 in "Concrete changes needed." The detection mechanism (`context.WithTimeout` +
`ctx.Err() == context.DeadlineExceeded`) was already verified in the "Structured error envelope"
section above; this pass tested the *behavioral* questions that section didn't cover — whether a
kill is actually clean against a real, genuinely slow query, and what a sane default duration is.

- **`ggsql exec` doesn't fork any subprocesses of its own — verified via `ps -ef --forest` while
  a real query was running**, not assumed. DuckDB is embedded (an in-process engine, not a
  separate spawned `duckdb` binary), so the process tree under a running `ggsql exec` is exactly
  one process. This matters because Go's default `exec.CommandContext` kill only signals the
  direct child, not any of its descendants — if `ggsql` spawned worker processes, a naive kill
  could leave orphans running after the parent died. It doesn't, so **no `SysProcAttr{Setpgid:
  true}` / process-group kill is needed** — the plain default `exec.CommandContext` behavior is
  sufficient.
- **Reproduced a genuinely slow query to test against, not a synthetic sleep**: a real 2M-row CSV
  (`SELECT * ... VISUALISE a AS x, b AS y, c AS color DRAW point`, unaggregated scatter) takes
  5.5s and produces a 525MB Vega-Lite spec (every row embedded as a literal data point — no
  server-side aggregation for a point mark). Ran this under a 1s `context.WithTimeout`: killed
  cleanly at 1.08s elapsed, `err == "signal: killed"`, `ctx.Err() ==
  "context deadline exceeded"`, **zero leftover processes** (checked `ps -ef` ~1s after the kill —
  nothing) and **zero leftover temp/spill files** (`find /tmp -newer ...` after the kill turned up
  nothing DuckDB-internal — no partial spill file, no cache artifact).
- **This same large-query case surfaces a related but separate problem worth flagging, not
  solving here**: 525MB for one render is enormous, and nothing bounds it today — this is exactly
  the "row-count cap on the SQL result" open question already on record in
  `ggsql-endpoint.md` ("needs... probably a row-count cap on the SQL result before it's ever
  written to CSV"). A timeout alone doesn't fix this class of problem, it only bounds how long the
  service spends producing an enormous response before giving up — the response-size problem is
  independent and belongs to that module's own row-count-cap decision (it owns the SQL query, this
  service only owns rendering what it's handed), not something to re-scope here.
- **Default duration**: no single "correct" number, but `caddy-duckdb-module`'s own
  `DUCKDB_QUERY_TIMEOUT` defaults to `10s` (per that module's `docker-compose.yml`), and every
  realistic render measured across this whole scoping effort — the concurrency tests, the HTTP
  prototype, ordinary demo-sized data — completed in well under 1s. A default around **30s**
  covers a real 5.5s-class large-data outlier with meaningful headroom while still failing fast
  relative to a caller's own upstream timeout (that module's `10s` `DUCKDB_QUERY_TIMEOUT` bounds
  its own query step separately, before CSV ever reaches this service) — not a hard requirement
  from this testing, just a number grounded in the actual latencies measured rather than picked
  arbitrarily. Should be configurable (env var or flag), consistent with this project's existing
  `DUCKDB_QUERY_TIMEOUT`-style pattern, not hardcoded.
- **Applies per subprocess call, not once across the whole request** — `containerPNG`'s replacement
  is two sequential `exec.Command` calls (`ggsql` then `vl-convert`, per the "`exec.Command`
  replacement" section). A single `context.WithTimeout` wrapping the whole handler and passed to
  both `exec.CommandContext` calls in sequence is simplest and correct: the timeout is a wall-clock
  budget for the request as a whole, and Go's `context` naturally enforces that across sequential
  calls sharing one `ctx` — no separate per-subprocess timeout budgeting needed, and nothing
  tested here found a reason to split the budget between the two steps.

## Does the `Containerfile` need `tini`?

No. Checked against both of `tini`'s actual purposes rather than the general "always add an
init process" reflex, and neither applies here.

- **Zombie reaping**: `tini`'s classic role is reaping orphaned children when the PID-1 process
  doesn't `wait()` on them (e.g. a shell script `ENTRYPOINT` that backgrounds a process and never
  reaps it). Doesn't apply here on either side: `ggsql exec` was already verified (in "Per-request
  timeout" above, via `ps -ef --forest` on a live run) to spawn zero subprocesses of its own —
  DuckDB is embedded, not a forked child — so there's nothing under `ggsql` for `ggvisual`
  to fail to reap. And `ggvisual`'s own children (`ggsql`, `vl-convert`, invoked via
  `exec.Command`) are correctly reaped by Go's `os/exec` package itself, which calls `wait4()` as
  part of every `cmd.Run()`/`cmd.Wait()` — already confirmed zombie-free after a kill in the
  timeout testing above.
- **SIGTERM being silently ignored as PID 1**: the commonly-cited reason to add `tini` to *any*
  container — Linux gives PID 1 of a namespace an implicit "ignore" disposition for signals with
  no explicitly-installed handler (`pid_namespaces(7)`), so a bare binary with no signal handling
  code can end up undying on `podman stop`/`docker stop` until the hard `SIGKILL` timeout.
  **Tested this directly against a real Go binary as `ENTRYPOINT` (no `tini`, no
  `signal.Notify` call anywhere in the code) — it does not reproduce for Go.** Built a
  minimal `FROM scratch` image with a plain Go binary as PID 1, ran it, and timed `podman stop`
  with a generous 3–5s grace period: the container exited in ~0.066s both times, far faster than
  the stop timeout, meaning `SIGTERM` was delivered and acted on immediately, not ignored. This
  matches a known (if less commonly repeated) detail of the Go runtime: it installs its own
  internal signal handling machinery unconditionally (for panic/stack-dump/preemption purposes),
  which means the kernel sees SIGTERM as "handled" for PID-1-exemption purposes even before any
  user code calls `signal.Notify` — so the classic tini-motivating failure mode specifically
  doesn't apply to plain Go binaries the way it does to shell scripts or some other language
  runtimes.
- **What this does *not* resolve**: instant, unhandled termination on `SIGTERM` is not graceful
  shutdown — it means `ggvisual` dying on `podman stop` doesn't finish in-flight requests or
  give the merged-container's `ggsql`/`vl-convert` children a chance to complete either (killing a
  container's PID 1 tears down its whole PID namespace, taking any remaining children with it
  regardless of tini). `tini` wouldn't fix that either — it forwards/reaps signals, it doesn't
  drain in-flight work. Graceful shutdown is a `ggvisual`-side feature
  (`signal.NotifyContext(os.Interrupt, syscall.SIGTERM)` + `http.Server.Shutdown(ctx)`), still
  unscoped and listed separately below — this section only answers the `tini` question, not that
  one.

## Graceful shutdown

Built and ran the real pattern (`signal.NotifyContext` + `http.Server.Shutdown(ctx)`) in a
container, driven with `podman stop` (not a bare `kill -TERM` in this shell — that reliably
crashed the sandbox's own process-group handling, exit code 144 every time; switching to a real
container and `podman stop`, the same technique already used for the `tini` test, worked cleanly).
Tested both the case where the shutdown grace period covers the in-flight request and the case
where it doesn't — the two produce materially different outcomes, which is the main finding here.

- **Clean case: grace period ≥ in-flight request duration.** A 4s shutdown grace against a
  handler that takes 3s: the in-flight request **completed normally and got its response**
  (`curl` exit 0, HTTP 200, `time_total` 3.005s), `Shutdown()` returned `nil`, and the container
  exited only after that. Confirms `http.Server.Shutdown` does what it's documented to do — stop
  accepting new connections, then wait for active handlers to finish on their own — and that this
  composes correctly with `podman stop`.
- **New connections are actively refused during drain, not silently hung** — fired a second
  request 0.5s into the shutdown window (server no longer accepting): got `Connection reset by
  peer` immediately (`curl` exit 56), not a timeout or a hang. A caller retrying against a
  restarting sidecar gets a fast, clear failure to react to, not an indefinitely blocked
  connection.
- **Broken case, the one worth designing around: grace period < in-flight request duration.**
  Same setup with a 2s shutdown grace against the same 3s handler: `Shutdown()` returned
  `context deadline exceeded` at the 2s mark, `main()` proceeded to exit — and the still-running
  handler goroutine was simply abandoned mid-flight (Go's `Shutdown` only *waits*, it never cancels
  or kills the handler when its own timeout elapses). The client got **`Empty reply from server`**
  (`curl` exit 52, status `000`) at 2.53s — not a clean HTTP error, a raw connection drop with no
  status code and no envelope. This means: **any caller of this service must treat a bare
  connection-reset/empty-reply as a retriable condition** (the render may have been valid and just
  got cut off by a redeploy), not as evidence of a bad request — worth a note for
  `caddy-duckdb-module`'s own error handling when it becomes a client of this service, alongside
  the JSON `ErrorResponse` envelope from "Structured error envelope" (that envelope only covers
  errors the handler itself returns deliberately; a mid-flight SIGKILL/shutdown-timeout produces
  no envelope at all, by construction — there's no code left running to write one).
- **Direct consequence: the shutdown grace period and the per-request timeout budget are coupled,
  and should be set with that coupling in mind, not independently.** The "Per-request timeout"
  section above recommended a ~30s default per-request budget (grounded in a measured 5.5s
  large-query outlier plus headroom). If the shutdown grace period is shorter than that — e.g.
  podman/docker's own default `stop` grace is 10s — then **any request past that 10s point gets
  the broken-case outcome above on every redeploy**, regardless of how generous the per-request
  timeout is. Two ways to resolve this, not mutually exclusive: (a) explicitly set a longer
  `stop_grace_period` in `compose.yaml`/`docker-compose.yml` for the service target, long enough
  to cover the real worst-case render time with margin (informed by the actual 5.5s measurement,
  not the 30s theoretical ceiling — a `stop_grace_period` doesn't need to match the *timeout*, just
  realistic actual durations); (b) accept that redeploys during a rare, unusually slow render can
  produce a connection-reset the caller must retry, and lean on the retriable-connection-reset
  handling from the point above instead of trying to make every possible request duration safe
  under shutdown. Not resolved definitively here — this is a real tradeoff between "faster
  redeploys" and "never cut off in-flight work," not a bug with one correct fix — but the specific
  numbers to reconcile (measured ~5.5s worst case tested so far, 30s configured timeout ceiling,
  and whatever `stop_grace_period` compose ends up setting) are now on record instead of three
  independently-chosen defaults that happen to not line up.
- **What this section does *not* need to handle**: killing `ggsql`/`vl-convert` subprocesses
  explicitly during shutdown. Per the "`tini`" section above, killing the container's PID-1
  process (`ggvisual`) tears down the whole PID namespace, taking any still-running child
  subprocess with it regardless of whether `duckpipe`'s own code explicitly kills them first — so
  there's no separate "also clean up subprocess children on shutdown" step to add here; it happens
  for free as a consequence of container teardown, same mechanism already established.

## Resource limits per subprocess

Tested a real enforcement mechanism against the actual failure mode it needs to guard —
the 2M-row / 525MB-spec / 7.4GB-peak-RSS outlier already surfaced in "Per-request timeout" — 
rather than assuming a mechanism works from documentation alone.

- **`prlimit` (from `util-linux`) is already present in the target image** — confirmed inside
  `localhost/ggsql:latest` itself (`which prlimit` → `/usr/bin/prlimit`, from the `ubuntu:24.04`
  base, no new package needed). `prlimit --as=<bytes> -- ggsql exec ...` wraps the target command
  and sets `RLIMIT_AS` before `exec()`, avoiding the race a `Setrlimit`-after-`Start()` approach in
  Go would have. This is directly usable from `exec.Command("prlimit", append([]string{"--as=" +
  n}, "--", "ggsql", "exec", ...)...)` — no cgroup delegation needed (which may not even be
  available depending on how the container is run), no `systemd-run` (there's no systemd running
  as PID 1 in this container — `ggvisual` is).
- **`RLIMIT_AS` constrains virtual address space, not actual resident memory — verified this
  matters a lot in practice.** A trivial 3-row query that only ever uses 42MB of real RSS
  (measured via `/usr/bin/time -v`) **still failed** under a 256MB `--as` limit
  (`Out of Memory Error: Allocation failure`) — DuckDB reserves address space well beyond what a
  tiny query actually touches. Had to raise the limit to **1GB before the same trivial query
  succeeded reliably** (tested 1GB/2GB/4GB, all fine at 1GB and above). **This means `RLIMIT_AS`
  can only be a coarse backstop against a genuinely runaway render, not a tight per-request memory
  budget** — anything meaningfully below ~1GB breaks ordinary small queries regardless of how
  little memory they actually need.
- **The failure mode under a real trip is bimodal, verified with the actual 2M-row outlier at a
  1GB limit** — and the two modes need different handling:
  - **DuckDB's own SQL execution catches allocation failure gracefully** in some cases (the small
    trivial-query test above): `Failed to execute query: Data source error: Failed to execute SQL:
    Out of Memory Error: Allocation failure`, clean `exit=1`. This **already matches the
    `Data source error: Failed to execute SQL:` prefix** from "Structured error envelope" above —
    falls into `bad_sql` with zero new categorization logic needed.
  - **`ggsql`'s own Rust code — building the giant in-memory JSON structure for an unaggregated
    point-mark result — does not catch it.** Running the real 2M-row/1GB-limit case: Rust's
    default global-allocator behavior on a failed allocation is to `abort()`, not return a
    catchable error — observed directly: `memory allocation of 1671168 bytes failed`, process
    terminated by `SIGABRT`, `exit=134` (128+6), not the `exit=1` every other tested failure mode
    produces. **Exit code is informative here, contradicting the earlier "exit code is useless"
    finding in the narrow case of an abnormal (signal-terminated) exit** — `cmd.ProcessState`
    exposes this via the Go `syscall.WaitStatus.Signaled()` check, which should be checked *before*
    falling into the stderr-prefix matching scheme from "Structured error envelope", since a
    process killed by a signal may have written partial/no meaningful stderr at all.
  - **Recommended categorization**: the DuckDB-level graceful OOM needs no new code (already
    `bad_sql`, per the existing prefix match). The signal-terminated crash case doesn't cleanly fit
    any of the four categories `ggsql-endpoint.md` asked for (it's not quite `internal` in the
    "our own bug" sense, but it's also not the caller's syntax being wrong) — simplest resolution
    without expanding that four-category contract: fold it into `internal` (500), since from the
    caller's point of view it's an unexpected server-side failure either way, and note in the
    envelope's `message` that it was resource-related (distinguishable from a true internal bug in
    logs via the `SIGABRT`/`134` detection, without inventing a fifth caller-facing category for a
    case that should be rare once a limit is actually in place).
- **Correction, found during implementation: `RLIMIT_AS` is not just coarse for `vl-convert` — it
  doesn't work at all, at any tested value up to 32GiB.** This section's testing above only ran
  `prlimit --as` against `ggsql`; when the same wrapper was applied to `vl-convert` while actually
  building `ggexec.Runner`, even a trivial 2-point scatter chart (76MB real RSS, confirmed via
  `/usr/bin/time -v`) aborted immediately: `Fatal process out of memory: Oilpan: CagedHeap
  reservation`. `vl-convert` embeds a V8-based engine (compiling Vega-Lite → Vega involves running
  real JS), and V8 reserves a huge virtual address space cage for its heap up front regardless of
  actual usage — raising the limit made no difference at 4GiB, 8GiB, 16GiB, or 32GiB, all failed
  identically. **`RLIMIT_AS` and V8 are fundamentally incompatible, not just a "needs a higher
  number" problem.** Fix, also verified directly: use `RLIMIT_DATA` (`prlimit --data=`) for
  `vl-convert` instead — 2GiB `--data` renders correctly, and a sanity check with an absurdly tiny
  1MB `--data` reliably fails, confirming it's a real enforced constraint and not silently a no-op.
  `ggsql`/DuckDB has no V8 dependency and isn't affected — it keeps using `--as`. Implemented as
  two separate wrap helpers (`wrapAS` for `ggsql`, `wrapData` for `vl-convert`) in
  `internal/ggexec/ggexec.go` rather than one shared one.
- **This is a backstop, not a replacement for the already-deferred concurrency semaphore or
  container-level memory limit — three independent layers, not one mechanism covering
  everything**: `prlimit` bounds any *single* subprocess's worst-case blowup (coarse, ~1GB floor,
  catches the pathological single-huge-query case); the concurrency semaphore already flagged as
  deferred in "Concurrency model" bounds how many subprocesses can run *at once* (protects against
  many-small-requests aggregate pressure, which a per-process `RLIMIT_AS` does nothing for); the
  container-level `deploy.resources.limits.memory` (the same pattern already used by
  `caddy-duckdb-module`'s own `docker-compose.yml` for its main service) is the final backstop if
  the first two are misconfigured or the workload is worse than expected — OOM-kills the whole
  container, `restart: unless-stopped` (already established pattern in that same file) brings it
  back. None of the three individually is sufficient; recommend all three rather than picking one.
- **Not tested this pass, lower priority**: a per-subprocess CPU quota (`prlimit --cpu=` limits
  CPU-*time*-consumed before `SIGXCPU`, not CPU-share/throttling the way `cgroups` `cpu.max` does)
  — the "Concurrency model" section's CPU-bound scaling findings didn't surface a CPU-exhaustion
  risk the way the memory outlier did, so this wasn't prioritized for verification in this pass.

## Implementation

Built as a **separate binary, `ggvisual`** (`cmd/ggvisual`), not a `ggvisual` subcommand as
originally sketched in "Concrete changes needed" — cleaner than mutating `duckpipe`'s own CLI
entrypoint, and it sidesteps the `ENTRYPOINT` conflict the "Merged `Containerfile`" section had to
work around (separate binary → separate `Containerfile` target → separate `ENTRYPOINT`, no shared-
image ambiguity to resolve). The pure PNG/spec → output-format conversion logic (every
`render*` function from the original `main.go`) moved into `internal/chartconv`, shared by both
`cmd/duckpipe` (still podman-based, byte-for-byte unchanged behavior) and `cmd/ggvisual`
(local-subprocess-based, per "`exec.Command` replacement"). Local-subprocess execution and error
categorization live in `internal/ggexec`.

Implements: `POST /render` (query-param `visual`/`format`/`width`/`height`/`png-width`/
`png-height`, raw CSV body — per "HTTP request encoding"), `POST /validate` (`ggsql validate`,
no `--reader`, syntax-only per ggsql's own scope), `GET /formats`, `GET /health`, `GET /version`
(real `ggsql --version`/`vl-convert --version` output, not hardcoded), the structured JSON error
envelope with category → HTTP status mapping, per-request `context.WithTimeout` (default 30s,
flag-configurable), graceful shutdown (`signal.NotifyContext` + `http.Server.Shutdown`, default
10s grace, flag-configurable), startup + periodic temp-file sweep, and per-subprocess `prlimit`
(see the correction above for why `ggsql` and `vl-convert` need different `prlimit` limit types).

Verified end-to-end inside the real built `localhost/ggvisual:latest` image (`podman build
--target service`): all 19 formats render correctly via real HTTP requests against the actual
`ggsql`/`vl-convert` binaries, `localhost/ggsql:latest` (`--target cli`) still builds and runs
identically to before, and the existing `duckpipe` CLI's `make demo` output is unchanged. `go fmt`/
`go vet`/`go test` all pass.

**Not implemented from the scoped design** (correctly out of scope for a first pass, per
"Simplicity First" — add when there's a concrete need, not speculatively): the concurrency
semaphore (deferred as "capacity management, not correctness" in "Concurrency model"), the
container-level `deploy.resources.limits.memory` backstop (a `caddy-duckdb-module`-side
`docker-compose.yml` concern once `ggvisual` is wired in there, per "Network exposure"), and error
`message` path-stripping (flagged as a small polish item in "Structured error envelope").

## Still open

- **Whether Arrow IPC is worth adopting as the wire format** between `caddy-duckdb-module` and
  this service instead of CSV, given that module already speaks Arrow internally — see "Data
  handoff" above. Not required to hit this doc's goal, so left as an independent, lower-priority
  question rather than blocking anything here.
