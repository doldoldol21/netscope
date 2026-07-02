//go:build linux

package resolver

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/doldoldol21/netscope/pkg/types"
)

// scan enumerates every process's TCP/UDP sockets via /proc and returns them as
// rawConn rows plus a PID→Process cache for path/name resolution.
func scan(pathCache map[int]types.Process) ([]rawConn, map[int]types.Process) {
	// 1. Collect all active socket inodes from /proc/net/{tcp,tcp6,udp,udp6}.
	inodes := collectSocketInodes()

	// 2. Build a reverse map: inode → PID by scanning /proc/*/fd/*.
	pidByInode := buildInodePIDMap(inodes)

	// 3. Walk /proc to gather PID→name/path for each discovered PID.
	procMap := make(map[int]types.Process)
	for _, pid := range pidByInode {
		if _, ok := procMap[pid]; ok {
			continue
		}
		if cached, ok := pathCache[pid]; ok {
			procMap[pid] = cached
			continue
		}
		name, path := procInfo(pid)
		procMap[pid] = types.Process{PID: pid, Name: name, Path: path}
	}

	// 4. Build rawConn rows from the parsed inodes.
	var rows []rawConn
	for _, s := range inodes {
		pid, ok := pidByInode[s.inode]
		if !ok {
			continue
		}
		rows = append(rows, rawConn{
			PID:   pid,
			Proto: s.proto,
			LPort: s.localPort,
			RAddr: s.remoteAddr,
			RPort: s.remotePort,
		})
	}
	return rows, procMap
}

type sockEntry struct {
	inode      uint64
	proto      types.Protocol
	localAddr  string
	localPort  uint16
	remoteAddr string
	remotePort uint16
}

// collectSocketInodes reads /proc/net/{tcp,tcp6,udp,udp6} and returns a flat
// list of active sockets with their inode numbers.
func collectSocketInodes() []sockEntry {
	var out []sockEntry
	parse := func(path string, proto types.Protocol) {
		f, err := os.Open(path)
		if err != nil {
			return
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		first := true
		for sc.Scan() {
			if first {
				first = false
				continue // skip header
			}
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			entry := parseProcNetLine(line, proto)
			if entry != nil {
				out = append(out, *entry)
			}
		}
	}
	parse("/proc/net/tcp", types.ProtoTCP)
	parse("/proc/net/tcp6", types.ProtoTCP)
	parse("/proc/net/udp", types.ProtoUDP)
	parse("/proc/net/udp6", types.ProtoUDP)
	return out
}

// parseProcNetLine parses one line from /proc/net/tcp (or udp). Format:
//
//	sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
//	 0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000   501        0 12345 1 0000000000000000 100 0 0 10 0
func parseProcNetLine(line string, proto types.Protocol) *sockEntry {
	fields := strings.Fields(line)
	if len(fields) < 10 {
		return nil
	}

	local := strings.SplitN(fields[1], ":", 2)
	remote := strings.SplitN(fields[2], ":", 2)
	if len(local) != 2 || len(remote) != 2 {
		return nil
	}

	localIP := parseHexIP(local[0])
	localPort := parseHexPort(local[1])
	remoteIP := parseHexIP(remote[0])
	remotePort := parseHexPort(remote[1])

	if remoteIP == "" {
		return nil // skip unconnected sockets
	}

	inode, err := strconv.ParseUint(fields[9], 10, 64)
	if err != nil {
		return nil
	}

	return &sockEntry{
		inode:      inode,
		proto:      proto,
		localAddr:  localIP,
		localPort:  localPort,
		remoteAddr: remoteIP,
		remotePort: remotePort,
	}
}

// parseHexIP converts a little-endian hex IP string (e.g. "0100007F") to a
// dotted quad (e.g. "127.0.0.1") or IPv6 colon-hex string.
func parseHexIP(s string) string {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) == 0 {
		return ""
	}
	if len(b) == 4 {
		// IPv4 — little-endian in /proc/net.
		return net.IP{b[3], b[2], b[1], b[0]}.String()
	}
	if len(b) == 16 {
		// IPv6 — network byte order in /proc/net.
		return net.IP(b).String()
	}
	return ""
}

func parseHexPort(s string) uint16 {
	v, err := strconv.ParseUint(s, 16, 16)
	if err != nil {
		return 0
	}
	return uint16(v)
}

// buildInodePIDMap returns a map from socket inode → PID by scanning every
// process's /proc/<pid>/fd/* for socket:[inode] symlinks.
func buildInodePIDMap(inodes []sockEntry) map[uint64]int {
	want := make(map[uint64]bool, len(inodes))
	for _, s := range inodes {
		want[s.inode] = true
	}
	if len(want) == 0 {
		return nil
	}

	out := make(map[uint64]int)

	entries, err := os.ReadDir("/proc")
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		fdDir := filepath.Join("/proc", e.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil {
				continue
			}
			// Link looks like: socket:[12345]
			const prefix = "socket:["
			if !strings.HasPrefix(link, prefix) {
				continue
			}
			inodeStr := link[len(prefix) : len(link)-1]
			inode, err := strconv.ParseUint(inodeStr, 10, 64)
			if err != nil {
				continue
			}
			if want[inode] {
				out[inode] = pid
			}
		}
	}
	return out
}

// procInfo returns the process name (from /proc/<pid>/comm) and executable path
// (from /proc/<pid>/exe) for the given PID.
func procInfo(pid int) (name, path string) {
	name = fmt.Sprintf("pid-%d", pid)
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid)); err == nil {
		name = strings.TrimSpace(string(b))
	}
	if link, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); err == nil {
		path = link
	}
	return
}
