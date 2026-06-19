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

const dashboardWidth, dashboardHeight = 1120, 760

var (
	appCtx     context.Context
	winMu      sync.Mutex
	winVisible bool
	dashMode   bool // the shared window is showing the full dashboard
)

func main() {
	sock := envOr("NETSCOPE_SOCK", ipc.DefaultSocketPath())
	proxy := ipc.NewReverseProxy(sock)

	// Bring the capture daemon up if it isn't already (one admin prompt on a
	// fresh direct-download install; no-op when installed via install.sh).
	go func() { _ = daemonctl.Ensure(ipc.Client(sock), sock) }()

	err := wails.Run(&options.App{
		Title: "netscope",
		// Only one menu-bar app at a time: a second launch (login agent +
		// installer's open, or the user re-opening) exits and pings the first.
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId:               "io.netscope.app",
			OnSecondInstanceLaunch: func(options.SecondInstanceData) { go onStatusItemClick() },
		},
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
			// The page asks to open/close the full dashboard window via events.
			wruntime.EventsOn(ctx, "netscope:opendash", func(...interface{}) { go openDashboard() })
			wruntime.EventsOn(ctx, "netscope:closedash", func(...interface{}) { go closeDashboard() })
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
	if dashMode {
		// The dashboard is a real window; just bring it to the front.
		focusPopover()
		return
	}
	if winVisible {
		wruntime.WindowHide(appCtx)
		winVisible = false
		resetToPanel() // restore the panel while hidden, ready for next show
		return
	}
	wruntime.WindowSetSize(appCtx, popoverWidth, popoverHeight)
	x, y := statusItemAnchor(popoverWidth)
	wruntime.WindowSetPosition(appCtx, x, y)
	wruntime.WindowShow(appCtx)
	focusPopover() // make it key so clicking away dismisses it
	winVisible = true
}

// openDashboard promotes the shared window into a standalone dashboard window:
// a regular app window (Dock + Cmd-Tab), centred, not pinned on top, showing the
// full dashboard page. Triggered by the panel's "Open Dashboard" button.
func openDashboard() {
	winMu.Lock()
	defer winMu.Unlock()
	if appCtx == nil {
		return
	}
	dashMode = true
	winVisible = true
	enterDashboardChrome()
	wruntime.WindowSetAlwaysOnTop(appCtx, false)
	wruntime.WindowSetSize(appCtx, dashboardWidth, dashboardHeight)
	wruntime.WindowCenter(appCtx)
	wruntime.WindowExecJS(appCtx, "if(location.pathname!=='/dashboard.html')location.replace('/dashboard.html')")
	wruntime.WindowShow(appCtx)
	focusPopover()
}

// closeDashboard demotes the window back to the menu-bar popover (hidden, panel
// page, anchored + always-on-top), ready for the next status-item click.
// Triggered by the dashboard's in-page close button.
func closeDashboard() {
	winMu.Lock()
	defer winMu.Unlock()
	if appCtx == nil {
		return
	}
	dashMode = false
	winVisible = false
	exitDashboardChrome()
	wruntime.WindowHide(appCtx)
	wruntime.WindowSetAlwaysOnTop(appCtx, true)
	resetToPanel()
}

// resetToPanel returns the (now hidden) window to the compact popover: it shrinks
// it back to popover size and tells the page to navigate to the panel ("/"). We
// do this on HIDE — while the window is off-screen — so the next click always
// shows the small panel, never the large dashboard crammed into the popover.
// Must run off the Cocoa main thread (it calls into the Wails runtime).
func resetToPanel() {
	if appCtx == nil {
		return
	}
	wruntime.WindowSetSize(appCtx, popoverWidth, popoverHeight)
	// Force the page back to the panel directly (don't depend on the loaded
	// page having a "netscope:show" listener — the dashboard's may not have run).
	wruntime.WindowExecJS(appCtx, "if(location.pathname!=='/')location.replace('/')")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
