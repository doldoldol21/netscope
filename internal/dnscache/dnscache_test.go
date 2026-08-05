package dnscache

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dnscache.json")
	c := New(time.Hour, 100)
	c.Put("1.2.3.4", "example.com")
	c.Put("5.6.7.8", "api.anthropic.com")
	if err := c.SaveTo(path); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	c2 := New(time.Hour, 100)
	if err := c2.LoadFrom(path); err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if got := c2.Lookup("1.2.3.4"); got != "example.com" {
		t.Fatalf("after load Lookup = %q, want example.com", got)
	}
	if got := c2.Lookup("5.6.7.8"); got != "api.anthropic.com" {
		t.Fatalf("after load Lookup = %q, want api.anthropic.com", got)
	}
}

func TestLoadDropsExpired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dnscache.json")
	now := time.Unix(10000, 0)
	c := New(time.Minute, 100)
	c.nowFn = func() time.Time { return now }
	c.Put("1.2.3.4", "fresh.example")
	c.SaveTo(path)

	// Load 2 minutes later: the entry is older than the 1-minute TTL → dropped.
	c2 := New(time.Minute, 100)
	c2.nowFn = func() time.Time { return now.Add(2 * time.Minute) }
	if err := c2.LoadFrom(path); err != nil {
		t.Fatal(err)
	}
	if got := c2.Lookup("1.2.3.4"); got != "" {
		t.Fatalf("expired entry should not load, got %q", got)
	}
}

func TestLoadMissingFileOK(t *testing.T) {
	c := New(time.Hour, 100)
	if err := c.LoadFrom(filepath.Join(t.TempDir(), "nope.json")); err != nil {
		t.Fatalf("missing file should be fine, got %v", err)
	}
}

func TestPutLookup(t *testing.T) {
	c := New(time.Hour, 100)
	c.Put("1.2.3.4", "example.com")
	if got := c.Lookup("1.2.3.4"); got != "example.com" {
		t.Fatalf("Lookup = %q, want example.com", got)
	}
	if got := c.Lookup("9.9.9.9"); got != "" {
		t.Fatalf("Lookup unknown = %q, want empty", got)
	}
}

func TestPutIgnoresEmpty(t *testing.T) {
	c := New(time.Hour, 100)
	c.Put("", "example.com")
	c.Put("1.2.3.4", "")
	if c.Len() != 0 {
		t.Fatalf("Len = %d, want 0", c.Len())
	}
}

func TestTTLExpiry(t *testing.T) {
	now := time.Unix(1000, 0)
	c := New(time.Minute, 100)
	c.nowFn = func() time.Time { return now }
	c.Put("1.2.3.4", "example.com")

	now = now.Add(30 * time.Second)
	if got := c.Lookup("1.2.3.4"); got != "example.com" {
		t.Fatalf("within TTL Lookup = %q, want example.com", got)
	}
	now = now.Add(2 * time.Minute)
	if got := c.Lookup("1.2.3.4"); got != "" {
		t.Fatalf("expired Lookup = %q, want empty", got)
	}
}

func TestEviction(t *testing.T) {
	now := time.Unix(0, 0)
	c := New(time.Hour, 2)
	c.nowFn = func() time.Time { return now }
	c.Put("1.1.1.1", "a")
	now = now.Add(time.Second)
	c.Put("2.2.2.2", "b")
	now = now.Add(time.Second)
	c.Put("3.3.3.3", "c") // evicts oldest (1.1.1.1)
	if c.Len() != 2 {
		t.Fatalf("Len = %d, want 2", c.Len())
	}
	if c.Lookup("1.1.1.1") != "" {
		t.Fatalf("oldest entry should have been evicted")
	}
	if c.Lookup("3.3.3.3") != "c" {
		t.Fatalf("newest entry missing")
	}
}

// The persisted cache lists every host this machine resolved, so it must not be
// readable by other local accounts.
func TestSaveToIsOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dnscache.json")
	c := New(time.Hour, 10)
	c.Put("1.2.3.4", "example.com")
	if err := c.SaveTo(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := fi.Mode().Perm(); mode != 0o600 {
		t.Errorf("cache mode = %#o, want 0600", mode)
	}
}

// An interrupted save — or one from a version that wrote 0644 — leaves a .tmp
// behind. WriteFile only applies its mode when it creates the file, so that
// leftover would otherwise carry the old mode through the rename.
func TestSaveToReplacesALeftoverTempFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dnscache.json")
	if err := os.WriteFile(path+".tmp", []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path+".tmp", 0o644); err != nil { // defeat the caller's umask
		t.Fatal(err)
	}
	c := New(time.Hour, 10)
	c.Put("1.2.3.4", "example.com")
	if err := c.SaveTo(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := fi.Mode().Perm(); mode != 0o600 {
		t.Errorf("cache mode after a stale temp file = %#o, want 0600", mode)
	}
}

// If the leftover cannot be removed, writing into it would publish the cache at
// whatever mode that file already had — so the save has to fail instead.
func TestSaveToStopsWhenAStaleTempSurvives(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permissions do not prevent removal")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "dnscache.json")
	if err := os.WriteFile(path+".tmp", []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Removing a file needs write permission on its directory; writing to an
	// already-open file does not. That is exactly the dangerous combination.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	c := New(time.Hour, 10)
	c.Put("1.2.3.4", "example.com")
	if err := c.SaveTo(path); err == nil {
		t.Fatal("save reported success despite an unremovable temp file")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("a cache file was published anyway")
	}
}
