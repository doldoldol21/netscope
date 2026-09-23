package daemonctl

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/doldoldol21/netscope/internal/buildinfo"
	"github.com/doldoldol21/netscope/internal/helperinstall"
)

// ErrRefreshUnavailable means the running daemon cannot install a successor
// itself — it predates the endpoint, is not root, or is not under launchd —
// and the admin-prompt install is the only way forward.
var ErrRefreshUnavailable = errors.New("daemon cannot refresh the helper itself")

// refreshViaDaemon asks the running daemon to install the bundled binary as
// its own successor. No prompt: the daemon is already root, and it vouches
// for the file against the release's checksums.txt rather than trusting us.
// A rejection (the daemon could not verify the file, or it is not newer) is
// an error too; callers decide whether the admin prompt is worth trying next.
func refreshViaDaemon(client *http.Client, bundled, version string) error {
	body, _ := json.Marshal(helperinstall.Request{Path: bundled, Version: version})
	resp, err := client.Post("http://netscoped/api/helper/refresh", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil
	case http.StatusNotFound, http.StatusNotImplemented, http.StatusMethodNotAllowed:
		return fmt.Errorf("%w: %s", ErrRefreshUnavailable, bytes.TrimSpace(msg))
	default:
		return fmt.Errorf("daemon refused the helper refresh (%s): %s", resp.Status, bytes.TrimSpace(msg))
	}
}

// waitForDaemon polls until the daemon answers again after a restart, or
// gives up after ~9s.
func waitForDaemon(client *http.Client) error {
	for i := 0; i < 30; i++ {
		if IsRunning(client) {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("daemon did not come back after refreshing the helper")
}

// AutoRefreshHelper is what a freshly launched app does when the installed
// helper differs from the daemon it ships: try the prompt-free path once, and
// say nothing if it does not work. It never opens an admin dialog — that stays
// behind the popover banner, at the user's request — so a dev build next to a
// release, or a daemon too old to have the endpoint, costs nothing but a log
// line. Returns true when the helper was refreshed.
func AutoRefreshHelper(client *http.Client, sock string) bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	bundled := bundledNetscoped(exe)
	if bundled == "" {
		return false
	}
	reason, err := helperInstallReason(bundled, helperPath)
	if err != nil || reason == "" {
		return false
	}
	if err := refreshViaDaemon(client, bundled, buildinfo.Version); err != nil {
		log.Printf("daemonctl: %s; prompt-free refresh not taken: %v", reason, err)
		return false
	}
	log.Printf("daemonctl: %s; the daemon installed %s itself", reason, buildinfo.Version)
	if err := waitForDaemon(client); err != nil {
		log.Printf("daemonctl: %v", err)
		return false
	}
	return true
}
