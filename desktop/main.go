//go:build darwin

// Command netscope-app is netscope's menu-bar application. It owns a native
// status-bar item (cgo) and a frameless Wails window that drops down from the
// item as a popover panel. "Open Dashboard" opens a separate native window (an
// NSWindow hosting a WKWebView) showing the full dashboard — independent of the
// popover, freely movable, with native window controls. Both render the embedded
// UI and reach the netscoped daemon's /api over its unix socket.
package main

import (
	"context"
	"net"
	"net/http"
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
	appCtx       context.Context
	winMu        sync.Mutex
	winVisible   bool
	dashboardURL string // loopback URL for the dashboard window's webview
)

func main() {
	sock := envOr("NETSCOPE_SOCK", ipc.DefaultSocketPath())
	proxy := ipc.NewReverseProxy(sock)

	// A loopback-only HTTP server feeds the standalone dashboard window's webview
	// (static UI + /api proxied to the unix socket). Bound to 127.0.0.1 only.
	dashboardURL = startLoopbackUI(proxy)

	// Bring the capture daemon up if it isn't already (one admin prompt on a
	// fresh direct-download install; no-op when installed via install.sh).
	client := ipc.Client(sock)
	go func() { _ = daemonctl.Ensure(client, sock) }()

	// Watch today's usage against the user's thresholds and notify on crossings.
	startAlertsLoop(client)

	// Show the live download/upload rate next to the menu-bar icon.
	startMenuBarReadout(client)

	// Periodically check GitHub for a newer release and notify on a new version.
	startUpdateLoop()

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
		OnDomReady: func(ctx context.Context) {
			// The popover loads hidden at startup; pause its live stream until the
			// user actually opens it (a show toggles it back on).
			setPanelLive(ctx, false)
		},
		OnStartup: func(ctx context.Context) {
			// Publish appCtx under winMu before installing the status item, so the
			// click callback (which reads appCtx under winMu) can't race the write.
			winMu.Lock()
			appCtx = ctx
			winMu.Unlock()
			installStatusItem(statusIcon())
			enablePopoverDismiss()
			// The panel's "Open Dashboard" button asks to open the dashboard window.
			wruntime.EventsOn(ctx, "netscope:opendash", func(...interface{}) {
				if dashboardURL != "" {
					openDashWindow(dashboardURL + "/dashboard.html")
				}
			})
			// Alert-threshold settings, edited in the popover.
			wruntime.EventsOn(ctx, "netscope:getalerts", func(...interface{}) {
				wruntime.EventsEmit(ctx, "netscope:alerts", alertsConfigJSON())
			})
			wruntime.EventsOn(ctx, "netscope:setalerts", func(data ...interface{}) {
				setAlertsFromEvent(data...)
			})
			// Software-update controls, surfaced in the popover.
			wruntime.EventsOn(ctx, "netscope:getupdate", func(...interface{}) {
				wruntime.EventsEmit(ctx, "netscope:update", updateStatusJSON())
			})
			wruntime.EventsOn(ctx, "netscope:checkupdate", func(...interface{}) {
				go func() {
					runUpdateCheck()
					wruntime.EventsEmit(ctx, "netscope:update", updateStatusJSON())
				}()
			})
			wruntime.EventsOn(ctx, "netscope:setautocheck", func(data ...interface{}) {
				on := true
				if len(data) > 0 {
					if b, ok := data[0].(bool); ok {
						on = b
					}
				}
				setAutoCheck(on)
			})
			wruntime.EventsOn(ctx, "netscope:doupdate", func(...interface{}) {
				go func() {
					if err := performUpdate(); err != nil {
						wruntime.EventsEmit(ctx, "netscope:updateerror", err.Error())
					}
				}()
			})
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

// startLoopbackUI serves the embedded dashboard UI and proxies /api to the unix
// socket on a random 127.0.0.1 port, returning its base URL. The dashboard
// window's WKWebView loads this; the capture daemon itself still opens no port.
func startLoopbackUI(proxy http.Handler) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return ""
	}
	mux := http.NewServeMux()
	mux.Handle("/api/", proxy)
	mux.Handle("/", http.FileServer(http.FS(webui.FS())))
	go func() { _ = http.Serve(ln, mux) }()
	return "http://" + ln.Addr().String()
}

// onStatusItemClick toggles the popover window beneath the status item. Runs on
// a goroutine (not the Cocoa main thread) so the Wails runtime calls are safe.
// The dashboard is a separate window, so this only ever drives the popover.
func onStatusItemClick() {
	winMu.Lock()
	defer winMu.Unlock()
	if appCtx == nil {
		return
	}
	if winVisible {
		setPanelLive(appCtx, false) // stop the live stream while hidden
		wruntime.WindowHide(appCtx)
		winVisible = false
		return
	}
	wruntime.WindowSetSize(appCtx, popoverWidth, popoverHeight)
	// Place the window directly (global coords) before showing it, so it lands
	// under the status item on whichever monitor the menu bar is on.
	positionPopover(popoverWidth, popoverHeight)
	wruntime.WindowShow(appCtx)
	focusPopover()             // make it key so clicking away dismisses it
	setPanelLive(appCtx, true) // resume live updates now that it's visible
	winVisible = true
}

// setPanelLive turns the popover's live SSE stream + today's-total polling on or
// off via the JS hook, so a hidden popover doesn't keep the daemon streaming.
// Uses ExecJS (reliable even on a hidden webview); the guard tolerates the JS
// hook not being defined yet. Callers already run off the Cocoa main thread.
func setPanelLive(ctx context.Context, on bool) {
	if ctx == nil {
		return
	}
	arg := "false"
	if on {
		arg = "true"
	}
	wruntime.WindowExecJS(ctx, "window.nsLive&&window.nsLive("+arg+")")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
