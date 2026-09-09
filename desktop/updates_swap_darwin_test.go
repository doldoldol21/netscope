//go:build darwin

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// swapFixture lays out an "installed" bundle, a freshly unpacked one, and a
// PATH whose open/curl are stubs that record their calls — so the swap script
// can be run for real without launching anything or restarting the daemon.
type swapFixture struct {
	root, app, newApp, tmp, log, calls string
	env                                []string
}

func newSwapFixture(t *testing.T) *swapFixture {
	t.Helper()
	root := t.TempDir()
	f := &swapFixture{
		root:   root,
		app:    filepath.Join(root, "Applications", "netscope.app"),
		newApp: filepath.Join(root, "tmp", "out", "netscope.app"),
		tmp:    filepath.Join(root, "tmp"),
		log:    filepath.Join(root, "update.log"),
		calls:  filepath.Join(root, "calls"),
	}
	for _, p := range []string{f.app, f.newApp} {
		if err := os.MkdirAll(filepath.Join(p, "Contents"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(f.app, "Contents", "version"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.newApp, "Contents", "version"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"open", "curl"} {
		stub := "#!/bin/bash\necho \"" + name + " $*\" >> " + shQuote(f.calls) + "\n"
		if err := os.WriteFile(filepath.Join(bin, name), []byte(stub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	return f
}

// run executes the swap script for a pid that has already exited, so the
// wait-for-exit loop falls straight through.
func (f *swapFixture) run(t *testing.T) error {
	t.Helper()
	gone := exec.Command("/usr/bin/true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(f.root, "swap.sh")
	if err := os.WriteFile(script, []byte(swapScript(gone.Process.Pid, f.app, f.newApp, f.tmp, f.log)), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/bash", script)
	cmd.Env = f.env
	return cmd.Run()
}

func (f *swapFixture) installedVersion(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.app, "Contents", "version"))
	if err != nil {
		t.Fatalf("installed bundle unreadable: %v", err)
	}
	return string(b)
}

func (f *swapFixture) callsMade(t *testing.T) string {
	t.Helper()
	b, _ := os.ReadFile(f.calls)
	return string(b)
}

func TestSwapScriptReplacesTheBundleAndRelaunchesIt(t *testing.T) {
	f := newSwapFixture(t)
	if err := f.run(t); err != nil {
		t.Fatalf("swap script failed: %v", err)
	}
	if got := f.installedVersion(t); got != "new" {
		t.Fatalf("installed bundle = %q, want the new one", got)
	}
	if leftovers, _ := filepath.Glob(f.app + ".bak.*"); len(leftovers) != 0 {
		t.Errorf("backup not removed after a successful swap: %v", leftovers)
	}
	if _, err := os.Stat(f.tmp); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("temp dir survived a successful swap")
	}
	calls := f.callsMade(t)
	if !strings.Contains(calls, "open "+f.app) {
		t.Errorf("new bundle was not relaunched; calls:\n%s", calls)
	}
	if !strings.Contains(calls, "curl ") {
		t.Errorf("daemon was not asked to restart; calls:\n%s", calls)
	}
}

// The regression: a bundle that is present but won't move aside used to be
// treated as "already gone", so the new bundle was moved *inside* the old one
// and the old build relaunched as though the update had succeeded.
func TestSwapScriptLeavesAnImmovableBundleAlone(t *testing.T) {
	f := newSwapFixture(t)
	// Renaming needs write permission on the parent, not on the bundle.
	parent := filepath.Dir(f.app)
	if err := os.Chmod(parent, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })

	err := f.run(t)
	if err == nil {
		t.Fatal("swap script reported success with the installed bundle immovable")
	}
	if got := f.installedVersion(t); got != "old" {
		t.Fatalf("installed bundle = %q, want the old one left untouched", got)
	}
	if _, err := os.Stat(filepath.Join(f.app, "netscope.app")); err == nil {
		t.Fatal("new bundle was nested inside the installed one")
	}
	if leftovers, _ := filepath.Glob(f.app + ".bak.*"); len(leftovers) != 0 {
		t.Errorf("a backup appeared even though nothing moved: %v", leftovers)
	}
	calls := f.callsMade(t)
	if !strings.Contains(calls, "open "+f.app) {
		t.Errorf("old bundle was not relaunched; calls:\n%s", calls)
	}
	if strings.Contains(calls, "curl ") {
		t.Errorf("daemon was asked to restart for a swap that didn't happen; calls:\n%s", calls)
	}
	logged, _ := os.ReadFile(f.log)
	if !strings.Contains(string(logged), "swap failed") {
		t.Errorf("failure was not written to the update log; got %q", logged)
	}
}

func TestSwapScriptInstallsWhenNothingIsThereToMove(t *testing.T) {
	f := newSwapFixture(t)
	if err := os.RemoveAll(f.app); err != nil {
		t.Fatal(err)
	}
	if err := f.run(t); err != nil {
		t.Fatalf("swap script failed with no installed bundle: %v", err)
	}
	if got := f.installedVersion(t); got != "new" {
		t.Fatalf("installed bundle = %q, want the new one", got)
	}
}

func TestUpdateErrorJSONNamesTheStage(t *testing.T) {
	got := updateErrorJSON(failAt("verify", errors.New("sha256 mismatch")))
	if got["stage"] != "verify" || got["detail"] != "sha256 mismatch" {
		t.Fatalf("updateErrorJSON = %v", got)
	}
	// An error nobody tagged still reaches the UI, just without a stage.
	got = updateErrorJSON(errors.New("boom"))
	if got["stage"] != "unknown" || got["detail"] != "boom" {
		t.Fatalf("untagged updateErrorJSON = %v", got)
	}
}

// The installed app on this Mac is the closest thing to a release bundle the
// suite can reach; what matters is that an ad-hoc signature satisfies the
// check, since that is how releases are signed.
func TestVerifyBundleSignatureAcceptsAnAdHocBundle(t *testing.T) {
	const app = "/Applications/netscope.app"
	if _, err := os.Stat(app); err != nil {
		t.Skip("no installed netscope.app to verify")
	}
	if err := verifyBundleSignature(app); err != nil {
		t.Fatalf("verifyBundleSignature(%s) = %v", app, err)
	}
}

func TestVerifyBundleSignatureRejectsAnUnsignedTree(t *testing.T) {
	app := filepath.Join(t.TempDir(), "netscope.app")
	if err := os.MkdirAll(filepath.Join(app, "Contents", "MacOS"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := verifyBundleSignature(app); err == nil {
		t.Fatal("an unsigned directory passed signature verification")
	}
}
