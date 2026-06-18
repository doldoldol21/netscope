package dnscache

import (
	"testing"
	"time"
)

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
