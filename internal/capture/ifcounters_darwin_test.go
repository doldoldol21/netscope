//go:build darwin

package capture

import (
	"net"
	"testing"
	"time"
)

// The deafness check is only as good as this reader: if it silently returns
// zero, a deaf handle looks exactly like an idle link again.
func TestInterfaceBytesReadsALiveInterface(t *testing.T) {
	name := someLiveInterface(t)
	got, ok := interfaceBytes(name)
	if !ok {
		t.Fatalf("could not read counters for %s", name)
	}
	if got == 0 {
		t.Fatalf("%s reported zero total bytes; the parse is probably wrong", name)
	}
}

// Counters must be monotonic and must actually move on a busy machine — a
// frozen reading would make the check permanently blind.
func TestInterfaceBytesIsMonotonic(t *testing.T) {
	name := someLiveInterface(t)
	first, ok := interfaceBytes(name)
	if !ok {
		t.Skipf("no counters for %s", name)
	}
	time.Sleep(200 * time.Millisecond)
	second, ok := interfaceBytes(name)
	if !ok {
		t.Fatalf("counters for %s became unreadable", name)
	}
	if second < first {
		t.Fatalf("counters went backwards: %d then %d", first, second)
	}
}

func TestInterfaceBytesRejectsAnUnknownInterface(t *testing.T) {
	if _, ok := interfaceBytes("definitely-not-an-interface0"); ok {
		t.Fatal("reported counters for an interface that does not exist")
	}
}

// Loopback always exists and always has traffic, so it is a stable target when
// the machine has no external link up.
func TestInterfaceBytesReadsLoopback(t *testing.T) {
	if _, ok := interfaceBytes("lo0"); !ok {
		t.Fatal("could not read loopback counters")
	}
}

func someLiveInterface(t *testing.T) string {
	t.Helper()
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
				return ifi.Name
			}
		}
	}
	t.Skip("no live external interface on this host")
	return ""
}

// Every interface must be reachable, not just the ones listed before the first
// address message. Walking by message length and skipping short entries is what
// makes that true; a parser that stops at the first short message silently hides
// the tail of the list, which would blind the deafness check on exactly the
// interfaces (tunnels, late-enumerated USB tethers) most likely to need it.
func TestInterfaceBytesReachesEveryInterface(t *testing.T) {
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Skipf("cannot enumerate interfaces: %v", err)
	}
	var missed []string
	for _, ifi := range ifaces {
		if _, ok := interfaceBytes(ifi.Name); !ok {
			missed = append(missed, ifi.Name)
		}
	}
	if len(missed) > 0 {
		t.Fatalf("no counters for %v — the message walk is stopping early", missed)
	}
}
