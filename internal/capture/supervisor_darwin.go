//go:build darwin

package capture

import (
	"context"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/doldoldol21/netscope/internal/dnscache"
	"github.com/doldoldol21/netscope/pkg/types"
)

// ifaceWatchInterval is how often the supervisor re-checks the default route
// (in auto mode) and how long it backs off between failed capture re-opens.
const ifaceWatchInterval = 5 * time.Second

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
	ls := &LiveSupervisor{dns: dns, prefPath: prefPath, pref: iface, resumeCh: make(chan struct{}, 1)}
	if iface == "" {
		if saved := ls.loadPref(); saved != "" {
			ls.pref = saved
		}
	}
	ls.active, _ = ls.resolve()
	return ls
}

func (ls *LiveSupervisor) resolve() (string, error) {
	ls.mu.Lock()
	p := ls.pref
	ls.mu.Unlock()
	if p != "" {
		return p, nil
	}
	return defaultInterface()
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
		err = src.Run(runCtx, out)
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

// watch cancels the running source when the default-route interface changes.
func (ls *LiveSupervisor) watch(ctx context.Context, current string, cancel context.CancelFunc) {
	t := time.NewTicker(ifaceWatchInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if now, err := defaultInterface(); err == nil && now != "" && now != current {
				log.Printf("capture: default interface changed %s -> %s", current, now)
				cancel()
				return
			}
		}
	}
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
		out = append(out, types.NetIface{
			Name:    ifi.Name,
			Display: ifi.Name + " (" + ip + ")",
			Up:      ifi.Flags&net.FlagUp != 0,
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
