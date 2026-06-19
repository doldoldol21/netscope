<div align="center">

<img src="assets/icon.png" width="120" alt="netscope" />

# netscope

**See which apps are using your network — live, right from the menu bar.**

A per-app network traffic monitor for macOS, in the spirit of RunCat / iStat:
always-on, glanceable, and useful for everyone. Everything runs locally — no
traffic contents are read, no data leaves your Mac, no open network port.

[![release](https://img.shields.io/github/v/release/doldoldol21/netscope?color=4493f8)](https://github.com/doldoldol21/netscope/releases)
[![license](https://img.shields.io/badge/license-MIT-bc8cff)](LICENSE)
![platform](https://img.shields.io/badge/macOS-11%2B-555)
![lang](https://img.shields.io/badge/built%20with-Go-00ADD8)

<img src="assets/shot-menubar.png" alt="netscope in the menu bar" />

*live download / upload, always in your menu bar*

</div>

## ⚡ Quick Start

```sh
curl -fsSL https://raw.githubusercontent.com/doldoldol21/netscope/main/install.sh | bash
```

That's it — `netscope.app` lands in **/Applications** and launches. **No
Gatekeeper warning** (curl-fetched apps aren't quarantined), no Homebrew, no
Apple account. The installer asks for your admin password **once, right there in
the terminal**, to set up the capture helper — so the app itself opens with no
pop-up dialogs, live in your menu bar and starting at boot.

Click the menu-bar item for the top apps, **Open Dashboard…** for the full
window, or toggle **Launch at Login**.

> **One app, fully self-managing.** Capture needs root (`/dev/bpf*`), so the app
> installs a small root daemon (the one admin prompt) that serves data over a
> local **Unix socket** — no TCP port is ever opened. Prefer the terminal?
> `brew install doldoldol21/netscope/netscope-cli` builds the `netscope` /
> `netscoped` / `netscope-bar` binaries from source.

## What you get

- **Menu bar** — live ↓↑ throughput + a dropdown of the top apps (RunCat-style).
- **Dashboard** — a native window: throughput chart, today/week rankings, and
  per-domain breakdown with neutral categories (cloud, cdn, media, ai, …).
- **CLI** — `netscope`, `netscope apps --range week`, `netscope domains` …
- **Private by design** — HTTPS stays encrypted; netscope only counts *bytes per
  process* and maps IPs to domains by watching your own DNS replies.

## How it works

```
┌── netscoped (root, launchd) ──────────────────────┐
│  libpcap capture ─► decode (IP/TCP/UDP + DNS)      │
│        │                    │                       │
│        ▼                    ▼                       │
│  resolver (libproc)    dnscache (IP→domain)        │
│   socket→PID→app       + reverse DNS               │
│        │                    │                       │
│        └──────► engine (attribute + aggregate)      │
│                     │            │                  │
│                     ▼            ▼                  │
│                 SQLite       live snapshot          │
│                     └─────┬────────┘                │
│                           ▼                          │
│                   /api over UNIX SOCKET             │
└───────────────────────────┬────────────────────────┘
              unix:///var/run/netscope/netscoped.sock  (0600, owned by you)
         ┌───────────────────┴───────────────────┐
         ▼                                        ▼
  netscope.app  (one app)                   netscope CLI
  menu bar + dashboard; installs            terminal viewer
  and manages the daemon for you
```

### Why a daemon, wrapped in one app

Packet capture needs root (`/dev/bpf*`); a GUI must run unprivileged in your
login session. netscope keeps these separate under the hood but ships them as a
**single `netscope.app`**:

- **`netscoped`** (bundled inside the app) — runs as root under launchd,
  captures and aggregates, and serves `/api` on a unix socket. No TCP port is
  opened, so no browser or other user's process can reach your traffic data;
  access is gated by the socket file's ownership (chowned to you, mode `0600`).
- **`netscope.app`** — a menu-bar app (Go + systray, no dock icon) that shows
  the live rate and top apps, **installs the daemon on first run**, and opens a
  nested Wails **dashboard window** (`internal/webui`, vanilla JS/CSS, no Node
  build) that reverse-proxies `/api` (incl. the live SSE stream) to the socket.
- **`netscope`** — a CLI that reads the same socket for terminal views.

## Install

- **App (recommended):** the [Quick Start](#-quick-start) one-liner. Installs
  `netscope.app` to /Applications with no Gatekeeper prompt.
- **CLI / Homebrew:** `brew install doldoldol21/netscope/netscope-cli` — builds
  the `netscoped`/`netscope`/`netscope-bar` binaries from source (also no
  Gatekeeper, since it's compiled locally).
- **Direct download:** grab `netscope.app` from the
  [latest release](https://github.com/doldoldol21/netscope/releases). If you
  download it in a browser, clear the quarantine flag once:
  `xattr -dr com.apple.quarantine /Applications/netscope.app`.

> Want a notarized, zero-step download instead? That needs an Apple Developer
> account; the signing/notarization hooks are wired in `scripts/package.sh`
> (`NETSCOPE_SIGN_ID` / `NETSCOPE_NOTARY_PROFILE`).

### Uninstall

```sh
# stop & remove the capture daemon
sudo launchctl bootout system/io.netscope.daemon 2>/dev/null
sudo rm -f /Library/LaunchDaemons/io.netscope.daemon.plist
# remove the app and its data
rm -rf /Applications/netscope.app
rm -f ~/Library/LaunchAgents/io.netscope.bar.plist     # "Launch at Login", if enabled
sudo rm -rf /var/db/netscope /var/run/netscope
```

### From source

Requires Go 1.24+ and macOS (Xcode Command Line Tools for the C toolchain).

```sh
git clone https://github.com/doldoldol21/netscope
cd netscope
make build          # bin/netscoped, bin/netscope, bin/netscope-bar
```

Build the binaries (daemon, CLI, menu-bar app) and the dashboard window:

```sh
make build                            # bin/netscoped, bin/netscope, bin/netscope-bar
go install github.com/wailsapp/wails/v2/cmd/wails@latest   # once (make app auto-installs if missing)
make app                              # desktop/build/bin/netscope.app (dashboard window)
make bar-app                          # bin/netscope-bar.app (menu-bar app, no dock icon)
```

Install the always-on capture daemon (launchd, runs as root) and the binaries:

```sh
make install        # copies to /usr/local/bin + loads the LaunchDaemon
make uninstall      # remove daemon + binaries
```

The menu-bar app has a **Launch at Login** toggle (a per-user LaunchAgent — no
admin needed). Use it to keep netscope always on.

### Packaging a release

```sh
make package        # builds + bundles both .app + binaries into dist/, ad-hoc signed
```

`dist/` gets `netscope.app`, `netscope-bar.app`, the `netscoped`/`netscope`
binaries, an installer and a zip. For distribution to other Macs, set
`NETSCOPE_SIGN_ID` (Developer ID) and `NETSCOPE_NOTARY_PROFILE` to sign +
notarize — otherwise Gatekeeper warns (right-click → Open works). Icons are
generated from `assets/app-icon.svg` via `make icons`.

The app needs `netscoped` running. After `make install` the daemon listens on
`/var/run/netscope/netscoped.sock`; the app connects there automatically.
Override the socket path with the `NETSCOPE_SOCK` env var.

## Usage

### Live capture (needs root)

```sh
sudo bin/netscoped           # capture + serve the API on the unix socket
bin/netscope open            # launch the native app
```

Packet capture reads `/dev/bpf*`, which requires root. Running under launchd
(`make install`) is the recommended way to avoid `sudo`. The daemon chowns the
socket to you so the (unprivileged) app and CLI can connect.

### CLI viewer (no root — talks to the running daemon)

```sh
bin/netscope                 # live terminal table (default: "top")
bin/netscope apps --range week
bin/netscope domains --range today
bin/netscope open            # launch the native app
```

`--range` is one of `hour | today | day | week`. Point at a non-default socket
with `--sock /path/to.sock`.

### Demo / development (no root)

One command — runs a synthetic-traffic daemon and launches the menu-bar app,
showing realistic named apps (Claude, ChatGPT, Safari, Spotify, …). Look at the
menu bar; use the menu's "Open Dashboard…" for the window:

```sh
make demo            # Ctrl-C (or the menu's Quit) to stop; uses a user-writable dev socket
```

For dashboard UI hot-reload, split it across two terminals (both use the dev
socket automatically, no env var needed):

```sh
make demo-daemon     # terminal 1: synthetic daemon
make app-dev         # terminal 2: dashboard with live-reload
```

To exercise the real capture pipeline from a file (note: apps show as
`unknown` because the original sockets no longer exist — domains still resolve):

```sh
go run tools/gensample.go testdata/sample.pcap
make run-pcap PCAP=testdata/sample.pcap
```

## Daemon flags

```
--iface       interface to capture (default: auto-detect)
--pcap        replay a pcap file instead of live capture (no root)
--demo        serve synthetic named-app traffic (no root; for UI/dev)
--sock        unix socket to serve the API on (default /var/run/netscope/netscoped.sock,
              or $NETSCOPE_SOCK)
--db          SQLite path (default /var/db/netscope/netscope.db as root)
--no-store    run in memory only, no persistence
--bucket      aggregation/flush granularity (default 10s)
--retention   how long to keep samples (default 720h; 0 = forever)
--live-window keep apps/domains in the live "session" view if active within this
              window (default 30m; 0 = whole session)
--print       also print top apps to stdout periodically
```

## API (over the unix socket)

The daemon serves JSON on the unix socket only — no static files, no TCP. Probe
it with `curl --unix-socket /var/run/netscope/netscoped.sock http://x/api/health`.

| Endpoint | Description |
|---|---|
| `GET /api/snapshot` | current live snapshot (rates + session apps/domains) |
| `GET /api/live` | Server-Sent Events stream of snapshots (1/s) |
| `GET /api/apps?range=today` | per-app totals over a range |
| `GET /api/domains?range=today` | per-domain totals over a range |
| `GET /api/summary?range=today` | totals, app/domain counts, top app + top domain |
| `GET /api/timeseries?range=day&step=60` | rx/tx time series |
| `GET /api/health` | liveness + persistence status |

## How attribution works

- **bytes → process**: macOS has no "4-tuple → PID" syscall, so the resolver
  periodically enumerates every process' socket file descriptors via `libproc`
  (`proc_pidinfo` / `proc_pidfdinfo`) and builds a reverse index keyed by
  protocol + local port + remote endpoint. Lookups rescan on demand (rate
  limited) when a brand-new connection misses the cache.
- **IP → domain**: the decoder sniffs DNS responses and caches each answer's
  `A`/`AAAA` IP against the queried name. IPs seen without a prior DNS answer
  (connections older than netscope, or encrypted DNS) get a background reverse-DNS
  (PTR) lookup so they still show a hostname where one exists.
- **category**: domains are matched (by registrable suffix) into neutral
  groups (cloud / cdn / media / social / ai / tracking) shown as a chip.

## Limitations

- macOS only in Phase 1 (Linux is a Phase 2 goal). Non-darwin builds compile but
  capture is stubbed out.
- HTTPS payloads are never decrypted — you see domain + byte counts only.
- Offline replay shows apps as `unknown` because the original sockets no longer
  exist on the host; live capture resolves real app names.
- App identity is keyed by executable/bundle name, so multiple processes of the
  same app are merged in rankings.

## Development

```sh
make test       # unit + offline integration tests (no root needed)
make cover
make vet
make fmt
```

## License

MIT — see [LICENSE](LICENSE).
