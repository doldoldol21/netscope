#!/usr/bin/env bash
# Regenerate app icons from assets/app-icon.svg.
# Produces:
#   assets/AppIcon.icns          (menu-bar bundle + packaging)
#   desktop/build/appicon.png    (1024px; Wails builds its .icns from this)
#
# Uses only macOS built-ins: qlmanage (SVG raster), sips (resize), iconutil.
set -euo pipefail
cd "$(dirname "$0")/.."

SVG=assets/app-icon.svg
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

echo "rasterizing $SVG → 1024px…"
qlmanage -t -s 1024 -o "$WORK" "$SVG" >/dev/null 2>&1
SRC="$WORK/$(basename "$SVG").png"
[ -f "$SRC" ] || { echo "error: qlmanage produced no PNG" >&2; exit 1; }

# Wails reads this 1024px PNG and generates its own iconfile.icns at build.
mkdir -p desktop/build
cp "$SRC" desktop/build/appicon.png
echo "wrote desktop/build/appicon.png"

# Build a proper .icns for the menu-bar bundle / packaging.
ICONSET="$WORK/AppIcon.iconset"
mkdir -p "$ICONSET"
gen() { sips -z "$2" "$2" "$SRC" --out "$ICONSET/icon_$1.png" >/dev/null; }
gen 16x16        16
gen 16x16@2x     32
gen 32x32        32
gen 32x32@2x     64
gen 128x128      128
gen 128x128@2x   256
gen 256x256      256
gen 256x256@2x   512
gen 512x512      512
gen 512x512@2x   1024
iconutil -c icns "$ICONSET" -o assets/AppIcon.icns
echo "wrote assets/AppIcon.icns"
