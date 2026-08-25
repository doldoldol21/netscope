//go:build darwin

package main

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// %q leaves $ and backticks intact, both of which are live inside the double
// quotes the swap script used to interpolate into. Single quotes are inert.
func TestShQuoteNeutralizesShellMetacharacters(t *testing.T) {
	for _, p := range []string{
		"/Applications/net$(whoami).app",
		"/Applications/net`whoami`.app",
		`/Applications/net"quote".app`,
		"/Applications/net$HOME.app",
		"/Users/someone/O'Brien/netscope.app",
		"/Applications/net scope.app",
	} {
		// Round-trip through a real shell: whatever we quote must come back byte
		// for byte, with no expansion.
		out, err := exec.Command("/bin/bash", "-c", "printf %s "+shQuote(p)).Output()
		if err != nil {
			t.Fatalf("bash rejected %s: %v", shQuote(p), err)
		}
		if got := string(out); got != p {
			t.Errorf("shQuote(%q) round-tripped as %q", p, got)
		}
	}
}

func TestShQuoteWrapsInSingleQuotes(t *testing.T) {
	got := shQuote("/Applications/netscope.app")
	if !strings.HasPrefix(got, "'") || !strings.HasSuffix(got, "'") {
		t.Fatalf("shQuote = %s, want single-quoted", got)
	}
}

func TestNextRetryDelayBacksOffWithinBounds(t *testing.T) {
	if got := nextRetryDelay(1); got != updateRetryMin {
		t.Fatalf("first retry = %s, want %s", got, updateRetryMin)
	}
	if got := nextRetryDelay(2); got != 2*updateRetryMin {
		t.Fatalf("second retry = %s, want %s", got, 2*updateRetryMin)
	}
	if got := nextRetryDelay(50); got != updateRetryMax {
		t.Fatalf("retry after many failures = %s, want the %s cap", got, updateRetryMax)
	}
	// A nonsensical count must not produce a zero delay and spin the loop.
	if got := nextRetryDelay(0); got != updateRetryMin {
		t.Fatalf("zero failures = %s, want %s", got, updateRetryMin)
	}
}

// The retry cadence has to be quicker than the healthy interval, or a failure
// would be slower to recover than a success — the bug this replaces.
func TestRetryIsFasterThanTheHealthyInterval(t *testing.T) {
	if updateRetryMax >= updateCheckInterval {
		t.Fatalf("retry cap %s is not shorter than the interval %s", updateRetryMax, updateCheckInterval)
	}
}

func TestCheckDueRunsOnceOnLaunch(t *testing.T) {
	if !checkDue(time.Now(), time.Time{}, updateCheckInterval) {
		t.Fatal("a never-checked app did not consider a check due")
	}
}

func TestCheckDueWaitsOutTheInterval(t *testing.T) {
	now := time.Now()
	if checkDue(now, now.Add(-time.Minute), updateCheckInterval) {
		t.Fatal("checked a minute ago but a check was already due")
	}
	if !checkDue(now, now.Add(-updateCheckInterval), updateCheckInterval) {
		t.Fatal("a check exactly at the interval was not due")
	}
}

// A machine suspended past its due time should check promptly on wake, not
// wherever a long Sleep happens to expire.
func TestCheckDueFiresAfterASuspend(t *testing.T) {
	now := time.Now()
	if !checkDue(now, now.Add(-24*time.Hour), updateCheckInterval) {
		t.Fatal("a check overdue by a day was not due")
	}
}
