# ggvisual

Tools for rendering [ggsql](https://github.com/posit-dev/ggsql) (Grammar-of-Graphics SQL) charts
outside of R — as a terminal chart, an SVG, a standalone HTML page, an asciinema recording, or
over HTTP.

This repo has two things in it:

- **`duckpipe`** (`cmd/duckpipe`) — a CLI that pipes CSV from the DuckDB CLI through ggsql and
  renders the result. See [`cmd/duckpipe/README.md`](cmd/duckpipe/README.md).
- **`ggvisual`** (`cmd/ggvisual`) — the same rendering pipeline as a persistent HTTP service
  (`POST /render`, `POST /validate`, `GET /formats`, `GET /health`, `GET /version`), for services
  that want to proxy chart requests instead of shelling out to a CLI per request. See
  [`persistent-service.md`](persistent-service.md) for the design and
  [`internal/chartconv`](internal/chartconv)/[`internal/ggexec`](internal/ggexec) for the shared
  implementation both binaries build on.

Both wrap the same `ggsql` + [`vl-convert`](https://github.com/vega/vl-convert) toolchain, built
via the `Containerfile` in this repo:

```sh
podman build --target cli     -t localhost/ggsql:latest .     # ggsql CLI, used by duckpipe
podman build --target service -t localhost/ggvisual:latest .  # the ggvisual HTTP service
```

## Requirements

- Go 1.25+
- [DuckDB](https://duckdb.org/) CLI (for `duckpipe`'s input)
- [Podman](https://podman.io/) (for building/running the `ggsql`/`ggvisual` images)

## Build

```sh
make build            # bin/duckpipe
make build-ggvisual   # bin/ggvisual
make image             # localhost/ggsql:latest
make image-ggvisual    # localhost/ggvisual:latest
```

## Development

```sh
make fmt vet test
```
