//go:build darwin

package capture

import (
	"context"
	"log"
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
	pref string // user-requested interface; "" means auto-detect
	dns  *dnscache.Cache

	mu     sync.Mutex
	active string
}

// NewLiveSupervisor returns a supervised live source. iface pins capture to a
// specific interface; empty auto-detects the default-route interface and tracks
// it across network changes.
func NewLiveSupervisor(iface string, dns *dnscache.Cache) *LiveSupervisor {
	ls := &LiveSupervisor{pref: iface, dns: dns}
	ls.active, _ = ls.resolve()
	return ls
}

func (ls *LiveSupervisor) resolve() (string, error) {
	if ls.pref != "" {
		return ls.pref, nil
	}
	return defaultInterface()
}

func (ls *LiveSupervisor) setActive(name string) {
	ls.mu.Lock()
	ls.active = name
	ls.mu.Unlock()
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
		// In auto mode, watch for the default route moving to another interface
		// and cancel this source so the loop re-opens on the new one.
		if ls.pref == "" {
			go ls.watch(runCtx, iface, cancel)
		}
		err = src.Run(runCtx, out)
		cancel()

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
