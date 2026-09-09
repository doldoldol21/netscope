package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSplitCommandFindsTheSubcommandAnywhere(t *testing.T) {
	cases := []struct {
		args []string
		cmd  string
		rest []string
	}{
		{nil, "top", []string{}},
		{[]string{"apps"}, "apps", []string{}},
		{[]string{"apps", "--range", "week"}, "apps", []string{"--range", "week"}},
		{[]string{"--sock", "/tmp/s", "apps"}, "apps", []string{"--sock", "/tmp/s"}},
		// A value flag's argument must not be mistaken for the subcommand,
		// even when it looks like one.
		{[]string{"--range", "apps", "domains"}, "domains", []string{"--range", "apps"}},
		// --flag=value carries its value with it; the next word is positional.
		{[]string{"--range=week", "apps"}, "apps", []string{"--range=week"}},
		// Boolean flags consume nothing.
		{[]string{"--version", "apps"}, "apps", []string{"--version"}},
		{[]string{"-v"}, "top", []string{"-v"}},
		// Extra positionals are passed through, not swallowed.
		{[]string{"export", "extra"}, "export", []string{"extra"}},
		// A value flag at the very end has nothing to consume.
		{[]string{"apps", "--range"}, "apps", []string{"--range"}},
	}
	for _, c := range cases {
		cmd, rest := splitCommand(c.args)
		if cmd != c.cmd || !reflect.DeepEqual(rest, c.rest) {
			t.Errorf("splitCommand(%q) = %q, %q; want %q, %q", c.args, cmd, rest, c.cmd, c.rest)
		}
	}
}

func TestHuman(t *testing.T) {
	cases := map[uint64]string{
		0:             "0 B",
		1023:          "1023 B",
		1024:          "1.0 KB",
		1536:          "1.5 KB",
		1 << 20:       "1.0 MB",
		5 * (1 << 30): "5.0 GB",
		1 << 40:       "1.0 TB",
		1<<40 + 1<<39: "1.5 TB",
		1 << 50:       "1.0 PB",
		1 << 60:       "1.0 EB",
		^uint64(0):    "16.0 EB",
	}
	for n, want := range cases {
		if got := human(n); got != want {
			t.Errorf("human(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestTruncKeepsShortStringsAndMarksCuts(t *testing.T) {
	if got := trunc("Safari", 26); got != "Safari" {
		t.Errorf("trunc(short) = %q", got)
	}
	if got := trunc("abcdef", 6); got != "abcdef" {
		t.Errorf("trunc(exact) = %q", got)
	}
	got := trunc("Google Chrome Helper (Renderer)", 10)
	if r := []rune(got); len(r) != 10 || !strings.HasSuffix(got, "…") {
		t.Errorf("trunc(long, 10) = %q (%d runes)", got, len(r))
	}
	// Multi-byte names count in runes, not bytes: the width is columns.
	got = trunc("카카오톡 메신저 앱", 5)
	if r := []rune(got); len(r) != 5 {
		t.Errorf("trunc(hangul, 5) = %q (%d runes)", got, len(r))
	}
}

// A width that cannot hold the ellipsis used to slice past the start of the
// string and panic. Nothing calls it that way today; nothing should be able
// to bring the CLI down if it does.
func TestTruncSurvivesAWidthTooSmallForTheEllipsis(t *testing.T) {
	for _, n := range []int{1, 0, -1} {
		if got := trunc("abc", n); got != "" {
			t.Errorf("trunc(%q, %d) = %q, want \"\"", "abc", n, got)
		}
	}
}

// openAppWith records what it would have launched.
func recordingOpen(t *testing.T, fail bool) (func(args ...string) error, *[][]string) {
	t.Helper()
	var calls [][]string
	return func(args ...string) error {
		calls = append(calls, args)
		if fail {
			return errors.New("open failed")
		}
		return nil
	}, &calls
}

func TestOpenAppUsesTheOverrideOnlyWhenSet(t *testing.T) {
	dev := filepath.Join(t.TempDir(), "netscope.app")
	if err := os.MkdirAll(dev, 0o755); err != nil {
		t.Fatal(err)
	}
	open, calls := recordingOpen(t, false)
	if err := openAppWith(dev, open); err != nil {
		t.Fatalf("openAppWith(override) = %v", err)
	}
	if len(*calls) != 1 || (*calls)[0][0] != dev {
		t.Fatalf("launched %v, want the override %s", *calls, dev)
	}
}

func TestOpenAppRefusesAMissingOverride(t *testing.T) {
	open, calls := recordingOpen(t, false)
	err := openAppWith(filepath.Join(t.TempDir(), "nope.app"), open)
	if err == nil {
		t.Fatal("a NETSCOPE_APP that doesn't exist was accepted")
	}
	if len(*calls) != 0 {
		t.Fatalf("launched %v despite the override being missing", *calls)
	}
}

// The regression: without an override, nothing relative to the working
// directory may be consulted. Run from a directory that contains the old
// implicit dev path and make sure it is never what gets launched.
func TestOpenAppIgnoresABundleUnderTheCurrentDirectory(t *testing.T) {
	dir := t.TempDir()
	planted := filepath.Join(dir, "desktop", "build", "bin", "netscope.app")
	if err := os.MkdirAll(planted, 0o755); err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	open, calls := recordingOpen(t, true) // every launch "fails": we only care what was tried
	_ = openAppWith("", open)
	if len(*calls) == 0 {
		t.Fatal("nothing was tried at all")
	}
	for _, c := range *calls {
		if c[0] == "-b" {
			continue // a bundle id for Launch Services, not a path
		}
		for _, a := range c {
			if !filepath.IsAbs(a) {
				t.Fatalf("launched a relative path %q from the working directory", a)
			}
		}
	}
}
