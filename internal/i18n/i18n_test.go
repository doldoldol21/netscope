package i18n

import (
	"strings"
	"testing"
)

// The default locale must be English without any call — libraries using T (and
// their tests) depend on that determinism.
func TestDefaultIsEnglish(t *testing.T) {
	if Locale() != "en" {
		t.Fatalf("default locale = %q, want en", Locale())
	}
	got := T("alert.app_total", "Safari", "2.0 GB", "1.0 GB")
	if got != "Safari used 2.0 GB today (limit 1.0 GB)." {
		t.Errorf("T(en) = %q", got)
	}
}

func TestSetLocaleAndFallback(t *testing.T) {
	defer SetLocale("en")

	SetLocale("ko-KR") // full tags normalize to the base language
	if Locale() != "ko" {
		t.Fatalf("locale = %q, want ko", Locale())
	}
	if got := T("alert.daily_total", "1.0 GB", "1.1 GB"); !strings.Contains(got, "트래픽") {
		t.Errorf("T(ko) = %q, want Korean", got)
	}

	SetLocale("de") // unsupported → English
	if Locale() != "en" {
		t.Fatalf("locale = %q, want en fallback", Locale())
	}

	SetLocale("ja")
	if got := T("menubar.style.arrows"); got != "矢印" {
		t.Errorf("T(ja menubar) = %q", got)
	}
	if got := T("no.such.key"); got != "no.such.key" {
		t.Errorf("unknown key = %q, want the key itself", got)
	}
}

// Indexed format verbs in translations must consume the same argument list as
// the English source.
func TestReorderedArgs(t *testing.T) {
	defer SetLocale("en")
	SetLocale("ko")
	got := T("alert.plan_warn", 85.0, "100 GB", "85 GB", "15 GB")
	for _, want := range []string{"85%", "100 GB", "85 GB", "15 GB"} {
		if !strings.Contains(got, want) {
			t.Errorf("T(ko plan_warn) = %q, missing %q", got, want)
		}
	}
}
