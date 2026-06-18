// Package classify maps domains to human-friendly categories and flags the
// AI services that motivated netscope in the first place.
package classify

import "strings"

// Category labels are coarse buckets used for dashboard grouping.
const (
	CatAI       = "ai"
	CatCloud    = "cloud"
	CatCDN      = "cdn"
	CatSocial   = "social"
	CatMedia    = "media"
	CatTracking = "tracking"
	CatOther    = ""
)

// aiSuffixes are registrable domains (or sufficiently specific suffixes) whose
// traffic we treat as "AI service" usage. Matching is suffix based so that
// sub-domains (api.openai.com, chatgpt.com edge nodes, …) are caught too.
var aiSuffixes = []string{
	"openai.com",
	"chatgpt.com",
	"oaistatic.com",
	"oaiusercontent.com",
	"anthropic.com",
	"claude.ai",
	"claudeusercontent.com",
	"gemini.google.com",
	"generativelanguage.googleapis.com",
	"bard.google.com",
	"perplexity.ai",
	"pplx.ai",
	"mistral.ai",
	"cohere.com",
	"cohere.ai",
	"huggingface.co",
	"hf.co",
	"x.ai",
	"groq.com",
	"deepseek.com",
	"githubcopilot.com",
	"copilot.microsoft.com",
	"midjourney.com",
	"runwayml.com",
	"stability.ai",
	"replicate.com",
	"together.ai",
	"perplexity.com",
}

// categorySuffixes maps non-AI suffixes to a coarse category. Only used for
// nicer grouping; absence simply yields CatOther.
var categorySuffixes = map[string]string{
	"amazonaws.com":         CatCloud,
	"googleapis.com":        CatCloud,
	"azure.com":             CatCloud,
	"azureedge.net":         CatCDN,
	"cloudfront.net":        CatCDN,
	"fastly.net":            CatCDN,
	"akamaized.net":         CatCDN,
	"cloudflare.com":        CatCDN,
	"gstatic.com":           CatCDN,
	"facebook.com":          CatSocial,
	"instagram.com":         CatSocial,
	"twitter.com":           CatSocial,
	"x.com":                 CatSocial,
	"tiktok.com":            CatSocial,
	"youtube.com":           CatMedia,
	"googlevideo.com":       CatMedia,
	"netflix.com":           CatMedia,
	"nflxvideo.net":         CatMedia,
	"spotify.com":           CatMedia,
	"doubleclick.net":       CatTracking,
	"google-analytics.com":  CatTracking,
	"scorecardresearch.com": CatTracking,
}

// IsAI reports whether the (lower-cased) host belongs to a known AI service.
func IsAI(host string) bool {
	host = normalize(host)
	for _, s := range aiSuffixes {
		if matchSuffix(host, s) {
			return true
		}
	}
	return false
}

// Category returns the coarse category for a host. AI services take priority.
func Category(host string) string {
	host = normalize(host)
	if IsAI(host) {
		return CatAI
	}
	for s, cat := range categorySuffixes {
		if matchSuffix(host, s) {
			return cat
		}
	}
	return CatOther
}

func normalize(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	host = strings.TrimSuffix(host, ".")
	return host
}

// matchSuffix reports whether host == suffix or host ends in "."+suffix, so
// that "api.openai.com" matches "openai.com" but "notopenai.com" does not.
func matchSuffix(host, suffix string) bool {
	if host == suffix {
		return true
	}
	return strings.HasSuffix(host, "."+suffix)
}
