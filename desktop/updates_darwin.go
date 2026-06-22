//go:build darwin

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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

// updatePrefs persists the user's auto-update preference and the last version we
// notified about (so a new release is announced once, not on every check).
type updatePrefs struct {
	AutoCheck       bool   `json:"autoCheck"`
	NotifiedVersion string `json:"notifiedVersion"`
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
				if st, ok := runUpdateCheck(); ok {
					maybeNotifyUpdate(st)
				}
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

// maybeNotifyUpdate posts one notification per newly-seen available version.
func maybeNotifyUpdate(st update.Status) {
	if !st.UpdateAvailable || st.Latest == "" {
		return
	}
	updMu.Lock()
	already := updPrefs.NotifiedVersion == st.Latest
	if !already {
		updPrefs.NotifiedVersion = st.Latest
		saveUpdatePrefsLocked()
	}
	updMu.Unlock()
	if already {
		return
	}
	notify("netscope update available",
		fmt.Sprintf("Version %s is available. Open netscope to update.", st.Latest))
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
	if err := download(st.AssetURL, zipPath); err != nil {
		return fmt.Errorf("download: %w", err)
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

	// A detached swapper: wait for us to exit, replace the bundle, relaunch.
	script := fmt.Sprintf(`#!/bin/bash
pid=%d
while kill -0 "$pid" 2>/dev/null; do sleep 0.3; done
rm -rf %q
mv %q %q
xattr -cr %q 2>/dev/null || true
open %q
rm -rf %q
`, os.Getpid(), appPath, newApp, appPath, appPath, appPath, tmp)
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

// download fetches url to dest.
func download(url, dest string) error {
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http %s", resp.Status)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}
