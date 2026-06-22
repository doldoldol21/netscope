// Package alerts decides when to notify the user that their network usage has
// crossed a configured threshold (a daily total cap, or a per-app daily cap).
// It is pure logic — posting the actual macOS notification is the caller's job —
// and it de-duplicates so each threshold fires at most once per calendar day.
package alerts

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Config holds the user's alert thresholds, in bytes. A zero value disables that
// threshold.
type Config struct {
	DailyTotalBytes int64 `json:"dailyTotalBytes"` // alert when today's total crosses this
	PerAppBytes     int64 `json:"perAppBytes"`     // alert when any one app crosses this today
	// Upload watch (privacy): uploads are data leaving your machine — surprise
	// backups, cloud sync, exfiltration. Zero disables.
	DailyUploadBytes  int64 `json:"dailyUploadBytes"`  // alert when today's total upload crosses this
	PerAppUploadBytes int64 `json:"perAppUploadBytes"` // alert when any one app's upload crosses this today
}

// Alert is a single notification to post.
type Alert struct {
	Title string
	Body  string
}

// Checker evaluates thresholds against today's totals and remembers what it has
// already fired today so it doesn't repeat. Safe for concurrent use.
type Checker struct {
	mu    sync.Mutex
	cfg   Config
	day   string
	fired map[string]bool
}

// New returns a Checker with the given initial config.
func New(cfg Config) *Checker {
	return &Checker{cfg: cfg, fired: map[string]bool{}}
}

// Config returns the current thresholds.
func (c *Checker) Config() Config {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg
}

// SetConfig replaces the thresholds and clears the per-day fired state, so new
// thresholds are evaluated cleanly against the current totals.
func (c *Checker) SetConfig(cfg Config) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cfg = cfg
	c.fired = map[string]bool{}
}

// Check evaluates the thresholds for the given day (e.g. "2026-06-19") against
// today's total bytes and per-app bytes, returning any alerts to post now. Each
// threshold fires at most once per day; the day rolling over resets that.
func (c *Checker) Check(day string, totalBytes int64, perApp map[string]int64) []Alert {
	c.mu.Lock()
	defer c.mu.Unlock()
	if day != c.day {
		c.day = day
		c.fired = map[string]bool{}
	}
	var out []Alert
	if c.cfg.DailyTotalBytes > 0 && totalBytes >= c.cfg.DailyTotalBytes && !c.fired["total"] {
		c.fired["total"] = true
		out = append(out, Alert{
			Title: "netscope",
			Body:  fmt.Sprintf("Today's traffic passed %s (now %s).", humanBytes(c.cfg.DailyTotalBytes), humanBytes(totalBytes)),
		})
	}
	if c.cfg.PerAppBytes > 0 {
		for name, b := range perApp {
			key := "app:" + name
			if b >= c.cfg.PerAppBytes && !c.fired[key] {
				c.fired[key] = true
				out = append(out, Alert{
					Title: "netscope",
					Body:  fmt.Sprintf("%s used %s today (limit %s).", name, humanBytes(b), humanBytes(c.cfg.PerAppBytes)),
				})
			}
		}
	}
	return out
}

// CheckUpload evaluates the upload-watch thresholds (privacy) for the day
// against today's total upload and per-app upload bytes. Like Check, each
// threshold fires at most once per day. Shares the day/fired state with Check.
func (c *Checker) CheckUpload(day string, totalUpload int64, perAppUpload map[string]int64) []Alert {
	c.mu.Lock()
	defer c.mu.Unlock()
	if day != c.day {
		c.day = day
		c.fired = map[string]bool{}
	}
	var out []Alert
	if c.cfg.DailyUploadBytes > 0 && totalUpload >= c.cfg.DailyUploadBytes && !c.fired["upload"] {
		c.fired["upload"] = true
		out = append(out, Alert{
			Title: "netscope",
			Body:  fmt.Sprintf("⬆ Uploads passed %s today (now %s).", humanBytes(c.cfg.DailyUploadBytes), humanBytes(totalUpload)),
		})
	}
	if c.cfg.PerAppUploadBytes > 0 {
		for name, b := range perAppUpload {
			key := "upapp:" + name
			if b >= c.cfg.PerAppUploadBytes && !c.fired[key] {
				c.fired[key] = true
				out = append(out, Alert{
					Title: "netscope",
					Body:  fmt.Sprintf("⬆ %s uploaded %s today (limit %s).", name, humanBytes(b), humanBytes(c.cfg.PerAppUploadBytes)),
				})
			}
		}
	}
	return out
}

// humanBytes formats a byte count like "5.2 GB".
func humanBytes(n int64) string {
	f := float64(n)
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d %s", n, units[i])
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

// ConfigPath returns the on-disk path for the alert config, or "" if it can't be
// determined (~/Library/Application Support/netscope/alerts.json on macOS).
func ConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		return ""
	}
	return filepath.Join(dir, "netscope", "alerts.json")
}

// Load reads the config from path; a missing or unreadable file yields a zero
// (all-disabled) Config rather than an error.
func Load(path string) Config {
	var cfg Config
	if path == "" {
		return cfg
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	_ = json.Unmarshal(b, &cfg)
	return cfg
}

// Save writes the config to path, creating the parent directory if needed.
func Save(path string, cfg Config) error {
	if path == "" {
		return fmt.Errorf("alerts: no config path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
