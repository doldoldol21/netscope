// Package daemonctl lets the unprivileged menu-bar app bring up the privileged
// capture daemon itself, so the user only ever launches one app. If the daemon
// isn't already running, it copies netscoped to a root-owned location and
// installs a system LaunchDaemon that runs it at boot — one macOS admin prompt,
// and another only when an app update brings a new daemon binary.
//
// Running from a root-owned copy is what stops the registered daemon from being
// swapped after installation. It does not vouch for the binary that gets copied;
// see the note on findNetscoped for what that would take.
//
// This is the pragmatic path that works for an un-notarized build. A fully
// click-once experience (SMAppService) needs a Developer ID signature.
package daemonctl

import (
	"bytes"
	"crypto/sha256"
	"encoding/xml"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	label     = "io.netscope.daemon"
	plistPath = "/Library/LaunchDaemons/" + label + ".plist"
	// helperPath is where the daemon is installed to be run as root. The app
	// bundle is not a safe place to run it from: /Applications is
	// drwxrwxr-x root:admin, and a bundle under ~/Applications or ~/Downloads
	// belongs to the user outright — so any process running as that user could
	// replace the binary after installation and own every subsequent boot.
	// Copying it somewhere only root can write is what Apple's SMJobBless does
	// for the same reason, and /Library/PrivilegedHelperTools is where macOS
	// keeps such helpers.
	helperPath = "/Library/PrivilegedHelperTools/" + label
)

// IsRunning reports whether the daemon answers on the socket.
func IsRunning(client *http.Client) bool {
	resp, err := client.Get("http://netscoped/api/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// Ensure makes the daemon available: returns nil if it is already running (or
// comes up shortly — e.g. just installed by the installer), otherwise installs
// and starts the LaunchDaemon (prompting for admin once) and waits for it.
func Ensure(client *http.Client, sock string) error {
	// Give an already-installed daemon a few seconds to answer before deciding
	// to (re)install — it may still be starting up after install/login/boot.
	for i := 0; i < 10; i++ {
		if IsRunning(client) {
			return refreshHelper(client, sock)
		}
		time.Sleep(300 * time.Millisecond)
	}
	netscoped, err := findNetscoped()
	if err != nil {
		return err
	}
	if err := installDaemon(netscoped, sock); err != nil {
		return err
	}
	// Wait (up to ~9s) for the socket to come alive.
	for i := 0; i < 30; i++ {
		if IsRunning(client) {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("daemon did not start")
}

// findNetscoped picks which netscoped installDaemon should copy to helperPath.
//
// The copy is what keeps the running daemon out of reach afterwards; this
// function narrows what is worth copying in the first place. The user is about
// to approve a prompt that names netscope, so the daemon shipped in the bundle
// they just launched is the honest answer. Anywhere else is a different program
// that merely shares a filename, and is taken only when no unprivileged process
// could have put it there.
//
// Known limitation: the bundle itself is writable by the user (/Applications is
// group-writable by admins, ~/Applications outright), so a process already
// running as that user can tamper with the bundled daemon *before* the admin
// prompt appears, and the copy would then preserve the tampered binary. Nothing
// here can detect that: with an ad-hoc signature there is no stable identity to
// check, and a tamperer can re-sign ad-hoc as easily as we could verify. Closing
// it needs a Developer ID signature and a designated-requirement check on the
// source (what SMJobBless pins for you) — see SECURITY.md.
func findNetscoped() (string, error) {
	// The binary shipped next to us: the one the prompt is really about.
	if exe, err := os.Executable(); err == nil {
		if p := bundledNetscoped(exe); p != "" {
			return p, nil
		}
	}
	// Separately installed copies (Homebrew, `make install`). These are
	// unrelated files, so accepting one is a new trust grant and is allowed
	// only where no unprivileged process could have written it. $PATH is not
	// consulted at all: it is caller-controlled, and a search path is the
	// classic way to get someone else's privileged code to run yours.
	var rejected []string
	for _, p := range []string{"/opt/homebrew/bin/netscoped", "/usr/local/bin/netscoped"} {
		err := checkRootOwnedPath(p)
		switch {
		case err == nil:
			return p, nil
		case os.IsNotExist(err):
			// Not installed there.
		default:
			rejected = append(rejected, fmt.Sprintf("%s (%v)", p, err))
		}
	}
	if len(rejected) > 0 {
		return "", fmt.Errorf("refusing to install a root helper from a location a non-root process can modify: %s; "+
			"reinstall netscope.app so the bundled daemon is used", strings.Join(rejected, "; "))
	}
	return "", fmt.Errorf("netscoped binary not found (is netscope installed?)")
}

// reinstallIfStale re-runs the privileged install when the copy under
// helperPath no longer matches the daemon shipped in the running bundle —
// i.e. after the app has updated itself. Without this the installed helper
// would stay on the old build forever, since restarting it only re-runs the
// same file. It costs one admin prompt per app update, the same trade
// SMJobBless makes when a helper's version changes.
//
// Anything unexpected here is logged and ignored: a stale-but-working daemon is
// much better than an app that won't start.
func refreshHelper(client *http.Client, sock string) error {
	exe, err := os.Executable()
	if err != nil {
		return nil
	}
	bundled := bundledNetscoped(exe)
	if bundled == "" {
		return nil // dev build: nothing to compare against
	}
	reason, err := helperInstallReason(bundled, helperPath)
	if err != nil {
		log.Printf("daemonctl: could not check the installed helper: %v", err)
		return nil
	}
	if reason == "" {
		return nil
	}
	log.Printf("daemonctl: %s; reinstalling the capture helper", reason)
	if ierr := installDaemon(bundled, sock); ierr != nil {
		log.Printf("daemonctl: could not refresh the helper: %v", ierr)
	}
	// Reinstalling boots the running daemon out before registering the new one,
	// so the caller was answering a moment ago and may not be now. Confirm it
	// came back rather than assuming: this path only runs when a daemon was
	// already up, which means nothing else downstream is checking.
	for i := 0; i < 30; i++ {
		if IsRunning(client) {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	log.Print("daemonctl: the daemon did not come back after refreshing the helper")
	return fmt.Errorf("daemon did not come back after refreshing the helper")
}

// helperInstallReason reports why the privileged install needs to run again, or
// "" when the installed helper is already the right one. An error means the
// question could not be answered, which is not the same as "no".
func helperInstallReason(bundled, helper string) (string, error) {
	switch _, err := os.Stat(helper); {
	case os.IsNotExist(err):
		// Upgrading from a version that pointed launchd straight at the app
		// bundle. The daemon answering right now is that replaceable binary, so
		// an existing install is exactly the case that needs migrating — the
		// one it would be easiest to skip by treating "no helper" as "nothing
		// to do".
		return "no root-owned helper is installed", nil
	case err != nil:
		return "", err
	}
	same, err := sameContents(bundled, helper)
	if err != nil {
		return "", err
	}
	if same {
		return "", nil
	}
	return "the installed helper differs from the bundled daemon", nil
}

// sameContents reports whether two files have identical bytes.
func sameContents(a, b string) (bool, error) {
	ha, err := fileDigest(a)
	if err != nil {
		return false, err
	}
	hb, err := fileDigest(b)
	if err != nil {
		return false, err
	}
	return ha == hb, nil
}

func fileDigest(path string) ([32]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(b), nil
}

// bundledNetscoped returns the daemon shipped alongside exe, or "" if there
// isn't one (a dev build, or a CLI-only install).
func bundledNetscoped(exe string) string {
	sib := filepath.Join(filepath.Dir(exe), "netscoped")
	if st, err := os.Stat(sib); err == nil && !st.IsDir() {
		return sib
	}
	return ""
}

// checkRootOwnedPath reports whether path — and every directory leading to it,
// with symlinks resolved — is owned by root and not writable by group or other.
//
// This is the rule launchd itself applies to the LaunchDaemon plists it agrees
// to run: something an unprivileged account can rewrite must not decide what
// runs as root. Homebrew's prefix is user-writable by design (`brew` refuses to
// run under sudo for the same reason), so this deliberately rejects a Homebrew
// install rather than promoting it to root.
func checkRootOwnedPath(path string) error {
	// Resolve first: a symlink's own ownership says nothing about the file that
	// would actually be executed, and Homebrew's bin entries are symlinks.
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err // keeps os.IsNotExist usable for "simply not installed"
	}
	for p := real; ; p = filepath.Dir(p) {
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("cannot determine the owner of %s", p)
		}
		if st.Uid != 0 {
			return fmt.Errorf("%s is not owned by root", p)
		}
		if fi.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("%s is writable by group or other", p)
		}
		if p == "/" {
			return nil
		}
	}
}

// installDaemon copies netscoped somewhere only root can write, writes the
// LaunchDaemon plist pointing at that copy, and loads it — all under a single
// privileged osascript, so the user sees one admin prompt.
func installDaemon(netscoped, sock string) error {
	// The program path in the plist is a constant; only the socket varies, and
	// it is XML text, so escape it rather than trusting it to be tag-free.
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key><string>%s</string>
    <key>ProgramArguments</key>
    <array><string>%s</string><string>--sock</string><string>%s</string></array>
    <key>RunAtLoad</key><true/>
    <key>KeepAlive</key><true/>
    <key>StandardErrorPath</key><string>/var/log/netscope.log</string>
    <key>StandardOutPath</key><string>/var/log/netscope.log</string>
</dict>
</plist>
`, label, helperPath, xmlText(sock))

	tmp, err := os.CreateTemp("", "netscoped-*.plist")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(plist); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()

	return runPrivileged(installScript(netscoped, tmp.Name()), "netscope wants to install its capture helper.")
}

// installScript builds the privileged script: put the daemon and the plist in
// place as root:wheel, then bootstrap. The bundle can live anywhere the user
// dropped it, so every path is quoted rather than assumed to be well-behaved.
//
// One command per line under `set -e`, rather than a single `&&` chain. In a
// chain, `&&` and `||` bind equally and associate left, so the `|| true` on a
// later launchctl line also absorbs a failure from an earlier one: a copy that
// never happened would still exit 0 and report the migration as done. Only the
// lines that may legitimately fail carry their own `|| true`.
func installScript(netscoped, plistSrc string) string {
	return strings.Join([]string{
		"set -e",
		// Copy the daemon out of the (user-writable) bundle first, so what
		// launchd runs as root can afterwards only be replaced by root.
		fmt.Sprintf("/usr/bin/install -d -m 755 -o root -g wheel %s", shellQuote(filepath.Dir(helperPath))),
		fmt.Sprintf("/usr/bin/install -m 755 -o root -g wheel %s %s", shellQuote(netscoped), shellQuote(helperPath)),
		fmt.Sprintf("/usr/bin/install -m 644 -o root -g wheel %s %s", shellQuote(plistSrc), shellQuote(plistPath)),
		"/bin/mkdir -p /var/run/netscope",
		// Bootout any existing (possibly stuck or wrongly-owned) instance first,
		// then bootstrap fresh — so re-running this actually restarts the daemon
		// rather than no-op'ing on an already-loaded service. kickstart -k is a
		// belt-and-suspenders force-restart if it was already loaded.
		//
		// Only two of these may fail. There may be nothing loaded to boot out,
		// and kickstart is redundant when bootstrap already started the service.
		// Registering, though, is the point of the whole script: bootstrap may
		// fall back to load, but if both fail the service is not registered —
		// and the bootout above has just stopped whatever was running. Reporting
		// that as success would leave no daemon and nobody looking for it.
		fmt.Sprintf("/bin/launchctl bootout system %s 2>/dev/null || true", shellQuote(plistPath)),
		fmt.Sprintf("/bin/launchctl bootstrap system %s 2>/dev/null || /bin/launchctl load %s 2>/dev/null", shellQuote(plistPath), shellQuote(plistPath)),
		fmt.Sprintf("/bin/launchctl kickstart -k system/%s 2>/dev/null || true", label),
	}, "\n")
}

// shellQuote renders s as a single-quoted shell word. These paths end up in a
// script that runs as root, and the app bundle's location is whatever the user
// made it, so a stray quote must not be able to end the quoting and have the
// rest of the path run as a command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// xmlText escapes s for use as XML character data in the plist.
func xmlText(s string) string {
	var b bytes.Buffer
	if err := xml.EscapeText(&b, []byte(s)); err != nil {
		return ""
	}
	return b.String()
}

// runPrivileged runs a shell script as root via the native admin prompt.
func runPrivileged(shellScript, prompt string) error {
	// Embed the shell script into an AppleScript string literal. Order matters:
	// double the backslashes first, so the escapes introduced below aren't
	// doubled in turn. AppleScript turns \n back into a newline, which is what
	// lets the script be one command per line instead of an && chain.
	esc := strings.ReplaceAll(shellScript, `\`, `\\`)
	esc = strings.ReplaceAll(esc, `"`, `\"`)
	esc = strings.ReplaceAll(esc, "\n", `\n`)
	osa := fmt.Sprintf(`do shell script "%s" with prompt "%s" with administrator privileges`, esc, prompt)
	out, err := exec.Command("osascript", "-e", osa).CombinedOutput()
	if err != nil {
		return fmt.Errorf("privileged install failed: %s", strings.TrimSpace(string(out)))
	}
	return nil
}
