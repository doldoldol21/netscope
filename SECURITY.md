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
- The dashboard window is fed by a **loopback-only (127.0.0.1)** server; nothing
  is exposed off-host.
- All data stays on the machine; nothing is uploaded.

Particularly valuable reports: anything that lets a non-root local process read
the socket or escalate via the daemon, packet-parsing memory-safety issues in
the decoder, or a path where capture data could leave the host.

## Supported versions

Fixes target the latest release. Please reproduce on the newest version before
reporting.
