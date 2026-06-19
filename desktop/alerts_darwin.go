//go:build darwin

package main

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/doldoldol21/netscope/internal/alerts"
)

// alertChecker holds the running threshold checker and where its config lives.
var (
	alertChecker  *alerts.Checker
	alertCfgPath  string
	alertHTTP     *http.Client
	alertSockHost = "http://x"
)

// startAlertsLoop loads the saved thresholds and polls today's totals over the
// socket every 30s, posting a notification when a threshold is crossed.
func startAlertsLoop(client *http.Client) {
	alertCfgPath = alerts.ConfigPath()
	alertChecker = alerts.New(alerts.Load(alertCfgPath))
	alertHTTP = client
	go func() {
		// Let the daemon come up before the first check.
		time.Sleep(8 * time.Second)
		for {
			runAlertCheck()
			time.Sleep(30 * time.Second)
		}
	}()
}

// runAlertCheck fetches today's total and per-app bytes and fires due alerts.
func runAlertCheck() {
	if alertChecker == nil || alertHTTP == nil {
		return
	}
	cfg := alertChecker.Config()
	if cfg.DailyTotalBytes == 0 && cfg.PerAppBytes == 0 {
		return // nothing armed
	}

	var sum struct {
		TotalRx int64 `json:"totalRx"`
		TotalTx int64 `json:"totalTx"`
	}
	if !getJSON("/api/summary?range=today", &sum) {
		return
	}
	perApp := map[string]int64{}
	if cfg.PerAppBytes > 0 {
		var apps []struct {
			Name    string `json:"name"`
			RxBytes int64  `json:"rxBytes"`
			TxBytes int64  `json:"txBytes"`
		}
		if getJSON("/api/apps?range=today", &apps) {
			for _, a := range apps {
				perApp[a.Name] += a.RxBytes + a.TxBytes
			}
		}
	}

	day := time.Now().Format("2006-01-02")
	for _, a := range alertChecker.Check(day, sum.TotalRx+sum.TotalTx, perApp) {
		notify(a.Title, a.Body)
	}
}

// getJSON GETs a daemon API path over the socket and decodes it into v.
func getJSON(path string, v any) bool {
	resp, err := alertHTTP.Get(alertSockHost + path)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	return json.NewDecoder(resp.Body).Decode(v) == nil
}

// alertsConfigJSON returns the current thresholds for the settings UI.
func alertsConfigJSON() alerts.Config {
	if alertChecker == nil {
		return alerts.Config{}
	}
	return alertChecker.Config()
}

// setAlertsFromEvent applies thresholds sent by the settings UI and persists
// them. The payload is the first EventsOn arg: {dailyTotalBytes, perAppBytes}.
func setAlertsFromEvent(data ...interface{}) {
	if alertChecker == nil || len(data) == 0 {
		return
	}
	m, ok := data[0].(map[string]interface{})
	if !ok {
		return
	}
	num := func(k string) int64 {
		if f, ok := m[k].(float64); ok && f > 0 {
			return int64(f)
		}
		return 0
	}
	cfg := alerts.Config{
		DailyTotalBytes: num("dailyTotalBytes"),
		PerAppBytes:     num("perAppBytes"),
	}
	alertChecker.SetConfig(cfg)
	_ = alerts.Save(alertCfgPath, cfg)
	// Re-evaluate immediately so a freshly-lowered threshold can fire now.
	go runAlertCheck()
}
