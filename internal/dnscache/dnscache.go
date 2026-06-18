// Package dnscache maps remote IP addresses back to the hostnames the local
// machine resolved them from. netscope can only see IPs on the wire; by
// sniffing DNS responses it recovers the human-meaningful domain for each flow.
package dnscache

import (
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
