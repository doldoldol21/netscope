//go:build darwin

package capture

import (
	"net"
	"syscall"
	"unsafe"

	"golang.org/x/net/route"
	"golang.org/x/sys/unix"
)

// interfaceBytes returns the total bytes the kernel says have crossed the named
// interface (in + out), and whether that number could be read.
//
// This is the one signal that resolves the question silence cannot: whether an
// interface is quiet or a pcap handle has gone deaf. The kernel counts every
// packet the NIC handles regardless of who is listening, so a counter that
// climbs while capture reports nothing is proof the handle is dead — no
// inference from timers, no guessing from traffic patterns.
//
// It reads the routing socket's interface list (the same source as
// `netstat -ib`) rather than shelling out. FetchRIB does the raw sysctl;
// if_msghdr2 carries 64-bit counters, so this does not wrap on a busy link.
func interfaceBytes(name string) (uint64, bool) {
	ifi, err := net.InterfaceByName(name)
	if err != nil {
		return 0, false
	}
	b, err := route.FetchRIB(syscall.AF_UNSPEC, unix.NET_RT_IFLIST2, 0)
	if err != nil {
		return 0, false
	}
	// The list interleaves per-interface messages (RTM_IFINFO2, if_msghdr2) with
	// shorter per-address ones. Every route message starts with the same length
	// and type fields, so walk by Msglen and only decode the ones that are both
	// the right type and long enough to be an if_msghdr2 — skipping the rest
	// rather than stopping, which would hide every interface listed after the
	// first address entry.
	const hdrLen = 4 // msglen (uint16) + version + type, present on every message
	for len(b) >= hdrLen {
		h := (*unix.IfMsghdr2)(unsafe.Pointer(&b[0]))
		msgLen := int(h.Msglen)
		// A zero or overlong length would loop forever or read past the buffer.
		if msgLen < hdrLen || msgLen > len(b) {
			return 0, false
		}
		if h.Type == unix.RTM_IFINFO2 && msgLen >= unix.SizeofIfMsghdr2 && int(h.Index) == ifi.Index {
			return h.Data.Ibytes + h.Data.Obytes, true
		}
		b = b[msgLen:]
	}
	return 0, false
}
