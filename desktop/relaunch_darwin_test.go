//go:build darwin

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBundleWatchRelaunchesOnlyIntoASettledReplacement(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	orig := exeID{ino: 1, size: 100, mod: t0}
	partial := exeID{ino: 2, size: 40, mod: t0.Add(time.Minute)}
	done := exeID{ino: 2, size: 120, mod: t0.Add(2 * time.Minute)}

	w := &bundleWatch{orig: orig, prev: orig, prevOK: true}
	steps := []struct {
		name string
		cur  exeID
		ok   bool
		want bool
	}{
		{"unchanged", orig, true, false},
		{"moved aside mid-swap", exeID{}, false, false},
		{"first sight of a copy in progress", partial, true, false},
		{"copy still growing", done, true, false},
		{"settled replacement", done, true, true},
	}
	for _, s := range steps {
		if got := w.step(s.cur, s.ok); got != s.want {
			t.Fatalf("%s: step = %v, want %v", s.name, got, s.want)
		}
	}
}

func TestBundleWatchSkipsARejectedCopyUntilItChanges(t *testing.T) {
	orig := exeID{ino: 1}
	bad := exeID{ino: 2}
	w := &bundleWatch{orig: orig, prev: bad, prevOK: true, rejected: bad}
	if w.step(bad, true) {
		t.Fatal("relaunched into a copy that already failed verification")
	}
	fixed := exeID{ino: 3}
	w.step(fixed, true)
	if !w.step(fixed, true) {
		t.Fatal("a new copy after a rejected one should relaunch once it settles")
	}
}

func TestBundleWatchIgnoresTheOriginalComingBack(t *testing.T) {
	orig := exeID{ino: 1}
	w := &bundleWatch{orig: orig, prev: orig, prevOK: true}
	w.step(exeID{}, false) // briefly missing
	w.step(orig, true)
	if w.step(orig, true) {
		t.Fatal("relaunched into the same executable it is already running")
	}
}

// The relaunch script is run for real, with open stubbed, for a pid that has
// already exited — so the wait falls through and the call can be checked.
func TestRelaunchScriptOpensTheBundleAfterExit(t *testing.T) {
	root := t.TempDir()
	calls := filepath.Join(root, "calls")
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	stub := "#!/bin/bash\necho \"open $*\" >> " + shQuote(calls) + "\n"
	if err := os.WriteFile(filepath.Join(bin, "open"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	gone := exec.Command("/usr/bin/true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	app := filepath.Join(root, "App's $(dir)", "netscope.app")
	cmd := exec.Command("/bin/bash", "-c", relaunchScript(gone.Process.Pid, app))
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	b, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(b)); got != "open "+app {
		t.Fatalf("calls = %q, want %q", got, "open "+app)
	}
}
