package dataplan

import (
	"testing"
	"time"
)

func date(y int, m time.Month, d, h int) time.Time {
	return time.Date(y, m, d, h, 0, 0, 0, time.Local)
}

func TestCycleStart(t *testing.T) {
	tests := []struct {
		name     string
		now      time.Time
		startDay int
		want     time.Time
	}{
		{"after the boundary", date(2026, time.July, 20, 13), 15, date(2026, time.July, 15, 0)},
		{"on the boundary", date(2026, time.July, 15, 0), 15, date(2026, time.July, 15, 0)},
		{"before the boundary rolls back", date(2026, time.July, 3, 9), 15, date(2026, time.June, 15, 0)},
		{"day 1 is the whole month", date(2026, time.July, 3, 9), 1, date(2026, time.July, 1, 0)},
		{"zero day defaults to 1", date(2026, time.July, 3, 9), 0, date(2026, time.July, 1, 0)},
		{"31st clamps in a short month", date(2026, time.March, 5, 9), 31, date(2026, time.February, 28, 0)},
		{"31st in a 31-day month", date(2026, time.March, 31, 9), 31, date(2026, time.March, 31, 0)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CycleStart(tt.now, tt.startDay); !got.Equal(tt.want) {
				t.Errorf("CycleStart = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCycleEndFollowsStart(t *testing.T) {
	for _, day := range []int{1, 15, 28, 31} {
		now := date(2026, time.January, 20, 12)
		start, end := CycleStart(now, day), CycleEnd(now, day)
		if !end.After(start) {
			t.Fatalf("day %d: end %v not after start %v", day, end, start)
		}
		// The end of a cycle is the start of the next one.
		if got := CycleStart(end, day); !got.Equal(end) {
			t.Errorf("day %d: CycleStart(end) = %v, want %v", day, got, end)
		}
	}
}

func TestCompute(t *testing.T) {
	const gb = int64(1) << 30
	cfg := Config{LimitBytes: 100 * gb, StartDay: 1}
	now := date(2026, time.July, 16, 0) // half way through a 31-day cycle
	st := Compute(cfg, 40*gb, now)

	if !st.Enabled {
		t.Fatal("Enabled = false")
	}
	if st.Percent != 40 {
		t.Errorf("Percent = %v, want 40", st.Percent)
	}
	if st.RemainingBytes != 60*gb {
		t.Errorf("RemainingBytes = %d, want %d", st.RemainingBytes, 60*gb)
	}
	if st.DaysLeft != 16 {
		t.Errorf("DaysLeft = %d, want 16 (Jul 16 → Aug 1)", st.DaysLeft)
	}
	// 40 GB in ~half the cycle projects to ~80 GB.
	if st.ProjectedBytes < 75*gb || st.ProjectedBytes > 85*gb {
		t.Errorf("ProjectedBytes = %d, want ~80 GB", st.ProjectedBytes)
	}
}

func TestComputeOverLimit(t *testing.T) {
	const gb = int64(1) << 30
	st := Compute(Config{LimitBytes: 10 * gb, StartDay: 1}, 12*gb, date(2026, time.July, 20, 0))
	if st.Percent != 120 {
		t.Errorf("Percent = %v, want 120", st.Percent)
	}
	if st.RemainingBytes != 0 {
		t.Errorf("RemainingBytes = %d, want 0 when over", st.RemainingBytes)
	}
}

func TestComputeDisabled(t *testing.T) {
	st := Compute(Config{}, 5<<30, time.Now())
	if st.Enabled || st.Percent != 0 || st.RemainingBytes != 0 {
		t.Errorf("zero config should be disabled and zeroed: %+v", st)
	}
}

func TestNoProjectionAtCycleStart(t *testing.T) {
	// Minutes into a cycle, extrapolation would be nonsense; suppress it.
	st := Compute(Config{LimitBytes: 100 << 30, StartDay: 1}, 1<<30, date(2026, time.July, 1, 0).Add(5*time.Minute))
	if st.ProjectedBytes != 0 {
		t.Errorf("ProjectedBytes = %d, want 0 right after the cycle opens", st.ProjectedBytes)
	}
}

func TestWarnDefault(t *testing.T) {
	if got := (Config{}).Warn(); got != 80 {
		t.Errorf("Warn() = %d, want 80", got)
	}
	if got := (Config{WarnPercent: 90}).Warn(); got != 90 {
		t.Errorf("Warn() = %d, want 90", got)
	}
	if got := (Config{WarnPercent: 150}).Warn(); got != 80 {
		t.Errorf("out-of-range Warn() = %d, want 80", got)
	}
}

func TestCycleKeyStableWithinCycle(t *testing.T) {
	a := CycleKey(date(2026, time.July, 16, 3), 15)
	b := CycleKey(date(2026, time.August, 2, 22), 15)
	c := CycleKey(date(2026, time.August, 16, 1), 15)
	if a != b {
		t.Errorf("keys differ inside one cycle: %s vs %s", a, b)
	}
	if a == c {
		t.Errorf("key unchanged across the cycle boundary: %s", c)
	}
}

// A cycle is one calendar month. CycleStart clamps a 29–31 start day to the
// month's last day, so CycleEnd cannot advance by a day count: 32 days on from
// January 31 is March 4, which skipped February's boundary entirely and left
// the meter reporting a two-month cycle — doubling daysLeft and computing the
// projection over the wrong window.
func TestCycleIsOneMonthForLateStartDays(t *testing.T) {
	for _, tc := range []struct {
		now             string
		day             int
		wantStart, want string
	}{
		{"2026-02-15T12:00:00Z", 31, "2026-01-31", "2026-02-28"},
		{"2026-03-01T12:00:00Z", 31, "2026-02-28", "2026-03-31"},
		{"2026-03-15T12:00:00Z", 30, "2026-02-28", "2026-03-30"},
		{"2026-02-10T12:00:00Z", 29, "2026-01-29", "2026-02-28"},
		{"2026-04-10T12:00:00Z", 31, "2026-03-31", "2026-04-30"},
		// A leap February still has a 29th to land on.
		{"2028-02-10T12:00:00Z", 29, "2028-01-29", "2028-02-29"},
		// Rolling into the next year must not rewind to January of this one.
		{"2026-12-20T12:00:00Z", 31, "2026-11-30", "2026-12-31"},
		{"2027-01-05T12:00:00Z", 31, "2026-12-31", "2027-01-31"},
		// Ordinary start days were never affected; pin them so they stay put.
		{"2026-06-15T12:00:00Z", 15, "2026-06-15", "2026-07-15"},
		{"2026-02-15T12:00:00Z", 1, "2026-02-01", "2026-03-01"},
	} {
		now, err := time.ParseInLocation(time.RFC3339, tc.now, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		start, end := CycleStart(now, tc.day), CycleEnd(now, tc.day)
		if got := start.Format("2006-01-02"); got != tc.wantStart {
			t.Errorf("CycleStart(%s, %d) = %s, want %s", tc.now, tc.day, got, tc.wantStart)
		}
		if got := end.Format("2006-01-02"); got != tc.want {
			t.Errorf("CycleEnd(%s, %d) = %s, want %s", tc.now, tc.day, got, tc.want)
		}
		// The invariants the meter depends on: the cycle contains now, and it
		// is a month rather than a stretch of them.
		if start.After(now) || !end.After(now) {
			t.Errorf("cycle %s–%s does not contain %s", start, end, tc.now)
		}
		if days := int(end.Sub(start).Hours() / 24); days < 28 || days > 31 {
			t.Errorf("cycle %s–%s spans %d days, want one month", start, end, days)
		}
	}
}
