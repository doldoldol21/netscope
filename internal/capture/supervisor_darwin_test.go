//go:build darwin

package capture

import (
	"errors"
	"net"
	"testing"
	"time"
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

func TestNextStallBudgetBacksOffWhileIdle(t *testing.T) {
	got := nextStallBudget(stallTimeout, false)
	if want := 2 * stallTimeout; got != want {
		t.Fatalf("idle session budget = %s, want %s", got, want)
	}
	// Repeated idleness converges on the cap and stops there.
	d := stallTimeout
	for i := 0; i < 20; i++ {
		d = nextStallBudget(d, false)
	}
	if d != maxStallTimeout {
		t.Fatalf("budget after prolonged idleness = %s, want %s", d, maxStallTimeout)
	}
}

// Silence *after* real traffic is the dead-handle signature, so recovery must
// stay fast no matter how far the budget had backed off.
func TestNextStallBudgetResetsAfterTraffic(t *testing.T) {
	if got := nextStallBudget(maxStallTimeout, true); got != stallTimeout {
		t.Fatalf("budget after a busy session = %s, want %s", got, stallTimeout)
	}
}

func TestStallBudgetNeverDropsBelowBase(t *testing.T) {
	ls := &LiveSupervisor{}
	if got := ls.stallBudget(); got != stallTimeout {
		t.Fatalf("zero-value budget = %s, want %s", got, stallTimeout)
	}
	ls.setStallBudget(time.Second)
	if got := ls.stallBudget(); got != stallTimeout {
		t.Fatalf("under-base budget = %s, want clamp to %s", got, stallTimeout)
	}
}

func TestResolveIfacePrefersTheUserChoice(t *testing.T) {
	detect := func() (string, error) { return "en0", nil }
	got, err := resolveIface("en7", "en0", detect, func(string) bool { return true })
	if err != nil || got != "en7" {
		t.Fatalf("resolveIface = %q, %v; want en7", got, err)
	}
}

// The transition case that used to blank capture for a full watch interval: the
// route probe fails, but the interface we are already on is still up.
func TestResolveIfaceFallsBackToTheLiveInterface(t *testing.T) {
	detect := func() (string, error) { return "", errors.New("no route to host") }
	got, err := resolveIface("", "en7", detect, func(name string) bool { return name == "en7" })
	if err != nil {
		t.Fatalf("resolveIface returned %v, want the en7 fallback", err)
	}
	if got != "en7" {
		t.Fatalf("resolveIface = %q, want en7", got)
	}
}

// When the last interface really is gone, the detection error must surface so
// Run backs off instead of retrying a dead name.
func TestResolveIfaceReportsFailureWhenNothingIsUsable(t *testing.T) {
	detect := func() (string, error) { return "", errors.New("no suitable network interface found") }
	if _, err := resolveIface("", "en7", detect, func(string) bool { return false }); err == nil {
		t.Fatal("resolveIface succeeded, want the detection error")
	}
}

func TestIfaceUsableRejectsUnknownAndLoopback(t *testing.T) {
	if ifaceUsable("definitely-not-an-interface0") {
		t.Fatal("a nonexistent interface reported usable")
	}
	// Loopback is up and addressed but carries no global unicast address, so it
	// must not qualify as somewhere to fall back to.
	if ifaceUsable("lo0") {
		t.Fatal("loopback reported usable")
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
