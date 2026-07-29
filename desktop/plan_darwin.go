//go:build darwin

package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/doldoldol21/netscope/internal/alerts"
	"github.com/doldoldol21/netscope/internal/dataplan"
)

// planNetwork is one network counted (or countable) against the data plan.
type planNetwork struct {
	Iface    string `json:"iface"`
	Friendly string `json:"friendly"`
	Tether   bool   `json:"tether"`
	Bytes    int64  `json:"bytes"`   // rx+tx this cycle
	Counted  bool   `json:"counted"` // included in the plan total
}

// planResponse is what the dashboard's data-plan meter renders.
type planResponse struct {
	Config   dataplan.Config `json:"config"`
	Status   dataplan.Status `json:"status"`
	Networks []planNetwork   `json:"networks"`
}

// netUsageRow mirrors the daemon's /api/netusage entries.
type netUsageRow struct {
	Iface    string `json:"iface"`
	Friendly string `json:"friendly"`
	Tether   bool   `json:"tether"`
	RxBytes  int64  `json:"rxBytes"`
	TxBytes  int64  `json:"txBytes"`
}

// planUsage sums this cycle's bytes for the networks the plan meters, and
// returns every candidate network so the UI can show what's counted and let the
// user pick a specific one. cfg.Iface == "" means "every tethering network",
// which is the useful default: a phone's USB and hotspot interfaces both count
// against the same carrier allowance.
func planUsage(cfg dataplan.Config, now time.Time) (used int64, nets []planNetwork) {
	start := dataplan.CycleStart(now, cfg.StartDay)
	var rows []netUsageRow
	if !getJSON("/api/netusage?since="+strconv.FormatInt(start.Unix(), 10), &rows) {
		return 0, nil
	}
	for _, r := range rows {
		counted := r.Tether
		if cfg.Iface != "" {
			counted = r.Iface == cfg.Iface
		}
		total := r.RxBytes + r.TxBytes
		if counted {
			used += total
		}
		// Every network is listed, not just the auto-detected tethers: a phone
		// hotspot joined over Wi-Fi looks like plain "Wi-Fi" (en0) to macOS, so
		// the user has to be able to pick it by hand.
		nets = append(nets, planNetwork{
			Iface: r.Iface, Friendly: r.Friendly, Tether: r.Tether,
			Bytes: total, Counted: counted,
		})
	}
	return used, nets
}

// planStatusJSON builds the current plan view for the dashboard.
func planStatusJSON() planResponse {
	cfg := alertsConfigJSON().Plan
	now := time.Now()
	used, nets := planUsage(cfg, now)
	if nets == nil {
		nets = []planNetwork{}
	}
	return planResponse{Config: cfg, Status: dataplan.Compute(cfg, used, now), Networks: nets}
}

// setPlan replaces the plan config and persists it alongside the other alert
// thresholds. Saving re-arms the cycle's threshold alerts, which is what you
// want after changing the allowance.
func setPlan(cfg dataplan.Config) {
	if alertChecker == nil {
		return
	}
	full := alertChecker.Config()
	full.Plan = cfg
	alertChecker.SetConfig(full)
	_ = alerts.Save(alertCfgPath, full)
}

// handlePlan serves GET (current config + usage) and POST (new config) for the
// dashboard's data-plan meter. It lives on the app's loopback UI server rather
// than the daemon because the plan is a user preference, stored with the other
// alert settings in the user's config dir.
func handlePlan(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var cfg dataplan.Config
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if cfg.LimitBytes < 0 {
			cfg.LimitBytes = 0
		}
		if cfg.StartDay < 1 || cfg.StartDay > 31 {
			cfg.StartDay = 1
		}
		setPlan(cfg)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(planStatusJSON())
}
