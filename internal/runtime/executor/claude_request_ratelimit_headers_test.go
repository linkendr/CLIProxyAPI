package executor

import (
	"net/http"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestClassifyClaudeUpstreamError429CarriesAllowlistedHeaders(t *testing.T) {
	headers := http.Header{}
	headers.Set("Retry-After", "45")
	headers.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
	headers.Set("Anthropic-Ratelimit-Unified-Reset", "1790000000")
	headers.Set("Set-Cookie", "a=b")
	headers.Set("Authorization", "Bearer x")

	err := classifyClaudeUpstreamError(http.StatusTooManyRequests, headers, []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow"}}`))
	got := cliproxyauth.SafeResponseHeaders(err)
	if got.Get("Retry-After") != "45" || got.Get("Anthropic-Ratelimit-Unified-Reset") != "1790000000" || got.Get("Anthropic-Ratelimit-Unified-Status") != "rejected" {
		t.Fatalf("headers = %v", got)
	}
	if got.Get("Set-Cookie") != "" || got.Get("Authorization") != "" {
		t.Fatalf("non-allowlisted header leaked: %v", got)
	}
	if h := cliproxyauth.SafeResponseHeaders(classifyClaudeUpstreamError(http.StatusBadGateway, headers, nil)); h != nil {
		t.Fatalf("non-429 must not carry headers, got %v", h)
	}
}
