#!/usr/bin/env bash
# Install (or remove) the netscoped LaunchDaemon so capture runs as root at boot.
#
#   sudo ./scripts/install.sh            install + load the daemon
#   sudo ./scripts/install.sh --uninstall  unload + remove the daemon
#
# Assumes the netscoped binary is already on PATH (Makefile `install` target
# copies it to /usr/local/bin first). Live capture needs root for /dev/bpf*.
set -euo pipefail

LABEL="io.netscope.daemon"
PLIST="/Library/LaunchDaemons/${LABEL}.plist"
BIN="$(command -v netscoped || echo /usr/local/bin/netscoped)"
LOG_DIR="/var/log/netscope"

if [[ "${1:-}" == "--uninstall" ]]; then
  echo "Unloading ${LABEL}…"
  launchctl bootout system "${PLIST}" 2>/dev/null || launchctl unload "${PLIST}" 2>/dev/null || true
  rm -f "${PLIST}"
  echo "Removed ${PLIST}"
  exit 0
fi

if [[ $EUID -ne 0 ]]; then
  echo "error: must run as root (use sudo)" >&2
  exit 1
fi

if [[ ! -x "${BIN}" ]]; then
  echo "error: netscoped not found at ${BIN}. Run 'make install' first." >&2
  exit 1
fi

mkdir -p "${LOG_DIR}"

echo "Writing ${PLIST}…"
cat > "${PLIST}" <<PLIST_EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>${LABEL}</string>
    <key>ProgramArguments</key>
    <array>
        <string>${BIN}</string>
        <string>--sock</string>
        <string>/var/run/netscope/netscoped.sock</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>${LOG_DIR}/netscoped.log</string>
    <key>StandardErrorPath</key>
    <string>${LOG_DIR}/netscoped.err.log</string>
</dict>
</plist>
PLIST_EOF

chown root:wheel "${PLIST}"
chmod 644 "${PLIST}"

echo "Loading ${LABEL}…"
launchctl bootout system "${PLIST}" 2>/dev/null || true
launchctl bootstrap system "${PLIST}" 2>/dev/null || launchctl load "${PLIST}"

echo "Done. API socket: /var/run/netscope/netscoped.sock"
echo "Launch the app:  netscope open   (or open the netscope.app bundle)"
echo "Logs: ${LOG_DIR}/netscoped.log"
