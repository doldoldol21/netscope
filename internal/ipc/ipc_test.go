package ipc

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultSocketPathEnv(t *testing.T) {
	t.Setenv("NETSCOPE_SOCK", "/tmp/custom.sock")
	if got := DefaultSocketPath(); got != "/tmp/custom.sock" {
		t.Fatalf("DefaultSocketPath = %q, want override", got)
	}
	t.Setenv("NETSCOPE_SOCK", "")
	if got := DefaultSocketPath(); got != "/var/run/netscope/netscoped.sock" {
		t.Fatalf("DefaultSocketPath = %q, want default", got)
	}
}

func serveOverSocket(t *testing.T, h http.Handler) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "test.sock")
	ln, err := Listen(sock)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return sock
}

func TestClientRoundTrip(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok"}`))
	})
	sock := serveOverSocket(t, mux)

	resp, err := Client(sock).Get("http://netscoped/api/health")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"status":"ok"}` {
		t.Fatalf("body = %q", body)
	}
}

func TestReverseProxyOverSocket(t *testing.T) {
	// Backend daemon listening on a unix socket.
	backend := http.NewServeMux()
	backend.HandleFunc("/api/ping", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("pong"))
	})
	sock := serveOverSocket(t, backend)

	// Front it with the same reverse proxy the Wails app uses.
	front := httptest.NewServer(NewReverseProxy(sock))
	t.Cleanup(front.Close)

	resp, err := http.Get(front.URL + "/api/ping")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "pong" {
		t.Fatalf("proxied body = %q, want pong", body)
	}
}

func TestListenSingleInstance(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "one.sock")
	ln, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	// A second daemon on the same socket path must refuse to start.
	if _, err := Listen(sock); err == nil {
		t.Fatal("second Listen on the same path must fail while the first instance holds the lock")
	}
}

func TestCloseLeavesSocketFileForSuccessor(t *testing.T) {
	// Short tempdir: t.TempDir() embeds the test name and overflows the 104-byte
	// sun_path limit for unix sockets on macOS.
	dir, err := os.MkdirTemp("", "ns")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "keep.sock")
	ln, lerr := Listen(sock)
	if lerr != nil {
		t.Fatal(lerr)
	}
	ln.Close()
	// Closing must NOT unlink the path: a superseded instance shutting down
	// would otherwise delete the socket a newer daemon re-bound at this path.
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("socket file must survive listener Close, got %v", err)
	}
}
