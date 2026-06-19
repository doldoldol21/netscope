#!/usr/bin/env bash
# Build the release artifacts into dist/: the single netscope.app (via
# build-app.sh) plus the CLI, an installer note, and a zip.
#
# Ad-hoc signed by default. For distribution set NETSCOPE_SIGN_ID (Developer ID)
# and NETSCOPE_NOTARY_PROFILE (an xcrun notarytool keychain profile) to sign +
# notarize. (Without them, install via install.sh — curl-fetched apps aren't
# quarantined, so there's no Gatekeeper prompt even unsigned.)
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
SIGN_ID="${NETSCOPE_SIGN_ID:--}"
DIST=dist

echo "==> building netscope.app"
NETSCOPE_SIGN_ID="$SIGN_ID" ./scripts/build-app.sh

echo "==> staging extras"
mkdir -p "$DIST/bin"
go build -ldflags "-s -w -X github.com/doldoldol21/netscope/internal/buildinfo.Version=${VERSION}" -o "$DIST/bin/netscope" ./cmd/netscope
cp install.sh "$DIST/install.sh"
cat > "$DIST/INSTALL.txt" <<'TXT'
netscope — install

Easiest (no Gatekeeper prompt): from the repo,
  curl -fsSL https://raw.githubusercontent.com/doldoldol21/netscope/main/install.sh | bash

Or manually: move netscope.app to /Applications, then run
  xattr -dr com.apple.quarantine /Applications/netscope.app   # only if you downloaded it in a browser
and launch it. The first run installs the capture helper (one admin prompt).
TXT

# Optional notarization (Developer ID builds only).
if [ "$SIGN_ID" != "-" ] && [ -n "${NETSCOPE_NOTARY_PROFILE:-}" ]; then
  echo "==> notarizing"
  ZIP="$DIST/netscope-notarize.zip"
  ditto -c -k --keepParent "$DIST/netscope.app" "$ZIP"
  xcrun notarytool submit "$ZIP" --keychain-profile "$NETSCOPE_NOTARY_PROFILE" --wait
  xcrun stapler staple "$DIST/netscope.app"
  rm -f "$ZIP"
fi

echo "done: $DIST/ (netscope.app, netscope-${VERSION}-app.zip, bin/netscope, install.sh)"
