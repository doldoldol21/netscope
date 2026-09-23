//go:build darwin

package resolver

import (
	"encoding/binary"
	"net"
	"os"
	"testing"

	"github.com/doldoldol21/netscope/pkg/types"
)

// rec builds one (len, kind) record of the given length; fill writes fields.
func rec(kind uint32, n int, fill func(b []byte)) []byte {
	b := make([]byte, n)
	binary.LittleEndian.PutUint32(b[0:4], uint32(n))
	binary.LittleEndian.PutUint32(b[4:8], kind)
	if fill != nil {
		fill(b)
	}
	return b
}

func inpcbRec(vflag byte, laddr, faddr string, lport, fport uint16) []byte {
	return rec(xsoInpcb, 104, func(b []byte) {
		binary.BigEndian.PutUint16(b[xiFport:], fport)
		binary.BigEndian.PutUint16(b[xiLport:], lport)
		b[xiVflag] = vflag
		put := func(off int, s string) {
			if s == "" {
				return
			}
			ip := net.ParseIP(s)
			if v4 := ip.To4(); v4 != nil && vflag&inpIPv4 != 0 {
				copy(b[off+12:off+16], v4)
			} else {
				copy(b[off:off+16], ip.To16())
			}
		}
		put(xiFaddr, faddr)
		put(xiLaddr, laddr)
	})
}

func socketRec(last, e int32, state uint16) []byte {
	return rec(xsoSocket, 104, func(b []byte) {
		binary.LittleEndian.PutUint16(b[xsState:], state)
		binary.LittleEndian.PutUint32(b[xsLastPID:], uint32(last))
		binary.LittleEndian.PutUint32(b[xsEPID:], uint32(e))
	})
}

// stream assembles records the way the kernel does: xinpgen header, records
// padded to 8 bytes, trailing xinpgen.
func stream(records ...[]byte) []byte {
	gen := make([]byte, xinpgenSize)
	binary.LittleEndian.PutUint32(gen[0:4], xinpgenSize)
	out := append([]byte(nil), gen...)
	for _, r := range records {
		out = append(out, r...)
		for len(out)%8 != 0 {
			out = append(out, 0)
		}
	}
	return append(out, gen...)
}

func TestParsePCBListDecodesTheKernelLayout(t *testing.T) {
	raw := stream(
		// A connected IPv4 TCP socket with the full record set, including an
		// odd-length tcpcb that forces the 8-byte padding rule.
		inpcbRec(inpIPv4, "10.0.0.5", "1.2.3.4", 50000, 443),
		socketRec(111, 0, 0x0102),
		rec(0x002, 32, nil), rec(0x004, 32, nil), rec(0x008, 136, nil), rec(0x020, 204, nil),
		// A disconnected IPv6 socket that was delegated to another process.
		inpcbRec(inpIPv6, "2001:db8::5", "2001:db8::1", 50001, 8443),
		socketRec(222, 333, 0x2131),
		// An unconnected (bound) socket: no foreign address.
		inpcbRec(inpIPv4, "", "", 5353, 0),
		socketRec(444, 0, 0x0100),
		// A pcb nobody owns is dropped.
		inpcbRec(inpIPv4, "10.0.0.5", "9.9.9.9", 50002, 53),
		socketRec(0, 0, 0x0102),
		// An unknown record kind between groups is skipped, not mis-read.
		rec(0x400, 48, nil),
		inpcbRec(inpIPv4, "10.0.0.5", "8.8.8.8", 50003, 53),
		socketRec(555, 0, 0x0102),
	)
	rows := parsePCBList(raw, types.ProtoTCP, nil)
	want := []pcbRow{
		{proto: "tcp", lport: 50000, fport: 443, laddr: "10.0.0.5", faddr: "1.2.3.4", lastPID: 111},
		{proto: "tcp", lport: 50001, fport: 8443, laddr: "2001:db8::5", faddr: "2001:db8::1", lastPID: 222, ePID: 333, dead: true},
		{proto: "tcp", lport: 5353, lastPID: 444},
		{proto: "tcp", lport: 50003, fport: 53, laddr: "10.0.0.5", faddr: "8.8.8.8", lastPID: 555},
	}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows %+v, want %d", len(rows), rows, len(want))
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, rows[i], want[i])
		}
	}
	if got := rows[1].pid(); got != 333 {
		t.Errorf("delegated pcb pid() = %d, want the effective pid 333", got)
	}
	if got := rows[0].pid(); got != 111 {
		t.Errorf("plain pcb pid() = %d, want so_last_pid 111", got)
	}
}

func TestParsePCBListSurvivesTruncationAndJunk(t *testing.T) {
	raw := stream(
		inpcbRec(inpIPv4, "10.0.0.5", "1.2.3.4", 50000, 443), socketRec(111, 0, 0x0102),
		inpcbRec(inpIPv4, "10.0.0.5", "1.2.3.4", 50001, 443), socketRec(112, 0, 0x0102),
	)
	// Cut inside the second group's socket record: the first row survives,
	// the half-written second one is dropped rather than read past the end.
	cut := raw[:len(raw)-xinpgenSize-40]
	rows := parsePCBList(cut, types.ProtoUDP, nil)
	if len(rows) != 1 || rows[0].lastPID != 111 || rows[0].proto != types.ProtoUDP {
		t.Fatalf("truncated stream: got %+v, want just pid 111 as udp", rows)
	}
	for _, junk := range [][]byte{nil, {1, 2, 3}, make([]byte, xinpgenSize), append(make([]byte, xinpgenSize), 0xff, 0xff, 0xff, 0xff, 0, 0, 0, 0)} {
		if got := parsePCBList(junk, types.ProtoTCP, nil); len(got) != 0 {
			t.Errorf("junk %v: got rows %+v", junk, got)
		}
	}
}

// The synthetic tests pin the parser to the offsets we believe in; this one
// pins those offsets to the running kernel by finding this process's own
// sockets — listener, connected client, bound UDP — in the real sysctl output.
func TestListPCBsFindsOurOwnSockets(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback:", err)
	}
	defer ln.Close()
	client, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	udp, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()

	me := os.Getpid()
	lport := uint16(ln.Addr().(*net.TCPAddr).Port)
	cport := uint16(client.LocalAddr().(*net.TCPAddr).Port)
	uport := uint16(udp.LocalAddr().(*net.UDPAddr).Port)

	want := map[pcbRow]bool{
		{proto: types.ProtoTCP, lport: lport, laddr: "127.0.0.1", lastPID: me}:                                   false, // listener
		{proto: types.ProtoTCP, lport: cport, fport: lport, laddr: "127.0.0.1", faddr: "127.0.0.1", lastPID: me}: false, // client side
		{proto: types.ProtoTCP, lport: lport, fport: cport, laddr: "127.0.0.1", faddr: "127.0.0.1", lastPID: me}: false, // accepted side
		{proto: types.ProtoUDP, lport: uport, laddr: "127.0.0.1", lastPID: me}:                                   false, // bound udp
	}
	rows := listPCBs()
	for _, r := range rows {
		r.ePID, r.dead = 0, false // not part of the identity we assert on
		if _, ok := want[r]; ok {
			want[r] = true
		}
	}
	for r, found := range want {
		if !found {
			t.Errorf("own socket %+v not in pcb list (%d rows)", r, len(rows))
		}
	}
}

func TestMergePCBsFillsOnlyWhatTheFdWalkMissed(t *testing.T) {
	fd := []rawConn{
		{PID: 10, Proto: types.ProtoTCP, LPort: 1000, RAddr: "1.1.1.1", RPort: 443},
		{PID: 11, Proto: types.ProtoUDP, LPort: 5353},
	}
	pcbs := []pcbRow{
		// Same tuple as an fd row: the fd owner keeps it.
		{proto: types.ProtoTCP, lport: 1000, fport: 443, faddr: "1.1.1.1", lastPID: 99},
		// Same bound port as an fd row: kept too (unconnected key matches).
		{proto: types.ProtoUDP, lport: 5353, lastPID: 98},
		// Kernel-owned flow nobody else has: added.
		{proto: types.ProtoTCP, lport: 2000, fport: 443, faddr: "2.2.2.2", lastPID: 20},
		// A proxy's shadow pcb and the app's delegated one share a tuple: the
		// delegated one wins regardless of order.
		{proto: types.ProtoTCP, lport: 3000, fport: 443, faddr: "3.3.3.3", lastPID: 30},
		{proto: types.ProtoTCP, lport: 3000, fport: 443, faddr: "3.3.3.3", lastPID: 31, ePID: 32},
		// A dead pcb only counts if nothing live claims the tuple.
		{proto: types.ProtoTCP, lport: 4000, fport: 443, faddr: "4.4.4.4", lastPID: 40, dead: true},
		{proto: types.ProtoTCP, lport: 4000, fport: 443, faddr: "4.4.4.4", lastPID: 41},
		{proto: types.ProtoTCP, lport: 5000, fport: 443, faddr: "5.5.5.5", lastPID: 50, dead: true},
	}
	got := mergePCBs(fd, pcbs)
	want := append(append([]rawConn(nil), fd...),
		rawConn{PID: 20, Proto: types.ProtoTCP, LPort: 2000, RAddr: "2.2.2.2", RPort: 443},
		rawConn{PID: 32, Proto: types.ProtoTCP, LPort: 3000, RAddr: "3.3.3.3", RPort: 443},
		rawConn{PID: 41, Proto: types.ProtoTCP, LPort: 4000, RAddr: "4.4.4.4", RPort: 443},
		rawConn{PID: 50, Proto: types.ProtoTCP, LPort: 5000, RAddr: "5.5.5.5", RPort: 443},
	)
	if len(got) != len(want) {
		t.Fatalf("got %d rows %+v, want %d %+v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// End to end through Refresh: a socket this process owns resolves to this
// process by tuple even when the fd walk is not what found it.
func TestResolverNamesAKernelListedSocket(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback:", err)
	}
	defer ln.Close()
	client, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	r := New(0)
	proc, ok := r.Lookup(types.ConnKey{
		Proto:      types.ProtoTCP,
		LocalPort:  uint16(client.LocalAddr().(*net.TCPAddr).Port),
		RemoteIP:   "127.0.0.1",
		RemotePort: uint16(ln.Addr().(*net.TCPAddr).Port),
	})
	if !ok || proc.PID != os.Getpid() {
		t.Fatalf("Lookup = %+v, %v; want our own pid %d", proc, ok, os.Getpid())
	}
}
