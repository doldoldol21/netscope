// Package helperinstall lets the running root daemon replace its own installed
// copy with a newer release binary the app hands it — without an admin prompt.
//
// The root-owned copy under /Library/PrivilegedHelperTools exists so that
// nothing running as the user can change what launchd starts as root (the
// bundle in /Applications is user-writable). Copying "whatever is in the
// bundle" would hand that protection straight back. So the daemon does not
// trust the file it is given: it fetches the checksums.txt GitHub published
// for the claimed version over an allowlisted HTTPS path and installs the
// bytes only if their SHA-256 is the one the release lists for netscoped, and
// only if that version is newer than the one running. A local process can
// offer any file it likes; unless it is byte-for-byte the release, nothing
// happens. The version gate also means nothing can talk the daemon back down
// to an older, buggier build.
package helperinstall

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/doldoldol21/netscope/internal/update"
)

// Request names a candidate daemon binary and the release it claims to be.
type Request struct {
	Path    string `json:"path"`    // absolute path to the candidate netscoped
	Version string `json:"version"` // release tag it claims to be, "vX.Y.Z"
}

// Installer holds what the daemon needs to vet and install a successor. Every
// field that touches the machine or the network is a value so tests can point
// it at a temp dir and a canned checksums.txt.
type Installer struct {
	Repo       string // "owner/name" on GitHub
	Current    string // version of the running daemon (buildinfo.Version)
	HelperPath string // root-owned copy launchd runs
	PlistPath  string // launchd service definition; its absence means "not installed"

	// Fetch downloads url (https, GitHub hosts only) and returns at most max
	// bytes. The daemon wires the same allowlisted client the update check uses.
	Fetch func(url string, max int64) ([]byte, error)
	// EUID reports the effective uid; only root may install. Overridable so
	// the install path itself is testable.
	EUID func() int
}

// Errors the API maps to status codes. Everything else is a 500-ish surprise.
var (
	ErrNotRoot    = errors.New("the daemon is not running as root")
	ErrNotManaged = errors.New("no launchd service is installed for the helper")
	ErrRejected   = errors.New("candidate rejected")
)

// Where install.sh and the app's privileged install put the helper. The label
// is the launchd service name; daemonctl derives the same paths from it.
const (
	Label             = "io.netscope.daemon"
	DefaultHelperPath = "/Library/PrivilegedHelperTools/" + Label
	DefaultPlistPath  = "/Library/LaunchDaemons/" + Label + ".plist"
)

const (
	// maxCandidateBytes bounds what the daemon will read into memory. The
	// daemon is ~14MB; anything past 64MB is not it.
	maxCandidateBytes = 64 << 20
	maxChecksumBytes  = 256 << 10
	// ChecksumName is the entry in checksums.txt that vouches for the daemon
	// binary inside the app bundle (scripts/package.sh writes it).
	ChecksumName = "netscoped"
)

var tagRe = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

// Install vets req and, if it passes, atomically replaces HelperPath with the
// candidate's bytes. The caller restarts the daemon afterwards; this function
// never does, so a rejected request leaves the running daemon untouched.
func (in *Installer) Install(req Request) error {
	if in.EUID != nil && in.EUID() != 0 {
		return ErrNotRoot
	}
	// Refresh only, never first install: a launchd service and a helper copy
	// must already be in place. An old install whose plist still points at
	// the bundle has no copy, and needs the privileged install to rewrite the
	// plist as well — swapping a file in here would leave it half migrated.
	if _, err := os.Stat(in.PlistPath); err != nil {
		return ErrNotManaged
	}
	if _, err := os.Stat(in.HelperPath); err != nil {
		return ErrNotManaged
	}
	// Version gate first: it needs no IO and refuses dev builds on either side
	// ("dev" parses as nothing, so Newer is false) and any downgrade or repeat.
	if !tagRe.MatchString(req.Version) {
		return fmt.Errorf("%w: %q is not a release tag", ErrRejected, req.Version)
	}
	if !update.Newer(req.Version, in.Current) {
		return fmt.Errorf("%w: %s is not newer than the running %s", ErrRejected, req.Version, in.Current)
	}
	if !filepath.IsAbs(req.Path) {
		return fmt.Errorf("%w: path must be absolute", ErrRejected)
	}

	// Read the candidate once, into memory. Everything below — the hash, the
	// bytes written — comes from this copy, so swapping the file on disk
	// between the check and the install changes nothing.
	candidate, err := readCapped(req.Path, maxCandidateBytes)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRejected, err)
	}

	sums, err := in.Fetch(fmt.Sprintf("https://github.com/%s/releases/download/%s/%s",
		in.Repo, req.Version, update.ChecksumAsset), maxChecksumBytes)
	if err != nil {
		return fmt.Errorf("fetch %s for %s: %w", update.ChecksumAsset, req.Version, err)
	}
	want, ok := update.FindChecksum(string(sums), ChecksumName)
	if !ok {
		return fmt.Errorf("%w: release %s publishes no checksum for %s", ErrRejected, req.Version, ChecksumName)
	}
	sum := sha256.Sum256(candidate)
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(want)) != 1 {
		return fmt.Errorf("%w: not the %s GitHub published for %s", ErrRejected, ChecksumName, req.Version)
	}

	return replaceFile(in.HelperPath, candidate)
}

// readCapped reads a regular file of at most max bytes.
func readCapped(path string, max int64) ([]byte, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if st.Size() > max {
		return nil, fmt.Errorf("%s is %d bytes, over the %d limit", path, st.Size(), max)
	}
	return os.ReadFile(path)
}

// replaceFile writes data next to dst and renames it into place, so launchd
// only ever sees the old complete binary or the new complete one. The
// directory is the root-owned one the installer created; the temp file is
// born 0755 root:wheel because we are root.
func replaceFile(dst string, data []byte) error {
	dir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dir, filepath.Base(dst)+".new-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		cleanup()
		return err
	}
	return nil
}
