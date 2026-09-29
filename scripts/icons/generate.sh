#!/bin/sh
# Regenerates every raster app icon from the SVG masters in packaging/icons/ (run after editing them; commit the
# results). Needs macOS (sips, iconutil), Google Chrome (renders the SVGs) and the desktop app's npm packages
# (desktop/node_modules, for `tauri icon`); the outputs are plain files, so builds and CI never need these tools.
#
#   packaging/icons/astraterm-1024.png   master raster (Windows resources of astraterm.exe are made from it)
#   desktop/src-tauri/icons/             the desktop app's icon set (tauri icon), with the macOS .icns drawn on the
#                                        macOS icon grid (packaging/icons/astraterm-macos.svg)
#   web/public/icons/*.png               web app manifest / apple-touch icons
set -eu
cd "$(dirname "$0")/../.."

chrome="${CHROME:-/Applications/Google Chrome.app/Contents/MacOS/Google Chrome}"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

render() { # svg out.png
	"$chrome" --headless=new --disable-gpu --hide-scrollbars --default-background-color=00000000 \
		--window-size=1024,1024 --screenshot="$2" "file://$PWD/$1" >/dev/null 2>&1
}
resize() { # in size out
	sips -z "$2" "$2" "$1" --out "$3" >/dev/null
}

render packaging/icons/astraterm.svg packaging/icons/astraterm-1024.png
render packaging/icons/astraterm-macos.svg "$tmp/macos-1024.png"

# Desktop app: the full set from the master, then the macOS-grid .icns.
(cd desktop && npx tauri icon ../packaging/icons/astraterm-1024.png -o src-tauri/icons >/dev/null)
rm -rf desktop/src-tauri/icons/android desktop/src-tauri/icons/ios
set_dir="$tmp/AstraTerm.iconset"
mkdir -p "$set_dir"
for s in 16 32 128 256 512; do
	resize "$tmp/macos-1024.png" "$s" "$set_dir/icon_${s}x${s}.png"
	resize "$tmp/macos-1024.png" $((s * 2)) "$set_dir/icon_${s}x${s}@2x.png"
done
iconutil -c icns "$set_dir" -o desktop/src-tauri/icons/icon.icns

# Web
mkdir -p web/public/icons
resize packaging/icons/astraterm-1024.png 512 web/public/icons/icon-512.png
resize packaging/icons/astraterm-1024.png 192 web/public/icons/icon-192.png
resize packaging/icons/astraterm-1024.png 180 web/public/icons/apple-touch-icon.png
resize packaging/icons/astraterm-1024.png 32 web/public/icons/icon-32.png

echo "icons: regenerated (packaging/icons, desktop/src-tauri/icons, web/public/icons)"
