// Package resolver maps a network connection (proto + local port + remote
// endpoint) to the process that owns it, and that process to a display name.
//
// On macOS there is no syscall that takes a 4-tuple and returns a PID, so the
// resolver periodically enumerates every process' open socket file descriptors
// (libproc) and builds a reverse index. Lookups consult that index and trigger
// an on-demand rescan when a key misses and the cache is stale.
package resolver

import (
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/doldoldol21/netscope/pkg/types"
)

// rawConn is one socket extracted from the OS, platform code fills these.
type rawConn struct {
	PID   int
	Proto types.Protocol
	LPort uint16
	RAddr string
	RPort uint16
}

type portKey struct {
	proto types.Protocol
	port  uint16
}

// Resolver holds the reverse index and process metadata cache. Safe for
// concurrent use.
type Resolver struct {
	mu      sync.RWMutex
	byTuple map[types.ConnKey]types.Process
	byPort  map[portKey]types.Process
	paths   map[int]types.Process // pid -> process (path/name) cache

	lastScan    time.Time
	minInterval time.Duration

	nowFn func() time.Time // overridable for tests
}

// New returns a Resolver. minInterval bounds how often an on-demand miss can
// force a rescan, protecting against busy-looping the (relatively expensive)
// full process/fd enumeration.
func New(minInterval time.Duration) *Resolver {
	if minInterval <= 0 {
		minInterval = 300 * time.Millisecond
	}
	return &Resolver{
		byTuple:     make(map[types.ConnKey]types.Process),
		byPort:      make(map[portKey]types.Process),
		paths:       make(map[int]types.Process),
		minInterval: minInterval,
		nowFn:       time.Now,
	}
}

// Lookup resolves a connection key to its owning process. The boolean reports
// whether attribution succeeded; on a miss the caller may still record the
// flow under an "unknown" bucket.
func (r *Resolver) Lookup(k types.ConnKey) (types.Process, bool) {
	if p, ok := r.lookupCached(k); ok {
		return p, true
	}
	// Miss: the connection may be newer than our last scan. Rescan (rate
	// limited) and try once more.
	if r.Refresh() {
		return r.lookupCached(k)
	}
	return types.Process{}, false
}

func (r *Resolver) lookupCached(k types.ConnKey) (types.Process, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if p, ok := r.byTuple[k]; ok {
		return p, true
	}
	if p, ok := r.byPort[portKey{k.Proto, k.LocalPort}]; ok {
		return p, true
	}
	return types.Process{}, false
}

// Refresh rebuilds the index from the OS. It is rate limited by minInterval and
// returns true if a scan actually ran.
func (r *Resolver) Refresh() bool {
	r.mu.Lock()
	if r.nowFn().Sub(r.lastScan) < r.minInterval {
		r.mu.Unlock()
		return false
	}
	r.lastScan = r.nowFn()
	// Snapshot the path cache so platform scan can reuse it without holding the
	// lock across syscalls.
	oldPaths := r.paths
	r.mu.Unlock()

	conns, paths := scan(oldPaths)

	byTuple := make(map[types.ConnKey]types.Process, len(conns))
	byPort := make(map[portKey]types.Process, len(conns))
	for _, c := range conns {
		proc := paths[c.PID]
		if c.RAddr != "" && c.RPort != 0 {
			byTuple[types.ConnKey{
				Proto:      c.Proto,
				LocalPort:  c.LPort,
				RemoteIP:   c.RAddr,
				RemotePort: c.RPort,
			}] = proc
		}
		// Local-port fallback: keep the first writer so a connected socket is
		// not shadowed by a later wildcard listener on the same port.
		pk := portKey{c.Proto, c.LPort}
		if _, exists := byPort[pk]; !exists {
			byPort[pk] = proc
		}
	}

	r.mu.Lock()
	r.byTuple = byTuple
	r.byPort = byPort
	r.paths = paths
	r.mu.Unlock()
	return true
}

// appName derives a friendly display name from an executable path. For macOS
// .app bundles it returns the bundle name (e.g. "/Applications/Foo.app/
// Contents/MacOS/foo" -> "Foo"); otherwise the executable basename.
func appName(path string) string {
	if path == "" {
		return "unknown"
	}
	if i := strings.Index(path, ".app/"); i >= 0 {
		bundle := path[:i] // ".../Foo"
		return filepath.Base(bundle)
	}
	return filepath.Base(path)
}
