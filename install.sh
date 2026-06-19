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

# Install the capture helper now, with a single terminal sudo prompt — so the
# app launches without any GUI password dialog. (If this is skipped, the app
# falls back to prompting on first launch.)
install_helper() {
  local label="io.netscope.daemon"
  local plist="/Library/LaunchDaemons/${label}.plist"
  local exe="${APPDIR}/Contents/MacOS/netscoped"
  local sock="/var/run/netscope/netscoped.sock"
  say "installing the capture helper (needs admin once)"
  sudo bash -s "$label" "$plist" "$exe" "$sock" <<'SUDO'
set -e
label="$1"; plist="$2"; exe="$3"; sock="$4"
mkdir -p /var/run/netscope
cat > "$plist" <<PL
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>${label}</string>
  <key>ProgramArguments</key><array><string>${exe}</string><string>--sock</string><string>${sock}</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardErrorPath</key><string>/var/log/netscope.log</string>
  <key>StandardOutPath</key><string>/var/log/netscope.log</string>
</dict></plist>
PL
chmod 644 "$plist"
launchctl bootstrap system "$plist" 2>/dev/null || launchctl load "$plist" 2>/dev/null || true
SUDO
}
install_helper || echo "    (skipped — the app will set this up on first launch)"

say "done — launching netscope"
echo "    netscope is now in your menu bar. Click it for live traffic."
open "$APPDIR" || true
