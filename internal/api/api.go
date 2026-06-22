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
	"github.com/doldoldol21/netscope/internal/storage"
	"github.com/doldoldol21/netscope/internal/update"
	"github.com/doldoldol21/netscope/pkg/types"
)

// Server wires the engine (live), store (history) and update checker to HTTP.
type Server struct {
	eng     *engine.Engine
	store   *storage.Store
	updater *update.Checker
}

// NewServer builds a Server. store and updater may be nil.
func NewServer(eng *engine.Engine, store *storage.Store, updater *update.Checker) *Server {
	return &Server{eng: eng, store: store, updater: updater}
}

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
	return mux
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
