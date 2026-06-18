package demo

import (
	"context"
	"testing"
	"time"

	"github.com/doldoldol21/netscope/internal/dnscache"
	"github.com/doldoldol21/netscope/pkg/types"
)

func TestResolverMapsKnownPorts(t *testing.T) {
	r := NewResolver()
	// Every synthetic connection's local port must resolve to its app.
	for _, c := range conns {
		p, ok := r.Lookup(types.ConnKey{Proto: types.ProtoTCP, LocalPort: c.lport})
		if !ok || p.Name != c.app {
			t.Fatalf("Lookup(port %d) = (%+v, %v), want app %q", c.lport, p, ok, c.app)
		}
	}
	if _, ok := r.Lookup(types.ConnKey{LocalPort: 1}); ok {
		t.Error("unknown port should not resolve")
	}
}

func TestSeedDNS(t *testing.T) {
	c := dnscache.New(time.Hour, 1000)
	SeedDNS(c)
	if got := c.Lookup("160.79.104.10"); got != "api.anthropic.com" {
		t.Fatalf("seeded DNS = %q, want api.anthropic.com", got)
	}
}

func TestSourceEmitsFlows(t *testing.T) {
	s := NewSource(42)
	s.tick = 5 * time.Millisecond // speed up for the test

	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan types.Flow, 256)
	go func() { _ = s.Run(ctx, out) }()

	deadline := time.After(2 * time.Second)
	var got types.Flow
	select {
	case got = <-out:
	case <-deadline:
		cancel()
		t.Fatal("demo source produced no flows")
	}
	cancel()

	if got.Bytes == 0 || got.LocalPort == 0 {
		t.Errorf("flow looks empty: %+v", got)
	}
	if got.Direction != types.DirIn && got.Direction != types.DirOut {
		t.Errorf("flow direction unset: %+v", got)
	}
}
