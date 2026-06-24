// Package metered tracks data usage on "metered" interfaces — typically a phone
// tethered over USB, where the bytes count against a carrier data plan. The user
// tags an interface as metered, optionally sets a monthly budget and the day the
// billing cycle resets; usage is summed from the store's per-interface daily
// totals since the current cycle's start.
//
// Caveats (surfaced in the UI): netscope counts captured packet bytes, which
// won't match a carrier's metering exactly (typically within a few percent), it
// can't identify the carrier (the user labels the plan), and it only counts
// while this Mac uses the tether with netscope running.
package metered

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// Plan is a metered interface's configuration.
type Plan struct {
	Label         string `json:"label"`         // user label, e.g. "SKT iPhone"
	BudgetBytes   uint64 `json:"budgetBytes"`   // 0 = no budget
	CycleStartDay int    `json:"cycleStartDay"` // day of month the cycle resets (1..28); 0 => 1
}

// Config is the persisted set of metered interfaces, keyed by interface name.
type Config struct {
	Interfaces map[string]Plan `json:"interfaces"`
}

// Store loads/saves the metered config from a JSON file, guarding concurrent
// access from the API handlers.
type Store struct {
	path string
	mu   sync.Mutex
	cfg  Config
}

// Open reads the config at path (a missing/corrupt file yields an empty config).
func Open(path string) *Store {
	s := &Store{path: path, cfg: Config{Interfaces: map[string]Plan{}}}
	if b, err := os.ReadFile(path); err == nil {
		var c Config
		if json.Unmarshal(b, &c) == nil && c.Interfaces != nil {
			s.cfg = c
		}
	}
	return s
}

// Plans returns a copy of the configured interfaces.
func (s *Store) Plans() map[string]Plan {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]Plan, len(s.cfg.Interfaces))
	for k, v := range s.cfg.Interfaces {
		out[k] = v
	}
	return out
}

// Set marks an interface metered with the given plan (and persists). A zero
// CycleStartDay normalizes to 1; values are clamped to 1..28.
func (s *Store) Set(iface string, p Plan) error {
	if p.CycleStartDay <= 0 {
		p.CycleStartDay = 1
	}
	if p.CycleStartDay > 28 {
		p.CycleStartDay = 28
	}
	s.mu.Lock()
	if s.cfg.Interfaces == nil {
		s.cfg.Interfaces = map[string]Plan{}
	}
	s.cfg.Interfaces[iface] = p
	s.mu.Unlock()
	return s.save()
}

// Remove un-marks an interface as metered (and persists).
func (s *Store) Remove(iface string) error {
	s.mu.Lock()
	delete(s.cfg.Interfaces, iface)
	s.mu.Unlock()
	return s.save()
}

func (s *Store) save() error {
	s.mu.Lock()
	b, err := json.MarshalIndent(s.cfg, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return err
	}
	// Atomic write: temp + rename so a crash mid-write can't truncate the file.
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// CycleStart returns the local-midnight unix time when the current billing cycle
// began: the most recent occurrence of day-of-month cycleStartDay at or before
// now. Days past the end of a short month roll to that month's start.
func CycleStart(now time.Time, cycleStartDay int) int64 {
	if cycleStartDay <= 0 {
		cycleStartDay = 1
	}
	if cycleStartDay > 28 {
		cycleStartDay = 28
	}
	y, m, d := now.Date()
	loc := now.Location()
	// Candidate: this month's cycle day.
	start := time.Date(y, m, cycleStartDay, 0, 0, 0, 0, loc)
	if d < cycleStartDay {
		// Cycle hasn't started this month yet — use last month's.
		start = start.AddDate(0, -1, 0)
	}
	return start.Unix()
}
