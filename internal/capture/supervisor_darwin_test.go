//go:build darwin

package capture

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/doldoldol21/netscope/pkg/types"
)

// A VPN that owns the default route for a single poll and hands it straight back
// must not cost us a capture teardown — that flap was the bulk of the spurious
// re-opens seen in the wild.
func TestRouteSwitchIgnoresAFlap(t *testing.T) {
	var sw routeSwitch
	if got := sw.observe("en7", "utun19"); got != switchPending {
		t.Fatalf("first sighting = %v, want switchPending", got)
	}
	if got := sw.observe("en7", "en7"); got != switchStay {
		t.Fatalf("route came back, got %v, want switchStay", got)
	}
	// The reverted flap must not count toward the next candidate.
	if got := sw.observe("en7", "utun19"); got != switchPending {
		t.Fatalf("after reverting, first sighting = %v, want switchPending", got)
	}
}

func TestRouteSwitchConfirmsASustainedChange(t *testing.T) {
	var sw routeSwitch
	sw.observe("en7", "en0")
	if got := sw.observe("en7", "en0"); got != switchNow {
		t.Fatalf("second consecutive sighting = %v, want switchNow", got)
	}
}

// Two different candidates in a row are two flaps, not a confirmation.
func TestRouteSwitchRequiresTheSameCandidateTwice(t *testing.T) {
	var sw routeSwitch
	sw.observe("en7", "en0")
	if got := sw.observe("en7", "utun19"); got != switchPending {
		t.Fatalf("different candidate = %v, want switchPending", got)
	}
}

func TestRouteSwitchTreatsDetectionFailureAsNoChange(t *testing.T) {
	var sw routeSwitch
	sw.observe("en7", "en0")
	if got := sw.observe("en7", ""); got != switchStay {
		t.Fatalf("failed detection = %v, want switchStay", got)
	}
	if got := sw.observe("en7", "en0"); got != switchPending {
		t.Fatalf("candidate should have been cleared, got %v", got)
	}
}

func TestNextStallBudgetBacksOff(t *testing.T) {
	got := nextStallBudget(stallTimeout)
	if want := 2 * stallTimeout; got != want {
		t.Fatalf("backed-off budget = %s, want %s", got, want)
	}
	// Repeated ambiguous silence converges on the cap and stops there.
	d := stallTimeout
	for i := 0; i < 20; i++ {
		d = nextStallBudget(d)
	}
	if d != maxStallTimeout {
		t.Fatalf("budget after prolonged silence = %s, want %s", d, maxStallTimeout)
	}
}

// maxStallTimeout doubles as the worst-case recovery delay for a handle that
// dies with no wake and no link change, so it must stay short enough to be a
// nuisance rather than an outage.
func TestMaxStallTimeoutStaysBounded(t *testing.T) {
	if maxStallTimeout > 5*time.Minute {
		t.Fatalf("maxStallTimeout = %s; worst-case recovery is too long", maxStallTimeout)
	}
	if maxStallTimeout <= stallTimeout {
		t.Fatalf("maxStallTimeout = %s must exceed the base %s", maxStallTimeout, stallTimeout)
	}
}

// A different interface is a fresh situation and must not inherit the backoff
// the previous one earned by sitting idle.
func TestSetActiveResetsTheBackoff(t *testing.T) {
	ls := &LiveSupervisor{stallFor: maxStallTimeout, active: "en7"}
	ls.setActive("en7")
	if got := ls.stallBudget(); got != maxStallTimeout {
		t.Fatalf("re-opening the same interface reset the budget to %s", got)
	}
	ls.setActive("en0")
	if got := ls.stallBudget(); got != stallTimeout {
		t.Fatalf("budget after an interface change = %s, want %s", got, stallTimeout)
	}
}

// Darwin's monotonic clock stops while the system is suspended, so a wake shows
// up as wall time running ahead of monotonic time between two ticks.
func TestSleptDetectsAWake(t *testing.T) {
	if slept(stallTick, stallTick) {
		t.Fatal("an ordinary tick was reported as a wake")
	}
	// Slept an hour: the wall clock advanced, the monotonic clock barely did.
	if !slept(time.Hour, stallTick) {
		t.Fatal("an hour of suspension was not reported as a wake")
	}
	// Just under the slack — jitter, not a suspend.
	if slept(stallTick+wakeSlack-time.Second, stallTick) {
		t.Fatal("sub-slack drift was reported as a wake")
	}
}

// A clock stepped backwards (NTP correction, timezone change) is not a wake.
func TestSleptIgnoresABackwardClockStep(t *testing.T) {
	if slept(-time.Hour, stallTick) {
		t.Fatal("a backward clock step was reported as a wake")
	}
}

// clockGap must read wall and monotonic from the same pair without drifting.
func TestClockGapReadsBothClocks(t *testing.T) {
	prev := time.Now()
	wall, mono := clockGap(prev, prev.Add(stallTick))
	if wall != stallTick || mono != stallTick {
		t.Fatalf("clockGap = (%s, %s), want (%s, %s)", wall, mono, stallTick, stallTick)
	}
}

func TestStallBudgetNeverDropsBelowBase(t *testing.T) {
	ls := &LiveSupervisor{}
	if got := ls.stallBudget(); got != stallTimeout {
		t.Fatalf("zero-value budget = %s, want %s", got, stallTimeout)
	}
	ls.setStallBudget(context.Background(), time.Second)
	if got := ls.stallBudget(); got != stallTimeout {
		t.Fatalf("under-base budget = %s, want clamp to %s", got, stallTimeout)
	}
}

// A tick and the session's cancellation can become ready together. The outgoing
// watchdog's late write must not clobber the base budget the next session set
// for a different interface.
func TestSetStallBudgetIgnoresACancelledSession(t *testing.T) {
	ls := &LiveSupervisor{stallFor: stallTimeout}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ls.setStallBudget(ctx, maxStallTimeout)
	if got := ls.stallBudget(); got != stallTimeout {
		t.Fatalf("a cancelled session wrote the budget: %s", got)
	}
}

func TestResolveIfacePrefersTheUserChoice(t *testing.T) {
	routed := func() (string, error) { return "en0", nil }
	guess := func() (string, error) { return "en0", nil }
	got, err := resolveIface("en7", "en0", routed, guess, func(string) bool { return true })
	if err != nil || got != "en7" {
		t.Fatalf("resolveIface = %q, %v; want en7", got, err)
	}
}

// The transition case that used to blank capture for a full watch interval: the
// route probe fails, but the interface we are already on is still up. The
// index-order scan must not win here — it can return a stale or virtual
// interface, which is how capture ended up on the wrong one.
func TestResolveIfaceFallsBackToTheLiveInterfaceNotTheScan(t *testing.T) {
	routed := func() (string, error) { return "", errors.New("no route to host") }
	guess := func() (string, error) { return "bridge100", nil }
	got, err := resolveIface("", "en7", routed, guess, func(name string) bool { return name == "en7" })
	if err != nil {
		t.Fatalf("resolveIface returned %v, want the en7 fallback", err)
	}
	if got != "en7" {
		t.Fatalf("resolveIface = %q, want en7", got)
	}
}

// When the last interface really is gone, fall through to the scan rather than
// capturing nothing.
func TestResolveIfaceFallsThroughToTheScan(t *testing.T) {
	routed := func() (string, error) { return "", errors.New("no route to host") }
	guess := func() (string, error) { return "en0", nil }
	got, err := resolveIface("", "en7", routed, guess, func(string) bool { return false })
	if err != nil || got != "en0" {
		t.Fatalf("resolveIface = %q, %v; want en0", got, err)
	}
}

func TestResolveIfaceReportsFailureWhenNothingIsUsable(t *testing.T) {
	routed := func() (string, error) { return "", errors.New("no route to host") }
	guess := func() (string, error) { return "", errors.New("no suitable network interface found") }
	if _, err := resolveIface("", "en7", routed, guess, func(string) bool { return false }); err == nil {
		t.Fatal("resolveIface succeeded, want the detection error")
	}
}

func TestIfaceUsableRejectsAnUnknownInterface(t *testing.T) {
	if ifaceUsable("definitely-not-an-interface0") {
		t.Fatal("a nonexistent interface reported usable")
	}
}

// lo0 has no global unicast address but pcap captures on it fine, and -iface lo0
// is a legitimate pin. Judging it "gone" would cancel capture in a tight loop.
func TestIfaceUsableAcceptsLoopback(t *testing.T) {
	if !ifaceUsable("lo0") {
		t.Fatal("loopback reported unusable")
	}
}

func TestIfaceUsableAcceptsALiveInterface(t *testing.T) {
	live := ""
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Skipf("cannot enumerate interfaces: %v", err)
	}
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.IsGlobalUnicast() {
				live = ifi.Name
			}
		}
	}
	if live == "" {
		t.Skip("no live interface on this host")
	}
	if !ifaceUsable(live) {
		t.Fatalf("live interface %s reported unusable", live)
	}
}

// Until a source actually opens, the supervisor must say so. The engine's flag
// starts optimistically true for sources that have no supervisor, so a
// supervisor that fails to open on its first pass has to correct it — otherwise
// a daemon that can't find an interface looks like it is capturing.
func TestSupervisorReportsNotLiveWhileItCannotOpen(t *testing.T) {
	ls := NewLiveSupervisor("definitely-not-an-interface0", nil, "")

	var mu sync.Mutex
	var seen []bool
	ls.SetOnLive(func(live bool) {
		mu.Lock()
		seen = append(seen, live)
		mu.Unlock()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	out := make(chan types.Flow, 1)
	_ = ls.Run(ctx, out)

	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("supervisor never reported its live state while failing to open")
	}
	for i, live := range seen {
		if live {
			t.Fatalf("report %d claimed capture was live on an interface that cannot open", i)
		}
	}
}

// The case that motivated all of this: an iPhone USB tether that keeps carrying
// traffic while the pcap handle silently stops delivering. The kernel's counters
// climb, the handle hands over nothing, and that combination is proof — not
// inference.
func TestHandleLooksDeafWhenTheKernelSeesTrafficAndTheHandleDoesNot(t *testing.T) {
	if !handleLooksDeaf(1_000_000, 1_000_000+deafBytes, true, 42, 42) {
		t.Fatal("a busy interface delivering no packets was not called deaf")
	}
}

// One delivered packet proves the handle still works, however much the byte
// totals moved — capture is behind, not deaf.
func TestHandleLooksDeafIgnoresAHandleStillDelivering(t *testing.T) {
	if handleLooksDeaf(0, 10<<20, true, 42, 43) {
		t.Fatal("a handle that delivered a packet was called deaf")
	}
}

// The counts must measure comparable populations. The kernel counts every
// packet crossing the NIC, including ICMP, ESP and GRE, none of which the
// decoder turns into flows — so a VPN uplink carrying only ESP delivers plenty
// of packets while producing zero flows. Comparing bytes against *packets* is
// what keeps that healthy handle from being torn down every tick.
func TestHandleLooksDeafToleratesTrafficThatDecodesToNoFlows(t *testing.T) {
	// 8 MB of ESP across the tick, no flows decoded, but packets kept arriving.
	if handleLooksDeaf(0, 8<<20, true, 1000, 6000) {
		t.Fatal("a link carrying only non-TCP/UDP traffic was called deaf")
	}
}

// A genuinely idle link moves no bytes either, which is the whole point: this
// check must stay silent so the idle backoff can do its job.
func TestHandleLooksDeafStaysQuietOnAnIdleLink(t *testing.T) {
	if handleLooksDeaf(1_000_000, 1_000_000, true, 7, 7) {
		t.Fatal("an idle link was called deaf")
	}
	// A trickle below the threshold could be a packet racing the tick boundary.
	if handleLooksDeaf(1_000_000, 1_000_000+1024, true, 7, 7) {
		t.Fatal("a trickle below the threshold was called deaf")
	}
}

// Interfaces whose counters can't be read (or a failed sysctl) leave silence
// ambiguous. Guessing "deaf" there would re-open capture forever.
func TestHandleLooksDeafRequiresReadableCounters(t *testing.T) {
	if handleLooksDeaf(0, 100<<20, false, 7, 7) {
		t.Fatal("called deaf without any counters to compare against")
	}
}

// A counter that goes backwards means the interface was replaced underneath us.
func TestHandleLooksDeafIgnoresACounterReset(t *testing.T) {
	if handleLooksDeaf(10<<20, 4096, true, 7, 7) {
		t.Fatal("a counter reset was read as traffic")
	}
}
