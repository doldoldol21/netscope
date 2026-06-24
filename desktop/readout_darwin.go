//go:build darwin

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/doldoldol21/netscope/internal/alerts"
)

// readoutInterval is how often the menu-bar rate text refreshes. The menu bar is
// always visible, so this polls continuously (unlike the popover's live stream,
// which pauses when hidden) — but 2s is light and keeps the numbers steady.
const readoutInterval = 2 * time.Second

// seg is a colored run of the menu-bar text. tag: 'd' download, 'u' upload,
// 'n' neutral (used for separators and when color is off).
type seg struct {
	tag  byte
	text string
}

// menuBarStyle controls how the live rate renders next to the icon. Users pick a
// style in settings; segs() turns a (rx,tx) pair into colored runs.
type menuBarStyle struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	segs  func(rx, tx string) []seg
}

// menuBarStyles are the selectable readout styles (symbol variants). The first
// is the default.
var menuBarStyles = []menuBarStyle{
	{ID: "arrows", Label: "Arrows", segs: func(rx, tx string) []seg {
		return []seg{{'d', "↓" + rx}, {'n', " "}, {'u', "↑" + tx}}
	}},
	{ID: "triangles", Label: "Triangles", segs: func(rx, tx string) []seg {
		return []seg{{'d', "▼" + rx}, {'n', " "}, {'u', "▲" + tx}}
	}},
	{ID: "caret", Label: "Carets", segs: func(rx, tx string) []seg {
		return []seg{{'d', "⇣" + rx}, {'n', " "}, {'u', "⇡" + tx}}
	}},
	{ID: "suffix", Label: "Suffix", segs: func(rx, tx string) []seg {
		return []seg{{'d', rx + "↓"}, {'n', " "}, {'u', tx + "↑"}}
	}},
	{ID: "downonly", Label: "Download only", segs: func(rx, tx string) []seg {
		return []seg{{'d', "↓" + rx}}
	}},
	{ID: "icononly", Label: "Icon only", segs: func(rx, tx string) []seg { return nil }},
}

var (
	readoutMu    sync.Mutex
	readoutStyle = "arrows"
	readoutColor = false
	readoutAnim  = true // animate the menu-bar icon with traffic by default
	readoutPath  string
	lastRx       string
	lastTx       string
	lastTotalBps float64 // most recent rx+tx, drives the icon animation speed
	readoutHTTP  *http.Client
)

// currentRateBps returns the last-seen total throughput (rx+tx) in bytes/sec and
// whether icon animation is enabled — read by the menu-bar animator.
func currentRateBps() (bps float64, animate bool) {
	readoutMu.Lock()
	defer readoutMu.Unlock()
	return lastTotalBps, readoutAnim
}

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
				lastTotalBps = rx + tx
				readoutMu.Unlock()
				renderReadout()
			} else {
				// Daemon unreachable: clear the cached rates so the icon
				// animation falls back to idle instead of forever animating at
				// the last-seen throughput (a dead daemon would otherwise look
				// like steady mid-traffic).
				readoutMu.Lock()
				lastRx, lastTx, lastTotalBps = "", "", 0
				readoutMu.Unlock()
				setStatusText("") // icon only
			}
			time.Sleep(readoutInterval)
		}
	}()
}

// renderReadout formats the last-seen rates with the current style + color and
// pushes the colored-segment string to the menu bar.
func renderReadout() {
	readoutMu.Lock()
	style, color, rx, tx := readoutStyle, readoutColor, lastRx, lastTx
	readoutMu.Unlock()
	if rx == "" && tx == "" {
		return
	}
	setStatusText(encodeSegs(styleByID(style).segs(rx, tx), color))
}

// encodeSegs serializes colored runs into the cgo protocol: "<tag>:<text>"
// joined by US (0x1f). With color off every run is neutral.
func encodeSegs(segs []seg, color bool) string {
	out := ""
	for i, s := range segs {
		if i > 0 {
			out += "\x1f"
		}
		tag := s.tag
		if !color {
			tag = 'n'
		}
		out += string(tag) + ":" + s.text
	}
	return out
}

func styleByID(id string) menuBarStyle {
	for _, s := range menuBarStyles {
		if s.ID == id {
			return s
		}
	}
	return menuBarStyles[0]
}

// menuBarStylesJSON returns the available styles and the current selection +
// color preference for the settings UI.
func menuBarStylesJSON() map[string]any {
	readoutMu.Lock()
	cur, color := readoutStyle, readoutColor
	readoutMu.Unlock()
	opts := make([]map[string]string, 0, len(menuBarStyles))
	for _, s := range menuBarStyles {
		opts = append(opts, map[string]string{"id": s.ID, "label": s.Label})
	}
	readoutMu.Lock()
	anim := readoutAnim
	readoutMu.Unlock()
	return map[string]any{"current": cur, "color": color, "animate": anim, "options": opts}
}

// setMenuBarAnim toggles (and persists) the animated menu-bar icon. When turned
// off the animator drops back to the static idle glyph.
func setMenuBarAnim(on bool) {
	readoutMu.Lock()
	readoutAnim = on
	readoutMu.Unlock()
	saveReadoutStyle()
	if !on {
		setStatusImage(statusIcon())
	}
}

// setMenuBarStyle applies and persists a style, refreshing the menu bar at once.
func setMenuBarStyle(id string) {
	readoutMu.Lock()
	readoutStyle = styleByID(id).ID // normalize (ignore unknown ids)
	readoutMu.Unlock()
	saveReadoutStyle()
	renderReadout()
}

// setMenuBarColor toggles per-direction coloring (green ↓ / orange ↑).
func setMenuBarColor(on bool) {
	readoutMu.Lock()
	readoutColor = on
	readoutMu.Unlock()
	saveReadoutStyle()
	renderReadout()
}

type readoutPrefs struct {
	Style   string `json:"style"`
	Color   bool   `json:"color"`
	Animate *bool  `json:"animate"` // pointer so a missing field keeps the default (on)
}

func loadReadoutStyle() {
	b, err := os.ReadFile(readoutPath)
	if err != nil {
		return
	}
	var p readoutPrefs
	if json.Unmarshal(b, &p) == nil {
		readoutMu.Lock()
		if p.Style != "" {
			readoutStyle = styleByID(p.Style).ID
		}
		readoutColor = p.Color
		if p.Animate != nil {
			readoutAnim = *p.Animate
		}
		readoutMu.Unlock()
	}
}

func saveReadoutStyle() {
	if readoutPath == "" {
		return
	}
	readoutMu.Lock()
	anim := readoutAnim
	p := readoutPrefs{Style: readoutStyle, Color: readoutColor, Animate: &anim}
	readoutMu.Unlock()
	_ = os.MkdirAll(filepath.Dir(readoutPath), 0o755)
	if b, err := json.MarshalIndent(p, "", "  "); err == nil {
		_ = os.WriteFile(readoutPath, b, 0o644)
	}
}

// themePath is where the dashboard's theme choice is persisted, alongside the
// other GUI prefs (menubar.json, alert config).
func themePath() string {
	cp := alerts.ConfigPath()
	if cp == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(cp), "theme")
}

// loadTheme returns the persisted dashboard theme ("auto" if unset/invalid).
func loadTheme() string {
	p := themePath()
	if p == "" {
		return "auto"
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "auto"
	}
	switch t := strings.TrimSpace(string(b)); t {
	case "light", "dark", "auto":
		return t
	default:
		return "auto"
	}
}

// saveTheme persists the dashboard theme choice (ignored if invalid).
func saveTheme(theme string) {
	switch theme {
	case "light", "dark", "auto":
	default:
		return
	}
	p := themePath()
	if p == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, []byte(theme), 0o644)
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
