package resolver

import "testing"

func TestAppName(t *testing.T) {
	cases := map[string]string{
		"/Applications/Safari.app/Contents/MacOS/Safari":               "Safari",
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome": "Google Chrome",
		"/usr/bin/curl":          "curl",
		"/opt/homebrew/bin/node": "node",
		"":                       "unknown",
		"/Applications/Foo.app/Contents/MacOS/foo-helper": "Foo",
	}
	for path, want := range cases {
		if got := appName(path); got != want {
			t.Errorf("appName(%q) = %q, want %q", path, got, want)
		}
	}
}
