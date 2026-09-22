# duckpipe

Pipe CSV from DuckDB through [ggsql](https://github.com/posit-dev/ggsql) and render the result as a chart in your terminal, as an SVG, as a standalone HTML page, or as an asciinema recording.

For a persistent HTTP service instead of a per-invocation CLI, see `../ggvisual` and
`/persistent-service.md`.

## Requirements

- [DuckDB](https://duckdb.org/) CLI
- [Podman](https://podman.io/) with the `localhost/ggsql:latest` image (see below)

## Installation

Build from source:

```sh
make build   # produces bin/duckpipe
```

## Usage

```
duckdb ... | duckpipe --visual "<ggsql clause>" [--format <fmt>] [options]
```

`duckpipe` reads CSV (with a header row) from stdin, renders a Vega-Lite chart via ggsql running in a Podman container, and writes the result to stdout.

### `--visual`

The ggsql `VISUALISE` clause — everything that follows the `SELECT` statement. Required.

```
VISUALISE <col> AS x, <col> AS y [, <col> AS color]
DRAW <mark>
[LABEL title => '<text>']
```

Common marks: `point`, `bar`, `line`, `area`.

### `--format`

| Format | Output |
|---|---|
| `url` *(default)* | Vega-Lite editor URL (shareable link) |
| `vegalite` | Raw Vega-Lite JSON |
| `html` | Self-contained `<div>` + ESM `<script>` (Quarto / embed) |
| `html-body` | `<div>` + `<script>` using a globally-loaded vega-embed |
| `html-page` | Standalone HTML page with vega-embed from CDN |
| `png` | PNG image bytes |
| `ansi` | ANSI true-colour half-block art (terminal) |
| `braille` | Coloured Unicode braille art (terminal) |
| `html-page-offline` | Standalone HTML page with vega-embed inlined (no CDN, works fully offline) |
| `svg` | SVG (half-block art) |
| `braille-svg` | SVG (braille art) |
| `text` | Plain monochrome half-block character grid |
| `braille-text` | Plain monochrome braille character grid |
| `xterm` | xterm.js `<div>` + `<script>` (half-block art; host page must load xterm.js) |
| `xterm-page` | Standalone HTML page with xterm.js from CDN (half-block art) |
| `braille-xterm` | xterm.js `<div>` + `<script>` (braille art; host page must load xterm.js) |
| `braille-xterm-page` | Standalone HTML page with xterm.js from CDN (braille art) |
| `cast` | asciinema v3 `.cast` recording |
| `cast-page` | Standalone HTML page with asciinema-player from CDN |

### Other options

| Flag | Default | Description |
|---|---|---|
| `--image` | `localhost/ggsql:latest` | Podman image for ggsql + vl-convert |
| `--width` | auto | Output width in characters (ansi / braille) |
| `--height` | auto | Output height in characters (ansi / braille) |
| `--png-width` | `600` | PNG width in pixels |
| `--png-height` | `400` | PNG height in pixels |

## Examples

### Shareable Vega-Lite URL

```sh
duckdb < penguins.sql \
  | duckpipe --visual "VISUALISE species AS x, n AS y, species AS color DRAW bar"
```

### Terminal chart (ANSI)

```sh
duckdb < penguins.sql \
  | duckpipe --format ansi \
             --visual "VISUALISE species AS x, n AS y, species AS color DRAW bar"
```

### Standalone HTML page

```sh
duckdb < penguins.sql \
  | duckpipe --format html-page \
             --visual "VISUALISE species AS x, n AS y DRAW bar" \
  > chart.html
```

### asciinema recording (HTML page)

```sh
duckdb < penguins.sql \
  | duckpipe --format cast-page \
             --visual "VISUALISE species AS x, n AS y DRAW bar" \
  > cast.html
```

The `.cast` format uses asciinema v3 NDJSON. The `cast-page` format embeds the recording as a `data:` URL so the file works in `file://` and server contexts without any additional assets.

#### Playing `.cast` files in a terminal

```sh
asciinema play chart.cast
asciinema play https://example.com/chart.cast   # also works directly over HTTP(S), no download needed
```

**Version matters here.** `pip install asciinema` installs the old Python implementation
(currently `2.4.0` on PyPI) — it only reads asciicast v1/v2 and will reject a `duckpipe`-generated
`.cast` file with `playback failed: only asciicast v1 and v2 formats can be opened`. The current
CLI is a separate Rust rewrite, distributed via [GitHub releases](https://github.com/asciinema/asciinema/releases)
(not published to PyPI under the same package), and does support v3 — verified directly against
`duckpipe --format cast` output.

### Embed in a Quarto document

Use `--format html` for ESM-based embedding (no globals needed):

````markdown
```{python}
#| output: asis
import subprocess

result = subprocess.run(
    ["duckpipe", "--format", "html",
     "--visual", "VISUALISE x AS x, y AS y DRAW point"],
    input=csv_bytes, capture_output=True
)
print(result.stdout.decode())
```
````

Use `--format html-body` when vega-embed is already loaded via `include-in-header`.

## Container image

The container runs ggsql and [vl-convert](https://github.com/vega/vl-convert) to render a Vega-Lite spec to PNG. Build it with:

```sh
make image   # podman build --target cli -t localhost/ggsql:latest .
```

This `Containerfile` also has a `service` target that adds the `ggvisual` HTTP service binary —
see `../ggvisual`.

## Demo

```sh
make demo          # writes demo/ (SVG, HTML, .cast)
make demo-stdout   # renders ansi + braille to the terminal
```
