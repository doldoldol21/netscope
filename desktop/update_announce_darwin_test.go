//go:build darwin

package main

import (
	"path/filepath"
	"testing"

	"github.com/doldoldol21/netscope/internal/update"
)

// withCleanUpdateState isolates the package-level prefs a test mutates, and
// points the prefs file at a temp dir so nothing touches the real one.
func withCleanUpdateState(t *testing.T) *[]string {
	t.Helper()
	oldPrefs, oldPath, oldNotify := updPrefs, updPrefPath, postNotification
	t.Cleanup(func() {
		updPrefs, updPrefPath, postNotification = oldPrefs, oldPath, oldNotify
	})
	updPrefs = updatePrefs{AutoCheck: true}
	updPrefPath = filepath.Join(t.TempDir(), "updates.json")
	var sent []string
	postNotification = func(title, body string) { sent = append(sent, title) }
	return &sent
}

// The whole point: you get told once about a version you didn't know about.
func TestAnnounceUpdateNotifiesOncePerVersion(t *testing.T) {
	sent := withCleanUpdateState(t)
	st := update.Status{Current: "v0.22.1", Latest: "v0.23.0", UpdateAvailable: true}

	announceUpdate(st)
	if len(*sent) != 1 {
		t.Fatalf("first sighting sent %d notifications, want 1", len(*sent))
	}
	// Every subsequent check finds the same release; none of them may re-announce.
	for i := 0; i < 5; i++ {
		announceUpdate(st)
	}
	if len(*sent) != 1 {
		t.Fatalf("repeat checks sent %d notifications, want 1", len(*sent))
	}
}

// A newer release than the one already announced is news again.
func TestAnnounceUpdateNotifiesForEachNewVersion(t *testing.T) {
	sent := withCleanUpdateState(t)
	announceUpdate(update.Status{Current: "v0.22.1", Latest: "v0.23.0", UpdateAvailable: true})
	announceUpdate(update.Status{Current: "v0.22.1", Latest: "v0.24.0", UpdateAvailable: true})
	if len(*sent) != 2 {
		t.Fatalf("two distinct releases sent %d notifications, want 2", len(*sent))
	}
}

// The decision is persisted, so relaunching doesn't re-announce what the user
// has already been told.
func TestAnnounceUpdateRemembersAcrossRestarts(t *testing.T) {
	sent := withCleanUpdateState(t)
	st := update.Status{Current: "v0.22.1", Latest: "v0.23.0", UpdateAvailable: true}
	announceUpdate(st)

	// Simulate a restart: forget everything in memory, reload from disk.
	updPrefs = updatePrefs{AutoCheck: true}
	loadUpdatePrefs()
	announceUpdate(st)

	if len(*sent) != 1 {
		t.Fatalf("a restart re-announced the same version (%d notifications)", len(*sent))
	}
}

func TestAnnounceUpdateStaysQuietWhenUpToDate(t *testing.T) {
	sent := withCleanUpdateState(t)
	announceUpdate(update.Status{Current: "v0.23.0", Latest: "v0.23.0", UpdateAvailable: false})
	// A check that failed leaves Latest empty; that is not news either.
	announceUpdate(update.Status{Current: "v0.23.0", UpdateAvailable: true})
	if len(*sent) != 0 {
		t.Fatalf("sent %d notifications with nothing to report", len(*sent))
	}
}

// A manual "check now" shows the answer on screen, which counts as being told.
// Recording it stops the background loop from firing an OS banner hours later
// about a release the user already read about.
func TestNoteUpdateSeenSuppressesALaterAnnouncement(t *testing.T) {
	sent := withCleanUpdateState(t)
	st := update.Status{Current: "v0.22.1", Latest: "v0.23.0", UpdateAvailable: true}

	if !noteUpdateSeen(st) {
		t.Fatal("the first sighting was not treated as news")
	}
	if len(*sent) != 0 {
		t.Fatalf("noteUpdateSeen posted %d notifications; it must be silent", len(*sent))
	}
	announceUpdate(st)
	if len(*sent) != 0 {
		t.Fatalf("announced a release the user had already been shown (%d)", len(*sent))
	}
}

func TestNoteUpdateSeenReportsNothingToSee(t *testing.T) {
	withCleanUpdateState(t)
	if noteUpdateSeen(update.Status{Current: "v0.23.0", Latest: "v0.23.0"}) {
		t.Fatal("being up to date was treated as news")
	}
	if noteUpdateSeen(update.Status{Current: "v0.23.0", UpdateAvailable: true}) {
		t.Fatal("a failed check with no version was treated as news")
	}
}
