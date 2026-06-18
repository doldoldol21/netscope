//go:build ignore

// gensample writes a small synthetic pcap useful for exercising netscoped
// without root:  go run tools/gensample.go testdata/sample.pcap
//
// It emits a DNS response plus a handful of bidirectional TCP data packets
// across two "remote" services, one of which (1.2.3.4 -> api.openai.com) is an
// AI service so the AI highlighting can be seen in the dashboard.
package main

import (
	"fmt"
	"net"
	"os"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcapgo"
)

const localIP = "192.168.1.10"

func main() {
	out := "testdata/sample.pcap"
	if len(os.Args) > 1 {
		out = os.Args[1]
	}
	if err := os.MkdirAll("testdata", 0o755); err != nil {
		panic(err)
	}
	f, err := os.Create(out)
	if err != nil {
		panic(err)
	}
	defer f.Close()

	w := pcapgo.NewWriter(f)
	if err := w.WriteFileHeader(2048, layers.LinkTypeEthernet); err != nil {
		panic(err)
	}

	ts := time.Unix(1700000000, 0)
	write := func(ls ...gopacket.SerializableLayer) {
		buf := gopacket.NewSerializeBuffer()
		if err := gopacket.SerializeLayers(buf, gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}, ls...); err != nil {
			panic(err)
		}
		b := buf.Bytes()
		ts = ts.Add(50 * time.Millisecond)
		_ = w.WritePacket(gopacket.CaptureInfo{Timestamp: ts, CaptureLength: len(b), Length: len(b)}, b)
	}

	dnsReply("api.openai.com", "1.2.3.4", write)
	dnsReply("github.com", "5.6.7.8", write)

	// Simulate a chatty AI download and a smaller github upload.
	for i := 0; i < 40; i++ {
		data(localIP, "1.2.3.4", 50001, 443, "in", 1400, write)  // download from AI
		data(localIP, "1.2.3.4", 50001, 443, "out", 120, write)  // request bytes
	}
	for i := 0; i < 8; i++ {
		data(localIP, "5.6.7.8", 50002, 443, "out", 900, write) // upload to github
		data(localIP, "5.6.7.8", 50002, 443, "in", 200, write)
	}

	fmt.Printf("wrote %s\n", out)
}

func ether() *layers.Ethernet {
	return &layers.Ethernet{
		SrcMAC: net.HardwareAddr{2, 0, 0, 0, 0, 1}, DstMAC: net.HardwareAddr{2, 0, 0, 0, 0, 2},
		EthernetType: layers.EthernetTypeIPv4,
	}
}

func dnsReply(name, ip string, write func(...gopacket.SerializableLayer)) {
	dip := &layers.IPv4{Version: 4, IHL: 5, TTL: 64, Protocol: layers.IPProtocolUDP,
		SrcIP: net.ParseIP("8.8.8.8"), DstIP: net.ParseIP(localIP)}
	udp := &layers.UDP{SrcPort: 53, DstPort: 50000}
	udp.SetNetworkLayerForChecksum(dip)
	write(ether(), dip, udp, &layers.DNS{
		QR: true,
		Questions: []layers.DNSQuestion{{Name: []byte(name), Type: layers.DNSTypeA, Class: layers.DNSClassIN}},
		Answers:   []layers.DNSResourceRecord{{Name: []byte(name), Type: layers.DNSTypeA, Class: layers.DNSClassIN, IP: net.ParseIP(ip)}},
	})
}

func data(local, remote string, lport, rport layers.TCPPort, dir string, payload int, write func(...gopacket.SerializableLayer)) {
	ip := &layers.IPv4{Version: 4, IHL: 5, TTL: 64, Protocol: layers.IPProtocolTCP}
	tcp := &layers.TCP{}
	if dir == "out" {
		ip.SrcIP, ip.DstIP = net.ParseIP(local), net.ParseIP(remote)
		tcp.SrcPort, tcp.DstPort = lport, rport
	} else {
		ip.SrcIP, ip.DstIP = net.ParseIP(remote), net.ParseIP(local)
		tcp.SrcPort, tcp.DstPort = rport, lport
	}
	tcp.SetNetworkLayerForChecksum(ip)
	write(ether(), ip, tcp, gopacket.Payload(make([]byte, payload)))
}
