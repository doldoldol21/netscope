// Package daemonctl lets the unprivileged menu-bar app bring up the privileged
// capture daemon itself, so the user only ever launches one app. If the daemon
// isn't already running, it installs a system LaunchDaemon (one macOS admin
// prompt) that runs netscoped at boot — after that, no prompts ever again.
//
// This is the pragmatic path that works for an un-notarized build. A fully
// click-once experience (SMAppService) needs a Developer ID signature.
package daemonctl

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	label     = "io.netscope.daemon"
	plistPath = "/Library/LaunchDaemons/" + label + ".plist"
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

// Ensure makes the daemon available: returns nil immediately if it is already
// running, otherwise installs + starts the LaunchDaemon (prompting for admin
// once) and waits for it to come up.
func Ensure(client *http.Client, sock string) error {
	if IsRunning(client) {
		return nil
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

// findNetscoped locates the netscoped binary: PATH first (a stable symlink that
// survives upgrades), then a sibling of this executable.
func findNetscoped() (string, error) {
	if p, err := exec.LookPath("netscoped"); err == nil {
		return p, nil
	}
	if exe, err := os.Executable(); err == nil {
		sib := filepath.Join(filepath.Dir(exe), "netscoped")
		if st, err := os.Stat(sib); err == nil && !st.IsDir() {
			return sib, nil
		}
	}
	return "", fmt.Errorf("netscoped binary not found (is netscope installed?)")
}

// installDaemon writes the LaunchDaemon plist to a temp file and, via a single
// privileged osascript, installs it to /Library/LaunchDaemons and loads it.
func installDaemon(netscoped, sock string) error {
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
`, label, netscoped, sock)

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

	// Short privileged script: install the plist as root:wheel and bootstrap it.
	// Paths have no spaces/quotes, so single-quoting is enough.
	script := strings.Join([]string{
		fmt.Sprintf("/usr/bin/install -m 644 -o root -g wheel '%s' '%s'", tmp.Name(), plistPath),
		"/bin/mkdir -p /var/run/netscope",
		fmt.Sprintf("/bin/launchctl bootstrap system '%s' 2>/dev/null || /bin/launchctl load '%s'", plistPath, plistPath),
	}, " && ")

	return runPrivileged(script, "netscope wants to install its capture helper.")
}

// runPrivileged runs a shell script as root via the native admin prompt.
func runPrivileged(shellScript, prompt string) error {
	// Embed the shell script into an AppleScript string literal.
	esc := strings.ReplaceAll(shellScript, `\`, `\\`)
	esc = strings.ReplaceAll(esc, `"`, `\"`)
	osa := fmt.Sprintf(`do shell script "%s" with prompt "%s" with administrator privileges`, esc, prompt)
	out, err := exec.Command("osascript", "-e", osa).CombinedOutput()
	if err != nil {
		return fmt.Errorf("privileged install failed: %s", strings.TrimSpace(string(out)))
	}
	return nil
}
