// Package demo provides a synthetic traffic source and resolver so the UI can
// be run and developed without root (no packet capture) while still showing
// realistic, named per-app traffic — unlike offline pcap replay, where apps
// resolve to "unknown" because the original sockets no longer exist.
package demo

import (
	"context"
	"math/rand"
	"time"

	"github.com/doldoldol21/netscope/internal/dnscache"
	"github.com/doldoldol21/netscope/pkg/types"
)

// conn is one synthetic connection belonging to an app, with a stable local
// port (so the resolver can map it back to the app) and a domain/IP.
type conn struct {
	app      string
	lport    uint16
	ip       string
	host     string
	activity float64 // probability of emitting traffic on a given tick
	baseRx   int     // typical download bytes per burst
	baseTx   int     // typical upload bytes per burst
}

// conns models a plausible mix: AI assistants dominate, plus everyday apps.
var conns = []conn{
	{"Claude", 51001, "160.79.104.10", "api.anthropic.com", 0.9, 90_000, 6_000},
	{"Claude", 51002, "104.18.32.5", "claude.ai", 0.5, 30_000, 3_000},
	{"ChatGPT", 51010, "104.18.7.10", "chatgpt.com", 0.7, 60_000, 5_000},
	{"ChatGPT", 51011, "23.102.140.10", "api.openai.com", 0.6, 50_000, 4_000},
	{"Cursor", 51015, "34.120.10.5", "api.openai.com", 0.5, 40_000, 8_000},
	{"Safari", 51020, "142.250.1.10", "www.google.com", 0.5, 25_000, 2_000},
	{"Safari", 51021, "140.82.113.10", "github.com", 0.4, 35_000, 3_000},
	{"Spotify", 51030, "35.186.224.10", "spotify.com", 0.6, 45_000, 800},
	{"Dropbox", 51040, "162.125.1.10", "dropbox.com", 0.25, 20_000, 40_000},
	{"Mail", 51050, "17.42.1.10", "imap.mail.me.com", 0.2, 8_000, 1_500},
}

// Resolver maps the synthetic local ports back to their app, satisfying
// engine.Resolver.
type Resolver struct {
	byPort map[uint16]types.Process
}

// NewResolver builds the demo resolver from the connection table.
func NewResolver() *Resolver {
	m := make(map[uint16]types.Process, len(conns))
	for _, c := range conns {
		m[c.lport] = types.Process{Name: c.app, Path: "/Applications/" + c.app + ".app"}
	}
	return &Resolver{byPort: m}
}

// Lookup resolves a connection key to its app by local port.
func (r *Resolver) Lookup(k types.ConnKey) (types.Process, bool) {
	p, ok := r.byPort[k.LocalPort]
	return p, ok
}

// SeedDNS pre-populates the cache with the demo IP→host mappings so the engine
// groups traffic by domain just as it would from sniffed DNS.
func SeedDNS(c *dnscache.Cache) {
	for _, cn := range conns {
		c.Put(cn.ip, cn.host)
	}
}

// Source generates synthetic flows over time, mimicking a live capture.
type Source struct {
	rnd  *rand.Rand
	tick time.Duration
}

// NewSource returns a demo source. seed varies the traffic; tick is the
// emission interval.
func NewSource(seed int64) *Source {
	return &Source{rnd: rand.New(rand.NewSource(seed)), tick: 250 * time.Millisecond}
}

// Name identifies the source in snapshots.
func (s *Source) Name() string { return "demo (synthetic)" }

// Run emits flows until the context is cancelled.
func (s *Source) Run(ctx context.Context, out chan<- types.Flow) error {
	t := time.NewTicker(s.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			for _, c := range conns {
				if s.rnd.Float64() > c.activity {
					continue
				}
				// Download burst.
				if rx := jitter(s.rnd, c.baseRx); rx > 0 {
					if !emit(ctx, out, c, types.DirIn, rx) {
						return ctx.Err()
					}
				}
				// Smaller, less frequent upload.
				if s.rnd.Float64() < 0.6 {
					if tx := jitter(s.rnd, c.baseTx); tx > 0 {
						if !emit(ctx, out, c, types.DirOut, tx) {
							return ctx.Err()
						}
					}
				}
			}
		}
	}
}

// jitter returns base scaled randomly in roughly [0.4x, 1.4x].
func jitter(r *rand.Rand, base int) int {
	if base <= 0 {
		return 0
	}
	return int(float64(base) * (0.4 + r.Float64()))
}

func emit(ctx context.Context, out chan<- types.Flow, c conn, dir types.Direction, bytes int) bool {
	f := types.Flow{
		Timestamp:  time.Now(),
		Proto:      types.ProtoTCP,
		Direction:  dir,
		LocalPort:  c.lport,
		RemoteIP:   c.ip,
		RemotePort: 443,
		Bytes:      uint64(bytes),
	}
	select {
	case out <- f:
		return true
	case <-ctx.Done():
		return false
	}
}
