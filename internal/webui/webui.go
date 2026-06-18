// Package webui holds the embedded dashboard assets shared by the daemon's
// HTTP server and the Wails desktop shell, so both render the exact same UI
// from a single source of truth.
package webui

import (
	"embed"
	"io/fs"
)

//go:embed assets
var assets embed.FS

// FS returns the dashboard asset filesystem rooted at the asset directory
// (so "index.html", "app.js", … are at the top level).
func FS() fs.FS {
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		// assets is a compile-time embed; this can only fail on a packaging
		// mistake, in which case failing loudly is correct.
		panic(err)
	}
	return sub
}
