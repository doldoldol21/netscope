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

	byIface := func(sinceDay int64) map[string]storageUsage {
		rows, err := s.IfaceUsageAllSince(sinceDay)
		if err != nil {
			t.Fatalf("all since %d: %v", sinceDay, err)
		}
		m := map[string]storageUsage{}
		for _, u := range rows {
			m[u.Iface] = storageUsage{u.Rx, u.Tx}
		}
		return m
	}
	all := byIface(1000)
	if all["en5"] != (storageUsage{310, 155}) { // (100+10+200)/(50+5+100)
		t.Errorf("en5 since 1000 = %+v, want {310 155}", all["en5"])
	}
	if all["en0"] != (storageUsage{999, 999}) {
		t.Errorf("en0 since 1000 = %+v, want {999 999}", all["en0"])
	}
	// Day boundary excludes the earlier day.
	if got := byIface(2000)["en5"]; got != (storageUsage{200, 100}) {
		t.Errorf("en5 since 2000 = %+v, want {200 100}", got)
	}
}

type storageUsage struct{ rx, tx uint64 }

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

// The daily rollups must answer whole-day ranges with exactly what the raw
// samples would have said — otherwise the dashboard's week view silently
// disagrees with the data it was derived from.
func TestDailyRollupMatchesSamples(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "roll.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	// Three buckets spread across today, including one in a later hour.
	for i, off := range []time.Duration{0, 3 * time.Hour, 11 * time.Hour} {
		b := midnight.Add(off).Unix()
		if err := s.FlushApps(b, []types.AppTraffic{
			{Name: "claude", Path: "/Applications/Claude.app", RxBytes: uint64(100 * (i + 1)), TxBytes: 10, Connections: i + 1},
			{Name: "ssh", RxBytes: 5, TxBytes: 5},
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.FlushDomains(b, []types.DomainStat{
			{Domain: "api.anthropic.com", AppName: "claude", RxBytes: uint64(90 * (i + 1)), TxBytes: 9, Category: "ai", Country: "US"},
		}); err != nil {
			t.Fatal(err)
		}
	}

	tomorrow := midnight.AddDate(0, 0, 1)
	// Day-aligned: served from the rollup.
	apps, err := s.Apps(midnight, tomorrow)
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 2 || apps[0].Name != "claude" || apps[0].RxBytes != 600 || apps[0].TxBytes != 30 {
		t.Fatalf("rollup apps = %+v, want claude rx=600 tx=30 ranked first", apps)
	}
	if apps[0].Path != "/Applications/Claude.app" {
		t.Errorf("rollup lost the app path: %q", apps[0].Path)
	}
	doms, err := s.Domains(midnight, tomorrow)
	if err != nil {
		t.Fatal(err)
	}
	if len(doms) != 1 || doms[0].RxBytes != 540 || doms[0].Country != "US" || doms[0].Category != "ai" {
		t.Fatalf("rollup domains = %+v, want rx=540 with ai/US kept", doms)
	}

	// Same range widened by a minute on both ends: now it has no whole day
	// inside, so it is answered purely from the samples — same totals.
	raw, err := s.Apps(midnight.Add(-time.Minute), tomorrow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 2 || raw[0].RxBytes != 600 {
		t.Fatalf("sample-path apps = %+v, want the same totals", raw)
	}
}

// A database written before rollups existed must still answer ranked queries:
// Open backfills the rollup from whatever samples are already there.
func TestBackfillsRollupOnOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	if err := s.FlushApps(midnight.Add(time.Hour).Unix(), []types.AppTraffic{{Name: "claude", RxBytes: 42}}); err != nil {
		t.Fatal(err)
	}
	// Simulate a pre-rollup database: samples present, rollup empty.
	if _, err := s.db.Exec(`DELETE FROM app_daily`); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	apps, err := s2.Apps(midnight, midnight.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 1 || apps[0].RxBytes != 42 {
		t.Fatalf("after backfill apps = %+v, want the pre-existing 42 bytes", apps)
	}
}

// Retention must drop rollup days too, or ranked history outlives the samples.
func TestPurgeDropsRollupDays(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "purge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	old := midnight.AddDate(0, 0, -10)
	if err := s.FlushApps(old.Add(time.Hour).Unix(), []types.AppTraffic{{Name: "old", RxBytes: 7}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Purge(midnight.AddDate(0, 0, -1)); err != nil {
		t.Fatal(err)
	}
	apps, err := s.Apps(old, old.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 0 {
		t.Fatalf("purged day still ranked: %+v", apps)
	}
}

// todayBucket returns a sample timestamp offset into today's local day, pulled
// back to local midnight when that offset would land ahead of now.
//
// Tests that end a range at time.Now() have to write their samples behind it: a
// bucket in the future is outside `bucket >= from AND bucket < to`, so the row
// simply isn't there and the assertion reads as a rollup bug. Hard-coding
// mid+2h made that happen for the first two hours of every local day — which CI
// hits whenever a run lands there, its runners being UTC.
func todayBucket(now, mid time.Time, offset time.Duration) time.Time {
	if ts := mid.Add(offset); !ts.After(now) {
		return ts
	}
	return mid
}

// An open-ended range (…until now) must take today from the rollup instead of
// rescanning today's raw buckets — and must not double-count it.
func TestOpenEndedRangeUsesTodayRollup(t *testing.T) {
	s := openTemp(t)
	now := time.Now()
	mid := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	ts := todayBucket(now, mid, 2*time.Hour)
	if err := s.FlushApps(ts.Unix(), []types.AppTraffic{{Name: "claude", RxBytes: 100, TxBytes: 10}}); err != nil {
		t.Fatal(err)
	}
	apps, err := s.Apps(mid.AddDate(0, 0, -6), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 1 || apps[0].RxBytes != 100 || apps[0].TxBytes != 10 {
		t.Fatalf("open-ended week = %+v, want exactly one claude row with rx=100 tx=10 (no double count)", apps)
	}
	// The same range asked as a closed window over the same sample agrees.
	closed, err := s.Apps(mid.AddDate(0, 0, -6), ts.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(closed) != 1 || closed[0].RxBytes != 100 {
		t.Fatalf("closed window = %+v, want the same totals", closed)
	}
}

// Rollup day keys must be real local midnights, and must agree with the ones
// the flush path writes — v0.19.0's backfill was offset by the UTC offset,
// which left two key sets in the same table.
func TestRollupKeysAreLocalMidnight(t *testing.T) {
	s := openTemp(t)
	now := time.Now()
	mid := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	if err := s.FlushApps(mid.Add(5*time.Hour).Unix(), []types.AppTraffic{{Name: "a", RxBytes: 1}}); err != nil {
		t.Fatal(err)
	}
	var day int64
	if err := s.db.QueryRow(`SELECT day FROM app_daily`).Scan(&day); err != nil {
		t.Fatal(err)
	}
	if day != mid.Unix() {
		t.Fatalf("flush wrote day=%d (%s), want local midnight %d (%s)",
			day, time.Unix(day, 0), mid.Unix(), mid)
	}
	// Day keys now come from Go alone — there is no second (SQL) definition to
	// disagree with, which is what made the midnight-DST zones break.
}

// A database carrying v0.19.0's misaligned keys is rebuilt on open, leaving one
// consistent key set whose totals still match the samples.
func TestRepairsMisalignedRollupKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	mid := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	if err := s.FlushApps(mid.Add(time.Hour).Unix(), []types.AppTraffic{{Name: "a", RxBytes: 10}}); err != nil {
		t.Fatal(err)
	}
	// Simulate the v0.19.0 backfill: same totals, keys shifted off midnight.
	if _, err := s.db.Exec(`UPDATE app_daily SET day = day + 32400`); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	var day int64
	var total uint64
	if err := s2.db.QueryRow(`SELECT day, SUM(rx) FROM app_daily GROUP BY day`).Scan(&day, &total); err != nil {
		t.Fatal(err)
	}
	if day != mid.Unix() || total != 10 {
		t.Fatalf("after repair day=%d total=%d, want day=%d total=10", day, total, mid.Unix())
	}
}

// Property test: for a spread of ranges — including ones that start or end
// mid-day, span midnights, sit entirely inside one day, or run open-ended to
// now — the rollup-backed query must return exactly what a pure raw-sample
// aggregation would. This is the invariant the whole rollup rests on, so it is
// checked exhaustively rather than by example.
func TestRollupAgreesWithSamplesAcrossRanges(t *testing.T) {
	s := openTemp(t)
	now := time.Now()
	mid := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	// Five days of traffic, several buckets a day at varying hours.
	for d := 0; d < 5; d++ {
		day := mid.AddDate(0, 0, -d)
		for _, h := range []int{0, 6, 13, 23} {
			b := day.Add(time.Duration(h) * time.Hour).Unix()
			if b > now.Unix() {
				continue // don't write the future
			}
			rx := uint64(100*(d+1) + h)
			if err := s.FlushApps(b, []types.AppTraffic{
				{Name: "claude", RxBytes: rx, TxBytes: rx / 2},
				{Name: "ssh", RxBytes: 7, TxBytes: 3},
			}); err != nil {
				t.Fatal(err)
			}
			if err := s.FlushDomains(b, []types.DomainStat{
				{Domain: "api.anthropic.com", AppName: "claude", RxBytes: rx, TxBytes: rx / 2},
			}); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Raw reference: the same aggregation straight off the sample table.
	rawApps := func(since, until time.Time) map[string][2]uint64 {
		t.Helper()
		rows, err := s.rdb.Query(`
			SELECT app, SUM(rx), SUM(tx) FROM app_samples
			WHERE bucket >= ? AND bucket < ? GROUP BY app`, since.Unix(), until.Unix())
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string][2]uint64{}
		for rows.Next() {
			var name string
			var rx, tx uint64
			if err := rows.Scan(&name, &rx, &tx); err != nil {
				t.Fatal(err)
			}
			out[name] = [2]uint64{rx, tx}
		}
		return out
	}

	cases := []struct {
		name         string
		since, until time.Time
	}{
		{"today open-ended", mid, now},
		{"week open-ended", mid.AddDate(0, 0, -4), now},
		{"whole day, closed", mid.AddDate(0, 0, -2), mid.AddDate(0, 0, -1)},
		{"two whole days", mid.AddDate(0, 0, -3), mid.AddDate(0, 0, -1)},
		{"starts mid-day", mid.AddDate(0, 0, -3).Add(9 * time.Hour), mid},
		{"ends mid-day", mid.AddDate(0, 0, -3), mid.AddDate(0, 0, -1).Add(9 * time.Hour)},
		{"both edges partial", mid.AddDate(0, 0, -3).Add(3 * time.Hour), mid.AddDate(0, 0, -1).Add(20 * time.Hour)},
		{"inside one day", mid.AddDate(0, 0, -2).Add(5 * time.Hour), mid.AddDate(0, 0, -2).Add(20 * time.Hour)},
		{"empty window", mid.AddDate(0, 0, -2).Add(time.Hour), mid.AddDate(0, 0, -2).Add(2 * time.Hour)},
		{"until in the future", mid.AddDate(0, 0, -2), now.Add(48 * time.Hour)},
		{"since == until", mid, mid},
	}
	for _, c := range cases {
		want := rawApps(c.since, c.until)
		got, err := s.Apps(c.since, c.until)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(got) != len(want) {
			t.Errorf("%s: %d apps, want %d (%v vs %v)", c.name, len(got), len(want), got, want)
			continue
		}
		for _, a := range got {
			w, ok := want[a.Name]
			if !ok {
				t.Errorf("%s: unexpected app %q", c.name, a.Name)
				continue
			}
			if a.RxBytes != w[0] || a.TxBytes != w[1] {
				t.Errorf("%s: %s = rx %d tx %d, want rx %d tx %d",
					c.name, a.Name, a.RxBytes, a.TxBytes, w[0], w[1])
			}
		}
		// Ranked descending by total, always.
		for i := 1; i < len(got); i++ {
			if got[i-1].RxBytes+got[i-1].TxBytes < got[i].RxBytes+got[i].TxBytes {
				t.Errorf("%s: results not ranked by total", c.name)
				break
			}
		}
	}
}

// If a rollup day ever drifts from the samples (a future bug, a partial write),
// the periodic verification must notice and rebuild that day.
func TestVerifyRollupsRepairsDrift(t *testing.T) {
	s := openTemp(t)
	now := time.Now()
	mid := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	if err := s.FlushApps(todayBucket(now, mid, time.Hour).Unix(), []types.AppTraffic{
		{Name: "claude", RxBytes: 1000, TxBytes: 100},
	}); err != nil {
		t.Fatal(err)
	}
	// Nothing wrong yet.
	if repaired, err := s.VerifyRollups(3); err != nil || repaired {
		t.Fatalf("clean database reported repaired=%v err=%v", repaired, err)
	}
	// Corrupt the rollup: wrong total, and a row the samples never had.
	if _, err := s.db.Exec(`UPDATE app_daily SET rx = 1`); err != nil {
		t.Fatal(err)
	}
	repaired, err := s.VerifyRollups(3)
	if err != nil {
		t.Fatal(err)
	}
	if !repaired {
		t.Fatal("drift went unnoticed")
	}
	apps, err := s.Apps(mid, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 1 || apps[0].RxBytes != 1000 || apps[0].TxBytes != 100 {
		t.Fatalf("after repair = %+v, want the sample totals back (rx 1000, tx 100)", apps)
	}
}

// The daemon runs as root and its data directory holds a full record of which
// process talked to which host. Other local accounts must not be able to read
// it, so the directory is owner-only and the database files are 0600.

func TestPrepareDirCreatesOwnerOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "netscope")
	if err := PrepareDir(dir, dir); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if mode := statMode(t, dir); mode != 0o700 {
		t.Errorf("new dir mode = %#o, want 0700", mode)
	}
}

func TestPrepareDirTightensTheDefaultDir(t *testing.T) {
	// Installs created by earlier versions left the default directory 0755.
	dir := existingDir(t, 0o755)
	if err := PrepareDir(dir, dir); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if mode := statMode(t, dir); mode != 0o700 {
		t.Errorf("default dir mode = %#o, want 0700", mode)
	}
}

func TestPrepareDirTightensTheDefaultDirThroughASymlink(t *testing.T) {
	// macOS reaches the default /var/db/netscope through /var -> /private/var,
	// so the two spellings of the same directory have to compare equal.
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(real, 0o755); err != nil { // defeat the caller's umask
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := PrepareDir(link, real); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if mode := statMode(t, real); mode != 0o700 {
		t.Errorf("dir reached via a symlink = %#o, want 0700", mode)
	}
}

func TestPrepareDirLeavesAChosenDirAlone(t *testing.T) {
	// --db pointed somewhere the operator picked (a shared directory, or "."
	// for a relative path). Its permissions are not ours to change.
	dir := existingDir(t, 0o755)
	if err := PrepareDir(dir, filepath.Join(t.TempDir(), "elsewhere")); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if mode := statMode(t, dir); mode != 0o755 {
		t.Errorf("chosen dir mode = %#o, want it untouched at 0755", mode)
	}
}

func existingDir(t *testing.T, mode os.FileMode) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "netscope")
	if err := os.MkdirAll(dir, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, mode); err != nil { // defeat the caller's umask
		t.Fatal(err)
	}
	return dir
}

func TestOpenRestrictsDatabaseFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	// Simulate a database left world-readable by an earlier version.
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	if mode := statMode(t, path); mode != 0o600 {
		t.Errorf("db mode = %#o, want 0600", mode)
	}
}

func TestOpenRestrictsSidecarFiles(t *testing.T) {
	// SQLite creates -wal/-shm on first write, copying the database's mode. That
	// only holds because the database was already 0600 before SQLite opened it.
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	if err := s.FlushApps(1000, []types.AppTraffic{{Name: "a", RxBytes: 1}}); err != nil {
		t.Fatalf("flush: %v", err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if mode := statMode(t, path+suffix); mode != 0o600 {
			t.Errorf("%s mode = %#o, want 0600", "db"+suffix, mode)
		}
	}
}

func TestOpenLeavesNoFileForAnInMemoryDatabase(t *testing.T) {
	// internal/api opens ":memory:" for its tests. Securing a database means
	// creating it if absent, which must not turn that name into a real file.
	dir := t.TempDir()
	t.Chdir(dir)
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("in-memory open created %q", e.Name())
	}
}

func TestOpenRestrictsDatabaseFileWhoseNameContainsModeMemory(t *testing.T) {
	// "mode=memory" is a substring a real filename can contain. Treating it as
	// an in-memory database would skip the restriction on a file that very much
	// exists.
	path := filepath.Join(t.TempDir(), "netscope-mode=memory-backup.db")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	if mode := statMode(t, path); mode != 0o600 {
		t.Errorf("db mode = %#o, want 0600", mode)
	}
}

func statMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Mode().Perm()
}
