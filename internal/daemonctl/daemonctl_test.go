package daemonctl

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Whatever findNetscoped picks is installed as a root LaunchDaemon, so the
// candidate paths must not be writable by an unprivileged process.

func TestCheckRootOwnedPathAcceptsSystemBinary(t *testing.T) {
	// /bin/sh and every directory above it are root-owned and not group- or
	// world-writable on both macOS and Linux.
	if err := checkRootOwnedPath("/bin/sh"); err != nil {
		t.Errorf("/bin/sh rejected: %v", err)
	}
}

func TestCheckRootOwnedPathRejectsUserOwnedFile(t *testing.T) {
	// The Homebrew case: the prefix belongs to the logged-in user, so any
	// process running as that user could swap the binary out.
	if os.Geteuid() == 0 {
		t.Skip("running as root: a temporary file would be root-owned, which is the case under test")
	}
	p := filepath.Join(t.TempDir(), "netscoped")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := checkRootOwnedPath(p)
	if err == nil {
		t.Fatal("user-owned binary accepted")
	}
	if !strings.Contains(err.Error(), "not owned by root") {
		t.Errorf("unexpected reason: %v", err)
	}
}

func TestCheckRootOwnedPathRejectsWorldWritableDirectory(t *testing.T) {
	// A root-owned but world-writable directory is no better: anyone can
	// replace what lives inside it. /tmp is exactly that on macOS and Linux.
	err := checkRootOwnedPath("/tmp")
	if err == nil {
		t.Fatal("world-writable directory accepted")
	}
	if !strings.Contains(err.Error(), "writable by group or other") {
		t.Errorf("unexpected reason: %v", err)
	}
}

func TestCheckRootOwnedPathReportsMissingSeparately(t *testing.T) {
	// "Not installed there" must stay distinguishable from "installed somewhere
	// untrustworthy", so findNetscoped can skip the first and report the second.
	err := checkRootOwnedPath(filepath.Join(t.TempDir(), "absent"))
	if !os.IsNotExist(err) {
		t.Errorf("missing path gave %v, want a not-exist error", err)
	}
}

func TestBundledNetscopedFindsSibling(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "netscope")
	sib := filepath.Join(dir, "netscoped")
	if err := os.WriteFile(sib, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := bundledNetscoped(exe); got != sib {
		t.Errorf("bundledNetscoped = %q, want %q", got, sib)
	}
}

func TestBundledNetscopedIgnoresDirectoryAndAbsence(t *testing.T) {
	dir := t.TempDir()
	if got := bundledNetscoped(filepath.Join(dir, "netscope")); got != "" {
		t.Errorf("no sibling should give \"\", got %q", got)
	}
	if err := os.Mkdir(filepath.Join(dir, "netscoped"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := bundledNetscoped(filepath.Join(dir, "netscope")); got != "" {
		t.Errorf("a directory named netscoped should give \"\", got %q", got)
	}
}

// installDaemon builds a shell script that runs as root, and the bundle path it
// embeds is wherever the user put the app — so quoting has to hold for paths
// nobody sanitised.

func TestShellQuoteSurvivesAShell(t *testing.T) {
	for _, path := range []string{
		"/Applications/netscope.app/Contents/MacOS/netscoped",
		"/Users/some one/Applications/netscope.app/x",
		`/tmp/it's here/netscoped`,
		`/tmp/'; touch /tmp/netscope-quoting-escape; echo '`,
		`/tmp/$(id)/netscoped`,
		"/tmp/`id`/netscoped",
		`/tmp/a"b/netscoped`,
	} {
		out, err := exec.Command("/bin/sh", "-c", "printf %s "+shellQuote(path)).Output()
		if err != nil {
			t.Fatalf("%q: %v", path, err)
		}
		if string(out) != path {
			t.Errorf("shellQuote(%q): the shell saw %q", path, out)
		}
	}
}

func TestXMLTextEscapesPlistData(t *testing.T) {
	got := xmlText("/tmp/a&b</string><key>RunAtLoad")
	if strings.Contains(got, "<key>") || strings.Contains(got, "&b") {
		t.Errorf("xmlText left markup intact: %q", got)
	}
}

func TestSameContents(t *testing.T) {
	dir := t.TempDir()
	a, b, c := filepath.Join(dir, "a"), filepath.Join(dir, "b"), filepath.Join(dir, "c")
	if err := os.WriteFile(a, []byte("same"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("same"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c, []byte("different"), 0o644); err != nil {
		t.Fatal(err)
	}
	if same, err := sameContents(a, b); err != nil || !same {
		t.Errorf("identical files: same=%v err=%v", same, err)
	}
	if same, err := sameContents(a, c); err != nil || same {
		t.Errorf("differing files: same=%v err=%v", same, err)
	}
	// A helper that was never installed must not read as "up to date".
	if _, err := sameContents(a, filepath.Join(dir, "absent")); err == nil {
		t.Error("missing helper compared without error")
	}
}

// An install from a version that ran the daemon out of the app bundle has no
// root-owned copy at all. That is the case the whole patch exists for, and it
// is the one most easily skipped by treating a missing helper as "nothing to
// do" — so it gets its own test.

func TestHelperInstallReasonMigratesAnOldInstall(t *testing.T) {
	dir := t.TempDir()
	bundled := filepath.Join(dir, "netscoped")
	if err := os.WriteFile(bundled, []byte("daemon"), 0o755); err != nil {
		t.Fatal(err)
	}
	reason, err := helperInstallReason(bundled, filepath.Join(dir, "absent"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reason == "" {
		t.Error("a missing helper was treated as up to date")
	}
}

func TestHelperInstallReasonSkipsAMatchingHelper(t *testing.T) {
	dir := t.TempDir()
	bundled := filepath.Join(dir, "netscoped")
	helper := filepath.Join(dir, "installed")
	for _, p := range []string{bundled, helper} {
		if err := os.WriteFile(p, []byte("daemon"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	reason, err := helperInstallReason(bundled, helper)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reason != "" {
		t.Errorf("an up-to-date helper asked for a reinstall: %s", reason)
	}
}

func TestHelperInstallReasonRefreshesAStaleHelper(t *testing.T) {
	dir := t.TempDir()
	bundled := filepath.Join(dir, "netscoped")
	helper := filepath.Join(dir, "installed")
	if err := os.WriteFile(bundled, []byte("new build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(helper, []byte("old build"), 0o755); err != nil {
		t.Fatal(err)
	}
	reason, err := helperInstallReason(bundled, helper)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reason == "" {
		t.Error("a stale helper was treated as up to date")
	}
}

func TestHelperInstallReasonReportsAnUnreadableBundle(t *testing.T) {
	// "Could not tell" must not collapse into "nothing to do": that would
	// silently leave an old install on the replaceable binary.
	dir := t.TempDir()
	helper := filepath.Join(dir, "installed")
	if err := os.WriteFile(helper, []byte("daemon"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := helperInstallReason(filepath.Join(dir, "gone"), helper); err == nil {
		t.Error("an unreadable bundled daemon reported no error")
	}
}

// The privileged script decides whether a failed copy is reported as success.
// Its steps used to be one `&& ` chain, where a later `|| true` also absorbed
// an earlier failure — so these pin the exit status, not the wording.

// runInstallScript executes the generated script with the privileged commands
// stood in for, so its control flow can be checked unprivileged. launchctl gets
// a stub that can answer differently per subcommand, because the steps are not
// equally optional: bootout and kickstart may fail, registering may not.
func runInstallScript(t *testing.T, installCmd, launchctlBody string) error {
	t.Helper()
	script := strings.NewReplacer(
		"/usr/bin/install", installCmd,
		"/bin/mkdir", "true",
		"/bin/launchctl", "fake_launchctl",
	).Replace(installScript("/tmp/src", "/tmp/plist"))
	return exec.Command("/bin/sh", "-c",
		"fake_launchctl() { "+launchctlBody+" }\n"+script).Run()
}

const launchctlWorks = "return 0;"

func TestInstallScriptFailsWhenACopyFails(t *testing.T) {
	if err := runInstallScript(t, "false", launchctlWorks); err == nil {
		t.Error("a failed install step exited 0, so a helper that was never copied would look installed")
	}
}

func TestInstallScriptSucceedsWhenEveryStepDoes(t *testing.T) {
	if err := runInstallScript(t, "true", launchctlWorks); err != nil {
		t.Errorf("every step succeeded but the script reported %v", err)
	}
}

func TestInstallScriptToleratesNothingToBootOut(t *testing.T) {
	// A first install has no loaded service to stop; that is not a failure.
	body := `case "$1" in bootout) return 1;; *) return 0;; esac`
	if err := runInstallScript(t, "true", body); err != nil {
		t.Errorf("a failed bootout aborted the install: %v", err)
	}
}

func TestInstallScriptToleratesBootstrapFallingBackToLoad(t *testing.T) {
	body := `case "$1" in bootstrap) return 1;; *) return 0;; esac`
	if err := runInstallScript(t, "true", body); err != nil {
		t.Errorf("the load fallback did not rescue a failed bootstrap: %v", err)
	}
}

func TestInstallScriptFailsWhenTheServiceCannotBeRegistered(t *testing.T) {
	// bootout has already stopped whatever was running, so if neither bootstrap
	// nor load registers the new one, there is no daemon left. Reporting
	// success here is what would hide that.
	body := `case "$1" in bootstrap|load) return 1;; *) return 0;; esac`
	if err := runInstallScript(t, "true", body); err == nil {
		t.Error("registering failed entirely but the script exited 0")
	}
}

func TestPrivilegedScriptSurvivesAppleScriptQuoting(t *testing.T) {
	// runPrivileged embeds the script in an AppleScript string literal, so the
	// newlines and quoting have to survive the trip. osascript without
	// "with administrator privileges" runs as us, and needs no password.
	if runtime.GOOS != "darwin" {
		t.Skip("osascript is macOS only")
	}
	script := "set -e\necho 'it\"s here'\nfalse\necho reached"
	esc := strings.ReplaceAll(script, `\`, `\\`)
	esc = strings.ReplaceAll(esc, `"`, `\"`)
	esc = strings.ReplaceAll(esc, "\n", `\n`)

	out, err := exec.Command("osascript", "-e", `do shell script "`+esc+`"`).CombinedOutput()
	if err == nil {
		t.Fatal("set -e did not stop the script; the newlines were not honoured")
	}
	if strings.Contains(string(out), "reached") {
		t.Error("execution continued past the failing line")
	}
}
