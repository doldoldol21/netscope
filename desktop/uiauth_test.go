//go:build darwin

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

const self = "http://127.0.0.1:54321"

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot) // distinctive: proves we reached the mux
	})
}

func do(t *testing.T, build func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, self+"/api/snapshot", nil)
	build(r)
	w := httptest.NewRecorder()
	authUI(self, okHandler()).ServeHTTP(w, r)
	return w
}

func TestAuthUIRejectsUntokenedRequest(t *testing.T) {
	// The whole point: another local process that finds the port gets nothing.
	if got := do(t, func(*http.Request) {}).Code; got != http.StatusUnauthorized {
		t.Fatalf("no token: got %d, want 401", got)
	}
}

func TestAuthUIRejectsWrongToken(t *testing.T) {
	w := do(t, func(r *http.Request) { r.AddCookie(&http.Cookie{Name: uiTokenCookie, Value: "nope"}) })
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: got %d, want 401", w.Code)
	}
}

func TestAuthUIAcceptsToken(t *testing.T) {
	cases := map[string]func(*http.Request){
		"cookie": func(r *http.Request) { r.AddCookie(&http.Cookie{Name: uiTokenCookie, Value: uiToken}) },
		"bearer": func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+uiToken) },
		"query":  func(r *http.Request) { r.URL.RawQuery = "k=" + uiToken },
	}
	for name, build := range cases {
		if got := do(t, build).Code; got != http.StatusTeapot {
			t.Errorf("%s: got %d, want the request to reach the mux", name, got)
		}
	}
}

func TestAuthUISetsCookieOnTokenedNavigation(t *testing.T) {
	w := do(t, func(r *http.Request) { r.URL.RawQuery = "k=" + uiToken })
	for _, c := range w.Result().Cookies() {
		if c.Name == uiTokenCookie && c.Value == uiToken && c.HttpOnly {
			return
		}
	}
	t.Fatal("navigation carrying ?k= must set the session cookie (HttpOnly)")
}

func TestAuthUIRejectsCrossSite(t *testing.T) {
	// A page on some website can reach 127.0.0.1; CORS hides the response but
	// not the side effect, so cross-site requests are refused outright — even
	// when they somehow carry a valid token.
	cross := map[string]func(*http.Request){
		"origin":         func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") },
		"referer":        func(r *http.Request) { r.Header.Set("Referer", "https://evil.example/x") },
		"sec-fetch-site": func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") },
	}
	for name, mark := range cross {
		w := do(t, func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: uiTokenCookie, Value: uiToken})
			mark(r)
		})
		if w.Code != http.StatusForbidden {
			t.Errorf("%s: got %d, want 403", name, w.Code)
		}
	}
}

func TestAuthUIAllowsOwnOrigin(t *testing.T) {
	w := do(t, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: uiTokenCookie, Value: uiToken})
		r.Header.Set("Origin", self)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
	})
	if w.Code != http.StatusTeapot {
		t.Fatalf("same-origin tokened request: got %d, want it to pass", w.Code)
	}
}

func TestUITokenIsUnguessable(t *testing.T) {
	if len(uiToken) < 32 {
		t.Fatalf("token too short: %d chars", len(uiToken))
	}
	if mustToken() == mustToken() {
		t.Fatal("tokens must not repeat")
	}
}
