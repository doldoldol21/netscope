// Package menubar implements netscope's macOS menu-bar (status bar) app: a
// RunCat-style always-on presence that shows the live up/down rate in the menu
// bar and a native dropdown of the top apps, reading everything from the daemon
// over its unix socket. The rich dashboard stays a separate window, opened from
// the menu.
package menubar

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"fyne.io/systray"

	"github.com/doldoldol21/netscope/internal/daemonctl"
	"github.com/doldoldol21/netscope/internal/ipc"
	"github.com/doldoldol21/netscope/pkg/types"
)

const (
	maxApps = 6
	poll    = 1500 * time.Millisecond
)

// Run starts the menu-bar app for the daemon at sock. It blocks until Quit.
func Run(sock string) {
	app := &app{client: ipc.Client(sock), sock: sock}
	systray.Run(app.onReady, func() {})
}

type app struct {
	client *http.Client
	sock   string

	rate   *systray.MenuItem
	startD *systray.MenuItem
	apps   []*systray.MenuItem
	dash   *systray.MenuItem
	login  *systray.MenuItem
	update *systray.MenuItem
	quitI  *systray.MenuItem

	updateURL string
}

func (a *app) onReady() {
	systray.SetTemplateIcon(icon(), icon())
	systray.SetTitle(" –")
	systray.SetTooltip("netscope — per-app network monitor")

	a.rate = systray.AddMenuItem("Connecting…", "")
	a.rate.Disable()
	a.startD = systray.AddMenuItem("Start Capture Daemon…", "Install and start the capture helper")
	a.startD.Hide()
	systray.AddSeparator()

	header := systray.AddMenuItem("TOP APPS", "")
	header.Disable()
	a.apps = make([]*systray.MenuItem, maxApps)
	for i := range a.apps {
		a.apps[i] = systray.AddMenuItem("", "")
		a.apps[i].Disable()
		a.apps[i].Hide()
	}

	systray.AddSeparator()
	a.dash = systray.AddMenuItem("Open Dashboard…", "Open the full netscope window")
	a.login = systray.AddMenuItemCheckbox("Launch at Login", "Start netscope automatically", loginItemEnabled())
	a.update = systray.AddMenuItem("netscope", "")
	a.update.Disable()
	a.quitI = systray.AddMenuItem("Quit netscope", "")

	go a.bootstrapDaemon() // bring the capture daemon up if it isn't already
	go a.loop()
	go a.updateLoop()
	go a.handleClicks()
}

// bootstrapDaemon ensures the capture daemon is running, installing it (one
// admin prompt) on first launch. No prompt if it's already up.
func (a *app) bootstrapDaemon() {
	if err := daemonctl.Ensure(a.client, a.sock); err != nil {
		a.startD.Show() // let the user retry from the menu
	}
}

func (a *app) handleClicks() {
	for {
		select {
		case <-a.startD.ClickedCh:
			go a.bootstrapDaemon()
		case <-a.dash.ClickedCh:
			openDashboard()
		case <-a.login.ClickedCh:
			if a.login.Checked() {
				if disableLoginItem() == nil {
					a.login.Uncheck()
				}
			} else {
				if enableLoginItem() == nil {
					a.login.Check()
				}
			}
		case <-a.update.ClickedCh:
			if a.updateURL != "" {
				_ = exec.Command("open", a.updateURL).Run()
			}
		case <-a.quitI.ClickedCh:
			systray.Quit()
			return
		}
	}
}

// updateLoop polls /api/version and reflects the result in the menu: a plain
// disabled "netscope <version>" line normally, or a clickable "Update
// available" item (opening the release page) when a newer release exists.
func (a *app) updateLoop() {
	t := time.NewTicker(30 * time.Minute)
	defer t.Stop()
	a.checkUpdate()
	for range t.C {
		a.checkUpdate()
	}
}

func (a *app) checkUpdate() {
	resp, err := a.client.Get("http://netscoped/api/version")
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var v struct {
		Current         string `json:"current"`
		Latest          string `json:"latest"`
		UpdateAvailable bool   `json:"updateAvailable"`
		URL             string `json:"url"`
	}
	if json.NewDecoder(resp.Body).Decode(&v) != nil {
		return
	}
	if v.UpdateAvailable && v.URL != "" {
		a.updateURL = v.URL
		a.update.SetTitle(fmt.Sprintf("⬆ Update available: %s", v.Latest))
		a.update.Enable()
	} else {
		a.updateURL = ""
		a.update.SetTitle("netscope " + v.Current)
		a.update.Disable()
	}
}

// loop polls the daemon and refreshes the menu bar title + menu.
func (a *app) loop() {
	t := time.NewTicker(poll)
	defer t.Stop()
	a.refresh() // immediate first paint
	for range t.C {
		a.refresh()
	}
}

func (a *app) refresh() {
	snap, err := a.snapshot()
	if err != nil {
		systray.SetTitle(" ⏸")
		a.rate.SetTitle("Capture daemon not running")
		a.startD.Show()
		for _, m := range a.apps {
			m.Hide()
		}
		return
	}
	a.startD.Hide()
	systray.SetTitle(fmt.Sprintf(" ↓%s ↑%s", short(snap.RxPerSec), short(snap.TxPerSec)))
	a.rate.SetTitle(fmt.Sprintf("↓ %s/s    ↑ %s/s", human(snap.RxPerSec), human(snap.TxPerSec)))

	for i, m := range a.apps {
		if i < len(snap.Apps) {
			ap := snap.Apps[i]
			m.SetTitle(fmt.Sprintf("%-18s %9s", trunc(ap.Name, 18), human(ap.Total())))
			m.Show()
		} else {
			m.Hide()
		}
	}
}

func (a *app) snapshot() (types.Snapshot, error) {
	var s types.Snapshot
	resp, err := a.client.Get("http://netscoped/api/snapshot")
	if err != nil {
		return s, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return s, fmt.Errorf("status %d", resp.StatusCode)
	}
	return s, json.NewDecoder(resp.Body).Decode(&s)
}

// short formats bytes/sec compactly for the menu bar (e.g. "2.4M", "180K").
func short(n uint64) string {
	const u = 1024
	if n < u {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := uint64(u), 0
	for v := n / u; v >= u; v /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f%c", float64(n)/float64(div), "KMGTPE"[exp])
}

// human formats bytes with a space and unit (e.g. "2.4 MB").
func human(n uint64) string {
	const u = 1024
	if n < u {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(u), 0
	for v := n / u; v >= u; v /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// openDashboard launches the dashboard window. In a packaged build it lives at
// netscope.app/Contents/Resources/Dashboard.app (next to this executable);
// $NETSCOPE_APP and a local dev build are used as fallbacks.
func openDashboard() {
	for _, p := range dashboardCandidates() {
		if _, err := os.Stat(p); err == nil {
			_ = exec.Command("open", p).Run()
			return
		}
	}
	_ = exec.Command("open", "-b", "io.netscope.dashboard").Run()
}

func dashboardCandidates() []string {
	var c []string
	if exe, err := os.Executable(); err == nil {
		// .../netscope.app/Contents/MacOS/<exe> -> .../Resources/Dashboard.app
		c = append(c, filepath.Join(filepath.Dir(exe), "..", "Resources", "Dashboard.app"))
	}
	if p := os.Getenv("NETSCOPE_APP"); p != "" {
		c = append(c, p)
	}
	c = append(c, "desktop/build/bin/netscope.app", "/Applications/netscope.app/Contents/Resources/Dashboard.app")
	return c
}

// icon draws a tiny down/up triangle glyph as a template PNG; macOS renders a
// template image adaptively for light/dark menu bars.
func icon() []byte {
	const w, h = 36, 22
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	black := color.RGBA{0, 0, 0, 255}
	// download: wide top, point at bottom (▽)
	fillTri(img, 3, 4, 16, 4, 9, 18, black)
	// upload: point at top, wide bottom (△)
	fillTri(img, 27, 4, 20, 18, 33, 18, black)
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func fillTri(img *image.RGBA, ax, ay, bx, by, cx, cy int, col color.Color) {
	minY, maxY := min3(ay, by, cy), max3(ay, by, cy)
	edges := [][4]int{{ax, ay, bx, by}, {bx, by, cx, cy}, {cx, cy, ax, ay}}
	for y := minY; y <= maxY; y++ {
		var xs []int
		for _, e := range edges {
			x1, y1, x2, y2 := e[0], e[1], e[2], e[3]
			if y1 == y2 {
				continue
			}
			if (y1 <= y && y2 > y) || (y2 <= y && y1 > y) {
				t := float64(y-y1) / float64(y2-y1)
				xs = append(xs, x1+int(t*float64(x2-x1)))
			}
		}
		if len(xs) == 2 {
			lo, hi := xs[0], xs[1]
			if lo > hi {
				lo, hi = hi, lo
			}
			for x := lo; x <= hi; x++ {
				img.Set(x, y, col)
			}
		}
	}
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}

func max3(a, b, c int) int {
	m := a
	if b > m {
		m = b
	}
	if c > m {
		m = c
	}
	return m
}
