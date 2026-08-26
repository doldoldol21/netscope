//go:build darwin

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/doldoldol21/netscope/internal/alerts"
	"github.com/doldoldol21/netscope/internal/i18n"
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
// style in settings; segs() turns a (rx,tx) pair into colored runs. The label
// shown in settings is resolved through i18n at read time, keyed by ID.
type menuBarStyle struct {
	ID   string `json:"id"`
	segs func(rx, tx string) []seg
}

// menuBarStyles are the selectable readout styles (symbol variants). The first
// is the default.
var menuBarStyles = []menuBarStyle{
	{ID: "arrows", segs: func(rx, tx string) []seg {
		return []seg{{'d', "↓" + rx}, {'n', " "}, {'u', "↑" + tx}}
	}},
	{ID: "triangles", segs: func(rx, tx string) []seg {
		return []seg{{'d', "▼" + rx}, {'n', " "}, {'u', "▲" + tx}}
	}},
	{ID: "caret", segs: func(rx, tx string) []seg {
		return []seg{{'d', "⇣" + rx}, {'n', " "}, {'u', "⇡" + tx}}
	}},
	{ID: "suffix", segs: func(rx, tx string) []seg {
		return []seg{{'d', rx + "↓"}, {'n', " "}, {'u', tx + "↑"}}
	}},
	{ID: "downonly", segs: func(rx, tx string) []seg {
		return []seg{{'d', "↓" + rx}}
	}},
	{ID: "icononly", segs: func(rx, tx string) []seg { return nil }},
}

// modeMarker returns the glyph standing in for the rates when they would not
// mean what they appear to. Zeros stay reserved for a link that is genuinely
// connected and quiet.
func modeMarker(m readoutMode) (string, bool) {
	switch m {
	case readoutPaused:
		return "⏸", true
	case readoutStopped:
		return "—", true
	case readoutBehind:
		// Not a zero: a zero here would claim the network is quiet when the
		// link is busy and we simply cannot see it.
		return "⚠︎", true
	default:
		return "", false
	}
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
	// lastMode is what the readout should currently be saying. It starts stopped
	// so the seconds before the first successful poll don't show a zero rate the
	// daemon never reported.
	lastMode    = readoutStopped
	readoutHTTP *http.Client
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
			st, ok := fetchCapture(client)
			if ok {
				readoutMu.Lock()
				lastMode = st.mode()
				if lastMode == readoutRates {
					lastRx, lastTx = compactRate(st.rx), compactRate(st.tx)
					lastTotalBps = st.rx + st.tx
				} else {
					// Paused, stopped, or not seeing the link: no rate worth
					// animating, and the numbers would be indistinguishable
					// from a quiet link. A marker goes out instead.
					lastRx, lastTx, lastTotalBps = "", "", 0
				}
				readoutMu.Unlock()
				renderReadout()
			} else {
				// Daemon unreachable: clear the cached rates so the icon
				// animation falls back to idle instead of forever animating at
				// the last-seen throughput (a dead daemon would otherwise look
				// like steady mid-traffic).
				readoutMu.Lock()
				lastRx, lastTx, lastTotalBps = "", "", 0
				lastMode = readoutStopped
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
	style, color, rx, tx, mode := readoutStyle, readoutColor, lastRx, lastTx, lastMode
	readoutMu.Unlock()
	// "icon only" means the user asked for no text at all; respect that in every
	// state rather than sneaking a marker back in.
	if styleByID(style).ID == "icononly" {
		setStatusText("")
		return
	}
	if marker, ok := modeMarker(mode); ok {
		// Neutral, never colored: these are states, not throughput.
		setStatusText(encodeSegs([]seg{{'n', marker}}, false))
		return
	}
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
		opts = append(opts, map[string]string{"id": s.ID, "label": i18n.T("menubar.style." + s.ID)})
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

// captureState is what the menu bar needs to know from a snapshot: the rates,
// and whether those rates describe live capture at all.
type captureState struct {
	rx, tx    float64
	paused    bool
	capturing bool
	// linkBps is what the kernel says is crossing the capture interface. It is
	// the only way to tell a quiet link from one capture is missing, which
	// otherwise both read as zero.
	linkBps float64
}

// behindBytes is how much the link must be moving, while capture measures
// nothing, before the readout says capture is missing it. Keepalives and ARP
// chatter run well under this; a real transfer runs far above it.
const behindBytes = 4096

// mode says what the menu bar should show. Zeros are only honest when capture is
// actually running — otherwise they read as "nothing is happening on your
// network" when the truth is "nothing is being measured".
type readoutMode int

const (
	readoutRates   readoutMode = iota // capturing: the numbers mean what they say
	readoutPaused                     // the user stopped capture
	readoutStopped                    // between sources: re-opening, or no interface
	readoutBehind                     // a source is running but not seeing the link
)

func (c captureState) mode() readoutMode {
	switch {
	case c.paused:
		return readoutPaused
	case !c.capturing:
		return readoutStopped
	case c.behind():
		return readoutBehind
	default:
		return readoutRates
	}
}

// behind reports whether the link is demonstrably busy while capture measures
// nothing. Both halves are required: a zero link rate means "quiet or unknown",
// neither of which justifies a warning, and any captured traffic at all means
// capture is working — it need not match the kernel byte for byte, and never
// will, since the kernel also counts framing and traffic the decoder drops.
func (c captureState) behind() bool {
	return c.rx+c.tx == 0 && c.linkBps >= behindBytes
}

func fetchCapture(client *http.Client) (captureState, bool) {
	resp, err := client.Get(alertSockHost + "/api/snapshot")
	if err != nil {
		return captureState{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return captureState{}, false
	}
	return decodeCapture(resp.Body)
}

// decodeCapture reads a snapshot body into the state the menu bar needs.
func decodeCapture(r io.Reader) (captureState, bool) {
	// Capturing is a pointer so a missing field is distinguishable from false.
	// The app can outrun the daemon — the root-owned helper copy is refreshed on
	// demand, so a newer app routinely talks to an older daemon — and decoding
	// an absent field as "not capturing" would pin the menu bar to the stopped
	// marker forever. Absent means "this daemon can't tell us", which is not
	// grounds for claiming capture has stopped.
	var s struct {
		RxPerSec  float64 `json:"rxPerSec"`
		TxPerSec  float64 `json:"txPerSec"`
		Paused    bool    `json:"paused"`
		Capturing *bool   `json:"capturing"`
		LinkBps   float64 `json:"linkBytesPerSec"`
	}
	if json.NewDecoder(r).Decode(&s) != nil {
		return captureState{}, false
	}
	return captureState{
		rx:        s.RxPerSec,
		tx:        s.TxPerSec,
		paused:    s.Paused,
		capturing: s.Capturing == nil || *s.Capturing,
		linkBps:   s.LinkBps,
	}, true
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
