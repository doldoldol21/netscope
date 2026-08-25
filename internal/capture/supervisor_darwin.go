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
// every stall doubles the budget up to maxStallTimeout. That makes the timer
// self-tuning: it climbs until it clears the machine's own quiet gaps and then
// stops firing, which is what stops a quiet or bursty host from re-opening
// capture every 90s forever — and each re-open is a window where nothing is
// captured at all, which the readout truthfully reports as zero.
//
// Deliberately, traffic does not pull the budget back down; that would restart
// the climb on every burst and reinstate the churn. The budget resets only on
// evidence that the situation itself changed: a wake, a link loss, a deaf
// handle, or a different interface.
//
// This timer is now the last resort rather than the main defence. A handle that
// dies while the link keeps carrying traffic — the common case on USB tethering,
// with no wake and no link change to notice — is caught by comparing capture
// against the kernel's own byte counters, which settles in one tick regardless
// of how far this budget has backed off. What is left here is the residual case
// where the handle is dead *and* the link is genuinely silent, where there is
// nothing to compare against and nothing being missed either.
const (
	stallTimeout    = 90 * time.Second
	maxStallTimeout = 5 * time.Minute
	stallTick       = 10 * time.Second
)

// deafBytes is how much traffic the kernel must report on an interface, while
// capture reports none, before we call the pcap handle deaf. A handful of
// packets could race a tick boundary; tens of kilobytes across a whole interval
// with not one flow decoded cannot.
const deafBytes = 32 << 10

// wakeSlack is how far the wall clock may run ahead of the monotonic clock
// between two watchdog ticks before we conclude the machine was suspended.
// macOS stops the monotonic clock while asleep, so a lid-close shows up as a
// large wall-clock jump against a small monotonic one. Detecting the wake
// directly is what makes the dead-handle case cheap to catch: capture re-opens
// on the event itself instead of waiting out a silence timer that cannot tell a
// dead handle from a quiet link.
const wakeSlack = 20 * time.Second

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
	onLive   func(bool)         // notified as capture sources open and close
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

// SetOnLive registers a callback invoked with true when a capture source opens
// and false when one ends. Between those, no packets are being captured at all,
// which is what lets the UI tell an idle link from a gap in capture — the
// interface name cannot, since it keeps naming the last one across a re-open.
func (ls *LiveSupervisor) SetOnLive(fn func(bool)) {
	ls.mu.Lock()
	ls.onLive = fn
	ls.mu.Unlock()
}

// setLive reports a capture source opening or closing.
func (ls *LiveSupervisor) setLive(live bool) {
	ls.mu.Lock()
	fn := ls.onLive
	ls.mu.Unlock()
	if fn != nil {
		fn(live)
	}
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
	return resolveIface(p, last, routedInterface, defaultInterface, ifaceUsable)
}

// resolveIface is the interface-choosing rule, split out from the supervisor's
// locking so it can be exercised without a live network. routed follows the real
// default route and fails when it cannot be probed; guess is the best-effort
// scan; usable reports whether an interface is still up.
func resolveIface(pref, last string, routed func() (string, error), guess func() (string, error), usable func(string) bool) (string, error) {
	if pref != "" {
		return pref, nil
	}
	if name, err := routed(); err == nil && name != "" {
		return name, nil
	}
	// The route could not be probed. The interface we are already capturing is a
	// far better answer than an index-order scan, which can hand back a stale or
	// virtual interface — so prefer it whenever it is still up.
	if last != "" && usable(last) {
		return last, nil
	}
	return guess()
}

func (ls *LiveSupervisor) setActive(name string) {
	ls.mu.Lock()
	if ls.active != name {
		// A different interface is a fresh situation; don't inherit the backoff
		// the previous one earned by being idle.
		ls.stallFor = stallTimeout
	}
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
			ls.setLive(false)
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
			// Nothing is being captured while we retry. Said explicitly rather
			// than relying on a previous iteration having set it: on the very
			// first pass the flag is still at the engine's optimistic default.
			ls.setLive(false)
			log.Printf("capture: no usable interface yet; retrying in %s", ifaceWatchInterval)
			if !sleep(ctx, ifaceWatchInterval) {
				return ctx.Err()
			}
			continue
		}

		src, err := OpenLive(iface, ls.dns)
		if err != nil {
			ls.setLive(false)
			log.Printf("capture: open %q failed: %v; retrying in %s", iface, err, ifaceWatchInterval)
			if !sleep(ctx, ifaceWatchInterval) {
				return ctx.Err()
			}
			continue
		}
		ls.setActive(iface)
		ls.setLive(true)
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
		ls.setLive(false)
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

// watchStall re-opens capture when the evidence says the current pcap handle is
// no longer delivering. Silence on its own is not that evidence — an idle link
// and a dead handle look identical — so the watchdog checks the unambiguous
// signals on every tick, independently of any timer:
//
//   - the interface went away or went down: re-open now, at the base budget;
//   - the machine was suspended and woke: re-open now, at the base budget. This
//     is the sleep/wake-on-the-same-interface case the watchdog exists for, and
//     catching the wake directly beats inferring it from silence;
//   - the kernel counted traffic on this interface while capture decoded none:
//     the link is demonstrably busy, so the handle is deaf. Re-open now, at the
//     base budget. This is the signal silence could never provide, and it is
//     what makes the case below genuinely rare;
//   - otherwise, prolonged silence on a live interface is treated as what it
//     most likely is — an idle link — so capture still re-opens (cheap
//     insurance) but the budget backs off toward maxStallTimeout, whether or not
//     this session carried traffic. The budget returns to base only when one of
//     the unambiguous signals fires or the active interface changes.
func (ls *LiveSupervisor) watchStall(ctx context.Context, iface string, cancel context.CancelFunc, lastFlow, seen *int64) {
	t := time.NewTicker(stallTick)
	defer t.Stop()
	prev := time.Now()
	// Whether the link was alive when this session opened. If it was not, its
	// being down is not news, and treating it as a fresh loss every tick would
	// spin at the tick rate — pcap can open a down interface.
	startedUsable := ifaceUsable(iface)
	// Baselines for the deafness check: what the kernel had counted on this
	// interface, and how many flows capture had produced, as of the last tick.
	prevBytes, haveBytes := interfaceBytes(iface)
	prevSeen := atomic.LoadInt64(seen)
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			wall, mono := clockGap(prev, now)
			woke := slept(wall, mono)
			prev = now

			// Don't count a paused source as stalled — it's silent on purpose,
			// and its silence must not push the budget around either.
			ls.mu.Lock()
			paused, pinned := ls.paused, ls.pref != ""
			ls.mu.Unlock()
			if paused {
				atomic.StoreInt64(lastFlow, now.UnixNano())
				continue
			}

			// Unambiguous: the link is gone. Checked every tick, never gated on
			// the stall budget — a backed-off budget must not delay a real loss.
			if !ifaceUsable(iface) {
				if startedUsable {
					log.Printf("capture: %s is gone or down; re-opening", iface)
					ls.setStallBudget(ctx, stallTimeout)
				} else {
					// Opened on an already-down interface and it is still down.
					// Re-open to look for a better one, but back off.
					log.Printf("capture: %s is still down; re-opening and backing off to %s",
						iface, nextStallBudget(ls.stallBudget()))
					ls.setStallBudget(ctx, nextStallBudget(ls.stallBudget()))
				}
				cancel()
				return
			}
			// Unambiguous: the machine slept, so the handle is probably dead
			// even though the interface name never changed.
			if woke {
				log.Printf("capture: the machine woke from sleep; re-opening %s", iface)
				ls.setStallBudget(ctx, stallTimeout)
				cancel()
				return
			}

			// Unambiguous, and the one that silence alone could never settle:
			// the kernel counted real traffic on this interface while capture
			// decoded not one flow. The link is not quiet — the handle is deaf.
			// Checked every tick and never gated on the stall budget, because a
			// budget that has backed off for genuine idleness must not slow down
			// the recovery of a handle that is demonstrably broken.
			curBytes, ok := interfaceBytes(iface)
			curSeen := atomic.LoadInt64(seen)
			if handleLooksDeaf(prevBytes, curBytes, haveBytes && ok, prevSeen, curSeen) {
				log.Printf("capture: %s moved %d bytes with no flows decoded; the handle is deaf, re-opening",
					iface, curBytes-prevBytes)
				ls.setStallBudget(ctx, stallTimeout)
				cancel()
				return
			}
			prevBytes, haveBytes = curBytes, ok
			prevSeen = curSeen

			budget := ls.stallBudget()
			if now.Sub(time.Unix(0, atomic.LoadInt64(lastFlow))) < budget {
				continue
			}
			// Ambiguous silence on a live interface. Re-open anyway — it is the
			// only remaining way to shake off a handle that died without a wake
			// or a link change — but assume idleness and back off, so a machine
			// whose traffic simply has long gaps is not torn down every 90s.
			// A pinned interface can be legitimately silent for hours, so say so
			// rather than implying something is wrong.
			next := nextStallBudget(budget)
			ls.setStallBudget(ctx, next)
			log.Printf("capture: %s silent for %s (%d flows this session, pinned=%t); re-opening and backing off to %s",
				iface, budget, atomic.LoadInt64(seen), pinned, next)
			cancel()
			return
		}
	}
}

// handleLooksDeaf reports whether the kernel counted real traffic on the
// interface across a tick while capture decoded not a single flow.
//
// Both halves matter. Without counters (readable false) there is nothing to
// compare against, so silence stays ambiguous and this must not fire. And the
// flow count must be exactly unchanged: one decoded flow is enough to prove the
// handle is still delivering, whatever the byte totals say.
func handleLooksDeaf(prevBytes, curBytes uint64, readable bool, prevSeen, curSeen int64) bool {
	if !readable || curSeen != prevSeen {
		return false
	}
	// Counters only climb; a smaller reading means the interface was replaced
	// underneath us, which is a re-open for other reasons, not evidence here.
	if curBytes < prevBytes {
		return false
	}
	return curBytes-prevBytes >= deafBytes
}

// clockGap returns how far apart two ticks were by the wall clock and by the
// monotonic clock. Both readings come from the same time.Time pair — Round(0)
// strips the monotonic reading, leaving wall time — so no clock plumbing is
// needed beyond what the ticker already hands us.
func clockGap(prev, now time.Time) (wall, mono time.Duration) {
	return now.Round(0).Sub(prev.Round(0)), now.Sub(prev)
}

// slept reports whether the machine was suspended between two watchdog ticks.
// Darwin's CLOCK_MONOTONIC does not advance while the system is asleep, but the
// wall clock does, so a suspend shows up as wall time running well ahead of
// monotonic time. A negative or shrinking wall gap means the clock was stepped
// (NTP, timezone), which is not a wake and must not trigger one.
func slept(wall, mono time.Duration) bool {
	return wall-mono >= wakeSlack
}

// nextStallBudget doubles the stall budget up to maxStallTimeout. It is only
// reached when silence was ambiguous — a live interface, no wake — where the
// likeliest explanation is an idle link, and where re-opening every 90s forever
// is pure cost: each re-open is a window with no capture at all, which the
// readout reports as zero. Growing past the machine's own quiet gaps is the
// point; see the maxStallTimeout comment for why traffic does not undo it.
func nextStallBudget(cur time.Duration) time.Duration {
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

// setStallBudget records the budget for the next capture session. It drops the
// write if the session it came from is already cancelled: a tick and the
// cancellation can become ready together, and a late write from the outgoing
// watchdog would otherwise clobber the base budget the incoming session just set
// for a different interface.
func (ls *LiveSupervisor) setStallBudget(ctx context.Context, d time.Duration) {
	if ctx.Err() != nil {
		return
	}
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
			// routedInterface, not defaultInterface: abandoning a working
			// interface must follow the real route, never the index-order scan.
			now, err := routedInterface()
			if err != nil {
				now = ""
			}
			prev := sw.candidate
			switch sw.observe(current, now) {
			case switchNow:
				log.Printf("capture: default interface changed %s -> %s", current, now)
				cancel()
				return
			case switchPending:
				// Once per new candidate. A route alternating between two other
				// interfaces would otherwise log on every poll, forever.
				if now != prev {
					log.Printf("capture: default route moved %s -> %s; confirming before switching", current, now)
				}
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
