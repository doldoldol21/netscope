package alerts

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/doldoldol21/netscope/internal/dataplan"
)

func TestCheckUpload(t *testing.T) {
	c := New(Config{DailyUploadBytes: 1000, PerAppUploadBytes: 500})

	// Under both thresholds: nothing.
	if got := c.CheckUpload("2026-06-19", 900, map[string]int64{"Backup": 400}); len(got) != 0 {
		t.Fatalf("under thresholds should not alert, got %v", got)
	}
	// Cross daily upload + per-app upload: two alerts, once each.
	got := c.CheckUpload("2026-06-19", 1200, map[string]int64{"Backup": 600})
	if len(got) != 2 {
		t.Fatalf("want 2 upload alerts, got %d: %v", len(got), got)
	}
	if again := c.CheckUpload("2026-06-19", 5000, map[string]int64{"Backup": 9000}); len(again) != 0 {
		t.Fatalf("already fired today should be silent, got %v", again)
	}
	// Total bytes (download-heavy) must not trip the upload watch.
	c2 := New(Config{DailyUploadBytes: 1000})
	if got := c2.CheckUpload("2026-06-19", 10, nil); len(got) != 0 {
		t.Fatalf("low upload should not alert even with high total elsewhere, got %v", got)
	}
}

func TestCheckDailyTotalFiresOncePerDay(t *testing.T) {
	c := New(Config{DailyTotalBytes: 1000})

	if got := c.Check("2026-06-19", 500, nil); len(got) != 0 {
		t.Fatalf("under threshold should not alert, got %v", got)
	}
	got := c.Check("2026-06-19", 1200, nil)
	if len(got) != 1 {
		t.Fatalf("crossing threshold should alert once, got %d", len(got))
	}
	if again := c.Check("2026-06-19", 1500, nil); len(again) != 0 {
		t.Fatalf("same day should not re-alert, got %v", again)
	}
	// New day resets.
	if next := c.Check("2026-06-20", 1200, nil); len(next) != 1 {
		t.Fatalf("new day should alert again, got %d", len(next))
	}
}

func TestCheckPerApp(t *testing.T) {
	c := New(Config{PerAppBytes: 1000})
	perApp := map[string]int64{"Backup": 2000, "Safari": 100}
	got := c.Check("2026-06-19", 0, perApp)
	if len(got) != 1 {
		t.Fatalf("only the over-limit app should alert, got %d: %v", len(got), got)
	}
	// Safari grows past the limit next tick → its own one-time alert.
	perApp["Safari"] = 1500
	got = c.Check("2026-06-19", 0, perApp)
	if len(got) != 1 {
		t.Fatalf("newly-over app should alert once, got %d", len(got))
	}
}

func TestSetConfigResetsFired(t *testing.T) {
	c := New(Config{DailyTotalBytes: 1000})
	c.Check("2026-06-19", 1200, nil) // fires
	c.SetConfig(Config{DailyTotalBytes: 500})
	if got := c.Check("2026-06-19", 1200, nil); len(got) != 1 {
		t.Fatalf("config change should allow a fresh alert, got %d", len(got))
	}
}

func TestLoadSaveRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "alerts.json")
	want := Config{DailyTotalBytes: 5 << 30, PerAppBytes: 1 << 30}
	if err := Save(path, want); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got := Load(path); got != want {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got, want)
	}
	// Missing file → zero config, no panic.
	if got := Load(filepath.Join(t.TempDir(), "none.json")); got != (Config{}) {
		t.Fatalf("missing file should yield zero config, got %+v", got)
	}
}

func TestCheckPlan(t *testing.T) {
	const gb = int64(1) << 30
	c := New(Config{Plan: dataplan.Config{LimitBytes: 100 * gb, StartDay: 1}})

	if got := c.CheckPlan("2026-07-01", 50*gb); len(got) != 0 {
		t.Fatalf("half the plan should be quiet, got %d", len(got))
	}
	// Crossing the default 80% warning fires once.
	if got := c.CheckPlan("2026-07-01", 82*gb); len(got) != 1 {
		t.Fatalf("80%% crossing should alert once, got %d", len(got))
	}
	if got := c.CheckPlan("2026-07-01", 85*gb); len(got) != 0 {
		t.Fatalf("warning should not repeat in the same cycle, got %d", len(got))
	}
	// Running out fires its own alert, also once.
	if got := c.CheckPlan("2026-07-01", 101*gb); len(got) != 1 {
		t.Fatalf("100%% crossing should alert once, got %d", len(got))
	}
	if got := c.CheckPlan("2026-07-01", 130*gb); len(got) != 0 {
		t.Fatalf("over-limit alert should not repeat, got %d", len(got))
	}
	// A new billing cycle re-arms both.
	if got := c.CheckPlan("2026-08-01", 90*gb); len(got) != 1 {
		t.Fatalf("new cycle should re-arm the warning, got %d", len(got))
	}
}

func TestCheckPlanNoLimit(t *testing.T) {
	c := New(Config{})
	if got := c.CheckPlan("2026-07-01", 999<<30); got != nil {
		t.Fatalf("unset plan should never alert, got %v", got)
	}
}

// Already over the limit on the first look: only the "used up" alert, not both.
func TestCheckPlanStartsOverLimit(t *testing.T) {
	c := New(Config{Plan: dataplan.Config{LimitBytes: 10 << 30}})
	got := c.CheckPlan("2026-07-01", 15<<30)
	if len(got) != 1 {
		t.Fatalf("want a single alert, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0].Body, "used up") {
		t.Errorf("want the over-limit message, got %q", got[0].Body)
	}
}

// A day rollover resets the daily thresholds; the plan's cycle state must not
// be swept up with them.
func TestPlanSurvivesDayRollover(t *testing.T) {
	c := New(Config{DailyTotalBytes: 1000, Plan: dataplan.Config{LimitBytes: 10 << 30}})
	if got := c.CheckPlan("2026-07-01", 9<<30); len(got) != 1 {
		t.Fatalf("want the warning, got %d", len(got))
	}
	c.Check("2026-07-02", 1, nil) // new day resets the daily fired state
	if got := c.CheckPlan("2026-07-01", 9<<30); len(got) != 0 {
		t.Fatalf("plan warning refired after a day rollover, got %d", len(got))
	}
}
