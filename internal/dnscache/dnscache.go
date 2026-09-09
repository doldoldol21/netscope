// Package dnscache maps remote IP addresses back to the hostnames the local
// machine resolved them from. netscope can only see IPs on the wire; by
// sniffing DNS responses it recovers the human-meaningful domain for each flow.
package dnscache

import (
	"container/list"
	"encoding/json"
	"os"
	"sort"
	"sync"
	"time"
)

type entry struct {
	ip   string
	host string
	seen time.Time
	el   *list.Element // position in Cache.order
}

// Cache is a bounded, TTL'd IP -> hostname map. Safe for concurrent use.
//
// Entries are kept in a list ordered by when they were last seen, oldest at
// the front, so eviction is a pop rather than a scan. That matters on the
// packet path: a laptop that has been up for a few days runs this cache full,
// and full is where every DNS answer and every SNI hit used to pay an O(n)
// walk over 20 000 entries to find the one to drop.
type Cache struct {
	mu    sync.RWMutex
	byIP  map[string]*entry
	order *list.List // of *entry; front = least recently seen
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
		byIP:  make(map[string]*entry),
		order: list.New(),
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
	now := c.nowFn()
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.byIP[ip]; ok {
		// Seen again, so it is now the newest entry: the back of the list.
		e.host, e.seen = host, now
		c.order.MoveToBack(e.el)
		return
	}
	if len(c.byIP) >= c.max {
		c.evictOldestLocked()
	}
	e := &entry{ip: ip, host: host, seen: now}
	e.el = c.order.PushBack(e)
	c.byIP[ip] = e
}

// Lookup returns the hostname for ip, or "" if unknown or expired.
func (c *Cache) Lookup(ip string) string {
	c.mu.RLock()
	e, ok := c.byIP[ip]
	var host string
	var seen time.Time
	if ok {
		host, seen = e.host, e.seen
	}
	c.mu.RUnlock()
	if !ok {
		return ""
	}
	if c.nowFn().Sub(seen) > c.ttl {
		c.mu.Lock()
		// Re-check under write lock in case it was refreshed.
		if cur, ok := c.byIP[ip]; ok && c.nowFn().Sub(cur.seen) > c.ttl {
			c.removeLocked(cur)
		}
		c.mu.Unlock()
		return ""
	}
	return host
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
	for el := c.order.Front(); el != nil; el = el.Next() {
		e := el.Value.(*entry)
		recs = append(recs, record{IP: e.ip, Host: e.host, Seen: e.seen})
	}
	c.mu.RUnlock()
	b, err := json.Marshal(recs)
	if err != nil {
		return err
	}
	// Owner-only: the cache is a list of every host this machine resolved, so it
	// is as sensitive as the traffic database it sits next to.
	//
	// Drop any leftover temp file first. WriteFile applies its mode only when it
	// creates the file, so a .tmp left behind by an interrupted save — or by a
	// version that wrote 0644 — would keep that mode and hand it to the renamed
	// cache. A removal that fails for any reason other than absence has to stop
	// the save: writing into that file would publish the cache at whatever mode
	// it already had.
	tmp := path + ".tmp"
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
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
	// Oldest first, so each record lands at (or near) the back and the
	// position search below is a step or two, not a walk. SaveTo already
	// writes them in this order; a file from elsewhere is sorted here.
	sort.Slice(recs, func(i, j int) bool { return recs[i].Seen.Before(recs[j].Seen) })
	now := c.nowFn()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range recs {
		if r.IP == "" || r.Host == "" || now.Sub(r.Seen) > c.ttl {
			continue
		}
		if e, ok := c.byIP[r.IP]; ok {
			if !r.Seen.After(e.seen) {
				continue // what we already hold is at least as fresh
			}
			c.removeLocked(e)
		}
		if len(c.byIP) >= c.max {
			// Full: the record has to be newer than the oldest entry to earn a
			// place, otherwise it is the one that would be evicted next anyway.
			oldest := c.order.Front().Value.(*entry)
			if !r.Seen.After(oldest.seen) {
				continue
			}
			c.evictOldestLocked()
		}
		c.insertOrderedLocked(&entry{ip: r.IP, host: r.Host, seen: r.Seen})
	}
	return nil
}

// insertOrderedLocked places e by its seen time, searching back from the
// newest end. Caller holds mu.
func (c *Cache) insertOrderedLocked(e *entry) {
	c.byIP[e.ip] = e
	for el := c.order.Back(); el != nil; el = el.Prev() {
		if !el.Value.(*entry).seen.After(e.seen) {
			e.el = c.order.InsertAfter(e, el)
			return
		}
	}
	e.el = c.order.PushFront(e)
}

// removeLocked drops e from both the map and the order. Caller holds mu.
func (c *Cache) removeLocked(e *entry) {
	delete(c.byIP, e.ip)
	c.order.Remove(e.el)
}

// evictOldestLocked removes the least-recently-seen entry — the front of the
// order — in O(1). Caller holds mu.
func (c *Cache) evictOldestLocked() {
	if el := c.order.Front(); el != nil {
		c.removeLocked(el.Value.(*entry))
	}
}
