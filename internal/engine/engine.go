// Package engine is the heart of netscope: it consumes attributed flows,
// resolves each to an owning app and a domain, and maintains two sets of
// counters:
//
//   - window accumulators: reset every Bucket and flushed to storage. These
//     back the historical (today/week) queries.
//   - session accumulators: cumulative since the daemon started, never reset on
//     flush. These back the LIVE dashboard tables, so the view grows steadily
//     instead of emptying every flush interval.
//
// Instantaneous rates are derived from monotonic lifetime totals.
package engine

import (
	"context"
	"log"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/doldoldol21/netscope/internal/classify"
	"github.com/doldoldol21/netscope/internal/dnscache"
	"github.com/doldoldol21/netscope/internal/geoip"
	"github.com/doldoldol21/netscope/internal/storage"
	"github.com/doldoldol21/netscope/pkg/types"
)

// Resolver maps a connection to its owning process. Implemented by
// internal/resolver (live, libproc) and internal/demo (synthetic).
type Resolver interface {
	Lookup(types.ConnKey) (types.Process, bool)
}

// HostHinter is notified of remote IPs that have no known hostname, so it can
// resolve them out-of-band (reverse DNS). Implemented by internal/revdns.
type HostHinter interface {
	Enqueue(ip string)
}

// Config tunes the engine timings.
type Config struct {
	// Bucket is the storage flush granularity; window counters accumulate for
	// one bucket then get written and reset.
	Bucket time.Duration
	// SnapshotInterval is how often the live snapshot is recomputed.
	SnapshotInterval time.Duration
	// Retention bounds how long stored samples are kept; zero disables purging.
	Retention time.Duration
	// MaxDBBytes is a hard disk safety net: when the database (incl. WAL) exceeds
	// it, the oldest data is dropped until it fits, regardless of Retention. Zero
	// disables the cap.
	MaxDBBytes int64
	// Interface is the capture source name, surfaced in snapshots.
	Interface string
	// LiveTopN caps how many apps/domains the live snapshot carries.
	LiveTopN int
	// ActiveWindow defines "active now": apps/domains with traffic newer than
	// this are counted as active.
	ActiveWindow time.Duration
	// SessionHorizon prunes session entries idle longer than this to bound
	// memory on long-running daemons. Zero keeps everything for the session.
	SessionHorizon time.Duration
	// SelfPID, when non-zero, is the daemon's own PID; its traffic (e.g. update
	// checks) is excluded from accounting.
	SelfPID int
	// Hinter receives remote IPs with no known hostname for reverse-DNS
	// resolution. Optional.
	Hinter HostHinter
}

func (c *Config) withDefaults() {
	if c.Bucket <= 0 {
		c.Bucket = 10 * time.Second
	}
	if c.SnapshotInterval <= 0 {
		c.SnapshotInterval = time.Second
	}
	if c.LiveTopN <= 0 {
		c.LiveTopN = 25
	}
	if c.ActiveWindow <= 0 {
		c.ActiveWindow = 8 * time.Second
	}
	if c.SessionHorizon < 0 {
		c.SessionHorizon = 0
	}
}

type appAcc struct {
	name       string
	path       string
	rx         uint64
	tx         uint64
	conns      map[uint16]struct{} // distinct local ports ~ connections
	lastActive time.Time
}

type domAcc struct {
	domain     string
	app        string
	rx         uint64
	tx         uint64
	category   string
	country    string
	lastActive time.Time
}

type domKey struct{ domain, app string }

// connKey identifies a live connection: an app talking to a remote endpoint.
// Ephemeral local ports to the same endpoint collapse into one entry.
type connKey struct {
	proto      types.Protocol
	app        string
	remoteIP   string
	remotePort uint16
}

type connAcc struct {
	host      string
	path      string
	category  string
	country   string
	rx        uint64
	tx        uint64
	firstSeen time.Time
	lastSeen  time.Time
}

// Engine owns the accumulators and coordinates flushing/snapshots.
type Engine struct {
	cfg   Config
	res   Resolver
	dns   *dnscache.Cache
	store *storage.Store

	mu sync.Mutex
	// window: reset each flush, persisted to storage.
	winApps    map[string]*appAcc
	winDomains map[domKey]*domAcc
	// session: cumulative since start, drives the live view.
	sessApps    map[string]*appAcc
	sessDomains map[domKey]*domAcc
	sessStart   time.Time
	// Live connections (app ↔ remote endpoint), for the "Live connections" view.
	conns map[connKey]*connAcc

	// resetCh signals the Run loop to zero the live session counters. Handled in
	// the Run goroutine so it serializes with ingest/flush/snapshot (no races).
	resetCh chan struct{}

	// Monotonic lifetime totals, used to derive instantaneous rates.
	totalRx uint64
	totalTx uint64

	snapMu   sync.RWMutex
	snapshot types.Snapshot
	rateHist []types.RatePoint // last ~2 min of per-second rates, to seed the live chart

	ifaceMu sync.Mutex
	iface   string // capture interface, updatable as the supervisor re-opens
	paused  bool   // surfaced in snapshots so the UI can show "paused"

	rateRx, rateTx uint64
	rateAt         time.Time

	nowFn func() time.Time
}

// SetInterface updates the capture-interface name surfaced in snapshots (the
// supervisor calls this when it (re)opens on a different interface).
func (e *Engine) SetInterface(name string) {
	e.ifaceMu.Lock()
	e.iface = name
	e.ifaceMu.Unlock()
}

// SetPaused records whether capture is suspended, for snapshot reporting.
func (e *Engine) SetPaused(p bool) {
	e.ifaceMu.Lock()
	e.paused = p
	e.ifaceMu.Unlock()
}

func (e *Engine) isPaused() bool {
	e.ifaceMu.Lock()
	defer e.ifaceMu.Unlock()
	return e.paused
}

func (e *Engine) currentIface() string {
	e.ifaceMu.Lock()
	defer e.ifaceMu.Unlock()
	return e.iface
}

// New constructs an Engine. res may be nil (everything attributes to "unknown");
// store may be nil to run without persistence.
func New(cfg Config, res Resolver, dns *dnscache.Cache, store *storage.Store) *Engine {
	cfg.withDefaults()
	now := time.Now()
	e := &Engine{
		cfg:         cfg,
		res:         res,
		dns:         dns,
		store:       store,
		winApps:     make(map[string]*appAcc),
		winDomains:  make(map[domKey]*domAcc),
		sessApps:    make(map[string]*appAcc),
		sessDomains: make(map[domKey]*domAcc),
		conns:       make(map[connKey]*connAcc),
		resetCh:     make(chan struct{}, 1),
		nowFn:       time.Now,
		iface:       cfg.Interface,
	}
	e.sessStart = now
	return e
}

// Run consumes flows until the context is cancelled, driving periodic flushes
// and snapshots. It returns ctx.Err() on shutdown after a final flush.
func (e *Engine) Run(ctx context.Context, flows <-chan types.Flow) error {
	// sessStart was stamped in New with the wall clock; align it to nowFn so
	// tests with an injected clock report a sane session start.
	e.sessStart = e.nowFn()
	e.rateAt = e.nowFn()

	flushT := time.NewTicker(e.cfg.Bucket)
	snapT := time.NewTicker(e.cfg.SnapshotInterval)
	defer flushT.Stop()
	defer snapT.Stop()

	// Hourly storage maintenance: retention purge, disk-size cap, WAL checkpoint,
	// and VACUUM to actually reclaim freed space. Runs whenever we persist.
	var maintC <-chan time.Time
	if e.store != nil {
		mt := time.NewTicker(time.Hour)
		maintC = mt.C
		defer mt.Stop()
	}

	for {
		select {
		case <-ctx.Done():
			e.updateSnapshot() // leave a final, complete snapshot for viewers
			e.flush()
			return ctx.Err()
		case f, ok := <-flows:
			if !ok {
				// Source exhausted (e.g. offline replay): publish the final
				// session view so the dashboard reflects the whole capture.
				e.updateSnapshot()
				e.flush()
				return nil
			}
			e.safeIngest(f)
		case <-flushT.C:
			e.flush()
		case <-snapT.C:
			e.updateSnapshot()
		case <-e.resetCh:
			e.doResetSession()
			e.updateSnapshot() // publish the zeroed view immediately
		case <-maintC:
			e.maintainStore()
		}
	}
}

// maintainStore runs periodic DB upkeep: drop data past the retention window,
// enforce the hard disk-size cap, checkpoint the WAL so it can't grow without
// bound, and VACUUM to return freed pages to the filesystem.
func (e *Engine) maintainStore() {
	if e.store == nil {
		return
	}
	purged := false
	if e.cfg.Retention > 0 {
		if d, err := e.store.Purge(e.nowFn().Add(-e.cfg.Retention)); err == nil {
			purged = d
		}
	}
	// EnforceSizeCap already checkpoints + vacuums between its own deletions.
	capped, _ := e.store.EnforceSizeCap(e.cfg.MaxDBBytes)
	e.store.Checkpoint()
	if purged && !capped {
		_ = e.store.Vacuum() // reclaim space from the time-based purge
	}
}

// ingest attributes a single flow and folds it into both accumulator sets.
// safeIngest wraps ingest so a panic on a single malformed flow (a parser edge
// case, an unexpected nil) drops that one packet instead of crashing the whole
// daemon — which would lose all in-memory session state and force a launchd
// restart. The hot path's cost is just a deferred recover.
func (e *Engine) safeIngest(f types.Flow) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("engine: recovered from panic ingesting flow: %v", r)
		}
	}()
	e.ingest(f)
}

func (e *Engine) ingest(f types.Flow) {
	proc, ok := types.Process{Name: "unknown"}, false
	if e.res != nil {
		proc, ok = e.res.Lookup(types.ConnKey{
			Proto:      f.Proto,
			LocalPort:  f.LocalPort,
			RemoteIP:   f.RemoteIP,
			RemotePort: f.RemotePort,
		})
	}
	if !ok {
		proc = types.Process{Name: "unknown"}
	}
	// Drop our own traffic (update checks, etc.) so the daemon doesn't show up.
	if e.cfg.SelfPID != 0 && proc.PID == e.cfg.SelfPID {
		return
	}

	host := f.RemoteIP
	if e.dns != nil {
		if h := e.dns.Lookup(f.RemoteIP); h != "" {
			host = h
		} else if e.cfg.Hinter != nil {
			// No forward-DNS answer seen for this IP; resolve it via PTR in the
			// background. Future flows/snapshots will pick up the hostname.
			e.cfg.Hinter.Enqueue(f.RemoteIP)
		}
	}
	cat := classify.Category(host)
	country := geoip.Lookup(net.ParseIP(f.RemoteIP))
	now := e.nowFn()

	e.mu.Lock()
	defer e.mu.Unlock()
	applyTo(e.winApps, e.winDomains, f, proc, host, cat, country, now)
	applyTo(e.sessApps, e.sessDomains, f, proc, host, cat, country, now)

	ck := connKey{proto: f.Proto, app: proc.Name, remoteIP: f.RemoteIP, remotePort: f.RemotePort}
	c := e.conns[ck]
	if c == nil {
		c = &connAcc{host: host, path: proc.Path, category: cat, country: country, firstSeen: now}
		e.conns[ck] = c
	}
	if c.country == "" && country != "" {
		c.country = country
	}
	if c.host == "" || c.host == ck.remoteIP {
		c.host = host // upgrade to a hostname once DNS resolves
	}
	c.lastSeen = now
	switch f.Direction {
	case types.DirIn:
		c.rx += f.Bytes
	case types.DirOut:
		c.tx += f.Bytes
	}

	switch f.Direction {
	case types.DirIn:
		e.totalRx += f.Bytes
	case types.DirOut:
		e.totalTx += f.Bytes
	}
}

// applyTo folds a flow into one app map and one domain map. Caller holds e.mu.
func applyTo(apps map[string]*appAcc, domains map[domKey]*domAcc, f types.Flow, proc types.Process, host, cat, country string, now time.Time) {
	a := apps[proc.Name]
	if a == nil {
		a = &appAcc{name: proc.Name, path: proc.Path, conns: make(map[uint16]struct{})}
		apps[proc.Name] = a
	}
	if a.path == "" {
		a.path = proc.Path
	}
	a.conns[f.LocalPort] = struct{}{}
	a.lastActive = now

	dk := domKey{domain: host, app: proc.Name}
	d := domains[dk]
	if d == nil {
		d = &domAcc{domain: host, app: proc.Name, category: cat, country: country}
		domains[dk] = d
	}
	if d.country == "" && country != "" {
		d.country = country // fill once we learn it
	}
	d.lastActive = now

	switch f.Direction {
	case types.DirIn:
		a.rx += f.Bytes
		d.rx += f.Bytes
	case types.DirOut:
		a.tx += f.Bytes
		d.tx += f.Bytes
	}
}

// flush writes the current window to storage and resets only the window
// accumulators. Session accumulators are left intact.
func (e *Engine) flush() {
	e.mu.Lock()
	apps := appsSlice(e.winApps)
	domains := domainsSlice(e.winDomains)
	e.winApps = make(map[string]*appAcc)
	e.winDomains = make(map[domKey]*domAcc)
	e.mu.Unlock()

	if e.store == nil {
		return
	}
	bucket := e.nowFn().Truncate(e.cfg.Bucket).Unix()
	_ = e.store.FlushApps(bucket, apps)
	_ = e.store.FlushDomains(bucket, domains)

	// Attribute this bucket's bytes to the capturing interface for metered/
	// tethering tracking. Capture is single-interface at a time, so the whole
	// bucket belongs to the current one. Stored at day granularity.
	var rx, tx uint64
	for _, a := range apps {
		rx += a.RxBytes
		tx += a.TxBytes
	}
	if iface := e.currentIface(); iface != "" {
		_ = e.store.AddIfaceUsage(iface, dayStart(e.nowFn()), rx, tx)
	}
}

// ResetSession zeroes the live session counters (apps/domains/connections and
// the session totals) and restarts the session clock — a "measure from now"
// reset. Stored history (today/week/month) and per-interface usage are left
// intact. Safe to call from any goroutine; the work runs in the Run loop.
func (e *Engine) ResetSession() {
	select {
	case e.resetCh <- struct{}{}:
	default: // a reset is already pending
	}
}

// doResetSession performs the reset. Runs in the Run goroutine, so it serializes
// with ingest/flush/snapshot; e.mu still guards the maps against API readers.
func (e *Engine) doResetSession() {
	now := e.nowFn()
	e.mu.Lock()
	e.sessApps = make(map[string]*appAcc)
	e.sessDomains = make(map[domKey]*domAcc)
	e.conns = make(map[connKey]*connAcc)
	e.totalRx, e.totalTx = 0, 0
	e.sessStart = now
	e.mu.Unlock()
	// Rate fields are only touched in this goroutine — reset without a lock,
	// matching updateSnapshot. Zeroing them avoids a negative rate on the next
	// tick (totalRx restarts from 0).
	e.rateRx, e.rateTx, e.rateAt = 0, 0, now
	log.Printf("engine: live session counters reset")
}

// dayStart returns unix seconds at the local midnight of t — the day key used
// for per-interface usage rows.
func dayStart(t time.Time) int64 {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location()).Unix()
}

func appsSlice(m map[string]*appAcc) []types.AppTraffic {
	out := make([]types.AppTraffic, 0, len(m))
	for _, a := range m {
		out = append(out, types.AppTraffic{
			Name:        a.name,
			Path:        a.path,
			RxBytes:     a.rx,
			TxBytes:     a.tx,
			Connections: len(a.conns),
		})
	}
	return out
}

func domainsSlice(m map[domKey]*domAcc) []types.DomainStat {
	out := make([]types.DomainStat, 0, len(m))
	for _, d := range m {
		out = append(out, types.DomainStat{
			Domain:   d.domain,
			AppName:  d.app,
			RxBytes:  d.rx,
			TxBytes:  d.tx,
			Category: d.category,
			Country:  d.country,
		})
	}
	return out
}

// updateSnapshot recomputes the cached live snapshot from the SESSION
// accumulators (stable, cumulative) plus instantaneous rates.
func (e *Engine) updateSnapshot() {
	now := e.nowFn()

	e.mu.Lock()
	e.pruneLocked(now)
	apps := appsSlice(e.sessApps)
	domains := domainsSlice(e.sessDomains)
	active := 0
	cutoff := now.Add(-e.cfg.ActiveWindow)
	for _, a := range e.sessApps {
		if a.lastActive.After(cutoff) {
			active++
		}
	}
	totalRx, totalTx := e.totalRx, e.totalTx
	sessStart := e.sessStart
	e.mu.Unlock()

	elapsed := now.Sub(e.rateAt).Seconds()
	var rxps, txps uint64
	if elapsed > 0 {
		rxps = uint64(float64(totalRx-e.rateRx) / elapsed)
		txps = uint64(float64(totalTx-e.rateTx) / elapsed)
	}
	e.rateRx, e.rateTx, e.rateAt = totalRx, totalTx, now

	sortApps(apps)
	sortDomains(domains)
	if len(apps) > e.cfg.LiveTopN {
		apps = apps[:e.cfg.LiveTopN]
	}
	if len(domains) > e.cfg.LiveTopN {
		domains = domains[:e.cfg.LiveTopN]
	}

	snap := types.Snapshot{
		Time:         now,
		SessionStart: sessStart,
		Apps:         apps,
		Domains:      domains,
		TotalRx:      totalRx,
		TotalTx:      totalTx,
		RxPerSec:     rxps,
		TxPerSec:     txps,
		ActiveApps:   active,
		Interface:    e.currentIface(),
		Paused:       e.isPaused(),
	}
	e.snapMu.Lock()
	e.snapshot = snap
	// Keep a rolling per-second history so a freshly-opened dashboard can show
	// the recent live chart immediately instead of starting from blank.
	e.rateHist = append(e.rateHist, types.RatePoint{Time: now, RxPerSec: rxps, TxPerSec: txps})
	if n := len(e.rateHist); n > rateHistLen {
		e.rateHist = append(e.rateHist[:0], e.rateHist[n-rateHistLen:]...)
	}
	e.snapMu.Unlock()
}

// rateHistLen bounds the per-second rate ring (~2 minutes at one sample/sec).
const rateHistLen = 120

// RateHistory returns a copy of the recent per-second rate samples.
func (e *Engine) RateHistory() []types.RatePoint {
	e.snapMu.RLock()
	defer e.snapMu.RUnlock()
	out := make([]types.RatePoint, len(e.rateHist))
	copy(out, e.rateHist)
	return out
}

// pruneLocked drops session entries idle beyond SessionHorizon to bound memory.
// Caller holds e.mu. No-op when the horizon is zero.
func (e *Engine) pruneLocked(now time.Time) {
	// Connections are short-lived by nature; drop ones idle beyond connTTL so the
	// live view reflects what's actually open, and the map can't grow unbounded.
	// This runs regardless of SessionHorizon.
	cTTL := now.Add(-connTTL)
	for k, c := range e.conns {
		if c.lastSeen.Before(cTTL) {
			delete(e.conns, k)
		}
	}
	if e.cfg.SessionHorizon <= 0 {
		return
	}
	cutoff := now.Add(-e.cfg.SessionHorizon)
	for k, a := range e.sessApps {
		if a.lastActive.Before(cutoff) {
			delete(e.sessApps, k)
		}
	}
	for k, d := range e.sessDomains {
		if d.lastActive.Before(cutoff) {
			delete(e.sessDomains, k)
		}
	}
}

// connTTL bounds how long an idle connection lingers in the live-connections map.
const connTTL = 90 * time.Second

// Connections returns connections seen within activeWithin, most-active first.
// activeWithin <= 0 returns all tracked (un-pruned) connections.
func (e *Engine) Connections(activeWithin time.Duration) []types.Connection {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.nowFn()
	out := make([]types.Connection, 0, len(e.conns))
	for k, c := range e.conns {
		if activeWithin > 0 && c.lastSeen.Before(now.Add(-activeWithin)) {
			continue
		}
		out = append(out, types.Connection{
			Proto:      k.proto,
			App:        k.app,
			Path:       c.path,
			Host:       c.host,
			RemoteIP:   k.remoteIP,
			RemotePort: k.remotePort,
			Country:    c.country,
			Category:   c.category,
			RxBytes:    c.rx,
			TxBytes:    c.tx,
			FirstSeen:  c.firstSeen,
			LastSeen:   c.lastSeen,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Total() > out[j].Total() })
	return out
}

// Snapshot returns the most recent live snapshot.
func (e *Engine) Snapshot() types.Snapshot {
	e.snapMu.RLock()
	defer e.snapMu.RUnlock()
	return e.snapshot
}

func sortApps(a []types.AppTraffic) {
	sort.Slice(a, func(i, j int) bool { return a[i].Total() > a[j].Total() })
}

func sortDomains(d []types.DomainStat) {
	sort.Slice(d, func(i, j int) bool { return d[i].Total() > d[j].Total() })
}
