// Package buildinfo exposes the build version, shared by every netscope binary.
// The value is injected at build time with:
//
//	-ldflags "-X github.com/doldoldol21/netscope/internal/buildinfo.Version=v1.2.3"
//
// Unversioned (go run / plain go build) reports "dev".
package buildinfo

// Version is the release version (e.g. "v0.1.0"), or "dev" when not injected.
var Version = "dev"

// Repo is the GitHub repository used for update checks.
const Repo = "doldoldol21/netscope"
