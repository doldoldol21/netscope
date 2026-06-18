package menubar

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

// netscope's menu-bar app registers itself as a per-user LaunchAgent so it
// starts at login. A LaunchAgent (vs a privileged daemon) needs no entitlements
// or admin rights — the plist in ~/Library/LaunchAgents is enough.

const loginLabel = "io.netscope.bar"

func loginPlistPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "LaunchAgents", loginLabel+".plist")
}

// loginItemEnabled reports whether the launch-at-login agent is installed.
func loginItemEnabled() bool {
	p := loginPlistPath()
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}

// enableLoginItem writes the LaunchAgent for the currently-running executable
// and loads it so it also starts now.
func enableLoginItem() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.EvalSymlinks(exe)
	p := loginPlistPath()
	if p == "" {
		return fmt.Errorf("cannot resolve home directory")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key><string>%s</string>
    <key>ProgramArguments</key><array><string>%s</string></array>
    <key>RunAtLoad</key><true/>
    <key>ProcessType</key><string>Interactive</string>
</dict>
</plist>
`, loginLabel, exe)
	if err := os.WriteFile(p, []byte(plist), 0o644); err != nil {
		return err
	}
	// Load it now (ignore errors: presence of the plist is the source of truth,
	// and it will load at next login regardless).
	domain := "gui/" + strconv.Itoa(os.Getuid())
	_ = exec.Command("launchctl", "bootstrap", domain, p).Run()
	return nil
}

// disableLoginItem unloads and removes the LaunchAgent.
func disableLoginItem() error {
	p := loginPlistPath()
	if p == "" {
		return fmt.Errorf("cannot resolve home directory")
	}
	domain := "gui/" + strconv.Itoa(os.Getuid())
	_ = exec.Command("launchctl", "bootout", domain, p).Run()
	return os.Remove(p)
}
