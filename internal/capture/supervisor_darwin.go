//go:build darwin

package capture

import (
	"context"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/doldoldol21/netscope/internal/dnscache"
	"github.com/doldoldol21/netscope/pkg/types"
)

// ifaceWatchInterval is how often the supervisor re-checks the default route
// (in auto mode) and how long it backs off between failed capture re-opens.
const ifaceWatchInterval = 5 * time.Second

// stallTimeout is how long capture may emit zero flows before the supervisor
// assumes the pcap handle has gone dead (the classic case: a laptop sleeps and
// wakes on the *same* Wi-Fi, so the interface name never changes and the route
// watcher never fires, yet the old BPF handle silently returns no packets). The
// watchdog then cancels the source to force a clean re-open.
//
// Silence alone cannot tell a dead handle from a link that is merely idle, so
// the watchdog backs off when the evidence points at idleness: a session that
// never saw a single flow doubles the budget up to maxStallTimeout, while a
// session that was carrying traffic and then went silent — the actual dead-handle
// signature — keeps the tight base timeout. Without this, leaving the machine
// alone re-opens capture every 90s forever, and each re-open is a window where
// the readout truthfully reports zero because nothing is being captured.
const (
	stallTimeout    = 90 * time.Second
	maxStallTimeout = 12 * time.Minute
)

// routeChangeConfirmations is how many consecutive polls must agree before the
// supervisor abandons a working interface for a new default route. A VPN coming
// up can own the default route for a second and hand it straight back; acting on
// the first observation tears down healthy capture for nothing.
const routeChangeConfirmations = 2

// LiveSupervisor keeps live capture pinned to the active interface. A long-lived
// daemon outlives network changes — Wi-Fi↔Ethernet switches, VPNs coming up,
// cables unplugged — each of which can leave the original interface dead. The
// supervisor re-detects the default route and transparently re-opens capture on
// the new interface, feeding the same flow channel throughout, so data keeps
// flowing without restarting the daemon.
type LiveSupervisor struct {
	dns      *dnscache.Cache
	prefPath string // file persisting the user's interface choice ("" = none)

	mu       sync.Mutex
	pref     string             // user-requested interface; "" means auto-detect
	active   string             // interface currently being captured
	cancel   context.CancelFunc // cancels the running source to force a re-open
	onActive func(string)       // notified when the active interface (re)opens
	paused   bool               // when true the Run loop closes capture and waits
	resumeCh chan struct{}      // wakes a paused Run loop on resume
	stallFor time.Duration      // current stall budget; grows while capture looks idle
}

// SetOnInterface registers a callback invoked with the active interface name
// whenever capture (re)opens — lets the engine keep snapshots in sync.
func (ls *LiveSupervisor) SetOnInterface(fn func(string)) {
	ls.mu.Lock()
	ls.onActive = fn
	ls.mu.Unlock()
}

// NewLiveSupervisor returns a supervised live source. iface pins capture to a
// specific interface; empty auto-detects the default-route interface and tracks
// it across network changes. prefPath (optional) persists a runtime interface
// choice so it survives daemon restarts; a saved choice overrides an empty iface.
func NewLiveSupervisor(iface string, dns *dnscache.Cache, prefPath string) *LiveSupervisor {
	ls := &LiveSupervisor{dns: dns, prefPath: prefPath, pref: iface, resumeCh: make(chan struct{}, 1), stallFor: stallTimeout}
	if iface == "" {
		if saved := ls.loadPref(); saved != "" {
			ls.pref = saved
		}
	}
	ls.active, _ = ls.resolve()
	return ls
}

// resolve reports which interface capture should run on. A user preference wins
// outright; otherwise it follows the default route. When re-detection fails — the
// UDP probe behind defaultInterface briefly has no route to dial during a
// transition — it falls back to the interface already being captured as long as
// that one is still up, rather than blanking capture for a full watch interval
// while a perfectly good link carries traffic.
func (ls *LiveSupervisor) resolve() (string, error) {
	ls.mu.Lock()
	p, last := ls.pref, ls.active
	ls.mu.Unlock()
	return resolveIface(p, last, defaultInterface, ifaceUsable)
}

// resolveIface is the interface-choosing rule, split out from the supervisor's
// locking so it can be exercised without a live network. detect follows the
// default route; usable reports whether an interface is still up and addressed.
func resolveIface(pref, last string, detect func() (string, error), usable func(string) bool) (string, error) {
	if pref != "" {
		return pref, nil
	}
	name, err := detect()
	if err == nil && name != "" {
		return name, nil
	}
	if last != "" && usable(last) {
		return last, nil
	}
	return name, err
}

func (ls *LiveSupervisor) setActive(name string) {
	ls.mu.Lock()
	ls.active = name
	fn := ls.onActive
	ls.mu.Unlock()
	if fn != nil {
		fn(name)
	}
}

// Name returns the interface currently being captured (or last selected).
func (ls *LiveSupervisor) Name() string {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.active != "" {
		return ls.active
	}
	return "auto"
}

// Run captures until ctx is cancelled, re-opening on a different interface
// whenever the default route changes or the current capture source dies.
func (ls *LiveSupervisor) Run(ctx context.Context, out chan<- types.Flow) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Paused: capture is closed (no pcap handle, no CPU) until resumed.
		ls.mu.Lock()
		paused := ls.paused
		ls.mu.Unlock()
		if paused {
			log.Printf("capture: paused")
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ls.resumeCh:
				log.Printf("capture: resumed")
				continue
			}
		}

		iface, err := ls.resolve()
		if err != nil || iface == "" {
			log.Printf("capture: no usable interface yet; retrying in %s", ifaceWatchInterval)
			if !sleep(ctx, ifaceWatchInterval) {
				return ctx.Err()
			}
			continue
		}

		src, err := OpenLive(iface, ls.dns)
		if err != nil {
			log.Printf("capture: open %q failed: %v; retrying in %s", iface, err, ifaceWatchInterval)
			if !sleep(ctx, ifaceWatchInterval) {
				return ctx.Err()
			}
			continue
		}
		ls.setActive(iface)
		log.Printf("capture: live on %s", iface)

		runCtx, cancel := context.WithCancel(ctx)
		ls.mu.Lock()
		ls.cancel = cancel // SetPreferred cancels this to switch interfaces now
		auto := ls.pref == ""
		ls.mu.Unlock()
		// In auto mode, watch for the default route moving to another interface
		// and cancel this source so the loop re-opens on the new one.
		if auto {
			go ls.watch(runCtx, iface, cancel)
		}
		// Forward flows through a stall watchdog: if capture goes silent for
		// stallTimeout (a dead handle after sleep/wake on the same interface),
		// cancel so the loop re-opens. monOut tracks last-activity per flow.
		monOut, lastFlow, seen := monitored(runCtx, out)
		go ls.watchStall(runCtx, iface, cancel, lastFlow, seen)
		err = src.Run(runCtx, monOut)
		cancel()
		ls.mu.Lock()
		ls.cancel = nil
		ls.mu.Unlock()

		if ctx.Err() != nil {
			return ctx.Err()
		}
		// The source ended on its own (interface change or capture error);
		// back off briefly, then re-detect and re-open.
		if err != nil {
			log.Printf("capture: source on %s ended: %v; re-detecting interface", iface, err)
		} else {
			log.Printf("capture: re-detecting interface")
		}
		if !sleep(ctx, time.Second) {
			return ctx.Err()
		}
	}
}

// monitored returns a channel to hand to the capture source plus two pointers: the
// unix-nano timestamp of the most recent flow, and how many flows this session has
// produced. A forwarder goroutine (tied to ctx) copies flows to out and updates
// both, so the supervisor can tell whether capture is still producing — and
// whether it ever produced at all — without touching the hot decode path.
func monitored(ctx context.Context, out chan<- types.Flow) (chan<- types.Flow, *int64, *int64) {
	in := make(chan types.Flow, 64)
	lastFlow := new(int64)
	seen := new(int64)
	atomic.StoreInt64(lastFlow, time.Now().UnixNano())
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case f := <-in:
				atomic.StoreInt64(lastFlow, time.Now().UnixNano())
				atomic.AddInt64(seen, 1)
				select {
				case <-ctx.Done():
					return
				case out <- f:
				}
			}
		}
	}()
	return in, lastFlow, seen
}

// watchStall re-opens capture if no flow has arrived for the current stall
// budget. This recovers the sleep/wake-on-same-interface case where the route
// watcher never fires but the pcap handle is dead. Silence is ambiguous, so the
// watchdog reads the surrounding evidence: an interface that is gone or down
// re-opens immediately at the base budget, silence after real traffic keeps the
// base budget, and silence from a session that never saw a flow backs the budget
// off so an idle machine stops churning. See nextStallBudget.
func (ls *LiveSupervisor) watchStall(ctx context.Context, iface string, cancel context.CancelFunc, lastFlow, seen *int64) {
	budget := ls.stallBudget()
	t := time.NewTicker(budget / 3)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Don't count a paused source as stalled — it's silent on purpose.
			ls.mu.Lock()
			paused := ls.paused
			ls.mu.Unlock()
			if paused {
				continue
			}
			last := time.Unix(0, atomic.LoadInt64(lastFlow))
			if time.Since(last) < budget {
				continue
			}
			// The interface vanishing or going down is unambiguous: re-open now
			// and go back to the tight budget, since this was no false alarm.
			if !ifaceUsable(iface) {
				log.Printf("capture: %s is gone or down; re-opening", iface)
				ls.setStallBudget(stallTimeout)
				cancel()
				return
			}
			busy := atomic.LoadInt64(seen) > 0
			ls.setStallBudget(nextStallBudget(budget, busy))
			if busy {
				log.Printf("capture: no flows on %s for %s after earlier traffic; re-opening (suspected dead handle after sleep/wake)", iface, budget)
			} else {
				log.Printf("capture: %s has been silent for %s and looks idle rather than dead; re-opening and backing off to %s", iface, budget, ls.stallBudget())
			}
			cancel()
			return
		}
	}
}

// nextStallBudget returns the stall budget for the next capture session. A
// session that carried traffic and then fell silent is the dead-handle
// signature, so it stays at the tight base. A session that never saw one flow
// is far more likely an idle link — a laptop left alone — so the budget doubles
// up to maxStallTimeout, turning a re-open every 90s into one every 12 minutes.
func nextStallBudget(cur time.Duration, sawFlows bool) time.Duration {
	if sawFlows {
		return stallTimeout
	}
	next := cur * 2
	if next > maxStallTimeout {
		return maxStallTimeout
	}
	if next < stallTimeout {
		return stallTimeout
	}
	return next
}

func (ls *LiveSupervisor) stallBudget() time.Duration {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.stallFor < stallTimeout {
		return stallTimeout
	}
	return ls.stallFor
}

func (ls *LiveSupervisor) setStallBudget(d time.Duration) {
	ls.mu.Lock()
	ls.stallFor = d
	ls.mu.Unlock()
}

// watch cancels the running source when the default-route interface changes and
// stays changed. A single differing observation is not enough: a VPN interface
// can hold the default route for a moment and give it right back, and acting on
// that flap drops a healthy capture only to re-open on the same interface a
// second later. The candidate must be seen routeChangeConfirmations polls in a
// row, and any poll that returns to current resets the count.
func (ls *LiveSupervisor) watch(ctx context.Context, current string, cancel context.CancelFunc) {
	t := time.NewTicker(ifaceWatchInterval)
	defer t.Stop()
	var sw routeSwitch
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now, err := defaultInterface()
			if err != nil {
				now = ""
			}
			switch sw.observe(current, now) {
			case switchNow:
				log.Printf("capture: default interface changed %s -> %s", current, now)
				cancel()
				return
			case switchPending:
				log.Printf("capture: default route moved %s -> %s; confirming before switching", current, now)
			}
		}
	}
}

// routeSwitch debounces default-route observations. It is a plain state machine
// so the confirm-before-switch rule can be tested without a real network.
type routeSwitch struct {
	candidate string
	seen      int
}

type switchDecision int

const (
	switchStay switchDecision = iota
	switchPending
	switchNow
)

// observe folds one poll into the state machine. An empty or unchanged
// observation clears any pending candidate, so a route that flaps away and back
// leaves the running capture alone.
func (s *routeSwitch) observe(current, now string) switchDecision {
	if now == "" || now == current {
		s.candidate, s.seen = "", 0
		return switchStay
	}
	if now != s.candidate {
		s.candidate, s.seen = now, 0
	}
	s.seen++
	if s.seen < routeChangeConfirmations {
		return switchPending
	}
	return switchNow
}

// PreferredInterface returns the user's chosen interface ("" = auto-detect).
func (ls *LiveSupervisor) PreferredInterface() string {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return ls.pref
}

// SetPreferredInterface switches the capture interface at runtime (""=auto),
// persists the choice, and re-opens capture on it immediately.
func (ls *LiveSupervisor) SetPreferredInterface(name string) error {
	ls.mu.Lock()
	ls.pref = name
	c := ls.cancel
	ls.mu.Unlock()
	ls.savePref(name)
	log.Printf("capture: interface preference set to %q", name)
	if c != nil {
		c() // drop the current source; the Run loop re-opens on the new interface
	}
	return nil
}

// Paused reports whether live capture is currently suspended.
func (ls *LiveSupervisor) Paused() bool {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return ls.paused
}

// SetPaused suspends or resumes live capture. Pausing closes the pcap handle
// (no packets, no CPU) until resumed; resuming re-opens on the active interface.
func (ls *LiveSupervisor) SetPaused(p bool) {
	ls.mu.Lock()
	if ls.paused == p {
		ls.mu.Unlock()
		return
	}
	ls.paused = p
	c := ls.cancel
	ls.mu.Unlock()
	if p {
		if c != nil {
			c() // drop the live source; the Run loop then blocks at the top
		}
	} else {
		select {
		case ls.resumeCh <- struct{}{}: // wake the paused Run loop
		default:
		}
	}
}

// ListInterfaces returns the capturable interfaces, marking the active one.
func (ls *LiveSupervisor) ListInterfaces() []types.NetIface {
	out := Interfaces()
	active := ls.Name()
	for i := range out {
		out[i].Active = out[i].Name == active
	}
	return out
}

func (ls *LiveSupervisor) loadPref() string {
	if ls.prefPath == "" {
		return ""
	}
	b, err := os.ReadFile(ls.prefPath)
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(b))
	if s == "auto" {
		return ""
	}
	return s
}

func (ls *LiveSupervisor) savePref(name string) {
	if ls.prefPath == "" {
		return
	}
	v := name
	if v == "" {
		v = "auto"
	}
	_ = os.WriteFile(ls.prefPath, []byte(v+"\n"), 0o644)
}

// Interfaces lists non-loopback interfaces that have a global unicast address
// (the ones worth capturing on), for the settings UI.
func Interfaces() []types.NetIface {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	meta := interfaceMeta() // macOS friendly names ("Wi-Fi", "iPhone USB") by BSD name
	var out []types.NetIface
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifi.Addrs()
		ip := ""
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.IsGlobalUnicast() {
				ip = n.IP.String()
				break
			}
		}
		if ip == "" {
			continue // skip interfaces with no usable address
		}
		m := meta[ifi.Name]
		friendly := m.friendly
		if friendly == "" {
			friendly = ifi.Name
		}
		out = append(out, types.NetIface{
			Name:     ifi.Name,
			Display:  friendly + " (" + ip + ")",
			Friendly: friendly,
			Kind:     m.kind,
			Tether:   m.tether,
			Up:       ifi.Flags&net.FlagUp != 0,
		})
	}
	return out
}

// sleep waits for d or until ctx is cancelled. It returns false if ctx ended.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
