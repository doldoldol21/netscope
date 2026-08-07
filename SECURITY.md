# Security Policy

netscope captures packets via a small root daemon (`netscoped`) and serves data
over a local unix socket. We take its security seriously.

## Reporting a vulnerability

**Please do not open a public issue for security problems.**

Use GitHub's private reporting:
[**Report a vulnerability**](https://github.com/doldoldol21/netscope/security/advisories/new)
(Security ▸ Advisories on the repo). If that's unavailable, open a minimal issue
asking for a private contact and we'll follow up.

Please include:
- what the issue is and its impact,
- steps to reproduce or a proof of concept,
- affected version (dashboard footer / release tag) and macOS version.

We aim to acknowledge within a few days and to ship a fix promptly, crediting
you in the release notes unless you prefer otherwise.

## Scope & design notes

netscope is local-only by design, which bounds the attack surface:

- The capture daemon **opens no network port** — it serves `/api` only on a
  unix socket at `/var/run/netscope/netscoped.sock`, `chown`ed to the installing
  user with mode `0600`. No remote host can reach your traffic data.
- HTTPS payloads are never decrypted; netscope reads only packet headers, DNS
  answers, and the cleartext TLS SNI.
- The dashboard window is fed by a **loopback-only (127.0.0.1)** server on a
  random port. Loopback keeps remote hosts out; it does *not* isolate other
  local processes, so that server additionally requires a **per-launch token**
  (generated at startup, kept only in the app's memory, handed to the WebView in
  its URL and then held in an origin-scoped cookie). Requests without it get
  401, and requests whose `Origin`/`Referer`/`Sec-Fetch-Site` indicate another
  site get 403 — so a page in your browser can't drive the API either.
- All captured data stays on the machine; none of it is uploaded.
- netscope does make two outbound requests of its own, neither carrying capture
  data: reverse-DNS (PTR) lookups for IPs that DNS and TLS SNI left unnamed, and
  update checks against `api.github.com`. The PTR lookups do disclose which
  addresses the machine contacted to whoever runs its resolver. `--no-revdns`
  and `--no-update-check` turn them off; note the app performs its own update
  check, disabled separately in its settings.

Particularly valuable reports: anything that lets a non-root local process read
the socket or escalate via the daemon, packet-parsing memory-safety issues in
the decoder, or a path where capture data could leave the host.

## Known limitations

- **The app bundle is writable by the user it runs as.** `/Applications` is
  group-writable by admin accounts and a bundle under `~/Applications` belongs
  to the user outright. The capture daemon is copied to
  `/Library/PrivilegedHelperTools` before it is registered, so what launchd runs
  as root cannot be replaced afterwards — but a process already running as
  that user can tamper with the bundled daemon *before* the admin prompt
  appears, and
  the copy would preserve it. netscope's builds are ad-hoc signed, which seals
  contents but attests to no identity, so there is nothing stable to verify the
  source against. Closing this needs a Developer ID signature plus a
  designated-requirement check on the source, which is what `SMJobBless` /
  `SMAppService` pin on your behalf.

## Supported versions

Fixes target the latest release. Please reproduce on the newest version before
reporting.
