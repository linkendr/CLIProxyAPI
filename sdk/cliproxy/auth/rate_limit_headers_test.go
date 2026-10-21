package auth

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

type upstreamRateLimitErr struct{ headers http.Header }

func (e upstreamRateLimitErr) Error() string                         { return "429" }
func (e upstreamRateLimitErr) UpstreamRateLimitHeaders() http.Header { return e.headers }

func TestFilterUpstreamRateLimitHeadersAllowlist(t *testing.T) {
	in := http.Header{}
	in.Set("Retry-After", "120")
	in.Set("Anthropic-Ratelimit-Unified-Reset", "1790000000")
	in.Set("anthropic-ratelimit-unified-status", "rejected")
	in.Set("Anthropic-Ratelimit-Unified-Representative-Claim", "five_hour")
	in.Set("Authorization", "Bearer secret")
	in.Set("Set-Cookie", "a=b")
	in.Set("X-Api-Key", "secret")
	in.Set("Content-Type", "application/json")

	got := FilterUpstreamRateLimitHeaders(in)
	for _, key := range []string{"Retry-After", "Anthropic-Ratelimit-Unified-Reset", "Anthropic-Ratelimit-Unified-Status", "Anthropic-Ratelimit-Unified-Representative-Claim"} {
		if got.Get(key) == "" {
			t.Errorf("%s missing from filtered headers", key)
		}
	}
	if len(got) != 4 {
		t.Fatalf("filtered headers = %v, want exactly 4 allowlisted entries", got)
	}
	if FilterUpstreamRateLimitHeaders(http.Header{"Authorization": {"x"}}) != nil {
		t.Fatal("expected nil when nothing is allowlisted")
	}
}

func TestSafeResponseHeadersPassesUpstreamRateLimitHeaders(t *testing.T) {
	headers := FilterUpstreamRateLimitHeaders(http.Header{
		"Retry-After":                       {"60"},
		"Anthropic-Ratelimit-Unified-Reset": {"1790000000"},
	})
	got := SafeResponseHeaders(upstreamRateLimitErr{headers: headers})
	if got.Get("Retry-After") != "60" || got.Get("Anthropic-Ratelimit-Unified-Reset") != "1790000000" {
		t.Fatalf("SafeResponseHeaders = %v", got)
	}
}

func TestModelCooldownHeadersSynthesizeUnifiedReset(t *testing.T) {
	before := time.Now()
	err := newModelCooldownError("claude-haiku-5-5", "claude", 90*time.Minute)
	after := time.Now()

	headers := SafeResponseHeaders(err)
	if got := headers.Get("anthropic-ratelimit-unified-status"); got != "rejected" {
		t.Fatalf("unified-status = %q, want rejected", got)
	}
	if got := headers.Get("Retry-After"); got != "5400" {
		t.Fatalf("Retry-After = %q, want 5400", got)
	}
	reset, parseErr := strconv.ParseInt(headers.Get("anthropic-ratelimit-unified-reset"), 10, 64)
	if parseErr != nil {
		t.Fatalf("unified-reset not an epoch: %v", parseErr)
	}
	lo := before.Add(90 * time.Minute).Unix()
	hi := after.Add(90 * time.Minute).Unix()
	if reset < lo || reset > hi {
		t.Fatalf("unified-reset = %d, want within [%d, %d]", reset, lo, hi)
	}
}
