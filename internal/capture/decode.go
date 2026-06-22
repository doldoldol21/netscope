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
	localIPs map[[16]byte]bool // keyed by canonical 16-byte form (alloc-free lookups)
	dns      *dnscache.Cache
}

// NewDecoder builds a Decoder. localIPs is the set of addresses belonging to
// this host (used to decide upload vs download); dns may be nil to skip DNS
// learning.
func NewDecoder(localIPs []string, dns *dnscache.Cache) *Decoder {
	set := make(map[[16]byte]bool, len(localIPs))
	for _, s := range localIPs {
		if ip := net.ParseIP(s); ip != nil {
			if k, ok := ipKey(ip); ok {
				set[k] = true
			}
		}
	}
	return &Decoder{localIPs: set, dns: dns}
}

// ipKey returns a canonical fixed-size key for an IP without heap allocation, so
// per-packet membership checks don't allocate (unlike net.IP.String()).
func ipKey(ip net.IP) ([16]byte, bool) {
	var k [16]byte
	if v4 := ip.To4(); v4 != nil { // To4 returns a sub-slice, no allocation
		k[10], k[11] = 0xff, 0xff
		copy(k[12:], v4)
		return k, true
	}
	if len(ip) == net.IPv6len {
		copy(k[:], ip)
		return k, true
	}
	return k, false
}

// Decode reduces a packet to a Flow. The bool is false when the packet is not
// an attributable IP/TCP/UDP packet (e.g. ARP, loopback chatter) or when it is
// purely intra-host traffic we choose to ignore.
func (d *Decoder) Decode(pkt gopacket.Packet) (types.Flow, bool) {
	nl := pkt.NetworkLayer()
	if nl == nil {
		return types.Flow{}, false
	}

	// Keep the IPs as raw bytes; only the kept flow's remote IP is stringified
	// (once, below) — the per-packet hot path must not allocate.
	var srcIP, dstIP net.IP
	switch n := nl.(type) {
	case *layers.IPv4:
		srcIP, dstIP = n.SrcIP, n.DstIP
	case *layers.IPv6:
		srcIP, dstIP = n.SrcIP, n.DstIP
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
		RemoteIP:   remoteIP.String(), // the one string allocation, kept packets only
		RemotePort: remotePort,
		Bytes:      wireBytes(pkt),
	}

	// Learn IP->host from the TLS SNI in an outbound ClientHello. This covers
	// servers with no DNS answer we saw and no PTR record (Anthropic, Cloudflare,
	// Telegram, …) — the host is in cleartext even though the session is encrypted.
	if d.dns != nil && proto == types.ProtoTCP && dir == types.DirOut {
		if tl := pkt.TransportLayer(); tl != nil {
			if sni := parseSNI(tl.LayerPayload()); sni != "" {
				d.dns.Put(flow.RemoteIP, sni)
			}
		}
	}
	if flow.Timestamp.IsZero() {
		// Offline sources without per-packet timestamps; caller stamps later.
	}
	return flow, true
}

// direction figures out which endpoint is local and reports the flow direction
// and the normalised local/remote ports. ok is false for intra-host traffic.
func (d *Decoder) direction(srcIP, dstIP net.IP, sport, dport uint16) (types.Direction, uint16, net.IP, uint16, bool) {
	srcLocal := d.isLocal(srcIP)
	dstLocal := d.isLocal(dstIP)

	switch {
	case srcLocal && dstLocal:
		// Pure loopback / host-to-host: not interesting for bandwidth.
		return "", 0, nil, 0, false
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
		return "", 0, nil, 0, false
	}
}

func (d *Decoder) isLocal(ip net.IP) bool {
	k, ok := ipKey(ip)
	return ok && d.localIPs[k]
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

// parseSNI extracts the server_name (SNI) from a TLS ClientHello in a TCP
// payload, or "" if the payload isn't a ClientHello or has no SNI. It bails out
// immediately on non-handshake bytes so it's cheap on the per-packet hot path.
func parseSNI(p []byte) string {
	// TLS record: type(1)=0x16 handshake, version(2), length(2).
	if len(p) < 6 || p[0] != 0x16 {
		return ""
	}
	hs := p[5:]
	// Handshake: msg_type(1)=0x01 ClientHello, length(3), version(2), random(32).
	if len(hs) < 38 || hs[0] != 0x01 {
		return ""
	}
	i := 4 + 2 + 32 // skip msg_type+length(4), client_version(2), random(32)
	if i >= len(hs) {
		return ""
	}
	i += 1 + int(hs[i]) // session_id: len(1) + id
	if i+2 > len(hs) {
		return ""
	}
	i += 2 + (int(hs[i])<<8 | int(hs[i+1])) // cipher_suites: len(2) + suites
	if i+1 > len(hs) {
		return ""
	}
	i += 1 + int(hs[i]) // compression_methods: len(1) + methods
	if i+2 > len(hs) {
		return ""
	}
	end := i + 2 + (int(hs[i])<<8 | int(hs[i+1])) // extensions block
	i += 2
	if end > len(hs) {
		end = len(hs)
	}
	for i+4 <= end {
		extType := int(hs[i])<<8 | int(hs[i+1])
		extLen := int(hs[i+2])<<8 | int(hs[i+3])
		i += 4
		if i+extLen > len(hs) {
			break
		}
		if extType == 0x0000 { // server_name
			return sniName(hs[i : i+extLen])
		}
		i += extLen
	}
	return ""
}

// sniName reads the first host_name from a server_name extension body.
func sniName(b []byte) string {
	// server_name_list: list_len(2), then entries: name_type(1), name_len(2), name.
	i := 2
	for i+3 <= len(b) {
		nameType := b[i]
		nameLen := int(b[i+1])<<8 | int(b[i+2])
		i += 3
		if i+nameLen > len(b) {
			break
		}
		if nameType == 0x00 { // host_name
			return string(b[i : i+nameLen])
		}
		i += nameLen
	}
	return ""
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
func isPrivate(ip net.IP) bool {
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()
}
