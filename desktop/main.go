//go:build darwin

// Command netscope-app is netscope's menu-bar application. It owns a native
// status-bar item (cgo) and a frameless Wails window that drops down from the
// item as a popover panel, rendering the embedded UI and reverse-proxying /api
// to the netscoped daemon over its unix socket.
package main

import (
	"context"
	"os"
	"sync"

	"github.com/doldoldol21/netscope/internal/daemonctl"
	"github.com/doldoldol21/netscope/internal/ipc"
	"github.com/doldoldol21/netscope/internal/webui"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

const (
	popoverWidth  = 340
	popoverHeight = 500
)

var (
	appCtx     context.Context
	winMu      sync.Mutex
	winVisible bool
)

func main() {
	sock := envOr("NETSCOPE_SOCK", ipc.DefaultSocketPath())
	proxy := ipc.NewReverseProxy(sock)

	// Bring the capture daemon up if it isn't already (one admin prompt on a
	// fresh direct-download install; no-op when installed via install.sh).
	go func() { _ = daemonctl.Ensure(ipc.Client(sock), sock) }()

	err := wails.Run(&options.App{
		Title:             "netscope",
		Width:             popoverWidth,
		Height:            popoverHeight,
		DisableResize:     true,
		Frameless:         true,
		AlwaysOnTop:       true,
		StartHidden:       true,
		HideWindowOnClose: true,
		BackgroundColour:  &options.RGBA{R: 13, G: 17, B: 23, A: 0},
		AssetServer: &assetserver.Options{
			Assets:  webui.FS(),
			Handler: proxy,
		},
		OnStartup: func(ctx context.Context) {
			appCtx = ctx
			installStatusItem(statusIcon())
			enablePopoverDismiss()
		},
		Mac: &mac.Options{
			Appearance:           mac.NSAppearanceNameDarkAqua,
			WebviewIsTransparent: true,
			WindowIsTranslucent:  true,
		},
	})
	if err != nil {
		println("netscope-app:", err.Error())
		os.Exit(1)
	}
}

// onStatusItemClick toggles the popover window beneath the status item. Runs on
// a goroutine (not the Cocoa main thread) so the Wails runtime calls are safe.
func onStatusItemClick() {
	winMu.Lock()
	defer winMu.Unlock()
	if appCtx == nil {
		return
	}
	if winVisible {
		wruntime.WindowHide(appCtx)
		winVisible = false
		return
	}
	wruntime.WindowSetSize(appCtx, popoverWidth, popoverHeight)
	x, y := statusItemAnchor(popoverWidth)
	wruntime.WindowSetPosition(appCtx, x, y)
	wruntime.WindowShow(appCtx)
	focusPopover() // make it key so clicking away dismisses it
	winVisible = true
	// Tell the page to show the compact panel (in case the dashboard was open).
	wruntime.EventsEmit(appCtx, "netscope:show")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
