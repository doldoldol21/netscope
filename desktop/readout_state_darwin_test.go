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

// The state this whole line of work is about: capture is running on the right
// interface and the daemon reports the link is carrying traffic none of which
// is reaching the handle. Showing a zero here is the lie.
func TestCaptureStateFlagsALinkItCannotSee(t *testing.T) {
	st := captureState{capturing: true, linkUnseen: true}
	if st.mode() != readoutBehind {
		t.Fatalf("mode = %v, want readoutBehind", st.mode())
	}
}

// The verdict is the daemon's, formed over one window where both sides are
// sampled together. This process only has a one-second capture rate, so it must
// not second-guess: a burst that ended a moment ago reads as zero here while
// being perfectly captured, which is exactly the false alarm re-deriving would
// produce.
func TestCaptureStateDoesNotSecondGuessTheVerdict(t *testing.T) {
	// Zero captured rate, no verdict: an ordinary quiet moment, not a fault.
	if got := (captureState{capturing: true, linkUnseen: false}).mode(); got != readoutRates {
		t.Fatalf("a quiet moment = %v, want readoutRates", got)
	}
	// Traffic captured *and* a standing verdict: still a link we aren't seeing.
	if got := (captureState{rx: 5000, capturing: true, linkUnseen: true}).mode(); got != readoutBehind {
		t.Fatalf("partial capture with a standing verdict = %v, want readoutBehind", got)
	}
}

// Paused and stopped both outrank it: those explain the zero already, and
// "capture is missing traffic" would be misleading when capture is off.
func TestCaptureStateRanksExplanationsAboveTheWarning(t *testing.T) {
	if got := (captureState{paused: true, capturing: true, linkUnseen: true}).mode(); got != readoutPaused {
		t.Fatalf("paused with a standing verdict = %v, want readoutPaused", got)
	}
	if got := (captureState{capturing: false, linkUnseen: true}).mode(); got != readoutStopped {
		t.Fatalf("stopped with a standing verdict = %v, want readoutStopped", got)
	}
}

func TestModeMarkerForBehindIsDistinct(t *testing.T) {
	b, ok := modeMarker(readoutBehind)
	if !ok || b == "" {
		t.Fatal("behind produced no marker")
	}
	st, _ := modeMarker(readoutStopped)
	p, _ := modeMarker(readoutPaused)
	if b == st || b == p {
		t.Fatal("behind renders like another state, so they can't be told apart")
	}
}

func TestDecodeCaptureReadsTheVerdict(t *testing.T) {
	got, ok := decodeCapture(strings.NewReader(
		`{"rxPerSec":0,"txPerSec":0,"capturing":true,"linkUnseen":true}`))
	if !ok {
		t.Fatal("decode failed")
	}
	if !got.linkUnseen || got.mode() != readoutBehind {
		t.Fatalf("decoded %+v, mode %v", got, got.mode())
	}
}

// An older daemon sends no verdict at all, which must read as "no fault known"
// and never as a warning.
func TestDecodeCaptureTreatsAMissingVerdictAsFine(t *testing.T) {
	got, ok := decodeCapture(strings.NewReader(`{"rxPerSec":0,"txPerSec":0,"capturing":true}`))
	if !ok {
		t.Fatal("decode failed")
	}
	if got.mode() != readoutRates {
		t.Fatalf("mode = %v, want readoutRates", got.mode())
	}
}
