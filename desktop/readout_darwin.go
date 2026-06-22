//go:build darwin

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// readoutInterval is how often the menu-bar rate text refreshes. The menu bar is
// always visible, so this polls continuously (unlike the popover's live stream,
// which pauses when hidden) — but 2s is light and keeps the numbers steady.
const readoutInterval = 2 * time.Second

// startMenuBarReadout polls the daemon's live snapshot and shows the current
// download/upload rate next to the menu-bar icon (RunCat-style).
func startMenuBarReadout(client *http.Client) {
	go func() {
		time.Sleep(6 * time.Second) // let the daemon come up first
		for {
			rx, tx, ok := fetchRates(client)
			if ok {
				setStatusText(fmt.Sprintf("↓%s ↑%s", compactRate(rx), compactRate(tx)))
			} else {
				setStatusText("") // daemon unreachable: icon only
			}
			time.Sleep(readoutInterval)
		}
	}()
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
// "30K", "0"). No "/s" suffix — the ↓↑ arrows already say it's a rate.
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
