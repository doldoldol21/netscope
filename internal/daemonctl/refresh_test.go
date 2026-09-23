package daemonctl

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/doldoldol21/netscope/internal/helperinstall"
)

// daemonStub stands in for netscoped's unix socket: an httptest server plus a
// client that sends every request there whatever host the URL names.
func daemonStub(t *testing.T, h http.HandlerFunc) (*http.Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u)}}, srv
}

func TestRefreshViaDaemonSendsPathAndVersion(t *testing.T) {
	var got helperinstall.Request
	client, _ := daemonStub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/helper/refresh" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusNoContent)
	})
	if err := refreshViaDaemon(client, "/Applications/netscope.app/Contents/MacOS/netscoped", "v0.28.0"); err != nil {
		t.Fatal(err)
	}
	if got.Path != "/Applications/netscope.app/Contents/MacOS/netscoped" || got.Version != "v0.28.0" {
		t.Fatalf("daemon received %+v", got)
	}
}

func TestRefreshViaDaemonTellsUnavailableFromRefused(t *testing.T) {
	for _, tc := range []struct {
		code        int
		unavailable bool
	}{
		{http.StatusNotFound, true},       // daemon predates the endpoint
		{http.StatusNotImplemented, true}, // not root / not under launchd
		{http.StatusUnprocessableEntity, false},
		{http.StatusBadGateway, false},
		{http.StatusInternalServerError, false},
	} {
		client, _ := daemonStub(t, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", tc.code)
		})
		err := refreshViaDaemon(client, "/x", "v0.28.0")
		if err == nil {
			t.Errorf("%d: no error", tc.code)
			continue
		}
		if errors.Is(err, ErrRefreshUnavailable) != tc.unavailable {
			t.Errorf("%d: err = %v, unavailable = %v, want %v", tc.code, err, !tc.unavailable, tc.unavailable)
		}
	}
}

func TestAutoRefreshHelperDoesNothingForADevBuild(t *testing.T) {
	// The test binary has no bundled netscoped beside it, so there is nothing
	// to offer the daemon; in particular no request is made and no prompt
	// could ever open.
	called := false
	client, _ := daemonStub(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})
	if AutoRefreshHelper(client, "/tmp/none.sock") {
		t.Fatal("reported a refresh with nothing to install")
	}
	if called {
		t.Fatal("contacted the daemon without a bundled daemon to offer")
	}
}
