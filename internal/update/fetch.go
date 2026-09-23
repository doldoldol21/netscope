package update

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// AllowedHost restricts release downloads to GitHub itself and its asset CDN
// (release downloads redirect to *.githubusercontent.com). Anything else —
// even if it appears in an API response — is refused.
func AllowedHost(host string) bool {
	host = strings.ToLower(host)
	return host == "github.com" || host == "api.github.com" ||
		strings.HasSuffix(host, ".githubusercontent.com")
}

// Fetch downloads url into memory, refusing non-HTTPS URLs, non-GitHub hosts
// (including on redirects) and responses larger than maxBytes. It is the
// small-asset counterpart of the app's zip download: checksums.txt, not the
// bundle.
func Fetch(url string, maxBytes int64) ([]byte, error) {
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if !AllowedHost(req.URL.Hostname()) {
				return fmt.Errorf("redirect to untrusted host %q", req.URL.Hostname())
			}
			return nil
		},
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if req.URL.Scheme != "https" || !AllowedHost(req.URL.Hostname()) {
		return nil, fmt.Errorf("untrusted download URL %q", url)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %s", resp.Status)
	}
	if resp.ContentLength > maxBytes {
		return nil, fmt.Errorf("response too large (%d bytes)", resp.ContentLength)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > maxBytes {
		return nil, fmt.Errorf("response exceeds %d bytes", maxBytes)
	}
	return b, nil
}
