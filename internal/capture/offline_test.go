//go:build darwin

package capture

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/doldoldol21/netscope/internal/dnscache"
	"github.com/doldoldol21/netscope/pkg/types"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcapgo"
)

// writeSamplePcap creates a small Ethernet pcap with a DNS response and a
// couple of data packets, returning its path.
func writeSamplePcap(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sample.pcap")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	w := pcapgo.NewWriter(f)
	if err := w.WriteFileHeader(snapLen, layers.LinkTypeEthernet); err != nil {
		t.Fatal(err)
	}

	write := func(ls ...gopacket.SerializableLayer) {
		buf := gopacket.NewSerializeBuffer()
		opts := gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}
		if err := gopacket.SerializeLayers(buf, opts, ls...); err != nil {
			t.Fatal(err)
		}
		b := buf.Bytes()
		ci := gopacket.CaptureInfo{Timestamp: time.Unix(1700000000, 0), CaptureLength: len(b), Length: len(b)}
		if err := w.WritePacket(ci, b); err != nil {
			t.Fatal(err)
		}
	}

	e := func() *layers.Ethernet {
		return &layers.Ethernet{
			SrcMAC: net.HardwareAddr{2, 0, 0, 0, 0, 1}, DstMAC: net.HardwareAddr{2, 0, 0, 0, 0, 2},
			EthernetType: layers.EthernetTypeIPv4,
		}
	}

	// 1) DNS response: 1.2.3.4 -> ai.example
	dip := &layers.IPv4{Version: 4, IHL: 5, TTL: 64, Protocol: layers.IPProtocolUDP,
		SrcIP: net.ParseIP("8.8.8.8"), DstIP: net.ParseIP("192.168.1.10")}
	dudp := &layers.UDP{SrcPort: 53, DstPort: 50000}
	dudp.SetNetworkLayerForChecksum(dip)
	write(e(), dip, dudp, &layers.DNS{
		QR: true, Questions: []layers.DNSQuestion{{Name: []byte("ai.example"), Type: layers.DNSTypeA, Class: layers.DNSClassIN}},
		Answers: []layers.DNSResourceRecord{{Name: []byte("ai.example"), Type: layers.DNSTypeA, Class: layers.DNSClassIN, IP: net.ParseIP("1.2.3.4")}},
	})

	// 2) outbound data 192.168.1.10:50001 -> 1.2.3.4:443
	oip := &layers.IPv4{Version: 4, IHL: 5, TTL: 64, Protocol: layers.IPProtocolTCP,
		SrcIP: net.ParseIP("192.168.1.10"), DstIP: net.ParseIP("1.2.3.4")}
	otcp := &layers.TCP{SrcPort: 50001, DstPort: 443}
	otcp.SetNetworkLayerForChecksum(oip)
	write(e(), oip, otcp, gopacket.Payload(make([]byte, 100)))

	// 3) inbound data 1.2.3.4:443 -> 192.168.1.10:50001
	iip := &layers.IPv4{Version: 4, IHL: 5, TTL: 64, Protocol: layers.IPProtocolTCP,
		SrcIP: net.ParseIP("1.2.3.4"), DstIP: net.ParseIP("192.168.1.10")}
	itcp := &layers.TCP{SrcPort: 443, DstPort: 50001}
	itcp.SetNetworkLayerForChecksum(iip)
	write(e(), iip, itcp, gopacket.Payload(make([]byte, 500)))

	return path
}

func TestOfflineSourceEndToEnd(t *testing.T) {
	path := writeSamplePcap(t)
	dns := dnscache.New(time.Hour, 100)

	src, err := OpenOffline(path, []string{"192.168.1.10"}, dns)
	if err != nil {
		t.Fatal(err)
	}

	flows := make(chan types.Flow, 16)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		err := src.Run(ctx, flows)
		close(flows) // unblock the reader once the file is exhausted
		done <- err
	}()

	var got []types.Flow
	timeout := time.After(5 * time.Second)
loop:
	for {
		select {
		case f, ok := <-flows:
			if !ok {
				break loop
			}
			got = append(got, f)
		case <-timeout:
			t.Fatal("timed out reading flows")
		}
	}
	if err := <-done; err != nil {
		t.Fatalf("source run: %v", err)
	}

	// DNS packet is intra-direction-attributable too (8.8.8.8 -> local), so we
	// expect 3 flows: the DNS reply, the outbound, and the inbound data.
	if len(got) != 3 {
		t.Fatalf("got %d flows, want 3: %+v", len(got), got)
	}
	if h := dns.Lookup("1.2.3.4"); h != "ai.example" {
		t.Errorf("DNS not learned from replay: %q", h)
	}

	var out, in bool
	for _, f := range got {
		if f.Direction == types.DirOut && f.RemotePort == 443 {
			out = true
		}
		if f.Direction == types.DirIn && f.RemotePort == 443 && f.Bytes >= 500 {
			in = true
		}
	}
	if !out || !in {
		t.Errorf("missing expected directional flows: out=%v in=%v", out, in)
	}
}
