package main

import (
	"testing"
	"time"

	"github.com/doldoldol21/netscope/internal/dnscache"
)

// --no-revdns and --no-update-check exist so an operator can stop netscoped
// putting requests on the wire. Each is a single "build it or don't", which is
// exactly the kind of thing that silently stops working.

func TestHostHinterDisabledIsAGenuineNil(t *testing.T) {
	// The engine skips a nil hinter with `cfg.Hinter != nil`. Returning a nil
	// *revdns.Resolver instead of a nil interface would pass that check and get
	// called, so this asserts the interface itself is nil.
	if h := hostHinter(false, dnscache.New(time.Hour, 10)); h != nil {
		t.Errorf("disabled hinter = %#v, want a nil interface", h)
	}
}

func TestHostHinterEnabledResolves(t *testing.T) {
	if h := hostHinter(true, dnscache.New(time.Hour, 10)); h == nil {
		t.Error("enabled hinter is nil, so no PTR lookups would ever run")
	}
}

func TestUpdateCheckerDisabledIsNil(t *testing.T) {
	if c := updateChecker(false); c != nil {
		t.Error("disabled update checker is non-nil, so it would still poll GitHub")
	}
}

func TestUpdateCheckerEnabledExists(t *testing.T) {
	if c := updateChecker(true); c == nil {
		t.Error("enabled update checker is nil, so /api/version would never report one")
	}
}
