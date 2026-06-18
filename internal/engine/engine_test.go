package engine

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/doldoldol21/netscope/internal/dnscache"
	"github.com/doldoldol21/netscope/internal/storage"
	"github.com/doldoldol21/netscope/pkg/types"
)

func TestIngestAttributesDomainAndCategory(t *testing.T) {
	dns := dnscache.New(time.Hour, 100)
	dns.Put("1.2.3.4", "api.openai.com")
	dns.Put("5.6.7.8", "github.com")

	e := New(Config{}, nil, dns, nil)

	e.ingest(types.Flow{Proto: types.ProtoTCP, Direction: types.DirIn, LocalPort: 100, RemoteIP: "1.2.3.4", Bytes: 1000})
	e.ingest(types.Flow{Proto: types.ProtoTCP, Direction: types.DirOut, LocalPort: 100, RemoteIP: "1.2.3.4", Bytes: 200})
	e.ingest(types.Flow{Proto: types.ProtoTCP, Direction: types.DirIn, LocalPort: 101, RemoteIP: "5.6.7.8", Bytes: 50})

	e.updateSnapshot()
	snap := e.Snapshot()

	if snap.TotalRx != 1050 || snap.TotalTx != 200 {
		t.Fatalf("totals wrong: rx=%d tx=%d", snap.TotalRx, snap.TotalTx)
	}
	if len(snap.Apps) != 1 {
		t.Fatalf("want 1 app (unknown), got %d", len(snap.Apps))
	}
	if snap.Apps[0].Connections != 2 {
		t.Errorf("connections = %d, want 2 distinct local ports", snap.Apps[0].Connections)
	}

	// Domains ranked by total: openai (1200) before github (50).
	if len(snap.Domains) != 2 || snap.Domains[0].Domain != "api.openai.com" {
		t.Fatalf("domain ranking wrong: %+v", snap.Domains)
	}
	// Category is a neutral grouping; openai falls under "ai".
	if snap.Domains[0].Category != "ai" {
		t.Errorf("category = %q, want ai", snap.Domains[0].Category)
	}
}

type fakeResolver struct {
	proc types.Process
	ok   bool
}

func (f fakeResolver) Lookup(types.ConnKey) (types.Process, bool) { return f.proc, f.ok }

type fakeHinter struct{ ips []string }

func (h *fakeHinter) Enqueue(ip string) { h.ips = append(h.ips, ip) }

func TestSelfPIDFiltered(t *testing.T) {
	res := fakeResolver{proc: types.Process{PID: 4242, Name: "netscoped"}, ok: true}
	e := New(Config{SelfPID: 4242}, res, dnscache.New(time.Hour, 10), nil)
	e.ingest(types.Flow{Proto: types.ProtoTCP, Direction: types.DirIn, LocalPort: 1, RemoteIP: "1.2.3.4", Bytes: 5000})
	e.updateSnapshot()
	if got := len(e.Snapshot().Apps); got != 0 {
		t.Fatalf("own traffic should be filtered, got %d apps", got)
	}
	if e.Snapshot().TotalRx != 0 {
		t.Errorf("own bytes counted in totals")
	}
}

func TestHinterEnqueuedOnDNSMiss(t *testing.T) {
	h := &fakeHinter{}
	e := New(Config{Hinter: h}, nil, dnscache.New(time.Hour, 10), nil)
	e.ingest(types.Flow{Proto: types.ProtoTCP, Direction: types.DirIn, LocalPort: 1, RemoteIP: "9.9.9.9", Bytes: 100})
	if len(h.ips) != 1 || h.ips[0] != "9.9.9.9" {
		t.Fatalf("expected reverse-DNS hint for 9.9.9.9, got %v", h.ips)
	}
	// A known host must NOT trigger a hint.
	dns := dnscache.New(time.Hour, 10)
	dns.Put("5.5.5.5", "known.example")
	h2 := &fakeHinter{}
	e2 := New(Config{Hinter: h2}, nil, dns, nil)
	e2.ingest(types.Flow{Proto: types.ProtoTCP, Direction: types.DirIn, LocalPort: 1, RemoteIP: "5.5.5.5", Bytes: 100})
	if len(h2.ips) != 0 {
		t.Errorf("known host should not be hinted, got %v", h2.ips)
	}
}

func TestUnknownDomainFallsBackToIP(t *testing.T) {
	e := New(Config{}, nil, dnscache.New(time.Hour, 100), nil)
	e.ingest(types.Flow{Proto: types.ProtoUDP, Direction: types.DirOut, LocalPort: 9, RemoteIP: "203.0.113.9", Bytes: 10})
	e.updateSnapshot()
	d := e.Snapshot().Domains
	if len(d) != 1 || d[0].Domain != "203.0.113.9" {
		t.Fatalf("expected IP fallback domain, got %+v", d)
	}
}

// TestSnapshotSurvivesFlush is the regression test for the "tables vanish every
// 10s" bug: the live snapshot must keep showing cumulative session data across
// a storage flush boundary.
func TestSnapshotSurvivesFlush(t *testing.T) {
	e := New(Config{Bucket: 10 * time.Second}, nil, dnscache.New(time.Hour, 100), nil)

	e.ingest(types.Flow{Proto: types.ProtoTCP, Direction: types.DirIn, LocalPort: 1, RemoteIP: "1.2.3.4", Bytes: 1000})
	e.updateSnapshot()
	if got := len(e.Snapshot().Apps); got != 1 {
		t.Fatalf("before flush: apps = %d, want 1", got)
	}

	e.flush() // resets the window; must NOT clear the live view
	e.updateSnapshot()

	snap := e.Snapshot()
	if len(snap.Apps) != 1 {
		t.Fatalf("after flush: live apps disappeared (%d), the 10s-reset bug", len(snap.Apps))
	}
	if snap.Apps[0].RxBytes != 1000 {
		t.Errorf("after flush: cumulative bytes lost, rx = %d want 1000", snap.Apps[0].RxBytes)
	}

	// More traffic accumulates on top, not from zero.
	e.ingest(types.Flow{Proto: types.ProtoTCP, Direction: types.DirIn, LocalPort: 1, RemoteIP: "1.2.3.4", Bytes: 500})
	e.updateSnapshot()
	if rx := e.Snapshot().Apps[0].RxBytes; rx != 1500 {
		t.Errorf("session should accumulate: rx = %d want 1500", rx)
	}
}

func TestActiveApps(t *testing.T) {
	base := time.Unix(10000, 0)
	cur := base
	e := New(Config{ActiveWindow: 8 * time.Second}, nil, dnscache.New(time.Hour, 100), nil)
	e.nowFn = func() time.Time { return cur }

	e.ingest(types.Flow{Proto: types.ProtoTCP, Direction: types.DirIn, LocalPort: 1, RemoteIP: "1.1.1.1", Bytes: 10})
	cur = base.Add(20 * time.Second) // first app now idle
	e.ingest(types.Flow{Proto: types.ProtoTCP, Direction: types.DirIn, LocalPort: 2, RemoteIP: "2.2.2.2", Bytes: 10})
	e.updateSnapshot()

	snap := e.Snapshot()
	if len(snap.Apps) != 1 { // both resolve to "unknown" -> merged into one app
		t.Fatalf("apps = %d", len(snap.Apps))
	}
	if snap.ActiveApps != 1 {
		t.Errorf("activeApps = %d, want 1 (only the recent flow is within the window)", snap.ActiveApps)
	}
}

func TestFlushPersistsAndResets(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "e.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	fixed := time.Date(2026, 6, 18, 9, 0, 0, 0, time.UTC)
	e := New(Config{Bucket: 10 * time.Second}, nil, dnscache.New(time.Hour, 100), store)
	e.nowFn = func() time.Time { return fixed }

	e.ingest(types.Flow{Proto: types.ProtoTCP, Direction: types.DirIn, LocalPort: 100, RemoteIP: "1.2.3.4", Bytes: 4096})
	e.flush()

	// Window accumulators reset after flush…
	if len(e.winApps) != 0 || len(e.winDomains) != 0 {
		t.Errorf("window accumulators not reset after flush")
	}
	// …but the session accumulators must survive (this is the live view).
	if len(e.sessApps) != 1 || len(e.sessDomains) != 1 {
		t.Errorf("session accumulators should persist across flush, got apps=%d domains=%d", len(e.sessApps), len(e.sessDomains))
	}

	apps, err := store.Apps(fixed.Add(-time.Minute), fixed.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 1 || apps[0].RxBytes != 4096 {
		t.Fatalf("flush did not persist app: %+v", apps)
	}
}
