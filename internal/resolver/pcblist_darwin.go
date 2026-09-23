//go:build darwin

package resolver

import (
	"encoding/binary"
	"net"

	"golang.org/x/sys/unix"

	"github.com/doldoldol21/netscope/pkg/types"
)

// The fd walk in scan_darwin.go only sees sockets a process holds as file
// descriptors. Network.framework clients — every Apple daemon that speaks
// HTTP (cloudd, softwareupdated, apsd, trustd, nsurlsessiond, mDNSResponder's
// DNS-over-UDP …) and any third-party app on NSURLSession/nw_connection —
// hold a single NECP client fd (lsof lists it as NPOLICY) and let the kernel
// own the actual socket. Those flows never show up in proc_pidinfo, which is
// why Apple's own traffic used to land in "unknown" while netstat -v could
// still name the process: it reads the kernel's protocol control blocks,
// where every inpcb carries the pid that last touched it.
//
// This file reads the same sysctl. The record layouts are the private
// xinpcb_n / xsocket_n structs from XNU (bsd/netinet/in_pcb.h,
// bsd/sys/socketvar.h); the SDK does not ship them, so they are decoded by
// offset. The kernel emits them 4-byte packed (a u_int64_t sits at offset 20,
// not 24), which is why the offsets below differ from what the C declarations
// would give on arm64. Each record is self-describing (length, kind), so an
// unknown or grown record is skipped rather than mis-read, and the test suite
// pins the offsets by finding the test process's own sockets in the output.

const (
	xsoSocket = 0x001 // struct xsocket_n
	xsoInpcb  = 0x010 // struct xinpcb_n

	xinpgenSize = 24 // struct xinpgen: len, count, gen, sogen

	// struct xinpcb_n
	xiFport  = 16 // u_short, network byte order
	xiLport  = 18 // u_short, network byte order
	xiVflag  = 44 // u_char: INP_IPV4 = 1, INP_IPV6 = 2
	xiFaddr  = 48 // 16-byte union; IPv4 lives in the last 4 bytes
	xiLaddr  = 64 // 16-byte union; IPv4 lives in the last 4 bytes
	xiMinLen = 80

	// struct xsocket_n
	xsState   = 26 // short so_state
	xsLastPID = 68 // pid_t so_last_pid
	xsEPID    = 72 // pid_t so_e_pid (delegated/effective owner, 0 if none)
	xsMinLen  = 76

	ssIsDisconnected = 0x2000 // so_state: the connection is over, pcb lingers

	inpIPv4 = 0x1
	inpIPv6 = 0x2
)

// pcbRow is one kernel protocol control block and the process behind it.
type pcbRow struct {
	proto        types.Protocol
	lport, fport uint16
	laddr, faddr string // faddr is "" for an unconnected socket
	lastPID      int    // last process to act on the socket (what netstat shows)
	ePID         int    // effective owner when the socket was delegated, else 0
	dead         bool   // disconnected; kept so a trailing FIN/ACK still has a name
}

// pid is the process a flow on this pcb belongs to. A delegated socket
// (so_e_pid set) was opened by a system service on behalf of an app —
// nsurlsessiond for App Store, WebKit's networking process for Safari —
// and the app is the answer a "which app is using the network" question wants.
func (r pcbRow) pid() int {
	if r.ePID > 0 {
		return r.ePID
	}
	return r.lastPID
}

// listPCBs returns every TCP and UDP control block the kernel holds, with the
// owning pid. Errors are swallowed per protocol: a missing sysctl on some
// future macOS just means that protocol falls back to the fd walk.
func listPCBs() []pcbRow {
	var rows []pcbRow
	for _, src := range []struct {
		name  string
		proto types.Protocol
	}{
		{"net.inet.tcp.pcblist_n", types.ProtoTCP},
		{"net.inet.udp.pcblist_n", types.ProtoUDP},
	} {
		raw, err := unix.SysctlRaw(src.name)
		if err != nil {
			continue
		}
		rows = parsePCBList(raw, src.proto, rows)
	}
	return rows
}

// parsePCBList decodes one pcblist_n buffer: a leading xinpgen header, then
// per socket a run of (len, kind) records — xinpcb_n first, xsocket_n next,
// buffers/stats/tcpcb after — and a trailing xinpgen. Records are 8-byte
// aligned in the stream even though their len fields are not.
func parsePCBList(raw []byte, proto types.Protocol, rows []pcbRow) []pcbRow {
	le := binary.LittleEndian
	if len(raw) < xinpgenSize {
		return rows
	}
	off := int(le.Uint32(raw[0:4])) // xig_len
	if off < xinpgenSize {
		return rows
	}
	off = roundup8(off)

	var cur *pcbRow
	flush := func() {
		if cur != nil && cur.lastPID > 0 {
			rows = append(rows, *cur)
		}
		cur = nil
	}
	for off+8 <= len(raw) {
		l := int(le.Uint32(raw[off : off+4]))
		kind := le.Uint32(raw[off+4 : off+8])
		if l <= xinpgenSize || off+l > len(raw) {
			break // trailing xinpgen, or a truncated buffer
		}
		rec := raw[off : off+l]
		switch kind {
		case xsoInpcb:
			flush()
			if l < xiMinLen {
				break
			}
			r := pcbRow{
				proto: proto,
				fport: binary.BigEndian.Uint16(rec[xiFport : xiFport+2]),
				lport: binary.BigEndian.Uint16(rec[xiLport : xiLport+2]),
			}
			vflag := rec[xiVflag]
			r.faddr = inpAddr(rec[xiFaddr:xiFaddr+16], vflag)
			r.laddr = inpAddr(rec[xiLaddr:xiLaddr+16], vflag)
			cur = &r
		case xsoSocket:
			if cur != nil && l >= xsMinLen {
				cur.lastPID = int(int32(le.Uint32(rec[xsLastPID : xsLastPID+4])))
				cur.ePID = int(int32(le.Uint32(rec[xsEPID : xsEPID+4])))
				cur.dead = le.Uint16(rec[xsState:xsState+2])&ssIsDisconnected != 0
			}
		}
		off += roundup8(l)
	}
	flush()
	return rows
}

// inpAddr renders a 16-byte inpcb address union. An IPv4 socket keeps its
// address in the last four bytes (struct in_addr_4in6); the unspecified
// address renders as "" so callers can tell connected from bound sockets.
func inpAddr(b []byte, vflag byte) string {
	var ip net.IP
	switch {
	case vflag&inpIPv4 != 0:
		ip = net.IPv4(b[12], b[13], b[14], b[15]).To4()
	case vflag&inpIPv6 != 0:
		ip = net.IP(append([]byte(nil), b[:16]...))
	default:
		return ""
	}
	if ip.IsUnspecified() {
		return ""
	}
	return ip.String()
}

func roundup8(n int) int { return (n + 7) &^ 7 }

// mergePCBs appends, to the rows the fd walk produced, every pcb that walk
// could not see: a 4-tuple (or, for an unconnected socket, a local port) no
// fd-backed row already claims. fd rows keep precedence because they name
// the process that actually holds the socket; the pcb's pid is the kernel's
// idea of who touched it last. Among pcbs sharing a tuple — a transparent
// proxy such as a content blocker's network extension keeps a shadow pcb for
// every connection it diverts — a delegated one wins, and disconnected pcbs
// are ordered last so a lingering one never shadows a live socket that has
// reused its port.
func mergePCBs(conns []rawConn, pcbs []pcbRow) []rawConn {
	type tuple struct {
		proto        types.Protocol
		lport, fport uint16
		faddr        string
	}
	seen := make(map[tuple]bool, len(conns))
	for _, c := range conns {
		seen[tuple{c.Proto, c.LPort, c.RPort, c.RAddr}] = true
	}
	// Two passes: live pcbs first, then disconnected ones.
	pick := make(map[tuple]int, len(pcbs)) // tuple -> index into pcbs
	var order []tuple
	for pass := 0; pass < 2; pass++ {
		for i, p := range pcbs {
			if p.dead != (pass == 1) {
				continue
			}
			k := tuple{p.proto, p.lport, p.fport, p.faddr}
			if seen[k] {
				continue
			}
			if j, ok := pick[k]; ok {
				if pcbs[j].ePID == 0 && p.ePID > 0 {
					pick[k] = i
				}
				continue
			}
			pick[k] = i
			order = append(order, k)
		}
	}
	for _, k := range order {
		p := pcbs[pick[k]]
		conns = append(conns, rawConn{
			PID:   p.pid(),
			Proto: p.proto,
			LPort: p.lport,
			RAddr: p.faddr,
			RPort: p.fport,
		})
	}
	return conns
}
