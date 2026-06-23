// Command netscope is the user-facing CLI client for the netscope daemon. It
// talks to netscoped's API over a unix socket to show live traffic or
// historical rankings in the terminal, or to launch the native app.
//
// Usage:
//
//	netscope                 live terminal view (default)
//	netscope top             live terminal view
//	netscope apps  [-range]  per-app ranking (today|week|hour|day)
//	netscope domains [-range] per-domain ranking
//	netscope open            launch the native netscope app
package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/doldoldol21/netscope/internal/ipc"
	"github.com/doldoldol21/netscope/pkg/types"
)

// client reaches the daemon over its unix socket; host in the URL is ignored.
var client *http.Client

const base = "http://netscoped"

func main() {
	// Pull the subcommand out of the args regardless of position so both
	// `netscope apps --range week` and `netscope --sock p apps` work (stdlib
	// flag otherwise stops at the first non-flag argument).
	cmd, args := splitCommand(os.Args[1:])

	fs := flag.NewFlagSet("netscope", flag.ExitOnError)
	sock := fs.String("sock", ipc.DefaultSocketPath(), "netscoped unix socket path")
	rng := fs.String("range", "today", "time range for rankings: hour|today|day|week")
	format := fs.String("format", "csv", "export format: csv|json")
	typ := fs.String("type", "apps", "export data: apps|domains")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "usage: netscope [top|apps|domains|export|open] [--sock path] [--range hour|today|day|week]\n"+
			"       netscope export [--type apps|domains] [--format csv|json] [--range ...] > out.csv\n")
	}
	_ = fs.Parse(args)

	client = ipc.Client(*sock)

	var err error
	switch cmd {
	case "top", "live":
		err = liveView(base)
	case "apps":
		err = ranking(base, "apps", *rng)
	case "domains":
		err = ranking(base, "domains", *rng)
	case "export":
		err = exportData(base, *typ, *rng, *format)
	case "open":
		err = openApp()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", cmd)
		fs.Usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "netscope: %v\n", err)
		os.Exit(1)
	}
}

// valueFlags are the flags that consume the following argument; used to skip
// flag values when hunting for the positional subcommand.
var valueFlags = map[string]bool{
	"-sock": true, "--sock": true, "-range": true, "--range": true,
	"-format": true, "--format": true, "-type": true, "--type": true,
}

// splitCommand separates the subcommand (first bare positional argument) from
// the remaining flag arguments, defaulting to "top".
func splitCommand(args []string) (string, []string) {
	cmd := "top"
	found := false
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			rest = append(rest, a)
			if !strings.Contains(a, "=") && valueFlags[a] && i+1 < len(args) {
				i++
				rest = append(rest, args[i])
			}
			continue
		}
		if !found {
			cmd, found = a, true
			continue
		}
		rest = append(rest, a)
	}
	return cmd, rest
}

// exportData writes per-app or per-domain totals for a range to stdout as CSV
// or JSON, for archiving or analysis elsewhere. Redirect to a file as needed.
func exportData(base, kind, rng, format string) error {
	if kind != "apps" && kind != "domains" {
		return fmt.Errorf("--type must be apps or domains, got %q", kind)
	}
	if format != "csv" && format != "json" {
		return fmt.Errorf("--format must be csv or json, got %q", format)
	}
	url := fmt.Sprintf("%s/api/%s?range=%s", base, kind, rng)

	if kind == "apps" {
		var apps []types.AppTraffic
		if err := getJSON(url, &apps); err != nil {
			return err
		}
		sort.Slice(apps, func(i, j int) bool { return apps[i].Total() > apps[j].Total() })
		if format == "json" {
			return writeJSON(apps)
		}
		w := csv.NewWriter(os.Stdout)
		defer w.Flush()
		_ = w.Write([]string{"app", "path", "rx_bytes", "tx_bytes", "total_bytes", "connections"})
		for _, a := range apps {
			_ = w.Write([]string{a.Name, a.Path,
				strconv.FormatUint(a.RxBytes, 10), strconv.FormatUint(a.TxBytes, 10),
				strconv.FormatUint(a.Total(), 10), strconv.Itoa(a.Connections)})
		}
		return w.Error()
	}

	var doms []types.DomainStat
	if err := getJSON(url, &doms); err != nil {
		return err
	}
	sort.Slice(doms, func(i, j int) bool { return doms[i].Total() > doms[j].Total() })
	if format == "json" {
		return writeJSON(doms)
	}
	w := csv.NewWriter(os.Stdout)
	defer w.Flush()
	_ = w.Write([]string{"domain", "app", "category", "country", "rx_bytes", "tx_bytes", "total_bytes"})
	for _, d := range doms {
		_ = w.Write([]string{d.Domain, d.AppName, d.Category, d.Country,
			strconv.FormatUint(d.RxBytes, 10), strconv.FormatUint(d.TxBytes, 10),
			strconv.FormatUint(d.Total(), 10)})
	}
	return w.Error()
}

// writeJSON pretty-prints v to stdout.
func writeJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func getJSON(url string, v any) error {
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("%w (is netscoped running?)", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// liveView polls the snapshot endpoint and redraws a terminal table each second
// until interrupted.
func liveView(base string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	draw := func() error {
		var s types.Snapshot
		if err := getJSON(base+"/api/snapshot", &s); err != nil {
			return err
		}
		render(s)
		return nil
	}
	if err := draw(); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			fmt.Print("\033[?25h") // restore cursor
			return nil
		case <-t.C:
			if err := draw(); err != nil {
				return err
			}
		}
	}
}

func render(s types.Snapshot) {
	fmt.Print("\033[2J\033[H\033[?25l") // clear, home, hide cursor
	fmt.Printf("  netscope  ·  %s  ·  %s\n", s.Interface, s.Time.Format("15:04:05"))
	fmt.Printf("  ↓ %s/s   ↑ %s/s   (total ↓%s ↑%s)\n\n",
		human(s.RxPerSec), human(s.TxPerSec), human(s.TotalRx), human(s.TotalTx))
	fmt.Printf("  %-26s %10s %10s  %s\n", "APP", "DOWN", "UP", "")
	fmt.Printf("  %s\n", line(60))
	if len(s.Apps) == 0 {
		fmt.Println("  (no traffic captured yet)")
	}
	for i, a := range s.Apps {
		if i >= 15 {
			break
		}
		fmt.Printf("  %-26s %10s %10s\n", trunc(a.Name, 26), human(a.RxBytes), human(a.TxBytes))
	}
	if len(s.Domains) > 0 {
		fmt.Printf("\n  %-26s %10s %10s\n", "DOMAIN", "DOWN", "UP")
		fmt.Printf("  %s\n", line(60))
		for i, d := range s.Domains {
			if i >= 8 {
				break
			}
			fmt.Printf("  %-26s %10s %10s%s\n", trunc(d.Domain, 26), human(d.RxBytes), human(d.TxBytes), catTag(d.Category))
		}
	}
	fmt.Print("\n  Ctrl-C to quit\n")
}

func ranking(base, kind, rng string) error {
	url := fmt.Sprintf("%s/api/%s?range=%s", base, kind, rng)
	if kind == "apps" {
		var apps []types.AppTraffic
		if err := getJSON(url, &apps); err != nil {
			return err
		}
		sort.Slice(apps, func(i, j int) bool { return apps[i].Total() > apps[j].Total() })
		fmt.Printf("Top apps (%s):\n", rng)
		fmt.Printf("%-30s %12s %12s\n", "APP", "DOWN", "UP")
		for _, a := range apps {
			fmt.Printf("%-30s %12s %12s\n", trunc(a.Name, 30), human(a.RxBytes), human(a.TxBytes))
		}
		return nil
	}
	var doms []types.DomainStat
	if err := getJSON(url, &doms); err != nil {
		return err
	}
	sort.Slice(doms, func(i, j int) bool { return doms[i].Total() > doms[j].Total() })
	fmt.Printf("Top domains (%s):\n", rng)
	fmt.Printf("%-34s %12s %12s\n", "DOMAIN", "DOWN", "UP")
	for _, d := range doms {
		fmt.Printf("%-34s %12s %12s%s\n", trunc(d.Domain, 34), human(d.RxBytes), human(d.TxBytes), catTag(d.Category))
	}
	return nil
}

// catTag renders a neutral category label (e.g. "  ·media") or "" when none.
func catTag(cat string) string {
	if cat == "" {
		return ""
	}
	return "  ·" + cat
}

// openApp launches the native netscope app. It prefers a freshly-built local
// bundle (so dev never launches a stale Launch Services registration), then an
// installed copy, then falls back to the registered bundle id.
func openApp() error {
	for _, p := range []string{
		"desktop/build/bin/netscope.app",
		"/Applications/netscope.app",
	} {
		if _, err := os.Stat(p); err == nil {
			return exec.Command("open", p).Run()
		}
	}
	if err := exec.Command("open", "-b", "io.netscope.app").Run(); err == nil {
		return nil
	}
	return fmt.Errorf("netscope.app not found — build it with `make app`")
}

func human(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func line(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = '-'
	}
	return string(b)
}
