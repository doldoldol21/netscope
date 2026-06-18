// Package revdns resolves remote IPs to hostnames via reverse DNS (PTR) as a
// fallback when netscope never saw a forward DNS answer — e.g. connections that
// predate the daemon, or encrypted DNS (DoH/DoT). Lookups run asynchronously
// off the capture hot path, are de-duplicated and rate-limited, and their
// results are written into the shared dns cache so the regular lookup path
// picks them up.
package revdns

import (
	"context"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/doldoldol21/netscope/internal/dnscache"
)

// Resolver performs background PTR lookups, caching results in a dnscache.Cache.
type Resolver struct {
	cache    *dnscache.Cache
	queue    chan string
	lookupFn func(ctx context.Context, ip string) ([]string, error)
	timeout  time.Duration

	mu      sync.Mutex
	pending map[string]time.Time // de-dupes in-flight + recently-attempted IPs
	ttl     time.Duration
	nowFn   func() time.Time
}

// New starts a Resolver with the given number of workers writing into cache.
func New(cache *dnscache.Cache, workers int) *Resolver {
	if workers <= 0 {
		workers = 4
	}
	r := &Resolver{
		cache:    cache,
		queue:    make(chan string, 1024),
		lookupFn: (&net.Resolver{}).LookupAddr,
		timeout:  2 * time.Second,
		pending:  make(map[string]time.Time),
		ttl:      10 * time.Minute,
		nowFn:    time.Now,
	}
	for i := 0; i < workers; i++ {
		go r.worker()
	}
	return r
}

// Enqueue requests a PTR lookup for ip if it is a public address we have not
// recently attempted. Non-blocking; drops the request if the queue is full.
func (r *Resolver) Enqueue(ip string) {
	if !resolvable(ip) {
		return
	}
	now := r.nowFn()
	r.mu.Lock()
	if t, ok := r.pending[ip]; ok && now.Sub(t) < r.ttl {
		r.mu.Unlock()
		return
	}
	r.pending[ip] = now
	if len(r.pending) > 8192 {
		r.pruneLocked(now)
	}
	r.mu.Unlock()

	select {
	case r.queue <- ip:
	default: // queue full: forget the pending mark so it can retry later
		r.mu.Lock()
		delete(r.pending, ip)
		r.mu.Unlock()
	}
}

func (r *Resolver) worker() {
	for ip := range r.queue {
		ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
		names, err := r.lookupFn(ctx, ip)
		cancel()
		if err == nil && len(names) > 0 {
			if h := clean(names[0]); h != "" {
				r.cache.Put(ip, h)
			}
		}
	}
}

func (r *Resolver) pruneLocked(now time.Time) {
	for ip, t := range r.pending {
		if now.Sub(t) >= r.ttl {
			delete(r.pending, ip)
		}
	}
}

// clean normalises a PTR name: drop the trailing dot, lower-case.
func clean(name string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
}

// resolvable reports whether ip is a public address worth a PTR lookup.
func resolvable(ip string) bool {
	p := net.ParseIP(ip)
	if p == nil {
		return false
	}
	return !(p.IsLoopback() || p.IsPrivate() || p.IsLinkLocalUnicast() ||
		p.IsLinkLocalMulticast() || p.IsMulticast() || p.IsUnspecified())
}
