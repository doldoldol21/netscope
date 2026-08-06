//go:build darwin

package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strings"
)

// The dashboard window is fed by a loopback HTTP server that proxies /api to the
// daemon's unix socket. Loopback bounds who can reach it to this machine — but
// not to this user: any local process (another account, a sandboxed app) that
// scans 127.0.0.1 could otherwise read the whole capture through the proxy,
// defeating the socket's 0600 ownership. A page in the user's browser could
// likewise POST to it (localhost is reachable from any origin; the response is
// blocked by CORS but the side effect is not).
//
// So every request must present a per-launch token, and browser-originated
// requests are refused outright:
//
//   - the token is generated at startup, lives only in this process' memory
//     (never on disk), and is handed to the WebView in the dashboard URL;
//   - the page keeps it in a cookie scoped to this origin so later fetches from
//     app.js carry it without every call having to append it;
//   - requests carrying an Origin/Referer that isn't our own loopback origin, or
//     a Sec-Fetch-Site that says cross-site, are rejected before routing.
//
// This is a local-privilege boundary, not a network one: the daemon still opens
// no port at all.

// uiToken is the per-launch bearer for the loopback UI server.
var uiToken = mustToken()

const uiTokenCookie = "ns_k"

func mustToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// A process without a working CSPRNG must not fall back to a guessable
		// token — that would silently unlock the proxy for every local process.
		panic("netscope: cannot generate UI token: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// tokenFromRequest reads the token from the cookie, the Authorization header, or
// the ?k= query parameter (the last one is how the first navigation carries it).
func tokenFromRequest(r *http.Request) string {
	if c, err := r.Cookie(uiTokenCookie); err == nil && c.Value != "" {
		return c.Value
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return r.URL.Query().Get("k")
}

// sameOrigin reports whether a request's Origin/Referer (when present) points at
// our own loopback server. Requests without either are same-origin navigations
// or non-browser clients; the token check still gates those.
func sameOrigin(r *http.Request, self string) bool {
	if o := r.Header.Get("Origin"); o != "" {
		return o == self
	}
	if ref := r.Header.Get("Referer"); ref != "" {
		return strings.HasPrefix(ref, self+"/") || ref == self
	}
	return true
}

// authUI wraps the loopback mux: it rejects cross-site and untokened requests,
// and re-issues the session cookie on the navigation that carried ?k=.
func authUI(self string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Fetch metadata is the strongest signal a browser gives us: anything
		// other than same-origin/none came from another site's page.
		switch r.Header.Get("Sec-Fetch-Site") {
		case "", "same-origin", "none":
		default:
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if !sameOrigin(r, self) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		got := tokenFromRequest(r)
		if subtle.ConstantTimeCompare([]byte(got), []byte(uiToken)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Persist the token for subsequent same-origin fetches (the port, and so
		// the origin, changes every launch, so the cookie dies with the app).
		if r.URL.Query().Get("k") != "" {
			http.SetCookie(w, &http.Cookie{
				Name:     uiTokenCookie,
				Value:    uiToken,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteStrictMode,
			})
		}
		next.ServeHTTP(w, r)
	})
}
