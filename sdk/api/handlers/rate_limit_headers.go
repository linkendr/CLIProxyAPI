package handlers

import (
	"net/http"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// EchoRateLimitHeaders sets every allowlisted rate-limit header of safeHeaders
// (Retry-After and anthropic-ratelimit-*) on out, replacing any value already there, so
// a client can compute the reset time. The error writers call it after upstream's
// Retry-After copy.
func EchoRateLimitHeaders(out, safeHeaders http.Header) {
	for key, values := range safeHeaders {
		if !coreauth.IsRateLimitResponseHeader(key) {
			continue
		}
		out.Del(key)
		for _, value := range values {
			out.Add(key, value)
		}
	}
}
