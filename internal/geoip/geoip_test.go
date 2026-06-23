package geoip

import (
	"net"
	"testing"
)

func TestLookup(t *testing.T) {
	// Stable anchor: Google public DNS is US in DB-IP.
	if got := Lookup(net.ParseIP("8.8.8.8")); got != "US" {
		t.Errorf("Lookup(8.8.8.8) = %q, want US", got)
	}
	// Public IPs (v4 + v6) resolve to some 2-letter country code.
	for _, ip := range []string{"1.1.1.1", "2001:4860:4860::8888"} {
		if got := Lookup(net.ParseIP(ip)); len(got) != 2 {
			t.Errorf("Lookup(%s) = %q, want a 2-letter code", ip, got)
		}
	}
	// Private / loopback are not public countries -> "".
	for _, ip := range []string{"127.0.0.1", "10.0.0.1", "192.168.1.1", "::1"} {
		if got := Lookup(net.ParseIP(ip)); got != "" {
			t.Errorf("Lookup(%s) = %q, want empty", ip, got)
		}
	}
}

func TestLookupNil(t *testing.T) {
	if Lookup(nil) != "" {
		t.Fatal("nil IP should be empty")
	}
}
