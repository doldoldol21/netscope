package update

import (
	"strings"
	"testing"
)

func TestAllowedHost(t *testing.T) {
	for host, want := range map[string]bool{
		"github.com":                      true,
		"GitHub.com":                      true,
		"api.github.com":                  true,
		"objects.githubusercontent.com":   true,
		"example.com":                     false,
		"github.com.example.com":          false,
		"githubusercontent.com":           false,
		"notgithubusercontent.com":        false,
		"objects.githubusercontent.com.x": false,
	} {
		if got := AllowedHost(host); got != want {
			t.Errorf("AllowedHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestFetchRefusesUntrustedURLsWithoutDialing(t *testing.T) {
	for _, u := range []string{
		"http://github.com/x",               // plaintext
		"https://example.com/checksums.txt", // wrong host
		"ftp://github.com/x",
	} {
		_, err := Fetch(u, 1024)
		if err == nil || !strings.Contains(err.Error(), "untrusted") {
			t.Errorf("Fetch(%q) err = %v, want an untrusted-URL refusal", u, err)
		}
	}
}
