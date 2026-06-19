//go:build darwin

package main

import (
	"os/exec"
	"strings"
)

// notify posts a macOS notification banner. It shells out to osascript's
// `display notification`, which works for an unsigned/ad-hoc app with no
// notification entitlement or authorization prompt. (A branded
// UNUserNotificationCenter banner would need proper signing — a later polish.)
func notify(title, body string) {
	esc := func(s string) string {
		s = strings.ReplaceAll(s, `\`, `\\`)
		return strings.ReplaceAll(s, `"`, `\"`)
	}
	script := `display notification "` + esc(body) + `" with title "` + esc(title) + `"`
	_ = exec.Command("osascript", "-e", script).Start()
}
