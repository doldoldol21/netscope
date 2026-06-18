package storage

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/doldoldol21/netscope/pkg/types"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestFlushAndQueryApps(t *testing.T) {
	s := openTemp(t)
	base := time.Date(2026, 6, 18, 10, 0, 0, 0, time.UTC)
	b := base.Unix()

	if err := s.FlushApps(b, []types.AppTraffic{
		{Name: "Safari", Path: "/Applications/Safari.app", RxBytes: 1000, TxBytes: 200, Connections: 3},
		{Name: "Claude", RxBytes: 500, TxBytes: 100, Connections: 1},
	}); err != nil {
		t.Fatal(err)
	}
	// Second flush to the same bucket must accumulate.
	if err := s.FlushApps(b, []types.AppTraffic{
		{Name: "Safari", RxBytes: 50, TxBytes: 5},
	}); err != nil {
		t.Fatal(err)
	}

	apps, err := s.Apps(base.Add(-time.Minute), base.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 2 {
		t.Fatalf("got %d apps, want 2", len(apps))
	}
	// Ranked by total desc -> Safari first (1055+205 > 600).
	if apps[0].Name != "Safari" || apps[0].RxBytes != 1050 || apps[0].TxBytes != 205 {
		t.Errorf("safari aggregation wrong: %+v", apps[0])
	}
	if apps[1].Name != "Claude" {
		t.Errorf("second app = %q, want Claude", apps[1].Name)
	}
}

func TestQueryRangeExclusive(t *testing.T) {
	s := openTemp(t)
	base := time.Unix(1_000_000, 0)
	s.FlushApps(base.Unix(), []types.AppTraffic{{Name: "A", RxBytes: 10}})
	// Query a window that ends before the sample -> nothing.
	apps, err := s.Apps(base.Add(-time.Hour), base.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 0 {
		t.Fatalf("expected no apps outside range, got %d", len(apps))
	}
}

func TestDomainsAndTimeSeries(t *testing.T) {
	s := openTemp(t)
	t0 := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)

	for i := 0; i < 3; i++ {
		bucket := t0.Add(time.Duration(i) * 10 * time.Second).Unix()
		if err := s.FlushDomains(bucket, []types.DomainStat{
			{Domain: "api.openai.com", AppName: "Claude", RxBytes: 100, TxBytes: 10, Category: "ai"},
			{Domain: "github.com", AppName: "git", RxBytes: 20, TxBytes: 5},
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.FlushApps(bucket, []types.AppTraffic{{Name: "Claude", RxBytes: 100, TxBytes: 10}}); err != nil {
			t.Fatal(err)
		}
	}

	doms, err := s.Domains(t0.Add(-time.Minute), t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(doms) != 2 || doms[0].Domain != "api.openai.com" || doms[0].RxBytes != 300 {
		t.Fatalf("domain aggregation wrong: %+v", doms)
	}

	pts, err := s.TimeSeries(t0.Add(-time.Minute), t0.Add(time.Minute), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 3 {
		t.Fatalf("got %d time points, want 3", len(pts))
	}
	var totalRx uint64
	for _, p := range pts {
		totalRx += p.RxBytes
	}
	if totalRx != 300 {
		t.Errorf("timeseries total rx = %d, want 300", totalRx)
	}
}

func TestPurge(t *testing.T) {
	s := openTemp(t)
	old := time.Unix(1000, 0)
	recent := time.Unix(1_000_000, 0)
	s.FlushApps(old.Unix(), []types.AppTraffic{{Name: "old", RxBytes: 1}})
	s.FlushApps(recent.Unix(), []types.AppTraffic{{Name: "new", RxBytes: 1}})

	if err := s.Purge(time.Unix(500_000, 0)); err != nil {
		t.Fatal(err)
	}
	apps, _ := s.Apps(time.Unix(0, 0), time.Unix(2_000_000, 0))
	if len(apps) != 1 || apps[0].Name != "new" {
		t.Fatalf("purge wrong, remaining: %+v", apps)
	}
}
