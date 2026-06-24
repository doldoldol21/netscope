package metered

import (
	"path/filepath"
	"testing"
	"time"
)

func TestCycleStart(t *testing.T) {
	loc := time.UTC
	cases := []struct {
		now      string
		day      int
		wantDate string // expected cycle-start date (local midnight)
	}{
		// Today after the cycle day -> this month's cycle day.
		{"2026-06-24T10:00:00Z", 1, "2026-06-01"},
		// Today before the cycle day -> last month's cycle day.
		{"2026-06-24T10:00:00Z", 28, "2026-05-28"},
		// Exactly on the cycle day -> today.
		{"2026-06-15T09:00:00Z", 15, "2026-06-15"},
		// Zero normalizes to day 1.
		{"2026-06-24T00:00:00Z", 0, "2026-06-01"},
	}
	for _, c := range cases {
		now, _ := time.ParseInLocation(time.RFC3339, c.now, loc)
		got := time.Unix(CycleStart(now, c.day), 0).In(loc).Format("2006-01-02")
		if got != c.wantDate {
			t.Errorf("CycleStart(%s, %d) = %s, want %s", c.now, c.day, got, c.wantDate)
		}
	}
}

func TestStoreSetRemovePersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metered.json")
	s := Open(path)
	if err := s.Set("en5", Plan{Label: "SKT", BudgetBytes: 1000, CycleStartDay: 40}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// CycleStartDay clamps to 28.
	if got := s.Plans()["en5"].CycleStartDay; got != 28 {
		t.Errorf("CycleStartDay = %d, want clamped 28", got)
	}
	// Reload from disk: the plan persisted.
	if got := Open(path).Plans()["en5"].Label; got != "SKT" {
		t.Errorf("reloaded label = %q, want SKT", got)
	}
	if err := s.Remove("en5"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := Open(path).Plans()["en5"]; ok {
		t.Error("plan should be gone after Remove")
	}
}
