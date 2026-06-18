// Package capture turns raw packets into attributed flows. The decoding logic
// here is pure (no libpcap dependency) so it can be unit tested with crafted or
// replayed packets; the live/offline pcap plumbing lives in source.go.
package capture

import (
	"net"

	"github.com/doldoldol21/netscope/internal/dnscache"
	"github.com/doldoldol21/netscope/pkg/types"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
)

// Decoder converts gopacket packets into types.Flow, attributing direction
// against the set of local interface addresses and feeding observed DNS
// answers into the supplied cache.
type Decoder struct {
	localIPs map[string]bool
	dns      *dnscache.Cache
}

// NewDecoder builds a Decoder. localIPs is the set of addresses belonging to
// this host (used to decide upload vs download); dns may be nil to skip DNS
// learning.
func NewDecoder(localIPs []string, dns *dnscache.Cache) *Decoder {
	set := make(map[string]bool, len(localIPs))
	for _, ip := range localIPs {
		set[ip] = true
	}
	return &Decoder{localIPs: set, dns: dns}
}

// Decode reduces a packet to a Flow. The bool is false when the packet is not
// an attributable IP/TCP/UDP packet (e.g. ARP, loopback chatter) or when it is
// purely intra-host traffic we choose to ignore.
func (d *Decoder) Decode(pkt gopacket.Packet) (types.Flow, bool) {
	net := pkt.NetworkLayer()
	if net == nil {
		return types.Flow{}, false
	}

	var srcIP, dstIP string
	switch n := net.(type) {
	case *layers.IPv4:
		srcIP, dstIP = n.SrcIP.String(), n.DstIP.String()
	case *layers.IPv6:
		srcIP, dstIP = n.SrcIP.String(), n.DstIP.String()
	default:
		return types.Flow{}, false
	}

	var proto types.Protocol
	var sport, dport uint16
	switch t := pkt.TransportLayer().(type) {
	case *layers.TCP:
		proto, sport, dport = types.ProtoTCP, uint16(t.SrcPort), uint16(t.DstPort)
	case *layers.UDP:
		proto, sport, dport = types.ProtoUDP, uint16(t.SrcPort), uint16(t.DstPort)
	default:
		return types.Flow{}, false
	}

	// Learn IP->host mappings from DNS responses before deciding direction so
	// that even ignored intra-host DNS traffic still populates the cache.
	if d.dns != nil {
		if l := pkt.Layer(layers.LayerTypeDNS); l != nil {
			d.recordDNS(l.(*layers.DNS))
		}
	}

	dir, localPort, remoteIP, remotePort, ok := d.direction(srcIP, dstIP, sport, dport)
	if !ok {
		return types.Flow{}, false
	}

	flow := types.Flow{
		Timestamp:  pkt.Metadata().Timestamp,
		Proto:      proto,
		Direction:  dir,
		LocalPort:  localPort,
		RemoteIP:   remoteIP,
		RemotePort: remotePort,
		Bytes:      wireBytes(pkt),
	}
	if flow.Timestamp.IsZero() {
		// Offline sources without per-packet timestamps; caller stamps later.
	}
	return flow, true
}

// direction figures out which endpoint is local and reports the flow direction
// and the normalised local/remote ports. ok is false for intra-host traffic.
func (d *Decoder) direction(srcIP, dstIP string, sport, dport uint16) (types.Direction, uint16, string, uint16, bool) {
	srcLocal := d.isLocal(srcIP)
	dstLocal := d.isLocal(dstIP)

	switch {
	case srcLocal && dstLocal:
		// Pure loopback / host-to-host: not interesting for bandwidth.
		return "", 0, "", 0, false
	case srcLocal:
		return types.DirOut, sport, dstIP, dport, true
	case dstLocal:
		return types.DirIn, dport, srcIP, sport, true
	default:
		// Neither side is a known local address (common for offline replay
		// without an interface address list). Fall back to a private-range
		// heuristic: the private side is treated as local.
		if isPrivate(srcIP) && !isPrivate(dstIP) {
			return types.DirOut, sport, dstIP, dport, true
		}
		if isPrivate(dstIP) && !isPrivate(srcIP) {
			return types.DirIn, dport, srcIP, sport, true
		}
		return "", 0, "", 0, false
	}
}

func (d *Decoder) isLocal(ip string) bool {
	return d.localIPs[ip]
}

func (d *Decoder) recordDNS(dns *layers.DNS) {
	if !dns.QR || dns.ResponseCode != layers.DNSResponseCodeNoErr {
		return
	}
	name := ""
	if len(dns.Questions) > 0 {
		name = string(dns.Questions[0].Name)
	}
	for _, ans := range dns.Answers {
		if ans.Type != layers.DNSTypeA && ans.Type != layers.DNSTypeAAAA {
			continue
		}
		if ans.IP == nil {
			continue
		}
		host := name
		if host == "" {
			host = string(ans.Name)
		}
		d.dns.Put(ans.IP.String(), host)
	}
}

// wireBytes returns the original on-wire length of the packet, falling back to
// the captured length, then to the IP payload size.
func wireBytes(pkt gopacket.Packet) uint64 {
	md := pkt.Metadata()
	if md != nil && md.Length > 0 {
		return uint64(md.Length)
	}
	if md != nil && md.CaptureLength > 0 {
		return uint64(md.CaptureLength)
	}
	if n := pkt.NetworkLayer(); n != nil {
		return uint64(len(n.LayerContents()) + len(n.LayerPayload()))
	}
	return 0
}

// isPrivate reports whether ip is in an RFC1918 / link-local / loopback range.
func isPrivate(ip string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	return parsed.IsPrivate() || parsed.IsLoopback() || parsed.IsLinkLocalUnicast()
}
