#!/usr/bin/env bash
# Build the single distributable application: dist/netscope.app
#
# It is the menu-bar app (a native NSStatusItem + a frameless Wails popover that
# also hosts the full dashboard at /dashboard.html) with the capture daemon
# bundled inside:
#   Contents/MacOS/netscope     menu-bar app (entry point)
#   Contents/MacOS/netscoped    capture daemon (installed on first run)
#
# Ad-hoc signed by default; set NETSCOPE_SIGN_ID for a Developer ID build.
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
SIGN_ID="${NETSCOPE_SIGN_ID:--}"
LDFLAGS="-s -w -X github.com/doldoldol21/netscope/internal/buildinfo.Version=${VERSION}"
APP="dist/netscope.app"

echo "==> building netscope.app ${VERSION}"
./scripts/gen-icons.sh

WAILS="$(command -v wails 2>/dev/null || echo "$(go env GOPATH)/bin/wails")"
[ -x "$WAILS" ] || { echo "wails not found; run: go install github.com/wailsapp/wails/v2/cmd/wails@latest" >&2; exit 1; }

echo "==> building app (Wails)"
( cd desktop && "$WAILS" build -clean )

echo "==> assembling $APP"
mkdir -p dist
rm -rf "$APP"
cp -R desktop/build/bin/netscope.app "$APP"
CGO_ENABLED=1 go build -ldflags "$LDFLAGS" -o "$APP/Contents/MacOS/netscoped" ./cmd/netscoped

echo "==> signing (identity: $SIGN_ID)"
codesign --force --sign "$SIGN_ID" "$APP/Contents/MacOS/netscoped"
codesign --force --deep --sign "$SIGN_ID" "$APP"

echo "==> zipping"
( cd dist && ditto -c -k --keepParent netscope.app "netscope-${VERSION}-app.zip" )
echo "done: $APP  and  dist/netscope-${VERSION}-app.zip"
