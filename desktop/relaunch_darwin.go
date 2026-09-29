//go:build darwin

package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// A bundle replaced underneath the running app — `brew upgrade`, install.sh,
// a drag into /Applications — changes nothing about the process: it keeps
// running the build it started with, and its update check keeps comparing
// that build against the latest release. The popover then asks the user to
// "update" to the version that is already installed, and pressing it
// downloads and swaps in the same release a second time.
//
// So the app watches its own executable. When the file at that path is no
// longer the one it was started from, it relaunches into it — the same thing
// the in-app update does after its swap.

const bundleWatchTick = 5 * time.Second

// exeID is what identifies one copy of the executable on disk. A replaced
// bundle is a new directory tree, so the inode alone would do; size and mtime
// also catch an in-place overwrite.
type exeID struct {
	ino  uint64
	size int64
	mod  time.Time
}

func statExe(path string) (exeID, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return exeID{}, false
	}
	id := exeID{size: fi.Size(), mod: fi.ModTime()}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		id.ino = st.Ino
	}
	return id, true
}

// bundleWatch decides, one tick at a time, when a replaced executable is ready
// to relaunch into. It is the pure part of the watcher, kept apart for tests.
type bundleWatch struct {
	orig     exeID // what this process was started from
	prev     exeID // what the last tick saw
	prevOK   bool
	rejected exeID // a copy that failed verification; not retried until it changes
}

// step reports whether cur is a replacement worth relaunching into: different
// from the original, present, and unchanged since the previous tick. That last
// condition waits out a copy still in progress (a Finder drag writes the
// executable over several ticks); brew and install.sh move a finished bundle
// into place, so for them it costs one tick.
func (w *bundleWatch) step(cur exeID, ok bool) bool {
	stable := ok && w.prevOK && cur == w.prev
	w.prev, w.prevOK = cur, ok
	return stable && cur != w.orig && cur != w.rejected
}

var bundleWatchOnce sync.Once

// startBundleWatch relaunches the app when its bundle has been replaced. It
// does nothing when the app is not running from a bundle (`wails dev`).
func startBundleWatch() {
	bundleWatchOnce.Do(func() {
		app, err := installedAppPath()
		if err != nil {
			return
		}
		exe := filepath.Join(app, "Contents", "MacOS", "netscope")
		orig, ok := statExe(exe)
		if !ok {
			return
		}
		w := &bundleWatch{orig: orig, prev: orig, prevOK: true}
		go func() {
			for {
				time.Sleep(bundleWatchTick)
				guard("bundle watch", func() {
					cur, ok := statExe(exe)
					if !w.step(cur, ok) {
						return
					}
					// The in-app update is mid-handoff: its swapper relaunches.
					if updateRunning.Load() {
						return
					}
					// Only relaunch into a bundle that would run: a copy that is
					// incomplete or edited fails here, and the old build keeps
					// going rather than handing the menu bar to nothing.
					if err := verifyBundleSignature(app); err != nil {
						log.Printf("bundle watch: replaced bundle did not verify, staying on this build: %v", err)
						w.rejected = cur
						return
					}
					relaunchIntoBundle(app)
				})
			}
		}()
	})
}

// relaunchIntoBundle hands off to a detached script that waits for this
// process to exit and opens the bundle again, then exits. It waits while the
// popover is open, so the menu never vanishes from under the pointer; the next
// tick tries again.
func relaunchIntoBundle(app string) {
	winMu.Lock()
	if winVisible {
		winMu.Unlock()
		return
	}
	// Held until exit, so a click can't open the popover in the meantime.
	cmd := exec.Command("/bin/bash", "-c", relaunchScript(os.Getpid(), app))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // survive our exit
	if err := cmd.Start(); err != nil {
		winMu.Unlock()
		log.Printf("bundle watch: could not start the relaunch: %v", err)
		return
	}
	log.Printf("bundle watch: %s was replaced; relaunching into it", app)
	os.Exit(0)
}

// relaunchScript waits for pid to exit (the single-instance lock would turn a
// second copy away while this one runs) and opens app.
func relaunchScript(pid int, app string) string {
	return fmt.Sprintf(`while kill -0 %d 2>/dev/null; do sleep 0.2; done
open %s
`, pid, shQuote(app))
}
