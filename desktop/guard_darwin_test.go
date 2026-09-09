//go:build darwin

package main

import "testing"

func TestGuardTurnsAPanicIntoASkippedTick(t *testing.T) {
	ran := 0
	for i := 0; i < 3; i++ {
		guard("test loop", func() {
			ran++
			if i == 1 {
				var m map[string]int
				m["boom"] = 1 // the kind of slip a poll loop makes
			}
		})
	}
	if ran != 3 {
		t.Fatalf("loop ran %d ticks after a panic, want all 3", ran)
	}
}

func TestGuardRunsTheStep(t *testing.T) {
	called := false
	guard("test", func() { called = true })
	if !called {
		t.Fatal("guard did not run its step")
	}
}
