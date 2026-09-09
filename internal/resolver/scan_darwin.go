//go:build darwin

package resolver

/*
#include <stdlib.h>
#include <string.h>
#include <libproc.h>
#include <sys/proc_info.h>
#include <sys/socket.h>
#include <netinet/in.h>
#include <arpa/inet.h>

// ns_conn_row is one connected (or bound) socket, flattened to the few fields
// netscope needs. Addresses are rendered to text in C so Go never has to mirror
// the fragile in_sockinfo struct layout.
typedef struct {
	int      pid;
	int      proto;   // IPPROTO_TCP or IPPROTO_UDP
	uint16_t lport;   // host byte order
	uint16_t fport;   // host byte order
	long     start;   // process start time, unix seconds (0 if unknown)
	char     faddr[46];
} ns_conn_row;

// ns_proc_path fills buf with the executable path for pid. Returns length, or
// <=0 on failure.
static int ns_proc_path(int pid, char *buf, int size) {
	return proc_pidpath(pid, buf, (uint32_t)size);
}

// ns_proc_name fills buf with the kernel's short name for pid (p_comm, what ps
// shows). Unlike the path it does not depend on the executable still existing
// on disk. Returns length, or <=0 on failure.
static int ns_proc_name(int pid, char *buf, int size) {
	return proc_name(pid, buf, (uint32_t)size);
}

// ns_proc_start returns the process start time in unix seconds, or 0.
static long ns_proc_start(int pid) {
	struct proc_bsdinfo bi;
	int r = proc_pidinfo(pid, PROC_PIDTBSDINFO, 0, &bi, sizeof(bi));
	if (r < (int)sizeof(bi)) return 0;
	return (long)bi.pbi_start_tvsec;
}

// render_addr writes the textual remote address of an in_sockinfo into out.
static void render_addr(struct in_sockinfo *ini, char *out, int outlen) {
	out[0] = '\0';
	if (ini->insi_vflag & INI_IPV4) {
		struct in_addr a;
		a.s_addr = ini->insi_faddr.ina_46.i46a_addr4.s_addr;
		inet_ntop(AF_INET, &a, out, outlen);
	} else if (ini->insi_vflag & INI_IPV6) {
		inet_ntop(AF_INET6, &ini->insi_faddr.ina_6, out, outlen);
	}
}

// ns_scan enumerates every process' socket fds and fills up to cap rows.
// Returns the number of rows written, or -1 on a fatal enumeration error.
static int ns_scan(ns_conn_row *rows, int cap) {
	int n = 0;

	int pidcap = proc_listpids(PROC_ALL_PIDS, 0, NULL, 0);
	if (pidcap <= 0) return -1;
	int *pids = (int *)malloc(pidcap);
	if (!pids) return -1;
	int pidbytes = proc_listpids(PROC_ALL_PIDS, 0, pids, pidcap);
	if (pidbytes <= 0) { free(pids); return -1; }
	int npids = pidbytes / (int)sizeof(int);

	for (int i = 0; i < npids && n < cap; i++) {
		int pid = pids[i];
		if (pid <= 0) continue;

		int fdbytes = proc_pidinfo(pid, PROC_PIDLISTFDS, 0, NULL, 0);
		if (fdbytes <= 0) continue;
		struct proc_fdinfo *fds = (struct proc_fdinfo *)malloc(fdbytes);
		if (!fds) continue;
		int got = proc_pidinfo(pid, PROC_PIDLISTFDS, 0, fds, fdbytes);
		if (got <= 0) { free(fds); continue; }
		int nfds = got / (int)sizeof(struct proc_fdinfo);

		long start = -1; // looked up lazily, once per pid that has a socket

		for (int j = 0; j < nfds && n < cap; j++) {
			if (fds[j].proc_fdtype != PROX_FDTYPE_SOCKET) continue;

			struct socket_fdinfo si;
			int r = proc_pidfdinfo(pid, fds[j].proc_fd,
			                       PROC_PIDFDSOCKETINFO, &si, sizeof(si));
			if (r < (int)sizeof(si)) continue;

			struct in_sockinfo *ini = NULL;
			int proto = 0;
			if (si.psi.soi_kind == SOCKINFO_TCP) {
				ini = &si.psi.soi_proto.pri_tcp.tcpsi_ini;
				proto = IPPROTO_TCP;
			} else if (si.psi.soi_kind == SOCKINFO_IN) {
				ini = &si.psi.soi_proto.pri_in;
				proto = IPPROTO_UDP;
			} else {
				continue;
			}

			if (start < 0) start = ns_proc_start(pid);

			rows[n].pid   = pid;
			rows[n].proto = proto;
			rows[n].lport = (uint16_t)ntohs((uint16_t)ini->insi_lport);
			rows[n].fport = (uint16_t)ntohs((uint16_t)ini->insi_fport);
			rows[n].start = start;
			render_addr(ini, rows[n].faddr, sizeof(rows[n].faddr));
			n++;
		}
		free(fds);
	}
	free(pids);
	return n;
}
*/
import "C"

import (
	"bytes"
	"encoding/binary"

	"golang.org/x/sys/unix"

	"github.com/doldoldol21/netscope/pkg/types"
)

// maxRows caps a single scan. Even busy desktops rarely exceed a few thousand
// sockets; the bound keeps the C buffer fixed-size and the scan bounded.
const maxRows = 16384

// scan enumerates current sockets via libproc and resolves executable paths,
// reusing the supplied cache only for a PID that was resolved before and is
// still the same process (same start time). A PID that failed to resolve is
// tried again every scan, and a PID handed to a new process after a wrap is
// not mistaken for the one that had it.
func scan(pathCache map[int]procEntry) ([]rawConn, map[int]procEntry) {
	rows := make([]C.ns_conn_row, maxRows)
	n := int(C.ns_scan(&rows[0], C.int(maxRows)))
	if n <= 0 {
		return nil, pathCache
	}

	newPaths := make(map[int]procEntry, len(pathCache))
	conns := make([]rawConn, 0, n)

	for i := 0; i < n; i++ {
		row := rows[i]
		pid := int(row.pid)
		start := int64(row.start)

		entry, ok := newPaths[pid]
		if !ok {
			if cached, hit := pathCache[pid]; hit && cached.reusable(start) {
				entry = cached
			} else {
				entry = procEntry{proc: resolveProcess(pid), start: start}
			}
			newPaths[pid] = entry
		}

		proto := types.ProtoTCP
		if int(row.proto) == C.IPPROTO_UDP {
			proto = types.ProtoUDP
		}

		conns = append(conns, rawConn{
			PID:   pid,
			Proto: proto,
			LPort: uint16(row.lport),
			RAddr: C.GoString(&row.faddr[0]),
			RPort: uint16(row.fport),
		})
	}
	return conns, newPaths
}

// resolveProcess names a PID. proc_pidpath is the first choice, but it fails
// once the executable behind a running process is gone from disk — which is
// what every in-place upgrade does to the copies already running (a Homebrew
// cask bump, an Electron app updating itself). Those processes used to become
// "unknown" for the rest of their lives. The path they were exec'd from is
// still in the kernel's argument area, and the short name is still in the proc
// table, so ask for those before giving up.
func resolveProcess(pid int) types.Process {
	path := pidPath(pid)
	if path == "" {
		path = execPathFromArgs(pid)
	}
	name := appName(path)
	if path == "" {
		if short := pidName(pid); short != "" {
			name = short
		}
	}
	return types.Process{PID: pid, Path: path, Name: name}
}

func pidPath(pid int) string {
	buf := make([]C.char, C.PROC_PIDPATHINFO_MAXSIZE)
	n := int(C.ns_proc_path(C.int(pid), &buf[0], C.int(len(buf))))
	if n <= 0 {
		return ""
	}
	return C.GoStringN(&buf[0], C.int(n))
}

func pidName(pid int) string {
	buf := make([]C.char, 2*C.MAXCOMLEN+1)
	n := int(C.ns_proc_name(C.int(pid), &buf[0], C.int(len(buf))))
	if n <= 0 {
		return ""
	}
	return C.GoStringN(&buf[0], C.int(n))
}

// execPathFromArgs reads the path a process was exec'd from out of
// KERN_PROCARGS2. The buffer starts with argc as a native int32, then the exec
// path NUL-terminated, then padding and the argv strings; only the path is
// wanted here. It outlives the file, which is the point.
func execPathFromArgs(pid int) string {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil || len(raw) <= 4 {
		return ""
	}
	_ = binary.LittleEndian.Uint32(raw[:4]) // argc; the path follows
	rest := raw[4:]
	end := bytes.IndexByte(rest, 0)
	if end <= 0 {
		return ""
	}
	return string(rest[:end])
}
