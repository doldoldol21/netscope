package alerts

import (
	"path/filepath"
	"testing"
)

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
