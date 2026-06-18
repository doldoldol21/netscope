package classify

import "testing"

func TestIsAI(t *testing.T) {
	cases := map[string]bool{
		"api.openai.com":                    true,
		"openai.com":                        true,
		"chatgpt.com":                       true,
		"claude.ai":                         true,
		"foo.bar.anthropic.com":             true,
		"generativelanguage.googleapis.com": true,
		"notopenai.com":                     false, // must not match by substring
		"openai.com.evil.com":               false, // suffix attack
		"example.com":                       false,
		"github.com":                        false,
		"OPENAI.COM":                        true, // case-insensitive
		"api.openai.com.":                   true, // trailing dot
	}
	for host, want := range cases {
		if got := IsAI(host); got != want {
			t.Errorf("IsAI(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestCategory(t *testing.T) {
	cases := map[string]string{
		"api.openai.com":    CatAI,
		"d1.cloudfront.net": CatCDN,
		"vod.netflix.com":   CatMedia,
		"unknown.example":   CatOther,
		"s3.amazonaws.com":  CatCloud,
	}
	for host, want := range cases {
		if got := Category(host); got != want {
			t.Errorf("Category(%q) = %q, want %q", host, got, want)
		}
	}
}
