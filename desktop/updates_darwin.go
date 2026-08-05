//go:build darwin

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/doldoldol21/netscope/internal/alerts"
	"github.com/doldoldol21/netscope/internal/buildinfo"
	"github.com/doldoldol21/netscope/internal/update"
)

// updatePrefs persists the user's auto-update preference.
type updatePrefs struct {
	AutoCheck bool `json:"autoCheck"`
}

var (
	updMu       sync.Mutex
	updStatus   update.Status // most recent check result
	updPrefs    = updatePrefs{AutoCheck: true}
	updPrefPath string
)

const updateCheckInterval = 6 * time.Hour

// startUpdateLoop loads the saved preference and, when auto-check is on, polls
// GitHub for a newer release on launch and every few hours, posting a macOS
// notification the first time each new version appears.
func startUpdateLoop() {
	updPrefPath = filepath.Join(filepath.Dir(alerts.ConfigPath()), "updates.json")
	loadUpdatePrefs()
	go func() {
		time.Sleep(10 * time.Second) // let the app settle before any network call
		for {
			updMu.Lock()
			auto := updPrefs.AutoCheck
			updMu.Unlock()
			if auto {
				// Refresh the cached status for the in-app banner only. We
				// deliberately do NOT post a macOS notification — the popover/
				// dashboard banner is enough and an OS alert is intrusive.
				runUpdateCheck()
			}
			time.Sleep(updateCheckInterval)
		}
	}()
}

// runUpdateCheck queries GitHub and caches the result. ok is false on error
// (transient network failures keep the last good status).
func runUpdateCheck() (update.Status, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	st, err := update.Check(ctx, buildinfo.Repo, buildinfo.Version)
	if err != nil {
		return updStatusSnapshot(), false
	}
	updMu.Lock()
	updStatus = st
	updMu.Unlock()
	return st, true
}

func updStatusSnapshot() update.Status {
	updMu.Lock()
	defer updMu.Unlock()
	return updStatus
}

// updateStatusJSON is what the popover renders: the cached status plus the
// auto-check preference. Marshalled to a map so the JS gets a flat object.
func updateStatusJSON() map[string]any {
	st := updStatusSnapshot()
	updMu.Lock()
	auto := updPrefs.AutoCheck
	updMu.Unlock()
	return map[string]any{
		"current":         st.Current,
		"latest":          st.Latest,
		"updateAvailable": st.UpdateAvailable,
		"url":             st.URL,
		"checkedAt":       st.CheckedAt,
		"autoCheck":       auto,
	}
}

// setAutoCheck persists the auto-check toggle from the settings UI.
func setAutoCheck(on bool) {
	updMu.Lock()
	updPrefs.AutoCheck = on
	saveUpdatePrefsLocked()
	updMu.Unlock()
}

func loadUpdatePrefs() {
	b, err := os.ReadFile(updPrefPath)
	if err != nil {
		return // keep defaults (auto-check on)
	}
	var p updatePrefs
	if json.Unmarshal(b, &p) == nil {
		updMu.Lock()
		updPrefs = p
		updMu.Unlock()
	}
}

// saveUpdatePrefsLocked writes prefs; callers must hold updMu.
func saveUpdatePrefsLocked() {
	if updPrefPath == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(updPrefPath), 0o755)
	if b, err := json.MarshalIndent(updPrefs, "", "  "); err == nil {
		_ = os.WriteFile(updPrefPath, b, 0o644)
	}
}

// performUpdate downloads the latest app bundle and swaps it in. Because we
// can't replace our own running bundle in-place, it hands off to a detached
// shell script that waits for this process to exit, replaces the bundle, and
// relaunches — then we quit. Returns an error only if the handoff can't start;
// once the script is launched, the swap happens after we exit.
func performUpdate() error {
	st := updStatusSnapshot()
	if !st.UpdateAvailable || st.AssetURL == "" {
		return errors.New("no update available")
	}
	appPath, err := installedAppPath()
	if err != nil {
		return err
	}

	tmp, err := os.MkdirTemp("", "netscope-update-")
	if err != nil {
		return err
	}
	zipPath := filepath.Join(tmp, "netscope.zip")
	if err := download(st.AssetURL, zipPath, maxUpdateBytes); err != nil {
		return fmt.Errorf("download: %w", err)
	}
	if err := verifyDownload(st, zipPath, tmp); err != nil {
		return fmt.Errorf("verify: %w", err)
	}

	out := filepath.Join(tmp, "out")
	if err := exec.Command("/usr/bin/ditto", "-x", "-k", zipPath, out).Run(); err != nil {
		return fmt.Errorf("unpack: %w", err)
	}
	newApp := findBundle(out)
	if newApp == "" {
		return errors.New("archive did not contain netscope.app")
	}
	_ = exec.Command("/usr/bin/xattr", "-cr", newApp).Run() // strip any quarantine

	// A detached swapper: wait for us to exit, then replace the bundle. Move the
	// old bundle aside first and only delete it once the new one is in place —
	// so a failed mv (cross-volume, perms, SIP) never leaves the user with no
	// app. On any failure, restore the backup and relaunch it.
	script := fmt.Sprintf(`#!/bin/bash
pid=%[1]d
app=%[2]q
new=%[3]q
tmp=%[4]q
bak="$app.bak.$$"
while kill -0 "$pid" 2>/dev/null; do sleep 0.3; done
if ! mv "$app" "$bak" 2>/dev/null; then bak=""; fi   # may already be gone
if mv "$new" "$app" 2>/dev/null; then
  xattr -cr "$app" 2>/dev/null || true
  [ -n "$bak" ] && rm -rf "$bak"
  # Restart the capture helper so it runs the just-installed daemon binary.
  # KeepAlive would otherwise keep the OLD daemon process alive until reboot.
  # Ask the daemon to restart over its unix socket — since it already runs as
  # root, no admin prompt is needed. After it exits, launchd's KeepAlive
  # restarts it with the just-swapped binary.
  curl -s --unix-socket /var/run/netscope/netscoped.sock -X POST http://x/api/restart 2>/dev/null || true
else
  # restore the original so the user is never left without an app
  [ -n "$bak" ] && mv "$bak" "$app" 2>/dev/null
fi
open "$app"
rm -rf "$tmp"
`, os.Getpid(), appPath, newApp, tmp)
	scriptPath := filepath.Join(tmp, "swap.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		return err
	}
	cmd := exec.Command("/bin/bash", scriptPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // survive our exit
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start updater: %w", err)
	}
	// Hand off: quit so the swapper can replace the bundle and relaunch.
	go func() {
		time.Sleep(300 * time.Millisecond)
		os.Exit(0)
	}()
	return nil
}

// installedAppPath derives the .app bundle path from the running executable
// (…/netscope.app/Contents/MacOS/netscope → …/netscope.app).
func installedAppPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	app := filepath.Dir(filepath.Dir(filepath.Dir(exe))) // up out of Contents/MacOS
	if !strings.HasSuffix(app, ".app") {
		return "", fmt.Errorf("not running from an .app bundle (%s)", exe)
	}
	return app, nil
}

// findBundle returns the first netscope.app under root, or "".
func findBundle(root string) string {
	var found string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && d.Name() == "netscope.app" {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// Download caps: the app zip is ~15MB today, so 200MB leaves generous headroom
// while still bounding what a bad response can write to disk; checksums.txt is
// a few lines.
const (
	maxUpdateBytes   = 200 << 20
	maxChecksumBytes = 256 << 10
)

// allowedUpdateHost restricts update downloads to GitHub itself and its asset
// CDN (release downloads redirect to *.githubusercontent.com). Anything else —
// even if it appears in an API response — is refused.
func allowedUpdateHost(host string) bool {
	host = strings.ToLower(host)
	return host == "github.com" || host == "api.github.com" ||
		strings.HasSuffix(host, ".githubusercontent.com")
}

// download fetches url to dest, refusing non-GitHub hosts (including on
// redirects) and responses larger than maxBytes.
func download(url, dest string, maxBytes int64) error {
	client := &http.Client{
		Timeout: 5 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if !allowedUpdateHost(req.URL.Hostname()) {
				return fmt.Errorf("redirect to untrusted host %q", req.URL.Hostname())
			}
			return nil
		},
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if req.URL.Scheme != "https" || !allowedUpdateHost(req.URL.Hostname()) {
		return fmt.Errorf("untrusted download URL %q", url)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http %s", resp.Status)
	}
	if resp.ContentLength > maxBytes {
		return fmt.Errorf("response too large (%d bytes)", resp.ContentLength)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return err
	}
	if n > maxBytes {
		return fmt.Errorf("response exceeds %d bytes", maxBytes)
	}
	return nil
}

// verifyDownload checks the downloaded zip against the release's checksums.txt.
// Fail-closed: releases publish checksums from CI, so a missing file or entry
// means something is wrong with the release — refuse to install it.
func verifyDownload(st update.Status, zipPath, tmp string) error {
	if st.ChecksumURL == "" {
		return errors.New("release has no checksums.txt")
	}
	sumsPath := filepath.Join(tmp, "checksums.txt")
	if err := download(st.ChecksumURL, sumsPath, maxChecksumBytes); err != nil {
		return fmt.Errorf("fetch checksums.txt: %w", err)
	}
	sums, err := os.ReadFile(sumsPath)
	if err != nil {
		return err
	}
	// The zip was saved under a temp name; look its digest up by the asset's
	// published filename (the URL path's last segment, query stripped).
	name := filepath.Base(st.AssetURL)
	if u, err := neturl.Parse(st.AssetURL); err == nil {
		name = filepath.Base(u.Path)
	}
	want, ok := update.FindChecksum(string(sums), name)
	if !ok {
		return fmt.Errorf("checksums.txt has no entry for %s", name)
	}
	return update.VerifyFileSHA256(zipPath, want)
}
