//go:build darwin

package main

import (
	"math"
	"testing"
)

func TestSettingsHeightNeverShrinksBelowTheMainViewOrRunsAway(t *testing.T) {
	cases := []struct {
		req  float64
		want int
	}{
		{0, popoverHeight},                  // closing: back to the main view
		{-5, popoverHeight},                 // nonsense
		{math.NaN(), popoverHeight},         // nonsense that fails every comparison
		{popoverHeight - 40, popoverHeight}, // content shorter than the main view
		{490, 490},
		{490.6, 490},
		{1e9, 2000},
	}
	for _, c := range cases {
		if got := settingsHeight(c.req); got != c.want {
			t.Errorf("settingsHeight(%v) = %d, want %d", c.req, got, c.want)
		}
	}
}
