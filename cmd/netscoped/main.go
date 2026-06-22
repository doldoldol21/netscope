// Command netscoped is netscope's capture daemon. It sniffs packets (libpcap),
// attributes them to processes (libproc) and domains (DNS), aggregates the
// traffic into SQLite and serves the dashboard + JSON API over local HTTP.
//
// Live capture needs access to /dev/bpf*, i.e. root. For development you can
// replay a capture file with --pcap, which needs no privileges.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/doldoldol21/netscope/internal/api"
	"github.com/doldoldol21/netscope/internal/buildinfo"
	"github.com/doldoldol21/netscope/internal/capture"
	"github.com/doldoldol21/netscope/internal/demo"
	"github.com/doldoldol21/netscope/internal/dnscache"
	"github.com/doldoldol21/netscope/internal/engine"
	"github.com/doldoldol21/netscope/internal/ipc"
	"github.com/doldoldol21/netscope/internal/resolver"
	"github.com/doldoldol21/netscope/internal/revdns"
	"github.com/doldoldol21/netscope/internal/storage"
	"github.com/doldoldol21/netscope/internal/update"
	"github.com/doldoldol21/netscope/pkg/types"
)

func main() {
	var (
		iface     = flag.String("iface", "", "network interface to capture (default: auto-detect)")
		pcapFile  = flag.String("pcap", "", "replay a pcap file instead of live capture (no root needed)")
		demoMode  = flag.Bool("demo", false, "serve synthetic named-app traffic (no root, for UI/dev)")
		sock      = flag.String("sock", ipc.DefaultSocketPath(), "unix socket path to serve the API on")
		dbPath    = flag.String("db", defaultDBPath(), "SQLite database path")
		noStore   = flag.Bool("no-store", false, "run in memory only, no persistence")
		bucket    = flag.Duration("bucket", 10*time.Second, "aggregation/flush granularity")
		retention = flag.Duration("retention", 30*24*time.Hour, "how long to keep samples (0 = forever)")
		maxDB     = flag.Int64("max-db", 256<<20, "hard cap on database size in bytes; oldest data is dropped to stay under it (0 = no cap)")
		liveWin   = flag.Duration("live-window", 30*time.Minute, "live view keeps apps/domains active within this window (0 = whole session)")
		printTop  = flag.Bool("print", false, "also print the top apps to stdout every few seconds")
	)
	flag.Parse()

	if err := run(*iface, *pcapFile, *demoMode, *sock, *dbPath, *noStore, *bucket, *retention, *maxDB, *liveWin, *printTop); err != nil {
		log.Fatalf("netscoped: %v", err)
	}
}

// flowSource is the common interface for the live, offline and demo sources.
type flowSource interface {
	Run(ctx context.Context, out chan<- types.Flow) error
	Name() string
}

func run(iface, pcapFile string, demoMode bool, sock, dbPath string, noStore bool, bucket, retention time.Duration, maxDB int64, liveWin time.Duration, printTop bool) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 20k IP→host entries is ample for a personal machine and bounds the
	// daemon's idle footprint (entries also expire after the TTL).
	dns := dnscache.New(time.Hour, 20000)
	rev := revdns.New(dns, 4) // reverse-DNS fallback for unresolved IPs
	res := resolver.New(300 * time.Millisecond)

	var store *storage.Store
	if !noStore {
		if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
			return fmt.Errorf("create db dir: %w", err)
		}
		s, err := storage.Open(dbPath)
		if err != nil {
			return err
		}
		defer s.Close()
		store = s
		log.Printf("storage: %s", dbPath)
	}

	// Pick the flow source: synthetic demo, offline replay, or live capture.
	// The demo source needs no root and resolves real app names (its own
	// resolver), unlike pcap replay where apps show as "unknown".
	var src flowSource
	var eres engine.Resolver = res
	var err error
	switch {
	case demoMode:
		demo.SeedDNS(dns)
		eres = demo.NewResolver()
		src = demo.NewSource(time.Now().UnixNano())
	case pcapFile != "":
		src, err = capture.OpenOffline(pcapFile, capture.LocalIPs(), dns)
	default:
		// A supervised live source: tracks the default-route interface and
		// re-opens capture across network changes (Wi-Fi↔Ethernet, VPN, unplug).
		src = capture.NewLiveSupervisor(iface, dns)
	}
	if err != nil {
		return err
	}
	log.Printf("source: %s", src.Name())

	eng := engine.New(engine.Config{
		Bucket:         bucket,
		Retention:      retention,
		MaxDBBytes:     maxDB,
		SessionHorizon: liveWin,
		Interface:      src.Name(),
		SelfPID:        os.Getpid(),
		Hinter:         rev,
	}, eres, dns, store)

	flows := make(chan types.Flow, 4096)

	var wg sync.WaitGroup
	srcErr := make(chan error, 1)

	// Capture -> flows.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(flows)
		srcErr <- src.Run(ctx, flows)
	}()

	// Engine consumes flows.
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = eng.Run(ctx, flows)
	}()

	if printTop {
		wg.Add(1)
		go func() { defer wg.Done(); printer(ctx, eng) }()
	}

	// Background update checker (GitHub Releases); surfaced via /api/version.
	updater := update.NewChecker(buildinfo.Repo, buildinfo.Version, 6*time.Hour)
	wg.Add(1)
	go func() { defer wg.Done(); updater.Run(ctx) }()

	// API over a unix domain socket — no open TCP port. The native app and the
	// CLI connect here; there is no browser-facing dashboard.
	ln, err := ipc.Listen(sock)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", sock, err)
	}
	srv := &http.Server{Handler: api.NewServer(eng, store, updater).Handler()}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	log.Printf("api: unix://%s", sock)
	serveErr := srv.Serve(ln)
	if serveErr != nil && serveErr != http.ErrServerClosed {
		stop()
		wg.Wait()
		return fmt.Errorf("api server: %w", serveErr)
	}

	wg.Wait()
	// Surface a capture error (e.g. permission denied) so the user sees why.
	if e := <-srcErr; e != nil && e != context.Canceled {
		return e
	}
	return nil
}

// printer logs the current top apps periodically — useful when running the
// daemon directly in a terminal under sudo (M1 PoC validation).
func printer(ctx context.Context, eng *engine.Engine) {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s := eng.Snapshot()
			fmt.Printf("\n== %s  ↓%s/s ↑%s/s ==\n", s.Time.Format("15:04:05"),
				humanBytes(s.RxPerSec), humanBytes(s.TxPerSec))
			for i, a := range s.Apps {
				if i >= 8 {
					break
				}
				fmt.Printf("  %-22s ↓%9s ↑%9s\n", truncate(a.Name, 22),
					humanBytes(a.RxBytes), humanBytes(a.TxBytes))
			}
		}
	}
}

func defaultDBPath() string {
	// As root (the normal case, under launchd) there is no $HOME, so
	// os.UserConfigDir fails — use a fixed system path. Never return a relative
	// path: the daemon's CWD under launchd is "/", which isn't writable.
	if os.Geteuid() == 0 {
		return "/var/db/netscope/netscope.db"
	}
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "netscope", "netscope.db")
	}
	return filepath.Join(os.TempDir(), "netscope.db")
}

func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
