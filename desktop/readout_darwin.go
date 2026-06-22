//go:build darwin

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/doldoldol21/netscope/internal/alerts"
)

// readoutInterval is how often the menu-bar rate text refreshes. The menu bar is
// always visible, so this polls continuously (unlike the popover's live stream,
// which pauses when hidden) — but 2s is light and keeps the numbers steady.
const readoutInterval = 2 * time.Second

// menuBarStyle controls how the live rate renders next to the icon. Users pick a
// style in settings; format() turns a (rx,tx) pair into the displayed string.
type menuBarStyle struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	format func(rx, tx string) string
}

// menuBarStyles are the selectable readout styles (symbol variants). The first
// is the default.
var menuBarStyles = []menuBarStyle{
	{ID: "arrows", Label: "Arrows  ↓1.2M ↑30K", format: func(rx, tx string) string { return "↓" + rx + " ↑" + tx }},
	{ID: "triangles", Label: "Triangles  ▼1.2M ▲30K", format: func(rx, tx string) string { return "▼" + rx + " ▲" + tx }},
	{ID: "caret", Label: "Carets  ⇣1.2M ⇡30K", format: func(rx, tx string) string { return "⇣" + rx + " ⇡" + tx }},
	{ID: "suffix", Label: "Suffix  1.2M↓ 30K↑", format: func(rx, tx string) string { return rx + "↓ " + tx + "↑" }},
	{ID: "downonly", Label: "Download only  ↓1.2M", format: func(rx, tx string) string { return "↓" + rx }},
	{ID: "icononly", Label: "Icon only", format: func(rx, tx string) string { return "" }},
}

var (
	readoutMu    sync.Mutex
	readoutStyle = "arrows"
	readoutPath  string
	lastRx       string
	lastTx       string
	readoutHTTP  *http.Client
)

// startMenuBarReadout polls the daemon's live snapshot and shows the current
// download/upload rate next to the menu-bar icon in the user's chosen style.
func startMenuBarReadout(client *http.Client) {
	readoutHTTP = client
	readoutPath = filepath.Join(filepath.Dir(alerts.ConfigPath()), "menubar.json")
	loadReadoutStyle()
	go func() {
		time.Sleep(6 * time.Second) // let the daemon come up first
		for {
			rx, tx, ok := fetchRates(client)
			if ok {
				readoutMu.Lock()
				lastRx, lastTx = compactRate(rx), compactRate(tx)
				readoutMu.Unlock()
				renderReadout()
			} else {
				setStatusText("") // daemon unreachable: icon only
			}
			time.Sleep(readoutInterval)
		}
	}()
}

// renderReadout formats the last-seen rates with the current style and pushes
// the text to the menu bar.
func renderReadout() {
	readoutMu.Lock()
	style, rx, tx := readoutStyle, lastRx, lastTx
	readoutMu.Unlock()
	if rx == "" && tx == "" {
		return
	}
	setStatusText(styleByID(style).format(rx, tx))
}

func styleByID(id string) menuBarStyle {
	for _, s := range menuBarStyles {
		if s.ID == id {
			return s
		}
	}
	return menuBarStyles[0]
}

// menuBarStylesJSON returns the available styles and the current selection for
// the settings UI.
func menuBarStylesJSON() map[string]any {
	readoutMu.Lock()
	cur := readoutStyle
	readoutMu.Unlock()
	opts := make([]map[string]string, 0, len(menuBarStyles))
	for _, s := range menuBarStyles {
		opts = append(opts, map[string]string{"id": s.ID, "label": s.Label})
	}
	return map[string]any{"current": cur, "options": opts}
}

// setMenuBarStyle applies and persists a style, refreshing the menu bar at once.
func setMenuBarStyle(id string) {
	readoutMu.Lock()
	readoutStyle = styleByID(id).ID // normalize (ignore unknown ids)
	readoutMu.Unlock()
	saveReadoutStyle()
	renderReadout()
}

func loadReadoutStyle() {
	b, err := os.ReadFile(readoutPath)
	if err != nil {
		return
	}
	var p struct {
		Style string `json:"style"`
	}
	if json.Unmarshal(b, &p) == nil && p.Style != "" {
		readoutMu.Lock()
		readoutStyle = styleByID(p.Style).ID
		readoutMu.Unlock()
	}
}

func saveReadoutStyle() {
	if readoutPath == "" {
		return
	}
	readoutMu.Lock()
	style := readoutStyle
	readoutMu.Unlock()
	_ = os.MkdirAll(filepath.Dir(readoutPath), 0o755)
	if b, err := json.MarshalIndent(map[string]string{"style": style}, "", "  "); err == nil {
		_ = os.WriteFile(readoutPath, b, 0o644)
	}
}

func fetchRates(client *http.Client) (rx, tx float64, ok bool) {
	resp, err := client.Get(alertSockHost + "/api/snapshot")
	if err != nil {
		return 0, 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, 0, false
	}
	var s struct {
		RxPerSec float64 `json:"rxPerSec"`
		TxPerSec float64 `json:"txPerSec"`
	}
	if json.NewDecoder(resp.Body).Decode(&s) != nil {
		return 0, 0, false
	}
	return s.RxPerSec, s.TxPerSec, true
}

// compactRate formats a bytes/sec rate tersely for the menu bar (e.g. "1.2M",
// "30K", "0"). No "/s" suffix — the symbols already say it's a rate.
func compactRate(bps float64) string {
	const u = "KMGT"
	if bps < 1024 {
		return fmt.Sprintf("%.0f", bps)
	}
	v := bps / 1024
	i := 0
	for v >= 1024 && i < len(u)-1 {
		v /= 1024
		i++
	}
	if v < 10 {
		return fmt.Sprintf("%.1f%c", v, u[i])
	}
	return fmt.Sprintf("%.0f%c", v, u[i])
}
