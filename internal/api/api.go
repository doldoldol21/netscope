// Package api exposes the engine's live snapshot and the store's history over
// HTTP, and serves the embedded dashboard. It is consumed locally by the
// dashboard UI (and could back a Wails/native shell later).
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/doldoldol21/netscope/internal/buildinfo"
	"github.com/doldoldol21/netscope/internal/engine"
	"github.com/doldoldol21/netscope/internal/metered"
	"github.com/doldoldol21/netscope/internal/storage"
	"github.com/doldoldol21/netscope/internal/update"
	"github.com/doldoldol21/netscope/pkg/types"
)

// Capturer lets the API list and switch the capture interface. Implemented by
// the live capture supervisor; nil for demo/offline sources (which can't switch).
type Capturer interface {
	ListInterfaces() []types.NetIface
	PreferredInterface() string // "" means auto-detect
	SetPreferredInterface(name string) error
	Paused() bool
	SetPaused(p bool)
}

// Server wires the engine (live), store (history) and update checker to HTTP.
type Server struct {
	eng     *engine.Engine
	store   *storage.Store
	updater *update.Checker
	cap     Capturer
	metered *metered.Store // nil if metered tracking is unconfigured
}

// NewServer builds a Server. store, updater and capturer may be nil.
func NewServer(eng *engine.Engine, store *storage.Store, updater *update.Checker, capturer Capturer) *Server {
	return &Server{eng: eng, store: store, updater: updater, cap: capturer}
}

// SetMetered enables the metered-interface (tethering data) endpoints.
func (s *Server) SetMetered(m *metered.Store) { s.metered = m }

// Handler returns the API handler. netscope is app-only: the dashboard UI is
// served by the native app (which embeds internal/webui) — the daemon exposes
// data exclusively under /api over the unix socket and serves no static assets.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/snapshot", s.handleSnapshot)
	mux.HandleFunc("/api/live", s.handleLive)
	mux.HandleFunc("/api/apps", s.handleApps)
	mux.HandleFunc("/api/domains", s.handleDomains)
	mux.HandleFunc("/api/timeseries", s.handleTimeSeries)
	mux.HandleFunc("/api/summary", s.handleSummary)
	mux.HandleFunc("/api/version", s.handleVersion)
	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/api/interfaces", s.handleInterfaces)
	mux.HandleFunc("/api/ratehist", s.handleRateHist)
	mux.HandleFunc("/api/capture", s.handleCapture)
	mux.HandleFunc("/api/connections", s.handleConnections)
	mux.HandleFunc("/api/metered", s.handleMetered)
	return mux
}

// meteredPlan is one metered interface with its current-cycle usage.
type meteredPlan struct {
	Iface         string `json:"iface"`
	Label         string `json:"label"`
	BudgetBytes   uint64 `json:"budgetBytes"`
	CycleStartDay int    `json:"cycleStartDay"`
	UsedBytes     uint64 `json:"usedBytes"`
	CycleStart    int64  `json:"cycleStart"` // unix seconds (local midnight)
	OverBudget    bool   `json:"overBudget"`
}

// handleMetered: GET returns metered interfaces (with cycle usage) plus the list
// of available interfaces; POST sets/clears a metered interface.
//
//	POST body: {"iface":"en5","metered":true,"label":"SKT","budgetBytes":5368709120,"cycleStartDay":1}
func (s *Server) handleMetered(w http.ResponseWriter, r *http.Request) {
	if s.metered == nil || s.store == nil {
		http.Error(w, "metered tracking unavailable", http.StatusServiceUnavailable)
		return
	}
	if r.Method == http.MethodPost {
		var body struct {
			Iface         string `json:"iface"`
			Metered       bool   `json:"metered"`
			Label         string `json:"label"`
			BudgetBytes   uint64 `json:"budgetBytes"`
			CycleStartDay int    `json:"cycleStartDay"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Iface == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var err error
		if body.Metered {
			err = s.metered.Set(body.Iface, metered.Plan{
				Label: body.Label, BudgetBytes: body.BudgetBytes, CycleStartDay: body.CycleStartDay,
			})
		} else {
			err = s.metered.Remove(body.Iface)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	now := time.Now()
	plans := []meteredPlan{}
	for iface, p := range s.metered.Plans() {
		cs := metered.CycleStart(now, p.CycleStartDay)
		rx, tx, _ := s.store.IfaceUsageSince(iface, cs)
		used := rx + tx
		plans = append(plans, meteredPlan{
			Iface: iface, Label: p.Label, BudgetBytes: p.BudgetBytes,
			CycleStartDay: p.CycleStartDay, UsedBytes: used, CycleStart: cs,
			OverBudget: p.BudgetBytes > 0 && used >= p.BudgetBytes,
		})
	}
	sort.Slice(plans, func(i, j int) bool { return plans[i].Iface < plans[j].Iface })

	var ifaces []types.NetIface
	if s.cap != nil {
		ifaces = s.cap.ListInterfaces()
	}
	writeJSON(w, map[string]any{"plans": plans, "interfaces": ifaces})
}

// handleConnections returns the live connections active within the last window
// (default 15s; ?window=<seconds>), most-active first.
func (s *Server) handleConnections(w http.ResponseWriter, r *http.Request) {
	window := 15 * time.Second
	if q := r.URL.Query().Get("window"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 {
			window = time.Duration(n) * time.Second
		}
	}
	writeJSON(w, s.eng.Connections(window))
}

// handleCapture reports (GET) or sets (POST {"paused":true}) whether live
// capture is suspended. Pausing closes the pcap handle until resumed.
func (s *Server) handleCapture(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		if s.cap == nil {
			http.Error(w, "capture control not supported by this source", http.StatusNotImplemented)
			return
		}
		var body struct {
			Paused bool `json:"paused"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		s.cap.SetPaused(body.Paused)
		s.eng.SetPaused(body.Paused)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	paused := false
	if s.cap != nil {
		paused = s.cap.Paused()
	}
	writeJSON(w, map[string]any{"paused": paused})
}

// handleRateHist returns the recent per-second throughput samples, so the
// dashboard can seed its live chart immediately instead of from blank.
func (s *Server) handleRateHist(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.eng.RateHistory())
}

// handleInterfaces lists capturable interfaces (GET) and switches the capture
// interface (POST {"name":"en0"}; empty name = auto-detect). When the source
// can't switch (demo/offline), GET returns just the current capture interface.
func (s *Server) handleInterfaces(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		if s.cap == nil {
			http.Error(w, "interface switching not supported by this source", http.StatusNotImplemented)
			return
		}
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := s.cap.SetPreferredInterface(body.Name); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	resp := map[string]any{
		"current":  s.eng.Snapshot().Interface, // interface actually being captured
		"selected": "",                         // user's preference ("" = auto)
		"options":  []types.NetIface{},
	}
	if s.cap != nil {
		resp["selected"] = s.cap.PreferredInterface()
		resp["options"] = s.cap.ListInterfaces()
	}
	writeJSON(w, resp)
}

// handleVersion reports the running version and any available update.
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	if s.updater != nil {
		writeJSON(w, s.updater.Status())
		return
	}
	writeJSON(w, update.Status{Current: buildinfo.Version})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"status": "ok", "persistent": s.store != nil})
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.eng.Snapshot())
}

// handleLive streams snapshots as Server-Sent Events, one per second.
func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	enc := json.NewEncoder(w)
	send := func() {
		fmt.Fprint(w, "data: ")
		_ = enc.Encode(s.eng.Snapshot()) // Encode appends a newline
		fmt.Fprint(w, "\n")
		flusher.Flush()
	}
	send() // immediate first frame
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			send()
		}
	}
}

func (s *Server) handleApps(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSON(w, s.eng.Snapshot().Apps)
		return
	}
	since, until := parseRange(r)
	apps, err := s.store.Apps(since, until)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, apps)
}

func (s *Server) handleDomains(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSON(w, s.eng.Snapshot().Domains)
		return
	}
	since, until := parseRange(r)
	var (
		domains []types.DomainStat
		err     error
	)
	if app := r.URL.Query().Get("app"); app != "" {
		domains, err = s.store.DomainsForApp(app, since, until) // per-app drill-down
	} else {
		domains, err = s.store.Domains(since, until)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, domains)
}

func (s *Server) handleTimeSeries(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSON(w, []types.TimePoint{})
		return
	}
	since, until := parseRange(r)
	step := parseStep(r, until.Sub(since))
	var (
		points []types.TimePoint
		err    error
	)
	if app := r.URL.Query().Get("app"); app != "" {
		points, err = s.store.AppTimeSeries(app, since, until, step) // per-app drill-down
	} else {
		points, err = s.store.TimeSeries(since, until, step)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, points)
}

// topItem is a name+bytes pair for the summary cards.
type topItem struct {
	Name  string `json:"name"`
	Bytes uint64 `json:"bytes"`
}

// summary is the aggregate the dashboard cards consume.
type summary struct {
	Range       string  `json:"range"`
	TotalRx     uint64  `json:"totalRx"`
	TotalTx     uint64  `json:"totalTx"`
	AppCount    int     `json:"appCount"`
	DomainCount int     `json:"domainCount"`
	TopApp      topItem `json:"topApp"`
	TopDomain   topItem `json:"topDomain"`
}

// handleSummary returns totals, counts and the top app/domain for the range,
// derived from the stored aggregates (or the live snapshot when storeless).
func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	var out summary
	since, until := parseRange(r)
	out.Range = r.URL.Query().Get("range")
	if out.Range == "" {
		out.Range = "today"
	}

	var apps []types.AppTraffic
	var domains []types.DomainStat
	if s.store == nil {
		snap := s.eng.Snapshot()
		apps, domains = snap.Apps, snap.Domains
	} else {
		var err error
		if apps, err = s.store.Apps(since, until); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if domains, err = s.store.Domains(since, until); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	out.AppCount = len(apps)
	out.DomainCount = len(domains)
	for _, a := range apps {
		out.TotalRx += a.RxBytes
		out.TotalTx += a.TxBytes
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].Total() > apps[j].Total() })
	sort.Slice(domains, func(i, j int) bool { return domains[i].Total() > domains[j].Total() })
	if len(apps) > 0 {
		out.TopApp = topItem{Name: apps[0].Name, Bytes: apps[0].Total()}
	}
	if len(domains) > 0 {
		out.TopDomain = topItem{Name: domains[0].Domain, Bytes: domains[0].Total()}
	}
	writeJSON(w, out)
}

// parseRange interprets ?range=hour|today|week|day, defaulting to today.
func parseRange(r *http.Request) (since, until time.Time) {
	now := time.Now()
	until = now
	switch r.URL.Query().Get("range") {
	case "hour":
		since = now.Add(-time.Hour)
	case "day", "24h":
		since = now.Add(-24 * time.Hour)
	case "week":
		since = now.Add(-7 * 24 * time.Hour)
	case "month":
		since = now.Add(-30 * 24 * time.Hour)
	case "today", "":
		y, m, d := now.Date()
		since = time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	default:
		y, m, d := now.Date()
		since = time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	}
	return since, until
}

// parseStep picks a time-series bucket size: explicit ?step=<seconds> or a
// sensible default proportional to the window (~120 points).
func parseStep(r *http.Request, window time.Duration) time.Duration {
	if v := r.URL.Query().Get("step"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}
	step := window / 120
	if step < 10*time.Second {
		step = 10 * time.Second
	}
	return step
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
