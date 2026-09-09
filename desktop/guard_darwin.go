//go:build darwin

package main

import "log"

// guard runs one iteration of a background loop and turns a panic into a
// logged, skipped tick. The menu-bar app's poll loops — alerts, the readout
// next to the icon, the update check — are the process: a nil map or an
// unexpected response shape in any of them would otherwise take the icon out
// of the menu bar with nothing to show for it. The engine already treats its
// hot path this way (safeIngest); this is the same bargain for the desktop.
func guard(name string, step func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("%s: recovered from panic: %v", name, r)
		}
	}()
	step()
}
