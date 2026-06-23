# Contributing to netscope

Thanks for your interest! netscope is a per-app network monitor for macOS — a
root capture daemon (`netscoped`) plus a menu-bar app and CLI, all in Go.

## Ways to contribute

- **Bugs / ideas** — open an [issue](https://github.com/doldoldol21/netscope/issues)
  (templates provided). Include your macOS version and, for capture bugs, what
  interface you're on.
- **Code** — fork, branch, open a pull request. See below.

## Development setup

Requires **Go 1.25+** and **macOS** (Xcode Command Line Tools for the C
toolchain — capture uses cgo/libpcap, the menu bar uses cgo/Cocoa).

```sh
git clone https://github.com/<you>/netscope
cd netscope
make build      # bin/netscoped, bin/netscope
make app        # dist/netscope.app (Wails + cgo)
make demo       # synthetic traffic + menu-bar app — no root needed
```

You usually **don't need root** to develop: `make demo` runs a synthetic-traffic
daemon over a user socket and launches the UI. For real capture, run
`sudo bin/netscoped` in one terminal and the app/CLI in another.

## Before you open a PR

Run the same checks CI runs — all must pass:

```sh
make fmt        # gofmt -w (no diff after)
make vet        # go vet ./...
make test       # unit + offline integration (no root needed)
```

- Keep changes focused; one logical change per PR.
- Match the surrounding style (comment density, naming, idioms).
- Add/adjust tests for behavior changes — the decode/engine/storage/alerts/
  dnscache packages are pure and unit-tested; prefer testing logic there.
- Update `README.md` if you change user-facing behavior or flags.

## Commit messages

Conventional, imperative, lower-case summary:

```
feat: choose the capture interface from the popover
fix: Cmd-W closes the dashboard
perf: pause live streams when the popover is hidden
docs: …   test: …   refactor: …   chore: …
```

## Architecture (orientation)

- `cmd/netscoped` — root daemon: capture → attribute → aggregate → `/api` over a
  unix socket (no network port).
- `cmd/netscope` — CLI client (terminal views, `export`).
- `desktop/` — menu-bar app: native `NSStatusItem` + Wails popover (cgo) and a
  standalone dashboard `NSWindow`/`WKWebView`.
- `internal/` — `capture` (pcap + decode, incl. DNS/SNI), `resolver`
  (socket→PID via libproc), `dnscache`, `engine` (aggregation), `storage`
  (SQLite), `api`, `alerts`, `update`, `webui` (embedded dashboard assets).
- `pkg/types` — shared types.

Phase 1 targets macOS; non-darwin builds compile (capture is stubbed) so the
pure packages stay portable.

## License

By contributing, you agree your contributions are licensed under the
[MIT License](LICENSE).
