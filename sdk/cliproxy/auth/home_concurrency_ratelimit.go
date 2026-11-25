package auth

import (
	"net/http"
	"strings"
)

// Fork-owned rate-limit header allowlist. SafeResponseHeaders in home_concurrency.go
// returns these headers for an upstream 429 (UpstreamRateLimitHeaders).

const upstreamRateLimitHeaderPrefix = "anthropic-ratelimit-"

// IsRateLimitResponseHeader reports whether key may be echoed to clients on a
// 429: Retry-After and the anthropic-ratelimit-* family only.
func IsRateLimitResponseHeader(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	return key == "retry-after" || strings.HasPrefix(key, upstreamRateLimitHeaderPrefix)
}

// FilterUpstreamRateLimitHeaders copies the rate-limit headers a client needs to
// compute a reset time out of an upstream response. Everything else, including
// auth and cookie headers, is dropped.
func FilterUpstreamRateLimitHeaders(headers http.Header) http.Header {
	var out http.Header
	for key, values := range headers {
		if !IsRateLimitResponseHeader(key) || len(values) == 0 {
			continue
		}
		if out == nil {
			out = make(http.Header)
		}
		out[http.CanonicalHeaderKey(key)] = append([]string(nil), values...)
	}
	return out
}
