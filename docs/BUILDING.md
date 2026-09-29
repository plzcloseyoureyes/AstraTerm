# Building AstraTerm

AstraTerm is one Go executable with the React UI embedded (`go:embed` of `internal/webui/dist`). Building it means:
build the UI, precompress it, compile Go with `CGO_ENABLED=0`.

## Requirements

| Tool | Version | Used for |
|---|---|---|
| Go | 1.26 or newer (the `go` line of `go.mod`) | backend, cross-compilation, release tooling |
| Node.js + npm | 22 or newer (CI uses 24) | the web UI (`web/`) |
| make, a POSIX shell | any | the Makefile targets (Windows: Git Bash, MSYS2 or WSL; or run the underlying commands) |
| Docker | optional | smoke test and the test lab |

No C toolchain is needed: every dependency is pure Go (`CGO_ENABLED=0`). The only exception is `go test -race`,
which needs cgo.

## Everyday builds

```sh
make build          # npm ci (first time / lockfile change) → vite build → precompress → bin/astraterm
make build-go       # Go only, reusing the UI already in internal/webui/dist
./bin/astraterm version
```

`make web` does the UI part alone: it installs the locked npm dependencies when `web/package-lock.json` changed,
runs `vite build` into `internal/webui/dist`, then `go run ./internal/webui/precompress internal/webui/dist`.

Without a UI build the binary still compiles (the tracked `internal/webui/dist/.keep` keeps `go:embed` happy) and
serves a "frontend not built" page — handy for backend-only work together with `make dev-web`.

## How the UI is embedded

`internal/webui/precompress` replaces every compressible file under `dist/assets/` (JS, CSS, fonts other than woff2,
WASM, SVG, JSON) of at least 1 KiB by a gzip `-9` copy (`name.js` → `name.js.gz`) when that saves at least 10%.
Only the compressed copy is embedded. `internal/server/spa.go` serves it:

- to clients that accept gzip (every browser): the stored bytes with `Content-Encoding: gzip`;
- to anything else: decompressed on the fly;
- with `Cache-Control: public, max-age=31536000, immutable` for `/assets/*` (content-hashed names), `no-cache` for
  other top-level files, `no-store` for `index.html`, a strong `ETag` per representation, and
  `Vary: Accept-Encoding`.

This cuts the executable by about 20 MB (from ~75 MB to ~55 MB on darwin/arm64). gzip rather than brotli: browsers
only advertise `br` over HTTPS, while desktop mode serves plain HTTP on loopback; gzip also needs no decoder
dependency. The step is idempotent, and if it is skipped (e.g. a plain `npx vite build`) the uncompressed files are
embedded and served as before — run `make precompress` afterwards to shrink them.

## Build metadata

The Makefile and the release tooling set, through `-ldflags -X`:

| Variable | Value |
|---|---|
| `main.version` | `git describe --tags --always --dirty` (override: `make VERSION=v1.2.3 …`), `dev` without git |
| `main.commit` | `git rev-parse HEAD` |
| `main.date` | RFC 3339 UTC time from `SOURCE_DATE_EPOCH`, else the last commit's time |

`astraterm version` prints them with the Go version and platform. A plain `go build` falls back to Go's VCS stamp
(`vcs.revision`, `vcs.time`) when built inside a git checkout.

## Cross-compilation and release archives

```sh
make cross-check    # compile every package for every release platform (fast; no binaries)
make release        # make web + make dist
make dist           # archives from the UI already in internal/webui/dist
```

`make dist` runs `go run ./scripts/release/dist`, which builds `darwin`, `linux` and `windows` × `amd64`/`arm64` plus
`linux/arm/7` and `freebsd/amd64` (`PLATFORMS` in the Makefile) and writes to `dist/`:

```text
dist/
  build/<os>_<arch>/astraterm[.exe]                 the raw binaries
  astraterm_<version>_<os>_<arch>.tar.gz            macOS, Linux, FreeBSD
  astraterm_<version>_windows_<arch>.zip            Windows
  SHA256SUMS                                      sha256sum -c / shasum -a 256 -c compatible
```

Each archive contains one top-level folder `astraterm_<version>_<os>_<arch>/` with the `astraterm` executable,
`README.md`, `LICENSE` (once it exists), `CHANGELOG.md` and `THIRD_PARTY_NOTICES.md`. Windows executables carry the
icon, version information and manifest (`packaging/windows/winres.json`). The release workflow publishes exactly these
archives, next to the desktop app's installers ([DESKTOP.md](DESKTOP.md#building), built per platform).

To build a subset: `go run ./scripts/release/dist -version v1.2.3 -platforms linux/amd64,linux/arm64`. A 32-bit ARM
target is written `linux/arm/7` (GOARM=7).

## Reproducible builds

Release artifacts are reproducible: the same commit, Go version and UI build produce byte-identical binaries and
archives.

- `-trimpath`, `-buildvcs=false` and fixed `-ldflags` (no local paths or build-machine state in the binary);
- one timestamp — `SOURCE_DATE_EPOCH` or the commit time — for the embedded build date and every archive entry;
- archive entries sorted, owned by `root:root` (uid/gid 0), gzip headers without names or times;
- the UI build is deterministic for a given `package-lock.json` (content-hashed file names), and so is its
  precompression.

To check: build twice (or on two machines) with the same `SOURCE_DATE_EPOCH` and compare `dist/SHA256SUMS`.

```sh
SOURCE_DATE_EPOCH=$(git log -1 --format=%ct) make dist && cp dist/SHA256SUMS /tmp/a
SOURCE_DATE_EPOCH=$(git log -1 --format=%ct) make dist && diff /tmp/a dist/SHA256SUMS
```

## Third-party notices

`THIRD_PARTY_NOTICES.md` is generated by `scripts/release/notices` from the Go modules linked into the binary (for
every release platform) and the production npm packages in `web/package-lock.json`, with each component's license
files. It needs `web/node_modules` (run `npm ci` in `web/` first) and the Go module cache.

```sh
make notices        # regenerate (commit the result)
make notices-check  # fail when out of date
```

## Binary size

| Build (darwin/arm64, `-s -w`) | Size |
|---|---|
| without the UI | ~46.7 MB |
| with the UI, uncompressed | ~75.3 MB |
| with the UI, precompressed (default) | ~55.3 MB |
| release archive (tar.gz) | ~26 MB |

The largest Go contributors are the AWS SDK for the S3 file driver (~4.5 MB of code and data), the pure-Go SQLite
(~2 MB), the JavaScript engine for automation scripts (goja, ~1.8 MB) and `golang.org/x/text` collation tables used
by goja (~1.3 MB). Executable packers such as UPX are deliberately not used (slower start, higher memory use, antivirus
false positives).
