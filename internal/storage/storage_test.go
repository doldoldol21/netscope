package storage

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/doldoldol21/netscope/pkg/types"
)

func TestIfaceUsage(t *testing.T) {
	s := openTemp(t)
	// Two days of usage on en5, one on en0.
	if err := s.AddIfaceUsage("en5", 1000, 100, 50); err != nil {
		t.Fatalf("add: %v", err)
	}
	_ = s.AddIfaceUsage("en5", 1000, 10, 5) // same day accumulates
	_ = s.AddIfaceUsage("en5", 2000, 200, 100)
	_ = s.AddIfaceUsage("en0", 2000, 999, 999)
	// Empty iface / zero bytes are no-ops.
	_ = s.AddIfaceUsage("", 2000, 1, 1)
	_ = s.AddIfaceUsage("en5", 3000, 0, 0)

	rx, tx, err := s.IfaceUsageSince("en5", 1000)
	if err != nil {
		t.Fatalf("since: %v", err)
	}
	if rx != 310 || tx != 155 { // (100+10+200) / (50+5+100)
		t.Errorf("usage since 1000 = rx %d tx %d, want 310/155", rx, tx)
	}
	// Cycle boundary excludes the earlier day.
	rx, tx, _ = s.IfaceUsageSince("en5", 2000)
	if rx != 200 || tx != 100 {
		t.Errorf("usage since 2000 = rx %d tx %d, want 200/100", rx, tx)
	}
}

// TestOpenQuarantinesCorruptDB verifies a corrupt database file is moved aside
// and a fresh, usable one is created — rather than failing Open() forever (which
// would brick the daemon in a launchd KeepAlive crash loop).
func TestOpenQuarantinesCorruptDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	// Write a file that looks like a DB by name but is not valid SQLite.
	if err := os.WriteFile(path, []byte("this is not a sqlite database, it is garbage"), 0o644); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open should recover from a corrupt file, got: %v", err)
	}
	defer s.Close()
	// The corrupt original must have been quarantined.
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Errorf("expected quarantined %s.corrupt, stat err: %v", path, err)
	}
	// The fresh DB must be usable.
	if err := s.FlushApps(1, []types.AppTraffic{{Name: "x", RxBytes: 1}}); err != nil {
		t.Errorf("fresh DB not usable after recovery: %v", err)
	}
}

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestEnforceSizeCap(t *testing.T) {
	s := openTemp(t)
	day := int64(86400)
	base := int64(1_700_000_000)
	base -= base % day // align to a day boundary
	// Write several days of data with enough rows to grow the file.
	for d := int64(0); d < 6; d++ {
		bucket := base + d*day
		apps := make([]types.AppTraffic, 0, 200)
		doms := make([]types.DomainStat, 0, 200)
		for i := 0; i < 200; i++ {
			name := "app-" + string(rune('A'+i%26)) + string(rune('a'+i%26)) + time.Unix(bucket, 0).String()
			apps = append(apps, types.AppTraffic{Name: name, RxBytes: 12345, TxBytes: 6789})
			doms = append(doms, types.DomainStat{Domain: name + ".example.com", RxBytes: 5555, TxBytes: 4444})
		}
		if err := s.FlushApps(bucket, apps); err != nil {
			t.Fatal(err)
		}
		if err := s.FlushDomains(bucket, doms); err != nil {
			t.Fatal(err)
		}
	}
	s.Checkpoint()
	_ = s.Vacuum()
	before := s.SizeOnDisk()

	// A cap of 0 disables the safety net.
	if did, _ := s.EnforceSizeCap(0); did {
		t.Fatal("cap=0 should be a no-op")
	}
	// Cap just under the current size must drop the oldest day and shrink.
	did, err := s.EnforceSizeCap(before - 1)
	if err != nil {
		t.Fatal(err)
	}
	if !did {
		t.Fatal("expected EnforceSizeCap to delete data")
	}
	if after := s.SizeOnDisk(); after >= before {
		t.Fatalf("size did not shrink: before=%d after=%d", before, after)
	}
	// The newest day must survive (we delete oldest-first).
	newest := time.Unix(base+5*day, 0)
	apps, _ := s.Apps(newest.Add(-time.Hour), newest.Add(time.Hour))
	if len(apps) == 0 {
		t.Fatal("newest day was deleted; cap should drop oldest first")
	}
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

	if _, err := s.Purge(time.Unix(500_000, 0)); err != nil {
		t.Fatal(err)
	}
	apps, _ := s.Apps(time.Unix(0, 0), time.Unix(2_000_000, 0))
	if len(apps) != 1 || apps[0].Name != "new" {
		t.Fatalf("purge wrong, remaining: %+v", apps)
	}
}
