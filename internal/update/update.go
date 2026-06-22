// Package update checks whether a newer netscope release is available on
// GitHub. It never downloads or installs anything — it reports status that the
// menu bar and dashboard surface, with a link to the release page. (Automatic
// self-update is deferred: it needs code signing and privileged replacement of
// the root daemon.)
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Status is the cached result of an update check.
type Status struct {
	Current         string    `json:"current"`
	Latest          string    `json:"latest"`
	UpdateAvailable bool      `json:"updateAvailable"`
	URL             string    `json:"url"`      // release page (human-facing)
	AssetURL        string    `json:"assetUrl"` // app .zip download (in-app self-update)
	CheckedAt       time.Time `json:"checkedAt"`
}

// apiBase is the GitHub API root; overridable in tests.
var apiBase = "https://api.github.com"

// Check queries the repo's latest GitHub release and compares it to current.
// A repo with no releases yields a non-error Status with UpdateAvailable=false.
func Check(ctx context.Context, repo, current string) (Status, error) {
	st := Status{Current: current, CheckedAt: time.Now()}

	url := apiBase + "/repos/" + repo + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return st, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := (&http.Client{Timeout: 8 * time.Second}).Do(req)
	if err != nil {
		return st, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return st, nil // no releases published yet
	}
	if resp.StatusCode != http.StatusOK {
		return st, fmt.Errorf("github: %s", resp.Status)
	}

	var rel struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return st, err
	}
	st.Latest = rel.TagName
	st.URL = rel.HTMLURL
	st.UpdateAvailable = Newer(rel.TagName, current)
	// The app bundle is published as netscope-<tag>-app.zip; pick it for the
	// in-app self-update. Prefer the release's actual asset; fall back to the
	// conventional download URL (mirrors install.sh) if assets aren't listed.
	for _, a := range rel.Assets {
		if strings.HasSuffix(a.Name, "-app.zip") && a.URL != "" {
			st.AssetURL = a.URL
			break
		}
	}
	if st.AssetURL == "" && rel.TagName != "" {
		st.AssetURL = fmt.Sprintf("https://github.com/%s/releases/download/%s/netscope-%s-app.zip",
			repo, rel.TagName, rel.TagName)
	}
	return st, nil
}

// Newer reports whether version `latest` is strictly greater than `current`
// under semver (ignoring a leading "v" and any pre-release suffix). If either
// side is not parseable (e.g. a "dev" build), it returns false so we never nag.
func Newer(latest, current string) bool {
	lv, ok1 := parse(latest)
	cv, ok2 := parse(current)
	if !ok1 || !ok2 {
		return false
	}
	for i := 0; i < 3; i++ {
		if lv[i] != cv[i] {
			return lv[i] > cv[i]
		}
	}
	return false
}

// parse turns "v1.2.3" / "1.2.3-rc1" into [3]int{1,2,3}.
func parse(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 { // require a full x.y.z; partial tags aren't trusted
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// Checker periodically refreshes and caches an update Status.
type Checker struct {
	repo     string
	current  string
	interval time.Duration

	mu  sync.RWMutex
	st  Status
	now func() time.Time
}

// NewChecker builds a Checker. interval bounds how often GitHub is queried.
func NewChecker(repo, current string, interval time.Duration) *Checker {
	if interval <= 0 {
		interval = 6 * time.Hour
	}
	return &Checker{
		repo:     repo,
		current:  current,
		interval: interval,
		st:       Status{Current: current},
		now:      time.Now,
	}
}

// Run checks immediately, then on the interval, until ctx is cancelled.
func (c *Checker) Run(ctx context.Context) {
	c.refresh(ctx)
	t := time.NewTicker(c.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.refresh(ctx)
		}
	}
}

func (c *Checker) refresh(ctx context.Context) {
	st, err := Check(ctx, c.repo, c.current)
	if err != nil {
		return // keep last good status; transient network errors shouldn't nag
	}
	c.mu.Lock()
	c.st = st
	c.mu.Unlock()
}

// Status returns the most recent cached status.
func (c *Checker) Status() Status {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.st
}
