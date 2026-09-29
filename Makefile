VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --verify -q HEAD 2>/dev/null)
# Reproducible build date: SOURCE_DATE_EPOCH when set, else the last commit's time (empty without commits).
SOURCE_DATE_EPOCH ?= $(shell git log -1 --format=%ct 2>/dev/null)
BUILD_DATE := $(if $(SOURCE_DATE_EPOCH),$(shell date -u -d @$(SOURCE_DATE_EPOCH) +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -r $(SOURCE_DATE_EPOCH) +%Y-%m-%dT%H:%M:%SZ))
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(BUILD_DATE)
GOFLAGS := -trimpath
BIN := bin/astraterm
# Release matrix; keep in sync with scripts/release/dist and scripts/release/notices.
PLATFORMS := darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 linux/arm/7 windows/amd64 windows/arm64 freebsd/amd64
empty :=
space := $(empty) $(empty)
comma := ,

.PHONY: all web precompress build build-go dev dev-backend dev-web test go-test test-race test-web cross-check vet fmt fmt-check typecheck lint lint-ui check smoke flash-audit release dist desktop icons notices notices-check clean

all: build

# Exact dependency versions from the lockfile, reinstalled only when package.json / package-lock.json change
# (npm ci never rewrites the lockfile, unlike npm install).
# Reinstall only when the lockfile changes (script-only edits to package.json don't need it). `npm ci` wipes
# node_modules first, so it is used only for a fresh checkout; otherwise the non-destructive `npm install` runs.
web/node_modules/.package-lock.json: web/package-lock.json
	cd web && if [ -d node_modules ]; then npm install --no-audit --no-fund; else npm ci --no-audit --no-fund; fi
	@touch $@

web: web/node_modules/.package-lock.json
	cd web && npm run build
	go run ./internal/webui/precompress internal/webui/dist
	@touch internal/webui/dist/.keep

# Replace the large compressible assets of internal/webui/dist by gzip copies (the binary embeds only those; `make web`
# runs it). Needed only after building the frontend some other way, e.g. `cd web && npx vite build`.
precompress:
	go run ./internal/webui/precompress internal/webui/dist

# bin/astraterm: the server / CLI with the UI embedded.
build: web
	$(MAKE) build-go

# Backend only (assumes the frontend was already built into internal/webui/dist)
build-go:
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/astraterm

dev-backend:
	go run ./cmd/astraterm --dev --no-open --data-dir ./.astraterm-data

dev-web:
	cd web && npm run dev

dev:
	@echo "Run in two terminals: 'make dev-backend' and 'make dev-web' (http://localhost:5173)"

vet:
	go vet ./...

test: go-test

go-test:
	CGO_ENABLED=0 go test ./...

test-race:
	go test -race ./...

# Compile every package for the release platforms without producing binaries (catches OS-specific build breaks).
cross-check:
	@for p in $(PLATFORMS); do \
		os=$$(echo $$p | cut -d/ -f1); arch=$$(echo $$p | cut -d/ -f2); arm=$$(echo $$p | cut -d/ -f3); \
		echo "==> $$p"; CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch GOARM=$$arm go build ./... || exit 1; \
	done

GOSRC = $(shell find cmd internal scripts -name '*.go')

fmt:
	gofmt -w $(GOSRC)

fmt-check:
	@out=$$(gofmt -l $(GOSRC)); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

typecheck:
	cd web && npm run typecheck

# oxlint + the UI guardrail (loading-state primitives only; see docs/UX.md "Loading states").
lint: lint-ui
	cd web && npm run lint

lint-ui:
	node scripts/lint-ui.mjs

# Frontend unit tests (node --test over the pure TypeScript modules of lib, files, editor and termtransfer).
test-web:
	cd web && npm test

check: fmt-check vet go-test typecheck lint test-web

# End-to-end smoke test of the built binary (needs Docker for the SSH target; see scripts/smoke/README.md).
smoke: build
	node scripts/smoke/smoke.mjs

# Measures loading / boot flashes of the built UI in headless Chrome (docs/UX.md "Loading states"); needs Chrome.
flash-audit: build
	node scripts/flash-audit.mjs --strict
	node scripts/flash-audit.mjs --strict --latency 250

# Release archives for every platform (tar.gz; zip for Windows) with README, LICENSE, CHANGELOG and third-party
# notices, plus SHA256SUMS, in dist/ (docs/RELEASING.md). Reproducible for a given commit, Go version and UI build.
release: web
	$(MAKE) dist

# Same as `release`, reusing the frontend already built into internal/webui/dist.
dist:
	SOURCE_DATE_EPOCH=$(SOURCE_DATE_EPOCH) go run ./scripts/release/dist -version $(VERSION) -out dist \
		-platforms $(subst $(space),$(comma),$(PLATFORMS))

# The desktop app (desktop/, Tauri) for this machine: the server as its sidecar, then the native bundles (.app/.dmg,
# .msi/setup .exe, .deb/.rpm/.AppImage) in desktop/src-tauri/target/release/bundle/. Needs Rust (docs/DESKTOP.md).
desktop: web
	scripts/desktop-sidecar.sh
	cd desktop && if [ -d node_modules ]; then npm install --no-audit --no-fund; else npm ci --no-audit --no-fund; fi
	cd desktop && npx tauri build

# Regenerate the app icons (packaging/, web/public/icons/) from the SVG masters in packaging/icons/. macOS + Chrome only;
# the results are committed, so builds never need this.
icons:
	scripts/icons/generate.sh

# THIRD_PARTY_NOTICES.md from the Go modules in the binary and the npm packages in the UI bundle (needs web/node_modules).
notices:
	go run ./scripts/release/notices

notices-check:
	go run ./scripts/release/notices -check

clean:
	rm -rf bin dist
