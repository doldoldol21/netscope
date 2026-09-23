package api

import (
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/doldoldol21/netscope/internal/helperinstall"
)

type fakeInstaller struct {
	err error
	got helperinstall.Request
}

func (f *fakeInstaller) Install(req helperinstall.Request) error {
	f.got = req
	return f.err
}

func TestHelperRefreshIsUnavailableWithoutAnInstaller(t *testing.T) {
	s := &Server{RestartFunc: func() {}}
	rec := do(s.Handler(), http.MethodPost, "/api/helper/refresh", map[string]string{"path": "/x", "version": "v9.9.9"})
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rec.Code)
	}
	s = &Server{HelperInstaller: &fakeInstaller{}}
	rec = do(s.Handler(), http.MethodPost, "/api/helper/refresh", map[string]string{"path": "/x", "version": "v9.9.9"})
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("no RestartFunc: status = %d, want 501", rec.Code)
	}
}

func TestHelperRefreshInstallsThenRestarts(t *testing.T) {
	var restarted atomic.Int32
	inst := &fakeInstaller{}
	s := &Server{HelperInstaller: inst, RestartFunc: func() { restarted.Add(1) }}
	rec := do(s.Handler(), http.MethodPost, "/api/helper/refresh",
		map[string]string{"path": "/Applications/netscope.app/Contents/MacOS/netscoped", "version": "v0.28.0"})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body %q, want 204", rec.Code, rec.Body.String())
	}
	if inst.got.Path != "/Applications/netscope.app/Contents/MacOS/netscoped" || inst.got.Version != "v0.28.0" {
		t.Fatalf("installer got %+v", inst.got)
	}
	// The restart is deferred past the response so the caller sees the 204.
	if restarted.Load() != 0 {
		t.Fatal("restarted before the response was sent")
	}
	deadline := time.Now().Add(2 * time.Second)
	for restarted.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if restarted.Load() != 1 {
		t.Fatalf("restarted %d times, want 1", restarted.Load())
	}
}

func TestHelperRefreshMapsInstallerErrorsAndNeverRestarts(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code int
	}{
		{helperinstall.ErrRejected, http.StatusUnprocessableEntity},
		{errors.Join(helperinstall.ErrRejected, errors.New("checksum mismatch")), http.StatusUnprocessableEntity},
		{helperinstall.ErrNotRoot, http.StatusNotImplemented},
		{helperinstall.ErrNotManaged, http.StatusNotImplemented},
		{errors.New("fetch checksums.txt: offline"), http.StatusBadGateway},
	} {
		var restarted atomic.Int32
		s := &Server{HelperInstaller: &fakeInstaller{err: tc.err}, RestartFunc: func() { restarted.Add(1) }}
		rec := do(s.Handler(), http.MethodPost, "/api/helper/refresh", map[string]string{"path": "/x", "version": "v9.9.9"})
		if rec.Code != tc.code {
			t.Errorf("%v: status = %d, want %d", tc.err, rec.Code, tc.code)
		}
		time.Sleep(300 * time.Millisecond)
		if restarted.Load() != 0 {
			t.Errorf("%v: restarted on a failed install", tc.err)
		}
	}
}

func TestHelperRefreshRejectsBadRequests(t *testing.T) {
	s := &Server{HelperInstaller: &fakeInstaller{}, RestartFunc: func() {}}
	if rec := do(s.Handler(), http.MethodGet, "/api/helper/refresh", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: status = %d, want 405", rec.Code)
	}
	req := do(s.Handler(), http.MethodPost, "/api/helper/refresh", nil)
	if req.Code != http.StatusBadRequest || !strings.Contains(req.Body.String(), "bad request") {
		t.Errorf("empty body: status = %d body %q, want 400", req.Code, req.Body.String())
	}
}
