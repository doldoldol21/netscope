package revdns

import (
	"context"
	"testing"
	"time"

	"github.com/doldoldol21/netscope/internal/dnscache"
)

func TestResolvable(t *testing.T) {
	cases := map[string]bool{
		"8.8.8.8":     true,
		"1.1.1.1":     true,
		"192.168.1.5": false, // private
		"127.0.0.1":   false, // loopback
		"169.254.0.1": false, // link-local
		"::1":         false,
		"not-an-ip":   false,
	}
	for ip, want := range cases {
		if got := resolvable(ip); got != want {
			t.Errorf("resolvable(%q)=%v want %v", ip, got, want)
		}
	}
}

func TestEnqueueResolvesIntoCache(t *testing.T) {
	cache := dnscache.New(time.Hour, 100)
	r := &Resolver{
		cache:   cache,
		queue:   make(chan string, 16),
		timeout: time.Second,
		pending: make(map[string]time.Time),
		ttl:     time.Minute,
		nowFn:   time.Now,
		lookupFn: func(ctx context.Context, ip string) ([]string, error) {
			return []string{"edge-42.example.NET."}, nil // trailing dot + mixed case
		},
	}
	go r.worker()

	r.Enqueue("203.0.113.9")

	deadline := time.After(2 * time.Second)
	for {
		if h := cache.Lookup("203.0.113.9"); h != "" {
			if h != "edge-42.example.net" {
				t.Fatalf("cached host = %q, want normalised edge-42.example.net", h)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("PTR result never reached the cache")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestEnqueueSkipsPrivateAndDedups(t *testing.T) {
	var calls int
	r := &Resolver{
		cache:   dnscache.New(time.Hour, 100),
		queue:   make(chan string, 16),
		timeout: time.Second,
		pending: make(map[string]time.Time),
		ttl:     time.Minute,
		nowFn:   time.Now,
		lookupFn: func(ctx context.Context, ip string) ([]string, error) {
			calls++
			return nil, context.DeadlineExceeded
		},
	}
	r.Enqueue("10.0.0.1") // private -> skipped, never queued
	if len(r.queue) != 0 {
		t.Fatalf("private IP should not be queued")
	}
	r.Enqueue("8.8.8.8")
	r.Enqueue("8.8.8.8") // dup within ttl -> only one queued
	if got := len(r.queue); got != 1 {
		t.Fatalf("queued %d, want 1 (deduped)", got)
	}
}
