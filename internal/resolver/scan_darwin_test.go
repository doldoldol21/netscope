//go:build darwin

package resolver

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/doldoldol21/netscope/pkg/types"
)

// The test binary doubles as the process under observation: copied somewhere,
// started with this variable set, it just waits to be resolved.
const helperEnv = "NETSCOPE_RESOLVER_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "wait" {
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// startCopy copies the test binary to path, starts it, and returns the child.
func startCopy(t *testing.T, path string) *exec.Cmd {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	src, err := os.Open(self)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	dst.Close()

	cmd := exec.Command(path)
	cmd.Env = append(os.Environ(), helperEnv+"=wait")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	return cmd
}

func TestResolveProcessNamesARunningCopy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Ghost.app", "Contents", "MacOS", "Ghost")
	cmd := startCopy(t, path)
	p := resolveProcess(cmd.Process.Pid)
	if p.Name != "Ghost" || !samePath(path, p.Path) {
		t.Fatalf("resolveProcess = %+v, want Ghost at %s", p, path)
	}
}

// samePath compares a path with what the kernel reports for it, which has
// symlinks resolved (/var → /private/var on macOS).
func samePath(want, got string) bool {
	if got == want {
		return true
	}
	resolved, err := filepath.EvalSymlinks(want)
	return err == nil && got == resolved
}

// The regression: delete the executable behind a running process — what a
// Homebrew cask upgrade or a self-updating app does to every copy already
// running — and the process used to resolve to "unknown" for good.
func TestResolveProcessSurvivesADeletedExecutable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Ghost.app", "Contents", "MacOS", "Ghost")
	cmd := startCopy(t, path)
	resolvedBefore, _ := filepath.EvalSymlinks(path) // EvalSymlinks needs the file; take it now
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if pidPath(cmd.Process.Pid) != "" {
		t.Skip("proc_pidpath still resolves a deleted executable on this macOS; nothing to regress")
	}
	p := resolveProcess(cmd.Process.Pid)
	if p.Name != "Ghost" {
		t.Fatalf("resolveProcess after delete = %+v, want the bundle name Ghost", p)
	}
	if !samePath(path, p.Path) && p.Path != resolvedBefore {
		t.Errorf("Path = %q, want the exec path %s recovered from the argument area", p.Path, path)
	}
}

func TestExecPathFromArgsReadsOurOwnPath(t *testing.T) {
	self, _ := os.Executable()
	got := execPathFromArgs(os.Getpid())
	if got == "" {
		t.Fatal("no exec path from KERN_PROCARGS2 for the test binary")
	}
	// The kernel keeps the path as exec'd, which may be a symlink form of
	// what os.Executable resolves; compare by basename.
	if filepath.Base(got) != filepath.Base(self) {
		t.Errorf("execPathFromArgs = %q, want something ending in %s", got, filepath.Base(self))
	}
}

func TestPidNameReadsOurOwnName(t *testing.T) {
	if got := pidName(os.Getpid()); got == "" {
		t.Fatal("proc_name returned nothing for the test binary")
	}
}

func TestScanStartTimesArePopulated(t *testing.T) {
	_, paths := scan(nil)
	for pid, e := range paths {
		if pid == os.Getpid() && e.start == 0 {
			t.Fatal("scan recorded no start time for the test process")
		}
	}
}

func TestProcEntryReuseRules(t *testing.T) {
	good := procEntry{proc: procFor("/Applications/Foo.app/Contents/MacOS/Foo"), start: 100}
	if !good.reusable(100) {
		t.Error("a resolved entry with a matching start time must be reused")
	}
	if good.reusable(101) {
		t.Error("a different start time is a different process; the cache must not be reused")
	}
	failed := procEntry{proc: procFor(""), start: 100}
	if failed.reusable(100) {
		t.Error("a failed resolution must be retried, not cached")
	}
	nameOnly := procEntry{proc: procFor(""), start: 100}
	nameOnly.proc.Name = "claude" // proc_name fallback: no path, but a name
	if !nameOnly.reusable(100) {
		t.Error("a name-only resolution is still a resolution")
	}
}

func procFor(path string) types.Process {
	return types.Process{PID: 1, Path: path, Name: appName(path)}
}
