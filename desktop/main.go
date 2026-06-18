//go:build darwin

// Command netscope-app is the native macOS shell for netscope. It renders the
// exact same dashboard as the web view (shared internal/webui assets) inside a
// native Wails window, and reverse-proxies every /api request to the running
// netscoped daemon — so the GUI stays an unprivileged app while capture keeps
// running as root in the daemon.
package main

import (
	"os"

	"github.com/doldoldol21/netscope/internal/ipc"
	"github.com/doldoldol21/netscope/internal/webui"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
)

func main() {
	sock := envOr("NETSCOPE_SOCK", ipc.DefaultSocketPath())

	// Reverse-proxy /api/* (including the SSE stream) to the daemon over its
	// unix socket. Anything the embedded asset server can't satisfy lands here.
	proxy := ipc.NewReverseProxy(sock)

	err := wails.Run(&options.App{
		Title:     "netscope",
		Width:     1120,
		Height:    760,
		MinWidth:  900,
		MinHeight: 580,
		AssetServer: &assetserver.Options{
			Assets:  webui.FS(),
			Handler: proxy, // /api/* -> netscoped
		},
		BackgroundColour: &options.RGBA{R: 13, G: 17, B: 23, A: 1},
		Mac: &mac.Options{
			TitleBar:             mac.TitleBarHiddenInset(),
			Appearance:           mac.NSAppearanceNameDarkAqua,
			WebviewIsTransparent: false,
			WindowIsTranslucent:  false,
			About: &mac.AboutInfo{
				Title:   "netscope",
				Message: "Per-app network monitor for macOS.\nData stays on this machine.",
			},
		},
	})
	if err != nil {
		println("netscope-app:", err.Error())
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
