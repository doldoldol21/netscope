package storage

import (
	"testing"
	"time"

	"github.com/doldoldol21/netscope/pkg/types"
)

// These lock down data-correctness bugs found by auditing the daily rollups.
// Each one produced a wrong number on the dashboard before the fix, and each is
// written to fail loudly if the old shape ever comes back.

// useZone points time.Local at a zone for the duration of one test, so the DST
// cases run the same way under any CI timezone.
func useZone(t *testing.T, name string) {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("zone %s unavailable: %v", name, err)
	}
	prev := time.Local
	time.Local = loc
	t.Cleanup(func() { time.Local = prev })
}

// On a 25-hour fall-back day, stepping a day with +86400s lands at 23:00 rather
// than the next midnight, so the rollup day (which covers all 25 hours) and the
// trailing sample window overlapped for an hour — every open-ended query
// between 23:00 and midnight counted that hour twice.
func TestFallBackDayIsNotDoubleCounted(t *testing.T) {
	useZone(t, "America/New_York")
	s := openTemp(t)
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.Local) // 25 hours long
	// Wall-clock times, not day.Add(...): on a 25-hour day adding 23h lands at
	// 22:00, which misses the hour the bug lived in.
	wall := func(h, m int) time.Time { return time.Date(2026, 11, 1, h, m, 0, 0, time.Local) }
	for _, at := range []time.Time{wall(10, 0), wall(23, 10)} {
		if err := s.FlushApps(at.Unix(), []types.AppTraffic{{Name: "claude", RxBytes: 100}}); err != nil {
			t.Fatal(err)
		}
	}
	until := wall(23, 30)
	for _, c := range []struct {
		name  string
		since time.Time
	}{
		{"week", day.AddDate(0, 0, -6)},
		{"today", day},
	} {
		apps, err := s.Apps(c.since, until)
		if err != nil {
			t.Fatal(err)
		}
		if len(apps) != 1 || apps[0].RxBytes != 200 {
			t.Errorf("%s on the fall-back day = %+v, want a single row with rx=200", c.name, apps)
		}
	}
}

// Same arithmetic on the leading edge: for a `since` inside the repeated hour,
// +86400s stayed inside the same day, so the rollup window started *before*
// since and swallowed traffic the caller had excluded.
func TestFallBackDayLeadingEdge(t *testing.T) {
	useZone(t, "America/New_York")
	s := openTemp(t)
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.Local)
	for _, at := range []time.Time{day.Add(10 * time.Minute), day.Add(time.Hour)} {
		if err := s.FlushApps(at.Unix(), []types.AppTraffic{{Name: "claude", RxBytes: 100}}); err != nil {
			t.Fatal(err)
		}
	}
	apps, err := s.Apps(day.Add(30*time.Minute), day.Add(90*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 1 || apps[0].RxBytes != 100 {
		t.Fatalf("range starting mid-hour = %+v, want only the 100 bytes inside it", apps)
	}
}

// A closed range must not be served from today's rollup row: the row holds the
// whole day, including bytes recorded after `until`.
func TestClosedRangeIgnoresLaterBytes(t *testing.T) {
	s := openTemp(t)
	now := time.Now()
	mid := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	if now.Sub(mid) < 2*time.Hour {
		t.Skip("needs a couple of hours of today to have passed")
	}
	// One bucket an hour ago, one just now.
	if err := s.FlushApps(now.Add(-time.Hour).Unix(), []types.AppTraffic{{Name: "claude", RxBytes: 100}}); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushApps(now.Add(-10*time.Second).Unix(), []types.AppTraffic{{Name: "claude", RxBytes: 500}}); err != nil {
		t.Fatal(err)
	}
	apps, err := s.Apps(mid, now.Add(-30*time.Minute)) // excludes the recent 500
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 1 || apps[0].RxBytes != 100 {
		t.Fatalf("closed range = %+v, want only the 100 bytes recorded before `until`", apps)
	}
}

// The size cap used to cut samples at oldest+86400 (mid-day) but rollups at the
// preceding midnight, stranding a rollup day whose samples were gone — the
// ranked lists then reported bytes the chart couldn't see, permanently.
func TestSizeCapCutsSamplesAndRollupsTogether(t *testing.T) {
	s := openTemp(t)
	base := time.Date(2026, 6, 1, 23, 50, 0, 0, time.Local) // late in the day
	for d := 0; d < 4; d++ {
		day := base.AddDate(0, 0, d)
		apps := make([]types.AppTraffic, 0, 300)
		doms := make([]types.DomainStat, 0, 300)
		for i := 0; i < 300; i++ {
			n := "app-" + time.Unix(int64(i), 0).String() + day.String()
			apps = append(apps, types.AppTraffic{Name: n, RxBytes: 1000})
			doms = append(doms, types.DomainStat{Domain: n + ".example", RxBytes: 1000})
		}
		if err := s.FlushApps(day.Unix(), apps); err != nil {
			t.Fatal(err)
		}
		if err := s.FlushDomains(day.Unix(), doms); err != nil {
			t.Fatal(err)
		}
	}
	s.Checkpoint()
	_ = s.Vacuum()
	if _, err := s.EnforceSizeCap(s.SizeOnDisk() - 1); err != nil {
		t.Fatal(err)
	}
	assertRollupMatchesSamples(t, s)
}

// Retention cut the samples at an arbitrary instant but the rollup at midnight,
// so the day straddling the cutoff kept a full rollup row over partial samples.
func TestPurgeRebuildsStraddlingDay(t *testing.T) {
	s := openTemp(t)
	now := time.Now()
	mid := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	yesterday := mid.AddDate(0, 0, -1)
	for _, at := range []time.Time{yesterday.Add(2 * time.Hour), yesterday.Add(20 * time.Hour)} {
		if err := s.FlushApps(at.Unix(), []types.AppTraffic{{Name: "claude", RxBytes: 100}}); err != nil {
			t.Fatal(err)
		}
	}
	// Cut in the middle of yesterday: the morning bucket goes, the evening stays.
	if _, err := s.Purge(yesterday.Add(10 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertRollupMatchesSamples(t, s)
	apps, err := s.Apps(yesterday, mid)
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 1 || apps[0].RxBytes != 100 {
		t.Fatalf("after purge = %+v, want only the surviving 100 bytes", apps)
	}
}

// Rollup days whose samples are gone are invisible to a samples-driven
// comparison, so VerifyRollups sweeps them explicitly.
func TestVerifyRollupsSweepsOrphanDays(t *testing.T) {
	s := openTemp(t)
	now := time.Now()
	mid := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	if err := s.FlushApps(mid.Add(time.Hour).Unix(), []types.AppTraffic{{Name: "claude", RxBytes: 100}}); err != nil {
		t.Fatal(err)
	}
	// A rollup day from long ago whose samples no longer exist.
	old := mid.AddDate(0, 0, -40).Unix()
	if _, err := s.db.Exec(`INSERT INTO app_daily (day, app, rx, tx) VALUES (?, 'ghost', 999, 0)`, old); err != nil {
		t.Fatal(err)
	}
	repaired, err := s.VerifyRollups(3)
	if err != nil {
		t.Fatal(err)
	}
	if !repaired {
		t.Fatal("orphan rollup day went unnoticed")
	}
	assertRollupMatchesSamples(t, s)
}

// assertRollupMatchesSamples is the invariant behind every ranked number the
// dashboard shows: for each local day, rollup totals equal sample totals.
func assertRollupMatchesSamples(t *testing.T, s *Store) {
	t.Helper()
	for _, m := range []struct{ daily, samples string }{
		{"app_daily", "app_samples"},
		{"domain_daily", "domain_samples"},
	} {
		days := map[int64]bool{}
		rows, err := s.db.Query(`SELECT DISTINCT day FROM ` + m.daily)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var d int64
			if err := rows.Scan(&d); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			days[d] = true
		}
		rows.Close()
		brows, err := s.db.Query(`SELECT DISTINCT bucket FROM ` + m.samples)
		if err != nil {
			t.Fatal(err)
		}
		for brows.Next() {
			var b int64
			if err := brows.Scan(&b); err != nil {
				brows.Close()
				t.Fatal(err)
			}
			days[dayStart(b)] = true
		}
		brows.Close()

		for day := range days {
			var srx, stx, rrx, rtx uint64
			if err := s.db.QueryRow(`SELECT IFNULL(SUM(rx),0), IFNULL(SUM(tx),0) FROM `+m.samples+
				` WHERE bucket >= ? AND bucket < ?`, day, nextDay(day)).Scan(&srx, &stx); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRow(`SELECT IFNULL(SUM(rx),0), IFNULL(SUM(tx),0) FROM `+m.daily+
				` WHERE day = ?`, day).Scan(&rrx, &rtx); err != nil {
				t.Fatal(err)
			}
			if srx != rrx || stx != rtx {
				t.Errorf("%s %s: rollup %d/%d != samples %d/%d",
					m.daily, time.Unix(day, 0).Format("2006-01-02"), rrx, rtx, srx, stx)
			}
		}
	}
}
