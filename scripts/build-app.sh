#!/usr/bin/env bash
# Build the single distributable application bundle: dist/netscope.app
#
# This is the menu-bar app as the primary application. It bundles:
#   Contents/MacOS/netscope-bar      the menu-bar app (entry point)
#   Contents/MacOS/netscoped         the capture daemon (installed on first run)
#   Contents/Resources/Dashboard.app the Wails dashboard window
#   Contents/Resources/AppIcon.icns
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

# Resolve the wails CLI (same logic as the Makefile).
WAILS="$(command -v wails 2>/dev/null || echo "$(go env GOPATH)/bin/wails")"
[ -x "$WAILS" ] || { echo "wails not found; run: go install github.com/wailsapp/wails/v2/cmd/wails@latest" >&2; exit 1; }

echo "==> building dashboard (Wails)"
( cd desktop && "$WAILS" build -clean )

echo "==> assembling $APP"
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"

CGO_ENABLED=1 go build -ldflags "$LDFLAGS" -o "$APP/Contents/MacOS/netscope-bar" ./cmd/netscope-bar
CGO_ENABLED=1 go build -ldflags "$LDFLAGS" -o "$APP/Contents/MacOS/netscoped"     ./cmd/netscoped

cp assets/AppIcon.icns "$APP/Contents/Resources/AppIcon.icns"
cp -R desktop/build/bin/netscope.app "$APP/Contents/Resources/Dashboard.app"

cat > "$APP/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>CFBundleName</key><string>netscope</string>
    <key>CFBundleDisplayName</key><string>netscope</string>
    <key>CFBundleIdentifier</key><string>io.netscope.app</string>
    <key>CFBundleExecutable</key><string>netscope-bar</string>
    <key>CFBundleIconFile</key><string>AppIcon</string>
    <key>CFBundlePackageType</key><string>APPL</string>
    <key>CFBundleShortVersionString</key><string>${VERSION#v}</string>
    <key>CFBundleVersion</key><string>${VERSION#v}</string>
    <key>LSUIElement</key><true/>
    <key>LSMinimumSystemVersion</key><string>11.0</string>
    <key>NSHighResolutionCapable</key><true/>
</dict>
</plist>
PLIST

echo "==> signing (identity: $SIGN_ID)"
# Sign inside-out: nested dashboard first, then the outer app.
codesign --force --deep --sign "$SIGN_ID" "$APP/Contents/Resources/Dashboard.app" 2>/dev/null || true
codesign --force --sign "$SIGN_ID" "$APP/Contents/MacOS/netscoped"
codesign --force --deep --sign "$SIGN_ID" "$APP"

echo "==> zipping"
( cd dist && ditto -c -k --keepParent netscope.app "netscope-${VERSION}-app.zip" )
echo "done: $APP  and  dist/netscope-${VERSION}-app.zip"
