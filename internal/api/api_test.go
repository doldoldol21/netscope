package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/doldoldol21/netscope/internal/buildinfo"
	"github.com/doldoldol21/netscope/internal/dnscache"
	"github.com/doldoldol21/netscope/internal/engine"
	"github.com/doldoldol21/netscope/internal/storage"
	"github.com/doldoldol21/netscope/internal/update"
	"github.com/doldoldol21/netscope/pkg/types"
)

// fakeResolver always returns the same process.
type fakeResolver struct{}

func (f fakeResolver) Lookup(key types.ConnKey) (types.Process, bool) {
	return types.Process{PID: 42, Name: "testapp", Path: "/usr/bin/testapp"}, true
}

// fakeCapturer satisfies Capturer with canned data.
type fakeCapturer struct {
	pref string
	all  []types.NetIface
}

func (f *fakeCapturer) ListInterfaces() []types.NetIface { return f.all }
func (f *fakeCapturer) PreferredInterface() string       { return f.pref }
func (f *fakeCapturer) SetPreferredInterface(n string) error {
	f.pref = n
	return nil
}
func (f *fakeCapturer) Paused() bool { return false }

func (f *fakeCapturer) SetPaused(p bool) {}

// newTestServer builds an API server backed by in-memory storage.
// Set store=nil or capturer=nil to test degraded modes.
func newTestServer(t *testing.T) (*Server, *storage.Store, *fakeCapturer, func()) {
	t.Helper()

	store, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}

	dns := dnscache.New(24*time.Hour, 1000)
	res := fakeResolver{}
	eng := engine.New(engine.Config{Interface: "en0"}, res, dns, store)

	cap := &fakeCapturer{
		pref: "",
		all: []types.NetIface{
			{Name: "en0", Display: "Wi-Fi", Friendly: "Wi-Fi", Kind: "wifi", Up: true, Active: true},
			{Name: "en1", Display: "Ethernet", Friendly: "Thunderbolt", Kind: "ethernet", Up: true, Active: false},
		},
	}

	updater := update.NewChecker(buildinfo.Repo, buildinfo.Version, time.Hour)
	srv := NewServer(eng, store, updater, cap)

	cleanup := func() {
		store.Close()
	}

	return srv, store, cap, cleanup
}

// do executes an HTTP request against the server and returns the recorder.
func do(h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// jsonBody decodes the response body into v.
func jsonBody(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.NewDecoder(rec.Body).Decode(v); err != nil {
		t.Fatalf("decode body: %v\nbody=%s", err, rec.Body.String())
	}
}

func TestHealth(t *testing.T) {
	srv, store, _, cleanup := newTestServer(t)
	defer cleanup()

	h := srv.Handler()
	rec := do(h, "GET", "/api/health", nil)

	if rec.Code != 200 {
		t.Fatalf("health: status=%d", rec.Code)
	}
	var got map[string]any
	jsonBody(t, rec, &got)
	if got["status"] != "ok" {
		t.Errorf("status=%v, want ok", got["status"])
	}
	if got["persistent"] != true {
		t.Errorf("persistent=%v, want true", got["persistent"])
	}

	// Without store.
	_ = store
	srvNoStore := NewServer(srv.eng, nil, nil, nil)
	rec2 := do(srvNoStore.Handler(), "GET", "/api/health", nil)
	var got2 map[string]any
	jsonBody(t, rec2, &got2)
	if got2["persistent"] != false {
		t.Errorf("persistent (no store)=%v, want false", got2["persistent"])
	}
}

func TestSnapshot(t *testing.T) {
	srv, _, _, cleanup := newTestServer(t)
	defer cleanup()

	h := srv.Handler()
	rec := do(h, "GET", "/api/snapshot", nil)

	if rec.Code != 200 {
		t.Fatalf("snapshot: status=%d", rec.Code)
	}
	var snap types.Snapshot
	jsonBody(t, rec, &snap)
	// Engine hasn't been started, so snapshot data is zero-valued.
	if snap.Interface != "" && snap.Interface != "en0" {
		t.Errorf("interface=%q, want '' or en0", snap.Interface)
	}
}

func TestLive(t *testing.T) {
	srv, _, _, cleanup := newTestServer(t)
	defer cleanup()

	h := srv.Handler()
	// SSE endpoint streams forever; use a cancelable context to stop it.
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", "/api/live", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()

	// Let one frame arrive, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done // wait for the handler to finish

	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("live Content-Type=%q, want text/event-stream", ct)
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, "data: ") {
		t.Errorf("live body should start with 'data: ', got %q", body[:min(len(body), 50)])
	}
}

func TestApps(t *testing.T) {
	srv, _, _, cleanup := newTestServer(t)
	defer cleanup()

	h := srv.Handler()
	rec := do(h, "GET", "/api/apps?range=today", nil)
	if rec.Code != 200 {
		t.Fatalf("apps: status=%d", rec.Code)
	}
	var apps []types.AppTraffic
	jsonBody(t, rec, &apps)
	// Empty store returns null (JSON null), which decodes to nil slice.
	_ = apps

	// Test without store — should return snapshot apps.
	srvNoStore := NewServer(srv.eng, nil, nil, nil)
	rec2 := do(srvNoStore.Handler(), "GET", "/api/apps?range=today", nil)
	if rec2.Code != 200 {
		t.Fatalf("apps (no store): status=%d", rec2.Code)
	}
}

func TestDomains(t *testing.T) {
	srv, _, _, cleanup := newTestServer(t)
	defer cleanup()

	h := srv.Handler()
	rec := do(h, "GET", "/api/domains?range=today", nil)
	if rec.Code != 200 {
		t.Fatalf("domains: status=%d", rec.Code)
	}
	var domains []types.DomainStat
	jsonBody(t, rec, &domains)
	// Empty store returns null — nil decode is fine.
	_ = domains

	// Per-app drill-down.
	rec2 := do(h, "GET", "/api/domains?range=today&app=Safari", nil)
	if rec2.Code != 200 {
		t.Fatalf("domains (with app): status=%d", rec2.Code)
	}

	// Without store.
	srvNoStore := NewServer(srv.eng, nil, nil, nil)
	rec3 := do(srvNoStore.Handler(), "GET", "/api/domains?range=today", nil)
	if rec3.Code != 200 {
		t.Fatalf("domains (no store): status=%d", rec3.Code)
	}
}

func TestTimeSeries(t *testing.T) {
	srv, _, _, cleanup := newTestServer(t)
	defer cleanup()

	h := srv.Handler()
	rec := do(h, "GET", "/api/timeseries?range=hour", nil)
	if rec.Code != 200 {
		t.Fatalf("timeseries: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var pts []types.TimePoint
	jsonBody(t, rec, &pts)

	// Per-app.
	rec2 := do(h, "GET", "/api/timeseries?range=hour&app=Safari", nil)
	if rec2.Code != 200 {
		t.Fatalf("timeseries (with app): status=%d", rec2.Code)
	}

	// Without store.
	srvNoStore := NewServer(srv.eng, nil, nil, nil)
	rec3 := do(srvNoStore.Handler(), "GET", "/api/timeseries?range=hour", nil)
	if rec3.Code != 200 {
		t.Fatalf("timeseries (no store): status=%d", rec3.Code)
	}
	var emptyPts []types.TimePoint
	jsonBody(t, rec3, &emptyPts)
	if len(emptyPts) != 0 {
		t.Errorf("timeseries (no store) has %d points, want 0", len(emptyPts))
	}
}

func TestSummary(t *testing.T) {
	srv, _, _, cleanup := newTestServer(t)
	defer cleanup()

	h := srv.Handler()
	rec := do(h, "GET", "/api/summary?range=today", nil)
	if rec.Code != 200 {
		t.Fatalf("summary: status=%d", rec.Code)
	}
	var s summary
	jsonBody(t, rec, &s)
	if s.Range != "today" {
		t.Errorf("range=%q, want today", s.Range)
	}

	// Default range.
	rec2 := do(h, "GET", "/api/summary", nil)
	if rec2.Code != 200 {
		t.Fatalf("summary (no range): status=%d", rec2.Code)
	}
	var s2 summary
	jsonBody(t, rec2, &s2)
	if s2.Range != "today" {
		t.Errorf("default range=%q, want today", s2.Range)
	}

	// Without store.
	srvNoStore := NewServer(srv.eng, nil, nil, nil)
	rec3 := do(srvNoStore.Handler(), "GET", "/api/summary?range=today", nil)
	if rec3.Code != 200 {
		t.Fatalf("summary (no store): status=%d", rec3.Code)
	}
}

func TestVersion(t *testing.T) {
	srv, _, _, cleanup := newTestServer(t)
	defer cleanup()

	h := srv.Handler()
	rec := do(h, "GET", "/api/version", nil)
	if rec.Code != 200 {
		t.Fatalf("version: status=%d", rec.Code)
	}
	var st update.Status
	jsonBody(t, rec, &st)
	if st.Current != buildinfo.Version {
		t.Errorf("current=%q, want %q", st.Current, buildinfo.Version)
	}

	// Without updater.
	srvNoUpdate := NewServer(srv.eng, nil, nil, nil)
	rec2 := do(srvNoUpdate.Handler(), "GET", "/api/version", nil)
	if rec2.Code != 200 {
		t.Fatalf("version (no updater): status=%d", rec2.Code)
	}
	var st2 update.Status
	jsonBody(t, rec2, &st2)
	if st2.Current == "" {
		t.Error("version (no updater) has empty current")
	}
}

func TestInterfaces_GET(t *testing.T) {
	srv, _, cap, cleanup := newTestServer(t)
	defer cleanup()

	h := srv.Handler()
	rec := do(h, "GET", "/api/interfaces", nil)
	if rec.Code != 200 {
		t.Fatalf("interfaces GET: status=%d", rec.Code)
	}
	var got map[string]any
	jsonBody(t, rec, &got)
	opts, ok := got["options"].([]any)
	if !ok {
		t.Fatal("options not an array")
	}
	if len(opts) != 2 {
		t.Errorf("options length=%d, want 2", len(opts))
	}
	if got["selected"] != "" {
		t.Errorf("selected=%v, want ''", got["selected"])
	}

	// Without capturer.
	_ = cap
	srvNoCap := NewServer(srv.eng, nil, nil, nil)
	rec2 := do(srvNoCap.Handler(), "GET", "/api/interfaces", nil)
	if rec2.Code != 200 {
		t.Fatalf("interfaces (no cap): status=%d", rec2.Code)
	}
}

func TestInterfaces_POST(t *testing.T) {
	srv, _, cap, cleanup := newTestServer(t)
	defer cleanup()

	h := srv.Handler()
	rec := do(h, "POST", "/api/interfaces", map[string]string{"name": "en1"})
	if rec.Code != 204 {
		t.Fatalf("interfaces POST: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if cap.pref != "en1" {
		t.Errorf("pref=%q after switch, want en1", cap.pref)
	}

	// Without capturer.
	srvNoCap := NewServer(srv.eng, nil, nil, nil)
	rec2 := do(srvNoCap.Handler(), "POST", "/api/interfaces", map[string]string{"name": "en0"})
	if rec2.Code != http.StatusNotImplemented {
		t.Errorf("interfaces POST (no cap): status=%d, want %d", rec2.Code, http.StatusNotImplemented)
	}

	// Bad body.
	rec3 := do(h, "POST", "/api/interfaces", "not json")
	if rec3.Code != http.StatusBadRequest {
		t.Errorf("interfaces POST (bad body): status=%d, want %d", rec3.Code, http.StatusBadRequest)
	}
}

func TestCapture_GET(t *testing.T) {
	srv, _, _, cleanup := newTestServer(t)
	defer cleanup()

	h := srv.Handler()
	rec := do(h, "GET", "/api/capture", nil)
	if rec.Code != 200 {
		t.Fatalf("capture GET: status=%d", rec.Code)
	}
	var got map[string]any
	jsonBody(t, rec, &got)
	if paused, ok := got["paused"]; !ok || paused != false {
		t.Errorf("paused=%v, want false", paused)
	}

	// Without capturer.
	srvNoCap := NewServer(srv.eng, nil, nil, nil)
	rec2 := do(srvNoCap.Handler(), "GET", "/api/capture", nil)
	if rec2.Code != 200 {
		t.Fatalf("capture GET (no cap): status=%d", rec2.Code)
	}
}

func TestCapture_POST(t *testing.T) {
	srv, _, _, cleanup := newTestServer(t)
	defer cleanup()

	h := srv.Handler()
	rec := do(h, "POST", "/api/capture", map[string]any{"paused": true})
	if rec.Code != 204 {
		t.Fatalf("capture POST: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Without capturer.
	srvNoCap := NewServer(srv.eng, nil, nil, nil)
	rec2 := do(srvNoCap.Handler(), "POST", "/api/capture", map[string]any{"paused": true})
	if rec2.Code != http.StatusNotImplemented {
		t.Errorf("capture POST (no cap): status=%d, want %d", rec2.Code, http.StatusNotImplemented)
	}

	// Bad body.
	rec3 := do(h, "POST", "/api/capture", "bad")
	if rec3.Code != http.StatusBadRequest {
		t.Errorf("capture POST (bad body): status=%d, want %d", rec3.Code, http.StatusBadRequest)
	}
}

func TestRateHist(t *testing.T) {
	srv, _, _, cleanup := newTestServer(t)
	defer cleanup()

	h := srv.Handler()
	rec := do(h, "GET", "/api/ratehist", nil)
	if rec.Code != 200 {
		t.Fatalf("ratehist: status=%d", rec.Code)
	}
	var pts []types.RatePoint
	jsonBody(t, rec, &pts)
}

func TestConnections(t *testing.T) {
	srv, _, _, cleanup := newTestServer(t)
	defer cleanup()

	h := srv.Handler()
	rec := do(h, "GET", "/api/connections?window=5", nil)
	if rec.Code != 200 {
		t.Fatalf("connections: status=%d", rec.Code)
	}
	var conns []types.Connection
	jsonBody(t, rec, &conns)

	// Default window.
	rec2 := do(h, "GET", "/api/connections", nil)
	if rec2.Code != 200 {
		t.Fatalf("connections (default): status=%d", rec2.Code)
	}

	// Bad window (should fall back to default).
	rec3 := do(h, "GET", "/api/connections?window=bad", nil)
	if rec3.Code != 200 {
		t.Fatalf("connections (bad window): status=%d", rec3.Code)
	}
}

func TestNetUsage(t *testing.T) {
	srv, store, _, cleanup := newTestServer(t)
	defer cleanup()

	// Need some data for netusage to return results. Write a sample.
	err := store.FlushApps(0, []types.AppTraffic{
		{Name: "test", RxBytes: 100, TxBytes: 50, Connections: 1},
	})
	if err != nil {
		t.Fatalf("FlushApps: %v", err)
	}

	h := srv.Handler()
	rec := do(h, "GET", "/api/netusage?range=today", nil)
	if rec.Code != 200 {
		t.Fatalf("netusage: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var usages []netUsage
	jsonBody(t, rec, &usages)
	// netusage is per-interface; may be empty if no iface data.

	// Different ranges.
	for _, r := range []string{"week", "month"} {
		rec2 := do(h, "GET", "/api/netusage?range="+r, nil)
		if rec2.Code != 200 {
			t.Errorf("netusage range=%s: status=%d", r, rec2.Code)
		}
	}

	// Without store.
	srvNoStore := NewServer(srv.eng, nil, nil, nil)
	rec3 := do(srvNoStore.Handler(), "GET", "/api/netusage", nil)
	if rec3.Code != http.StatusServiceUnavailable {
		t.Errorf("netusage (no store): status=%d, want %d", rec3.Code, http.StatusServiceUnavailable)
	}
}

func TestSessionReset(t *testing.T) {
	srv, _, _, cleanup := newTestServer(t)
	defer cleanup()

	h := srv.Handler()

	// Wrong method.
	rec := do(h, "GET", "/api/session/reset", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("session/reset GET: status=%d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}

	// Correct method.
	rec2 := do(h, "POST", "/api/session/reset", nil)
	if rec2.Code != 204 {
		t.Errorf("session/reset POST: status=%d, want 204", rec2.Code)
	}
}

func TestParseRange(t *testing.T) {
	tests := []struct {
		q    string
		want string // approximate — just check it's not zero
	}{
		{"hour", "hour"},
		{"day", "day"},
		{"24h", "24h"},
		{"week", "week"},
		{"month", "month"},
		{"today", "today"},
		{"", "today"}, // default
		{"unknown", "today"},
	}

	for _, tt := range tests {
		req := httptest.NewRequest("GET", "/api/apps?range="+tt.q, nil)
		since, until := parseRange(req)
		if since.IsZero() {
			t.Errorf("parseRange(%q): since is zero", tt.q)
		}
		if until.IsZero() {
			t.Errorf("parseRange(%q): until is zero", tt.q)
		}
		if until.Before(since) {
			t.Errorf("parseRange(%q): until %v before since %v", tt.q, until, since)
		}
	}
}

func TestParseStep(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/timeseries?step=30", nil)
	if got := parseStep(req, time.Hour); got != 30*time.Second {
		t.Errorf("step=30 -> %v, want 30s", got)
	}

	// Auto step (hour window -> ~30s)
	req2 := httptest.NewRequest("GET", "/api/timeseries", nil)
	got := parseStep(req2, time.Hour)
	if got != time.Hour/120 {
		t.Errorf("auto step (1h)=%v, want %v", got, time.Hour/120)
	}

	// Auto step (short window -> clamped to 10s)
	req3 := httptest.NewRequest("GET", "/api/timeseries", nil)
	got3 := parseStep(req3, time.Minute)
	if got3 != 10*time.Second {
		t.Errorf("auto step (1m)=%v, want 10s (clamped)", got3)
	}
}

func TestWriteJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, map[string]string{"hello": "world"})
	if rec.Code != 200 {
		t.Errorf("writeJSON status=%d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type=%q, want application/json", ct)
	}
	var got map[string]string
	json.NewDecoder(rec.Body).Decode(&got)
	if got["hello"] != "world" {
		t.Errorf("body=%v", got)
	}
}

func TestNewServer(t *testing.T) {
	store, err := storage.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	dns := dnscache.New(time.Hour, 100)
	eng := engine.New(engine.Config{}, fakeResolver{}, dns, store)
	srv := NewServer(eng, store, nil, nil)
	if srv == nil {
		t.Fatal("NewServer returned nil")
	}
	h := srv.Handler()
	if h == nil {
		t.Fatal("Handler returned nil")
	}
}
