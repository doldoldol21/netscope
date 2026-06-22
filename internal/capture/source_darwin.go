//go:build darwin

package capture

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/doldoldol21/netscope/internal/dnscache"
	"github.com/doldoldol21/netscope/pkg/types"
	"github.com/google/gopacket"
	"github.com/google/gopacket/pcap"
)

// snapLen is how many bytes of each packet pcap copies to userspace. It only
// needs to cover L2-L4 headers plus typical DNS responses; total byte counts
// come from the original wire length in packet metadata, not the copy.
const snapLen = 2048

// Source wraps a pcap handle and decoder, yielding attributed flows.
type Source struct {
	handle *pcap.Handle
	src    *gopacket.PacketSource
	dec    *Decoder
	name   string
}

// OpenLive starts live capture on iface (empty selects a sensible default) and
// builds a decoder seeded with all local interface addresses.
func OpenLive(iface string, dns *dnscache.Cache) (*Source, error) {
	if iface == "" {
		var err error
		iface, err = defaultInterface()
		if err != nil {
			return nil, err
		}
	}
	handle, err := pcap.OpenLive(iface, snapLen, false, 100*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("pcap open %q: %w (need root / bpf access?)", iface, err)
	}
	// Only IP traffic is attributable; drop the rest in the kernel.
	if err := handle.SetBPFFilter("ip or ip6"); err != nil {
		handle.Close()
		return nil, fmt.Errorf("set bpf filter: %w", err)
	}
	src := gopacket.NewPacketSource(handle, handle.LinkType())
	src.Lazy = true
	src.NoCopy = true
	return &Source{
		handle: handle,
		src:    src,
		dec:    NewDecoder(LocalIPs(), dns),
		name:   iface,
	}, nil
}

// OpenOffline replays a pcap file. localIPs may be supplied to drive direction;
// when empty the decoder falls back to its private-range heuristic.
func OpenOffline(path string, localIPs []string, dns *dnscache.Cache) (*Source, error) {
	handle, err := pcap.OpenOffline(path)
	if err != nil {
		return nil, fmt.Errorf("pcap open offline %q: %w", path, err)
	}
	src := gopacket.NewPacketSource(handle, handle.LinkType())
	src.Lazy = true
	src.NoCopy = true
	return &Source{
		handle: handle,
		src:    src,
		dec:    NewDecoder(localIPs, dns),
		name:   path,
	}, nil
}

// Name returns the interface name or file path being read.
func (s *Source) Name() string { return s.name }

// Run reads packets until the context is cancelled or the source is exhausted,
// emitting attributed flows on out. It closes the pcap handle on return.
func (s *Source) Run(ctx context.Context, out chan<- types.Flow) error {
	defer s.handle.Close()
	packets := s.src.Packets()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case pkt, ok := <-packets:
			if !ok {
				return nil // offline source exhausted
			}
			flow, ok := s.dec.Decode(pkt)
			if !ok {
				continue
			}
			if flow.Timestamp.IsZero() {
				flow.Timestamp = time.Now()
			}
			select {
			case out <- flow:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

// LocalIPs returns every unicast address configured on the host's interfaces.
func LocalIPs() []string {
	var ips []string
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ips
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok {
			ips = append(ips, ipnet.IP.String())
		}
	}
	return ips
}

// defaultInterface picks the interface backing the host's default route.
//
// It asks the kernel which source address it would use for an outbound
// connection (a UDP "dial" selects a route without sending any packet) and
// maps that address back to its interface. This follows the real default
// route, unlike scanning net.Interfaces() in index order — which can pick an
// inactive interface (e.g. a stale en7) over the active one (en0).
func defaultInterface() (string, error) {
	if conn, err := net.Dial("udp", "8.8.8.8:53"); err == nil {
		local := conn.LocalAddr().(*net.UDPAddr).IP
		conn.Close()
		if name := interfaceForIP(local); name != "" {
			return name, nil
		}
	}
	// Fallback: first up, non-loopback interface with a global unicast address.
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.IsGlobalUnicast() {
				return ifi.Name, nil
			}
		}
	}
	return "", fmt.Errorf("no suitable network interface found")
}

// interfaceForIP returns the name of the interface that owns ip, or "".
func interfaceForIP(ip net.IP) string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, ifi := range ifaces {
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.Equal(ip) {
				return ifi.Name
			}
		}
	}
	return ""
}
