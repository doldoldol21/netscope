package main

import (
	"testing"
	"time"
)

// --live-window 0 has always meant "the whole session"; the engine now reads
// zero as its default bound, so the flag has to translate.
func TestLiveWindowZeroMeansTheWholeSession(t *testing.T) {
	if got := sessionHorizon(0); got >= 0 {
		t.Fatalf("sessionHorizon(0) = %v, want a negative (never prune)", got)
	}
	if got := sessionHorizon(30 * time.Minute); got != 30*time.Minute {
		t.Fatalf("sessionHorizon(30m) = %v", got)
	}
}
