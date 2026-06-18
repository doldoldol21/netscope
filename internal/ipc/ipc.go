// Package ipc centralises the local IPC between the privileged netscoped daemon
// and its unprivileged clients (the Wails app and the CLI). Communication is
// plain HTTP carried over a Unix domain socket — so there is no open TCP port a
// browser or another user's process could reach; access is gated by the socket
// file's ownership and permissions instead.
package ipc

import (
	"context"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// DefaultSocketPath is where the daemon listens and clients connect. It is a
// fixed absolute path (not derived from $HOME) so the root daemon and the
// user's app/CLI agree on it regardless of who runs them. Override with the
// NETSCOPE_SOCK environment variable (handy for un-privileged offline dev).
func DefaultSocketPath() string {
	if v := os.Getenv("NETSCOPE_SOCK"); v != "" {
		return v
	}
	return "/var/run/netscope/netscoped.sock"
}

// dialContext returns a DialContext bound to a unix socket path, ignoring the
// network/address the HTTP stack would otherwise use.
func dialContext(sock string) func(context.Context, string, string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 3 * time.Second}
	return func(ctx context.Context, _, _ string) (net.Conn, error) {
		return d.DialContext(ctx, "unix", sock)
	}
}

// Client returns an HTTP client that reaches the daemon over the unix socket.
// Use any URL host (e.g. http://netscoped/api/health); only the path matters.
func Client(sock string) *http.Client {
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{DialContext: dialContext(sock)},
	}
}

// NewReverseProxy returns a reverse proxy that forwards requests to the daemon
// over the unix socket, streaming responses immediately (so SSE works). The
// Wails asset server mounts this as its fallback handler for /api/*.
func NewReverseProxy(sock string) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Director: func(r *http.Request) {
			r.URL.Scheme = "http"
			r.URL.Host = "netscoped"
		},
		Transport:     &http.Transport{DialContext: dialContext(sock)},
		FlushInterval: -1,
	}
}

// Listen creates the unix socket listener for the daemon. It ensures the parent
// directory exists, removes a stale socket, and (when running as root) hands
// ownership of the socket to the target user with 0600 perms so only that user
// and root can connect.
func Listen(sock string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		return nil, err
	}
	// A leftover socket from an unclean shutdown would make Listen fail.
	if _, err := os.Stat(sock); err == nil {
		_ = os.Remove(sock)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	secureSocket(sock)
	return ln, nil
}

// secureSocket restricts the socket to the target user. When the daemon runs as
// root (sudo or launchd), the GUI app runs as a normal user and must still be
// able to connect, so we chown the socket to that user and chmod 0600.
func secureSocket(sock string) {
	if os.Geteuid() == 0 {
		if uid, gid, ok := targetUser(); ok {
			_ = os.Chown(sock, uid, gid)
		}
	}
	_ = os.Chmod(sock, 0o600)
}

// targetUser determines which user should own the socket: the sudo invoker if
// present, otherwise the logged-in console user (owner of /dev/console).
func targetUser() (uid, gid int, ok bool) {
	if s := os.Getenv("SUDO_UID"); s != "" {
		u, err1 := strconv.Atoi(s)
		g, err2 := strconv.Atoi(os.Getenv("SUDO_GID"))
		if err1 == nil && err2 == nil {
			return u, g, true
		}
	}
	if fi, err := os.Stat("/dev/console"); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			return int(st.Uid), int(st.Gid), true
		}
	}
	return 0, 0, false
}
