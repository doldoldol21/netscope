package helperinstall

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// fixture is an installer pointed at a temp dir, with a fake GitHub that
// serves one checksums.txt for one tag.
type fixture struct {
	in        *Installer
	helper    string
	candidate string
	newBytes  []byte
	fetched   []string
}

func newFixture(t *testing.T, current, tag string) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{
		helper:    filepath.Join(dir, "io.netscope.daemon"),
		candidate: filepath.Join(dir, "bundle", "netscoped"),
		newBytes:  []byte("new daemon " + tag),
	}
	if err := os.WriteFile(f.helper, []byte("old daemon"), 0o755); err != nil {
		t.Fatal(err)
	}
	plist := filepath.Join(dir, "io.netscope.daemon.plist")
	if err := os.WriteFile(plist, []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(f.candidate), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.candidate, f.newBytes, 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(f.newBytes)
	sums := fmt.Sprintf("%s  netscope-%s-app.zip\n%s  netscoped\n", hex.EncodeToString(sum[:]), tag, hex.EncodeToString(sum[:]))
	f.in = &Installer{
		Repo:       "owner/repo",
		Current:    current,
		HelperPath: f.helper,
		PlistPath:  plist,
		EUID:       func() int { return 0 },
		Fetch: func(url string, max int64) ([]byte, error) {
			f.fetched = append(f.fetched, url)
			want := "https://github.com/owner/repo/releases/download/" + tag + "/checksums.txt"
			if url != want {
				return nil, fmt.Errorf("no such release: %s", url)
			}
			return []byte(sums), nil
		},
	}
	return f
}

func (f *fixture) helperBytes(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(f.helper)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestInstallReplacesTheHelperWithThePublishedBinary(t *testing.T) {
	f := newFixture(t, "v0.27.0", "v0.28.0")
	if err := f.in.Install(Request{Path: f.candidate, Version: "v0.28.0"}); err != nil {
		t.Fatal(err)
	}
	if got := f.helperBytes(t); got != string(f.newBytes) {
		t.Fatalf("helper = %q, want the candidate's bytes", got)
	}
	st, _ := os.Stat(f.helper)
	if st.Mode().Perm() != 0o755 {
		t.Errorf("helper mode = %o, want 0755", st.Mode().Perm())
	}
	if len(f.fetched) != 1 {
		t.Errorf("fetched %v, want exactly the tag's checksums.txt", f.fetched)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(f.helper), "*.new-*")); len(left) != 0 {
		t.Errorf("temp files left behind: %v", left)
	}
}

func TestInstallRefusesBytesTheReleaseDidNotPublish(t *testing.T) {
	f := newFixture(t, "v0.27.0", "v0.28.0")
	// A local attacker drops their own binary into the (user-writable) bundle.
	if err := os.WriteFile(f.candidate, []byte("evil"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := f.in.Install(Request{Path: f.candidate, Version: "v0.28.0"})
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("err = %v, want ErrRejected", err)
	}
	if got := f.helperBytes(t); got != "old daemon" {
		t.Fatalf("helper changed to %q on a rejected candidate", got)
	}
}

func TestInstallVersionGateNeedsNoNetwork(t *testing.T) {
	for _, tc := range []struct{ current, claimed string }{
		{"v0.27.0", "v0.27.0"},   // same
		{"v0.27.0", "v0.26.9"},   // downgrade
		{"dev", "v0.28.0"},       // dev daemon
		{"v0.27.0", "dev"},       // dev app
		{"v0.27.0", "0.28.0"},    // not a tag
		{"v0.27.0", "v0.28"},     // partial
		{"v0.27.0", "v0.28.0/x"}, // url games
	} {
		f := newFixture(t, tc.current, "v0.28.0")
		err := f.in.Install(Request{Path: f.candidate, Version: tc.claimed})
		if !errors.Is(err, ErrRejected) {
			t.Errorf("current %s claimed %s: err = %v, want ErrRejected", tc.current, tc.claimed, err)
		}
		if len(f.fetched) != 0 {
			t.Errorf("current %s claimed %s: fetched %v before the version gate", tc.current, tc.claimed, f.fetched)
		}
		if f.helperBytes(t) != "old daemon" {
			t.Errorf("current %s claimed %s: helper changed", tc.current, tc.claimed)
		}
	}
}

func TestInstallRefusesWhenItCannotVouch(t *testing.T) {
	t.Run("fetch fails", func(t *testing.T) {
		f := newFixture(t, "v0.27.0", "v0.28.0")
		f.in.Fetch = func(string, int64) ([]byte, error) { return nil, errors.New("offline") }
		if err := f.in.Install(Request{Path: f.candidate, Version: "v0.28.0"}); err == nil || errors.Is(err, ErrRejected) {
			t.Fatalf("err = %v, want a fetch error (not a rejection: the candidate may be fine)", err)
		}
		if f.helperBytes(t) != "old daemon" {
			t.Fatal("helper changed")
		}
	})
	t.Run("release lists no netscoped", func(t *testing.T) {
		f := newFixture(t, "v0.27.0", "v0.28.0")
		f.in.Fetch = func(string, int64) ([]byte, error) { return []byte("abc  netscope-v0.28.0-app.zip\n"), nil }
		if err := f.in.Install(Request{Path: f.candidate, Version: "v0.28.0"}); !errors.Is(err, ErrRejected) {
			t.Fatalf("err = %v, want ErrRejected", err)
		}
	})
	t.Run("candidate missing", func(t *testing.T) {
		f := newFixture(t, "v0.27.0", "v0.28.0")
		if err := f.in.Install(Request{Path: f.candidate + ".nope", Version: "v0.28.0"}); !errors.Is(err, ErrRejected) {
			t.Fatalf("err = %v, want ErrRejected", err)
		}
	})
	t.Run("relative path", func(t *testing.T) {
		f := newFixture(t, "v0.27.0", "v0.28.0")
		if err := f.in.Install(Request{Path: "bundle/netscoped", Version: "v0.28.0"}); !errors.Is(err, ErrRejected) {
			t.Fatalf("err = %v, want ErrRejected", err)
		}
	})
}

func TestInstallNeedsRootAndALaunchdService(t *testing.T) {
	f := newFixture(t, "v0.27.0", "v0.28.0")
	f.in.EUID = func() int { return 501 }
	if err := f.in.Install(Request{Path: f.candidate, Version: "v0.28.0"}); !errors.Is(err, ErrNotRoot) {
		t.Fatalf("err = %v, want ErrNotRoot", err)
	}
	f = newFixture(t, "v0.27.0", "v0.28.0")
	os.Remove(f.in.PlistPath)
	if err := f.in.Install(Request{Path: f.candidate, Version: "v0.28.0"}); !errors.Is(err, ErrNotManaged) {
		t.Fatalf("no plist: err = %v, want ErrNotManaged", err)
	}
	// An install that predates the root-owned copy is a migration, not a
	// refresh: the plist must be rewritten too, which only the privileged
	// install does. Refusing keeps it from ending up half done.
	f = newFixture(t, "v0.27.0", "v0.28.0")
	os.Remove(f.helper)
	if err := f.in.Install(Request{Path: f.candidate, Version: "v0.28.0"}); !errors.Is(err, ErrNotManaged) {
		t.Fatalf("no helper copy: err = %v, want ErrNotManaged", err)
	}
	if _, err := os.Stat(f.helper); err == nil {
		t.Fatal("created a helper copy without a migrated plist")
	}
}
