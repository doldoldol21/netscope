// Package dnscache maps remote IP addresses back to the hostnames the local
// machine resolved them from. netscope can only see IPs on the wire; by
// sniffing DNS responses it recovers the human-meaningful domain for each flow.
package dnscache

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

type entry struct {
	host string
	seen time.Time
}

// Cache is a bounded, TTL'd IP -> hostname map. Safe for concurrent use.
type Cache struct {
	mu    sync.RWMutex
	byIP  map[string]entry
	ttl   time.Duration
	max   int
	nowFn func() time.Time
}

// New returns a Cache. ttl bounds how long a mapping is trusted after it was
// last observed; max bounds the number of retained entries.
func New(ttl time.Duration, max int) *Cache {
	if ttl <= 0 {
		ttl = time.Hour
	}
	if max <= 0 {
		max = 50000
	}
	return &Cache{
		byIP:  make(map[string]entry),
		ttl:   ttl,
		max:   max,
		nowFn: time.Now,
	}
}

// Put records that ip resolves to host. The most recent mapping wins.
func (c *Cache) Put(ip, host string) {
	if ip == "" || host == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.byIP) >= c.max {
		if _, exists := c.byIP[ip]; !exists {
			c.evictOldestLocked()
		}
	}
	c.byIP[ip] = entry{host: host, seen: c.nowFn()}
}

// Lookup returns the hostname for ip, or "" if unknown or expired.
func (c *Cache) Lookup(ip string) string {
	c.mu.RLock()
	e, ok := c.byIP[ip]
	c.mu.RUnlock()
	if !ok {
		return ""
	}
	if c.nowFn().Sub(e.seen) > c.ttl {
		c.mu.Lock()
		// Re-check under write lock in case it was refreshed.
		if cur, ok := c.byIP[ip]; ok && c.nowFn().Sub(cur.seen) > c.ttl {
			delete(c.byIP, ip)
		}
		c.mu.Unlock()
		return ""
	}
	return e.host
}

// Len reports the number of cached mappings.
func (c *Cache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.byIP)
}

// record is the on-disk form of one mapping.
type record struct {
	IP   string    `json:"ip"`
	Host string    `json:"host"`
	Seen time.Time `json:"seen"`
}

// SaveTo writes the cache to path as JSON (atomically via a temp file + rename),
// so learned IP→host mappings survive a daemon restart.
func (c *Cache) SaveTo(path string) error {
	c.mu.RLock()
	recs := make([]record, 0, len(c.byIP))
	for ip, e := range c.byIP {
		recs = append(recs, record{IP: ip, Host: e.host, Seen: e.seen})
	}
	c.mu.RUnlock()
	b, err := json.Marshal(recs)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadFrom merges mappings from a file written by SaveTo, preserving each
// entry's original "seen" time (so the TTL still applies) and dropping any that
// have already expired. A missing file is not an error.
func (c *Cache) LoadFrom(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var recs []record
	if err := json.Unmarshal(b, &recs); err != nil {
		return err
	}
	now := c.nowFn()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range recs {
		if r.IP == "" || r.Host == "" || now.Sub(r.Seen) > c.ttl {
			continue
		}
		if len(c.byIP) >= c.max {
			c.evictOldestLocked()
		}
		c.byIP[r.IP] = entry{host: r.Host, seen: r.Seen}
	}
	return nil
}

// evictOldestLocked removes the least-recently-seen entry. Caller holds mu.
// O(n) but only runs at capacity, which is rare for a personal monitor.
func (c *Cache) evictOldestLocked() {
	var oldestIP string
	var oldest time.Time
	first := true
	for ip, e := range c.byIP {
		if first || e.seen.Before(oldest) {
			oldest = e.seen
			oldestIP = ip
			first = false
		}
	}
	if oldestIP != "" {
		delete(c.byIP, oldestIP)
	}
}
