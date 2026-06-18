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
	char     faddr[46];
} ns_conn_row;

// ns_proc_path fills buf with the executable path for pid. Returns length, or
// <=0 on failure.
static int ns_proc_path(int pid, char *buf, int size) {
	return proc_pidpath(pid, buf, (uint32_t)size);
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

			rows[n].pid   = pid;
			rows[n].proto = proto;
			rows[n].lport = (uint16_t)ntohs((uint16_t)ini->insi_lport);
			rows[n].fport = (uint16_t)ntohs((uint16_t)ini->insi_fport);
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

import "github.com/doldoldol21/netscope/pkg/types"

// maxRows caps a single scan. Even busy desktops rarely exceed a few thousand
// sockets; the bound keeps the C buffer fixed-size and the scan bounded.
const maxRows = 16384

// scan enumerates current sockets via libproc and resolves executable paths,
// reusing the supplied path cache to avoid redundant proc_pidpath calls.
func scan(pathCache map[int]types.Process) ([]rawConn, map[int]types.Process) {
	rows := make([]C.ns_conn_row, maxRows)
	n := int(C.ns_scan(&rows[0], C.int(maxRows)))
	if n <= 0 {
		return nil, pathCache
	}

	newPaths := make(map[int]types.Process, len(pathCache))
	conns := make([]rawConn, 0, n)

	for i := 0; i < n; i++ {
		row := rows[i]
		pid := int(row.pid)

		proc, ok := newPaths[pid]
		if !ok {
			if cached, hit := pathCache[pid]; hit {
				proc = cached
			} else {
				proc = resolveProcess(pid)
			}
			newPaths[pid] = proc
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

func resolveProcess(pid int) types.Process {
	buf := make([]C.char, C.PROC_PIDPATHINFO_MAXSIZE)
	n := int(C.ns_proc_path(C.int(pid), &buf[0], C.int(len(buf))))
	path := ""
	if n > 0 {
		path = C.GoStringN(&buf[0], C.int(n))
	}
	return types.Process{
		PID:  pid,
		Path: path,
		Name: appName(path),
	}
}
