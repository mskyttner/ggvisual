.PHONY: build build-ggvisual image image-ggvisual fmt vet test clean demo demo-stdout

BINARY         := duckpipe
IMAGE          := localhost/ggsql:latest
SERVICE_IMAGE  := localhost/ggvisual:latest

# Vega-Lite visual clause — bar chart so solid colored bars are visible at low
# terminal resolution (scatter dots would disappear into the white background).
VISUAL := VISUALISE species AS x, n AS y, species AS color \
          DRAW bar \
          LABEL title => 'Penguins by Species'

# Raw penguins CSV (one row per bird); aggregated CSV has one row per species.
DEMO_CSV     := /tmp/duckpipe-demo.csv
DEMO_BAR_CSV := /tmp/duckpipe-bar.csv

build:
	go build -o bin/$(BINARY) ./cmd/duckpipe

build-ggvisual:
	go build -o bin/ggvisual ./cmd/ggvisual

image:
	podman build --target cli -t $(IMAGE) .

image-ggvisual:
	podman build --target service -t $(SERVICE_IMAGE) .

fmt:
	go fmt ./...

vet:
	go vet ./...

test:
	go test ./...

clean:
	rm -rf bin/ demo/

# ── demo targets ─────────────────────────────────────────────────────────────
# Fetch raw penguins CSV once.
$(DEMO_CSV):
	duckdb < penguins.sql > $@

# Aggregate to one row per species (count); bar chart needs grouped data.
$(DEMO_BAR_CSV): $(DEMO_CSV)
	echo "COPY (SELECT species, COUNT(*) AS n FROM read_csv('$(DEMO_CSV)', header=true) GROUP BY species ORDER BY n DESC) TO '/dev/stdout' (FORMAT CSV, HEADER);" \
	  | duckdb 2>/dev/null > $@

demo: $(DEMO_BAR_CSV) | bin/$(BINARY)
	@mkdir -p demo
	@echo "→ ansi (terminal stdout)"
	@cat $(DEMO_BAR_CSV) | ./bin/$(BINARY) --format ansi --visual "$(VISUAL)"
	@echo "→ braille (terminal stdout)"
	@cat $(DEMO_BAR_CSV) | ./bin/$(BINARY) --format braille --visual "$(VISUAL)"
	@echo "→ blocks-svg → demo/blocks.svg"
	@cat $(DEMO_BAR_CSV) | ./bin/$(BINARY) --format svg --visual "$(VISUAL)" > demo/blocks.svg
	@echo "→ braille-svg → demo/braille.svg"
	@cat $(DEMO_BAR_CSV) | ./bin/$(BINARY) --format braille-svg --visual "$(VISUAL)" > demo/braille.svg
	@echo "→ vegalite → demo/chart.html"
	@cat $(DEMO_BAR_CSV) | ./bin/$(BINARY) --format html-page --visual "$(VISUAL)" > demo/chart.html
	@echo "→ vegalite (offline) → demo/chart-offline.html"
	@cat $(DEMO_BAR_CSV) | ./bin/$(BINARY) --format html-page-offline --visual "$(VISUAL)" > demo/chart-offline.html
	@echo "→ cast → demo/cast.cast + demo/cast.html"
	@cat $(DEMO_BAR_CSV) | ./bin/$(BINARY) --format cast --visual "$(VISUAL)" > demo/cast.cast
	@cat $(DEMO_BAR_CSV) | ./bin/$(BINARY) --format cast-page --visual "$(VISUAL)" > demo/cast.html
	@echo "done — outputs in demo/"

demo-stdout: $(DEMO_BAR_CSV) | bin/$(BINARY)
	@echo "→ url"
	@cat $(DEMO_BAR_CSV) | ./bin/$(BINARY) --format url --visual "$(VISUAL)"
	@echo "→ vegalite (JSON)"
	@cat $(DEMO_BAR_CSV) | ./bin/$(BINARY) --format vegalite --visual "$(VISUAL)" | head -5
	@echo "→ ansi"
	@cat $(DEMO_BAR_CSV) | ./bin/$(BINARY) --format ansi --visual "$(VISUAL)"
	@echo "→ braille"
	@cat $(DEMO_BAR_CSV) | ./bin/$(BINARY) --format braille --visual "$(VISUAL)"
