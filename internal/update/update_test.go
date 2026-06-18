package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		latest, current string
		want            bool
	}{
		{"v1.2.0", "v1.1.9", true},
		{"v1.2.0", "v1.2.0", false},
		{"v1.2.0", "v1.3.0", false},
		{"1.0.1", "1.0.0", true},
		{"v2.0.0", "v1.9.9", true},
		{"v1.2.3-rc1", "v1.2.2", true}, // pre-release suffix ignored
		{"v1.0.0", "dev", false},       // unparseable current -> no nag
		{"weird", "v1.0.0", false},     // unparseable latest -> no nag
	}
	for _, c := range cases {
		if got := Newer(c.latest, c.current); got != c.want {
			t.Errorf("Newer(%q,%q)=%v want %v", c.latest, c.current, got, c.want)
		}
	}
}

func TestCheckUpdateAvailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"tag_name":"v0.9.0","html_url":"https://example.com/releases/v0.9.0"}`))
	}))
	defer srv.Close()
	old := apiBase
	apiBase = srv.URL
	defer func() { apiBase = old }()

	st, err := Check(context.Background(), "owner/repo", "v0.8.0")
	if err != nil {
		t.Fatal(err)
	}
	if !st.UpdateAvailable || st.Latest != "v0.9.0" || st.URL == "" {
		t.Fatalf("unexpected status: %+v", st)
	}
}

func TestCheckNoReleases(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()
	old := apiBase
	apiBase = srv.URL
	defer func() { apiBase = old }()

	st, err := Check(context.Background(), "owner/repo", "v1.0.0")
	if err != nil {
		t.Fatalf("404 should be a soft no-op, got err: %v", err)
	}
	if st.UpdateAvailable {
		t.Errorf("no releases must not report an update: %+v", st)
	}
}
