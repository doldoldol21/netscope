//go:build darwin

package main

import (
	"strings"
	"testing"
)

func TestCaptureStateModes(t *testing.T) {
	for _, c := range []struct {
		name string
		st   captureState
		want readoutMode
	}{
		{"capturing and busy", captureState{rx: 1000, tx: 20, capturing: true}, readoutRates},
		// The case this exists for: connected, capturing, genuinely idle. Zeros
		// here are the truth and must stay zeros.
		{"capturing and idle", captureState{capturing: true}, readoutRates},
		{"between sources", captureState{capturing: false}, readoutStopped},
		{"paused", captureState{paused: true, capturing: true}, readoutPaused},
		// Paused wins: a paused source is also not capturing, but "you stopped
		// this" is the more useful thing to say.
		{"paused and stopped", captureState{paused: true, capturing: false}, readoutPaused},
	} {
		if got := c.st.mode(); got != c.want {
			t.Errorf("%s: mode = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestModeMarkerOnlyReplacesNonCapturingStates(t *testing.T) {
	if _, ok := modeMarker(readoutRates); ok {
		t.Fatal("capturing state got a marker instead of its rates")
	}
	for _, m := range []readoutMode{readoutPaused, readoutStopped} {
		s, ok := modeMarker(m)
		if !ok || s == "" {
			t.Fatalf("mode %v produced no marker", m)
		}
	}
	p, _ := modeMarker(readoutPaused)
	st, _ := modeMarker(readoutStopped)
	if p == st {
		t.Fatal("paused and stopped render identically, so they can't be told apart")
	}
}

func TestDecodeCaptureReadsTheState(t *testing.T) {
	got, ok := decodeCapture(strings.NewReader(
		`{"rxPerSec":2048,"txPerSec":512,"paused":true,"capturing":false}`))
	if !ok {
		t.Fatal("decode failed")
	}
	if got.rx != 2048 || got.tx != 512 || !got.paused || got.capturing {
		t.Fatalf("decoded %+v", got)
	}
}

// The app is routinely newer than the root-owned daemon copy, which is refreshed
// on demand. A daemon that doesn't know about `capturing` must not be read as
// "capture has stopped" — that would pin the menu bar to the stopped marker.
func TestDecodeCaptureTreatsAMissingFieldAsCapturing(t *testing.T) {
	got, ok := decodeCapture(strings.NewReader(`{"rxPerSec":10,"txPerSec":5,"paused":false}`))
	if !ok {
		t.Fatal("decode failed")
	}
	if !got.capturing {
		t.Fatal("an older daemon's snapshot was read as not capturing")
	}
	if got.mode() != readoutRates {
		t.Fatalf("mode = %v, want the rates to be shown", got.mode())
	}
}

// An explicit false is the daemon actually telling us capture stopped, and must
// not be confused with the field being absent.
func TestDecodeCaptureHonoursAnExplicitFalse(t *testing.T) {
	got, ok := decodeCapture(strings.NewReader(`{"rxPerSec":0,"txPerSec":0,"capturing":false}`))
	if !ok {
		t.Fatal("decode failed")
	}
	if got.mode() != readoutStopped {
		t.Fatalf("mode = %v, want readoutStopped", got.mode())
	}
}

func TestDecodeCaptureRejectsGarbage(t *testing.T) {
	if _, ok := decodeCapture(strings.NewReader("not json")); ok {
		t.Fatal("garbage was accepted as a snapshot")
	}
}
