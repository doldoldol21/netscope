// Package dataplan turns recorded per-network usage into "how much of my
// monthly data allowance is gone" — the number a tethered user would otherwise
// have to open their carrier's app to see. It is pure logic: billing-cycle
// arithmetic plus a quota status, with no I/O and no platform dependencies.
package dataplan

import "time"

// Config is the user's metered-data plan. A zero LimitBytes disables everything.
type Config struct {
	LimitBytes int64  `json:"limitBytes"` // monthly allowance in bytes (e.g. 100 GB)
	StartDay   int    `json:"startDay"`   // billing cycle start day-of-month, 1-31 (0 → 1)
	Iface      string `json:"iface"`      // BSD name to meter; "" = every tethering network
	// WarnPercent is the early-warning threshold; 0 uses 80. The 100% crossing
	// always alerts regardless.
	WarnPercent int `json:"warnPercent"`
}

// Enabled reports whether a limit is set.
func (c Config) Enabled() bool { return c.LimitBytes > 0 }

// Warn returns the early-warning percentage, defaulting to 80.
func (c Config) Warn() int {
	if c.WarnPercent <= 0 || c.WarnPercent >= 100 {
		return 80
	}
	return c.WarnPercent
}

// CycleStart returns local midnight on the first day of the billing cycle that
// contains now. A startDay past the end of a short month clamps to that month's
// last day (a 31st cycle starts Feb 28/29).
func CycleStart(now time.Time, startDay int) time.Time {
	startDay = clampStartDay(startDay)
	s := dayIn(now.Year(), now.Month(), startDay, now.Location())
	if now.Before(s) {
		// Still before this month's boundary — we're in the cycle that opened
		// last month.
		prev := s.AddDate(0, 0, -s.Day()) // last day of the previous month
		s = dayIn(prev.Year(), prev.Month(), startDay, now.Location())
	}
	return s
}

// CycleEnd returns the exclusive end of the cycle containing now, i.e. the next
// cycle's start.
func CycleEnd(now time.Time, startDay int) time.Time {
	s := CycleStart(now, startDay)
	// Step one calendar month from the cycle's own month, clamping the day the
	// same way its start was. Counting days instead cannot work: the start is
	// already clamped to the month's last day, so "32 days on" from January 31
	// lands on March 4 and February's boundary is skipped, leaving a cycle that
	// runs two months. time.Date normalises month 13 into the next January.
	return dayIn(s.Year(), s.Month()+1, clampStartDay(startDay), now.Location())
}

// dayIn builds local midnight on the given day-of-month, clamped to the month's
// real length.
func dayIn(y int, m time.Month, day int, loc *time.Location) time.Time {
	last := time.Date(y, m+1, 0, 0, 0, 0, 0, loc).Day() // day 0 of next month
	if day > last {
		day = last
	}
	return time.Date(y, m, day, 0, 0, 0, 0, loc)
}

func clampStartDay(d int) int {
	if d < 1 {
		return 1
	}
	if d > 31 {
		return 31
	}
	return d
}

// Status is a plan's state at a point in time — what the dashboard renders and
// what the alert checker compares against.
type Status struct {
	Enabled        bool    `json:"enabled"`
	LimitBytes     int64   `json:"limitBytes"`
	UsedBytes      int64   `json:"usedBytes"`
	RemainingBytes int64   `json:"remainingBytes"` // 0 once over the limit
	Percent        float64 `json:"percent"`        // may exceed 100
	CycleStart     int64   `json:"cycleStart"`     // unix seconds, local midnight
	CycleEnd       int64   `json:"cycleEnd"`       // unix seconds, exclusive
	DaysLeft       int     `json:"daysLeft"`       // whole days until the cycle rolls over
	// ProjectedBytes extrapolates the current daily average to the end of the
	// cycle — "at this rate you'll finish at X". 0 when there's nothing to
	// extrapolate from yet.
	ProjectedBytes int64 `json:"projectedBytes"`
}

// Compute builds the Status for used bytes in the cycle containing now.
func Compute(cfg Config, used int64, now time.Time) Status {
	start := CycleStart(now, cfg.StartDay)
	end := CycleEnd(now, cfg.StartDay)
	st := Status{
		Enabled:    cfg.Enabled(),
		LimitBytes: cfg.LimitBytes,
		UsedBytes:  used,
		CycleStart: start.Unix(),
		CycleEnd:   end.Unix(),
	}
	if d := int(end.Sub(now).Hours() / 24); d > 0 {
		st.DaysLeft = d
	}
	if cfg.LimitBytes > 0 {
		st.Percent = 100 * float64(used) / float64(cfg.LimitBytes)
		if rem := cfg.LimitBytes - used; rem > 0 {
			st.RemainingBytes = rem
		}
	}
	// Linear projection over the elapsed fraction of the cycle. Guarded so a
	// fresh cycle (elapsed ≈ 0) doesn't project an absurd number.
	elapsed := now.Sub(start)
	total := end.Sub(start)
	if used > 0 && elapsed >= time.Hour && total > 0 {
		st.ProjectedBytes = int64(float64(used) * (float64(total) / float64(elapsed)))
	}
	return st
}

// CycleKey identifies the cycle containing now, for once-per-cycle alert
// de-duplication (e.g. "2026-07-15").
func CycleKey(now time.Time, startDay int) string {
	return CycleStart(now, startDay).Format("2006-01-02")
}
