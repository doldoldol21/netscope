#!/usr/bin/env bash
# Build everything and assemble a distributable into dist/.
#
#   ./scripts/package.sh
#
# By default the bundles are AD-HOC signed (codesign -s -) so they run on THIS
# machine and via right-click→Open elsewhere. For real distribution set:
#
#   NETSCOPE_SIGN_ID="Developer ID Application: Your Name (TEAMID)" \
#   NETSCOPE_NOTARY_PROFILE="netscope-notary"  \   # xcrun notarytool keychain profile
#   ./scripts/package.sh
#
# (A Developer ID cert + an Apple Developer account are required for signing and
# notarization; without them Gatekeeper warns on other Macs.)
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
SIGN_ID="${NETSCOPE_SIGN_ID:--}"   # "-" = ad-hoc
DIST=dist

echo "==> building netscope $VERSION"
make icons
make build app bar-app

echo "==> staging $DIST/"
rm -rf "$DIST"
mkdir -p "$DIST/bin"
cp -R desktop/build/bin/netscope.app "$DIST/netscope.app"
cp -R bin/netscope-bar.app "$DIST/netscope-bar.app"
cp bin/netscoped bin/netscope "$DIST/bin/"
cp scripts/install.sh "$DIST/"

cat > "$DIST/INSTALL.txt" <<'TXT'
netscope — install

1) Move both apps to /Applications:
     netscope.app        (dashboard window)
     netscope-bar.app    (menu-bar app; use its "Launch at Login" toggle)

2) Install the capture daemon (needs admin once):
     sudo ./bin/netscoped --sock /var/run/netscope/netscoped.sock   # or:
     sudo ./install.sh                                              # launchd, starts at boot

3) Launch netscope-bar.app — the menu bar shows live ↓↑; "Open Dashboard…" opens the window.

Capture needs root (/dev/bpf*); the daemon runs as root via launchd and hands the
API socket to your user. The apps themselves are unprivileged.
TXT

echo "==> signing (identity: $SIGN_ID)"
sign() { codesign --force --deep --options runtime --sign "$SIGN_ID" "$1" 2>/dev/null || codesign --force --deep --sign "$SIGN_ID" "$1"; }
sign "$DIST/netscope.app"
sign "$DIST/netscope-bar.app"
codesign --force --sign "$SIGN_ID" "$DIST/bin/netscoped"
codesign --force --sign "$SIGN_ID" "$DIST/bin/netscope"

# Optional notarization (Developer ID builds only).
if [ "$SIGN_ID" != "-" ] && [ -n "${NETSCOPE_NOTARY_PROFILE:-}" ]; then
  echo "==> notarizing"
  ZIP="$DIST/netscope-notarize.zip"
  ditto -c -k --keepParent "$DIST/netscope.app" "$ZIP"
  xcrun notarytool submit "$ZIP" --keychain-profile "$NETSCOPE_NOTARY_PROFILE" --wait
  xcrun stapler staple "$DIST/netscope.app"
  xcrun stapler staple "$DIST/netscope-bar.app" || true
  rm -f "$ZIP"
fi

echo "==> zipping"
( cd "$DIST" && zip -qry "netscope-$VERSION.zip" netscope.app netscope-bar.app bin install.sh INSTALL.txt )
echo "done: $DIST/netscope-$VERSION.zip"
