# Two build targets sharing one `base` image (ggsql + vl-convert):
#   podman build --target cli     -t localhost/ggsql:latest .      (unchanged — ENTRYPOINT ggsql)
#   podman build --target service -t localhost/ggvisual:latest .   (adds the ggvisual binary)
# `cli` is the last stage, so a plain `podman build .` with no --target still
# produces today's image — see persistent-service.md's "Merged Containerfile"
# section for why the two can't share one ENTRYPOINT.

FROM golang:1.25-bookworm AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ggvisual ./cmd/ggvisual
COPY internal ./internal
RUN CGO_ENABLED=0 go build -o /ggvisual ./cmd/ggvisual

FROM ubuntu:26.04 AS base

RUN apt-get update \
 && apt-get install -y --no-install-recommends wget ca-certificates unixodbc unzip libfontconfig1 fonts-dejavu-core util-linux \
 && wget -q -O /tmp/ggsql.deb \
      https://github.com/posit-dev/ggsql/releases/download/v0.5.2/ggsql_0.5.2_amd64.deb \
 && dpkg -i /tmp/ggsql.deb \
 && rm /tmp/ggsql.deb \
 && wget -q -O /tmp/vl-convert.zip \
      https://github.com/vega/vl-convert/releases/download/v1.9.0/vl-convert_linux-64.zip \
 && unzip -q /tmp/vl-convert.zip -d /tmp/vl-convert \
 && install -m 755 /tmp/vl-convert/bin/vl-convert /usr/local/bin/vl-convert \
 && rm -rf /tmp/vl-convert.zip /tmp/vl-convert \
 && apt-get remove -y wget unzip \
 && apt-get autoremove -y \
 && rm -rf /var/lib/apt/lists/*

FROM base AS service
RUN apt-get update \
 && apt-get install -y --no-install-recommends curl \
 && rm -rf /var/lib/apt/lists/*
COPY --from=builder /ggvisual /usr/local/bin/ggvisual
ENTRYPOINT ["ggvisual"]

FROM base AS cli
ENTRYPOINT ["ggsql"]
