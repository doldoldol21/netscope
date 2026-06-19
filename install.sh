#!/usr/bin/env bash
#
# netscope installer.
#
#   curl -fsSL https://raw.githubusercontent.com/doldoldol21/netscope/main/install.sh | bash
#
# Downloads the latest netscope.app from GitHub Releases and installs it to
# /Applications. Because the app is fetched with curl (not a browser), macOS
# does NOT quarantine it — so there is no Gatekeeper "unverified developer"
# prompt. No Homebrew, no admin needed for the install itself.
set -euo pipefail

REPO="doldoldol21/netscope"
APPDIR="/Applications/netscope.app"

say() { printf '\033[1;34m==>\033[0m %s\n' "$1"; }

[ "$(uname -s)" = "Darwin" ] || { echo "netscope is macOS only." >&2; exit 1; }

say "finding the latest release"
TAG="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
        | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)"
[ -n "$TAG" ] || { echo "could not find a release for ${REPO}" >&2; exit 1; }
URL="https://github.com/${REPO}/releases/download/${TAG}/netscope-${TAG}-app.zip"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

say "downloading netscope ${TAG}"
curl -fsSL "$URL" -o "$TMP/netscope.zip"

say "unpacking"
ditto -x -k "$TMP/netscope.zip" "$TMP/out"
APP="$(/usr/bin/find "$TMP/out" -maxdepth 2 -name 'netscope.app' -type d | head -1)"
[ -n "$APP" ] || { echo "archive did not contain netscope.app" >&2; exit 1; }
# Belt and suspenders: strip any quarantine/extended attrs.
xattr -cr "$APP" 2>/dev/null || true

say "installing to ${APPDIR}"
if [ -w /Applications ]; then
  rm -rf "$APPDIR"; mv "$APP" "$APPDIR"
else
  sudo rm -rf "$APPDIR"; sudo mv "$APP" "$APPDIR"
fi

say "done — launching netscope"
echo "    netscope lives in your menu bar. The first launch asks for your admin"
echo "    password once to install the capture helper; after that it just works."
open "$APPDIR" || true
