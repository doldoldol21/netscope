package capture

import (
	"net"
	"testing"
	"time"

	"github.com/doldoldol21/netscope/internal/dnscache"
	"github.com/doldoldol21/netscope/pkg/types"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
)

// clientHello builds a minimal TLS ClientHello record carrying the given SNI.
func clientHello(host string) []byte {
	// server_name extension body: list_len(2), name_type(1)=0, name_len(2), name
	listLen := len(host) + 3
	var sn []byte
	sn = append(sn, byte(listLen>>8), byte(listLen))    // server_name_list length
	sn = append(sn, 0x00)                               // name_type host_name
	sn = append(sn, byte(len(host)>>8), byte(len(host))) // name length
	sn = append(sn, host...)
	// extension: type(2)=0x0000 server_name, len(2), body
	var ext []byte
	ext = append(ext, 0x00, 0x00) // ext type server_name
	ext = append(ext, byte(len(sn)>>8), byte(len(sn)))
	ext = append(ext, sn...)
	// handshake body: version(2), random(32), sid_len(1)=0, cs_len(2)=2 + 1 suite,
	// comp_len(1)=1 + 1 method, ext_len(2), extensions
	var hb []byte
	hb = append(hb, 0x03, 0x03)                 // client_version
	hb = append(hb, make([]byte, 32)...)        // random
	hb = append(hb, 0x00)                        // session_id len
	hb = append(hb, 0x00, 0x02, 0x13, 0x01)     // cipher_suites: len 2 + one suite
	hb = append(hb, 0x01, 0x00)                  // compression: len 1 + null
	hb = append(hb, byte(len(ext)>>8), byte(len(ext)))
	hb = append(hb, ext...)
	// handshake: type(1)=0x01, length(3), body
	var hs []byte
	hs = append(hs, 0x01, byte(len(hb)>>16), byte(len(hb)>>8), byte(len(hb)))
	hs = append(hs, hb...)
	// record: type(1)=0x16, version(2), length(2), handshake
	rec := []byte{0x16, 0x03, 0x01, byte(len(hs) >> 8), byte(len(hs))}
	return append(rec, hs...)
}

func TestParseSNI(t *testing.T) {
	if got := parseSNI(clientHello("api.anthropic.com")); got != "api.anthropic.com" {
		t.Fatalf("parseSNI = %q, want api.anthropic.com", got)
	}
	if got := parseSNI([]byte{0x17, 0x03, 0x03, 0, 0}); got != "" {
		t.Fatalf("non-handshake should yield empty, got %q", got)
	}
	if got := parseSNI([]byte{0x16}); got != "" {
		t.Fatalf("truncated should yield empty, got %q", got)
	}
}

// buildPacket serialises the given layers and re-parses them into a
// gopacket.Packet with a populated wire length, mimicking a captured frame.
func buildPacket(t *testing.T, ls ...gopacket.SerializableLayer) gopacket.Packet {
	t.Helper()
	buf := gopacket.NewSerializeBuffer()
	opts := gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}
	if err := gopacket.SerializeLayers(buf, opts, ls...); err != nil {
		t.Fatalf("serialize: %v", err)
	}
	pkt := gopacket.NewPacket(buf.Bytes(), layers.LinkTypeEthernet, gopacket.Default)
	md := pkt.Metadata()
	md.Length = len(buf.Bytes())
	md.CaptureLength = len(buf.Bytes())
	md.Timestamp = time.Unix(1700000000, 0)
	return pkt
}

func eth() *layers.Ethernet {
	return &layers.Ethernet{
		SrcMAC:       net.HardwareAddr{0x02, 0, 0, 0, 0, 1},
		DstMAC:       net.HardwareAddr{0x02, 0, 0, 0, 0, 2},
		EthernetType: layers.EthernetTypeIPv4,
	}
}

func TestDecodeOutbound(t *testing.T) {
	ip := &layers.IPv4{
		Version: 4, IHL: 5, TTL: 64, Protocol: layers.IPProtocolTCP,
		SrcIP: net.ParseIP("192.168.1.10"), DstIP: net.ParseIP("1.2.3.4"),
	}
	tcp := &layers.TCP{SrcPort: 50000, DstPort: 443}
	tcp.SetNetworkLayerForChecksum(ip)
	pkt := buildPacket(t, eth(), ip, tcp, gopacket.Payload(make([]byte, 100)))

	dec := NewDecoder([]string{"192.168.1.10"}, nil)
	flow, ok := dec.Decode(pkt)
	if !ok {
		t.Fatal("expected attributable flow")
	}
	if flow.Direction != types.DirOut {
		t.Errorf("direction = %v, want out", flow.Direction)
	}
	if flow.LocalPort != 50000 || flow.RemotePort != 443 || flow.RemoteIP != "1.2.3.4" {
		t.Errorf("normalisation wrong: %+v", flow)
	}
	if flow.Proto != types.ProtoTCP {
		t.Errorf("proto = %v", flow.Proto)
	}
	if flow.Bytes == 0 {
		t.Errorf("bytes should be > 0")
	}
}

func TestDecodeInbound(t *testing.T) {
	ip := &layers.IPv4{
		Version: 4, IHL: 5, TTL: 64, Protocol: layers.IPProtocolTCP,
		SrcIP: net.ParseIP("1.2.3.4"), DstIP: net.ParseIP("192.168.1.10"),
	}
	tcp := &layers.TCP{SrcPort: 443, DstPort: 50000}
	tcp.SetNetworkLayerForChecksum(ip)
	pkt := buildPacket(t, eth(), ip, tcp, gopacket.Payload(make([]byte, 200)))

	dec := NewDecoder([]string{"192.168.1.10"}, nil)
	flow, ok := dec.Decode(pkt)
	if !ok {
		t.Fatal("expected attributable flow")
	}
	if flow.Direction != types.DirIn {
		t.Errorf("direction = %v, want in", flow.Direction)
	}
	if flow.LocalPort != 50000 || flow.RemotePort != 443 || flow.RemoteIP != "1.2.3.4" {
		t.Errorf("normalisation wrong: %+v", flow)
	}
}

func TestDecodePrivateHeuristic(t *testing.T) {
	// No local IPs supplied: the private side should be treated as local.
	ip := &layers.IPv4{
		Version: 4, IHL: 5, TTL: 64, Protocol: layers.IPProtocolUDP,
		SrcIP: net.ParseIP("10.0.0.5"), DstIP: net.ParseIP("8.8.8.8"),
	}
	udp := &layers.UDP{SrcPort: 40000, DstPort: 4242}
	udp.SetNetworkLayerForChecksum(ip)
	pkt := buildPacket(t, eth(), ip, udp, gopacket.Payload(make([]byte, 50)))

	dec := NewDecoder(nil, nil)
	flow, ok := dec.Decode(pkt)
	if !ok {
		t.Fatal("expected attributable flow via heuristic")
	}
	if flow.Direction != types.DirOut || flow.RemoteIP != "8.8.8.8" {
		t.Errorf("heuristic wrong: %+v", flow)
	}
}

func TestDecodeIntraHostIgnored(t *testing.T) {
	ip := &layers.IPv4{
		Version: 4, IHL: 5, TTL: 64, Protocol: layers.IPProtocolTCP,
		SrcIP: net.ParseIP("127.0.0.1"), DstIP: net.ParseIP("127.0.0.1"),
	}
	tcp := &layers.TCP{SrcPort: 1, DstPort: 2}
	tcp.SetNetworkLayerForChecksum(ip)
	pkt := buildPacket(t, eth(), ip, tcp, gopacket.Payload(make([]byte, 10)))

	dec := NewDecoder([]string{"127.0.0.1"}, nil)
	if _, ok := dec.Decode(pkt); ok {
		t.Error("intra-host traffic should be ignored")
	}
}

func TestDecodeLearnsDNS(t *testing.T) {
	ip := &layers.IPv4{
		Version: 4, IHL: 5, TTL: 64, Protocol: layers.IPProtocolUDP,
		SrcIP: net.ParseIP("8.8.8.8"), DstIP: net.ParseIP("192.168.1.10"),
	}
	udp := &layers.UDP{SrcPort: 53, DstPort: 51000}
	udp.SetNetworkLayerForChecksum(ip)
	dns := &layers.DNS{
		QR: true, ResponseCode: layers.DNSResponseCodeNoErr,
		Questions: []layers.DNSQuestion{{Name: []byte("example.com"), Type: layers.DNSTypeA, Class: layers.DNSClassIN}},
		Answers: []layers.DNSResourceRecord{{
			Name: []byte("example.com"), Type: layers.DNSTypeA, Class: layers.DNSClassIN,
			IP: net.ParseIP("93.184.216.34"),
		}},
	}
	pkt := buildPacket(t, eth(), ip, udp, dns)

	cache := dnscache.New(time.Hour, 100)
	dec := NewDecoder([]string{"192.168.1.10"}, cache)
	if _, ok := dec.Decode(pkt); !ok {
		t.Fatal("DNS packet should still yield a flow")
	}
	if got := cache.Lookup("93.184.216.34"); got != "example.com" {
		t.Errorf("DNS cache = %q, want example.com", got)
	}
}
