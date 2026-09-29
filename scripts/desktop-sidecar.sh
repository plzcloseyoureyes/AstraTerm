#!/bin/sh
# Builds the astraterm server as the desktop app's sidecar for one Rust target triple (default: this machine), as
# desktop/src-tauri/binaries/astraterm-<triple>[.exe] — the name Tauri's externalBin expects. It embeds the web UI
# already built into internal/webui/dist (run `make web` first).
#
#   scripts/desktop-sidecar.sh [target-triple] [version]
set -eu
cd "$(dirname "$0")/.."

triple="${1:-$(rustc -vV | sed -n 's/^host: //p')}"
version="${2:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
case "$triple" in
	aarch64-apple-darwin) goos=darwin goarch=arm64 ;;
	x86_64-apple-darwin) goos=darwin goarch=amd64 ;;
	x86_64-pc-windows-msvc) goos=windows goarch=amd64 ;;
	aarch64-pc-windows-msvc) goos=windows goarch=arm64 ;;
	x86_64-unknown-linux-gnu) goos=linux goarch=amd64 ;;
	aarch64-unknown-linux-gnu) goos=linux goarch=arm64 ;;
	*) echo "desktop-sidecar: unsupported target $triple" >&2; exit 1 ;;
esac
ext=""
[ "$goos" = windows ] && ext=".exe"
test -f internal/webui/dist/index.html || { echo "desktop-sidecar: internal/webui/dist has no index.html - run make web first" >&2; exit 1; }

out="desktop/src-tauri/binaries/astraterm-$triple$ext"
mkdir -p "$(dirname "$out")"
CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch go build -trimpath -buildvcs=false \
	-ldflags "-s -w -X main.version=$version -X main.commit=$(git rev-parse --verify -q HEAD || true)" \
	-o "$out" ./cmd/astraterm
echo "desktop-sidecar: $out ($version)"
