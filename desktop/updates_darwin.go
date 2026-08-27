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
	"github.com/doldoldol21/netscope/internal/daemonctl"
	"github.com/doldoldol21/netscope/internal/i18n"
	"github.com/doldoldol21/netscope/internal/update"
)

// updatePrefs persists the user's auto-update preference.
type updatePrefs struct {
	AutoCheck bool `json:"autoCheck"`
	// DismissedHelperDigest is the stale-helper digest the user waved off, so the
	// popover banner doesn't nag about the same one every launch. A new staleness
	// (different digest) surfaces again; a successful refresh clears it.
	DismissedHelperDigest string `json:"dismissedHelperDigest,omitempty"`
	// NotifiedVersion is the release we've already announced. Persisted so a
	// restart doesn't re-announce the same one: the notification exists to tell
	// you something you don't know yet, and repeating it is the nagging this
	// deliberately avoids.
	NotifiedVersion string `json:"notifiedVersion,omitempty"`
}

var (
	updMu       sync.Mutex
	updStatus   update.Status // most recent check result
	updPrefs    = updatePrefs{AutoCheck: true}
	updPrefPath string

	// Outcome of the most recent check attempt. Kept separate from updStatus,
	// which holds the last *successful* result: the UI has to be able to say
	// "could not check" while still showing what it knew before.
	updLastErr   string
	updCheckedOK bool // a check has succeeded at least once this run
)

const (
	updateCheckInterval = 6 * time.Hour
	// Retry cadence after a failed check. A laptop is routinely offline at
	// launch, so failing back to the six-hour interval would mean knowing
	// nothing about updates for most of a day.
	updateRetryMin = 1 * time.Minute
	updateRetryMax = 30 * time.Minute
	// How often the loop wakes to ask whether a check is due. Deciding against
	// the wall clock on a short tick keeps the schedule honest across suspend,
	// instead of a long Sleep expiring at an arbitrary point after wake.
	updateLoopTick = 30 * time.Second
)

// nextRetryDelay backs a failing check off exponentially between updateRetryMin
// and updateRetryMax. failures counts consecutive failures, starting at 1.
func nextRetryDelay(failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	d := updateRetryMin
	for i := 1; i < failures && d < updateRetryMax; i++ {
		d *= 2
	}
	if d > updateRetryMax {
		return updateRetryMax
	}
	return d
}

// checkDue reports whether a check should run now: never checked yet, or enough
// wall-clock time has passed since the last attempt. Comparing against the wall
// clock (rather than sleeping for the interval) means a machine that was
// suspended past its due time checks promptly on wake.
func checkDue(now, last time.Time, wait time.Duration) bool {
	if last.IsZero() {
		return true
	}
	return !now.Before(last.Add(wait))
}

// startUpdateLoop loads the saved preference and, when auto-check is on, polls
// GitHub for a newer release on launch and every few hours.
//
// The loop wakes often and decides whether a check is due by looking at the wall
// clock, rather than sleeping for the whole interval. That keeps the schedule
// meaningful on a laptop that is suspended most of the day, and lets a failed
// check retry on a short backoff instead of disappearing for six hours.
func startUpdateLoop() {
	updPrefPath = filepath.Join(filepath.Dir(alerts.ConfigPath()), "updates.json")
	loadUpdatePrefs()
	go func() {
		time.Sleep(10 * time.Second) // let the app settle before any network call
		var last time.Time
		var failures int
		for {
			updMu.Lock()
			auto := updPrefs.AutoCheck
			updMu.Unlock()

			wait := updateCheckInterval
			if failures > 0 {
				wait = nextRetryDelay(failures)
			}
			if auto && checkDue(time.Now(), last, wait) {
				last = time.Now()
				if st, ok := runUpdateCheck(); ok {
					failures = 0
					announceUpdate(st)
				} else {
					failures++
				}
			}
			time.Sleep(updateLoopTick)
		}
	}()
}

// announceUpdate posts a notification the first time a given release is seen,
// and never again for that one.
//
// A menu-bar app is mostly not open, so the popover banner — which is where
// this used to end — only reaches someone who already happened to look. That
// left the app knowing about a new version and saying so nowhere the user would
// see it. Announcing once per version is the middle ground the previous
// all-or-nothing reasoning missed: an alert every check would be intolerable,
// but staying silent means never being told at all.
func announceUpdate(st update.Status) {
	if !noteUpdateSeen(st) {
		return
	}
	postNotification(i18n.T("update.available.title", st.Latest),
		i18n.T("update.available.body", st.Current))
}

// noteUpdateSeen records a release as one the user has been made aware of, and
// reports whether that was news. A manual "check now" calls this without
// announcing: the user is looking straight at the result, so telling them again
// hours later would be the nagging this is built to avoid. Being shown counts
// as being told, however they came to see it.
func noteUpdateSeen(st update.Status) bool {
	if !st.UpdateAvailable || st.Latest == "" {
		return false
	}
	updMu.Lock()
	defer updMu.Unlock()
	if updPrefs.NotifiedVersion == st.Latest {
		return false
	}
	updPrefs.NotifiedVersion = st.Latest
	saveUpdatePrefsLocked()
	return true
}

// postNotification is the seam tests replace, so exercising the announce-once
// rule doesn't fire real banners at whoever is running the suite.
var postNotification = notify

// updateIsAvailable reports whether a newer release is waiting. Read by the
// menu-bar readout, which shows a marker for as long as that is true — the
// notification can be missed or dismissed, this is what is still there tomorrow.
func updateIsAvailable() bool {
	updMu.Lock()
	defer updMu.Unlock()
	return updStatus.UpdateAvailable
}

// runUpdateCheck queries GitHub and caches the result. ok is false on error
// (transient network failures keep the last good status).
func runUpdateCheck() (update.Status, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	st, err := update.Check(ctx, buildinfo.Repo, buildinfo.Version)
	if err != nil {
		// Keep the last good status — a transient failure shouldn't erase what
		// we already know — but record that this attempt failed, so the UI can
		// say so instead of implying the version was confirmed current.
		updMu.Lock()
		updLastErr = err.Error()
		updMu.Unlock()
		return updStatusSnapshot(), false
	}
	updMu.Lock()
	updStatus = st
	updLastErr = ""
	updCheckedOK = true
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
	// One acquisition for all of it: snapshotting the status and the flags
	// separately lets a check landing in between pair a stale status with
	// checked=true for a render.
	updMu.Lock()
	st := updStatus
	auto := updPrefs.AutoCheck
	lastErr, checkedOK := updLastErr, updCheckedOK
	updMu.Unlock()
	return map[string]any{
		"current":         st.Current,
		"latest":          st.Latest,
		"updateAvailable": st.UpdateAvailable,
		"url":             st.URL,
		"checkedAt":       st.CheckedAt,
		"autoCheck":       auto,
		// checkFailed/checked let the UI distinguish "confirmed current" from
		// "never managed to ask". Without them a machine that has never reached
		// GitHub is told it is up to date.
		"checkFailed": lastErr != "",
		"checkError":  lastErr,
		"checked":     checkedOK,
	}
}

// setAutoCheck persists the auto-check toggle from the settings UI.
func setAutoCheck(on bool) {
	updMu.Lock()
	updPrefs.AutoCheck = on
	saveUpdatePrefsLocked()
	updMu.Unlock()
}

// helperStatusJSON is what the popover renders for the capture helper: whether
// the root-owned daemon copy is stale (the app updated but launchd still runs the
// old build), and whether that staleness is one the user hasn't already waved
// off. needsUpdate drives the banner; digest lets the JS echo back which one a
// dismissal refers to.
func helperStatusJSON() map[string]any {
	stale, digest := daemonctl.HelperStale()
	updMu.Lock()
	dismissed := updPrefs.DismissedHelperDigest
	updMu.Unlock()
	return map[string]any{
		"stale":       stale,
		"digest":      digest,
		"needsUpdate": stale && digest != dismissed,
	}
}

// dismissHelper records that the user waved off this stale-helper digest, so the
// banner stays hidden until a different build makes it stale again.
func dismissHelper(digest string) {
	updMu.Lock()
	updPrefs.DismissedHelperDigest = digest
	saveUpdatePrefsLocked()
	updMu.Unlock()
}

// performHelperUpdate re-installs the root-owned daemon copy from the bundle (one
// admin prompt) at the user's request, then clears any dismissal so a later
// staleness surfaces again.
func performHelperUpdate(client *http.Client, sock string) error {
	if err := daemonctl.RefreshHelper(client, sock); err != nil {
		return err
	}
	updMu.Lock()
	updPrefs.DismissedHelperDigest = ""
	saveUpdatePrefsLocked()
	updMu.Unlock()
	return nil
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
app=%[2]s
new=%[3]s
tmp=%[4]s
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
`, os.Getpid(), shQuote(appPath), shQuote(newApp), shQuote(tmp))
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

// shQuote renders s as a single-quoted shell word. Single quotes are the only
// shell quoting with no escapes inside, so nothing in the path can be expanded;
// an embedded quote is closed, escaped, and reopened. fmt's %q is NOT a
// substitute here — it produces Go syntax, which leaves $ and backticks intact,
// and both are live inside the double quotes this script used to use.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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
