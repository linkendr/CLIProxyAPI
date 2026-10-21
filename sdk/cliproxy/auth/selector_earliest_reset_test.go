package auth

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func resetAuth(id string, signals map[string]string, observedAt time.Time) *Auth {
	return &Auth{ID: id, Provider: "claude", Quota: QuotaState{Signals: signals, ObservedAt: observedAt}}
}

func epoch(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }

func pickIDs(t *testing.T, s Selector, auths []*Auth, n int) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		got, err := s.Pick(context.Background(), "claude", "claude-sonnet", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("Pick() error = %v", err)
		}
		ids = append(ids, got.ID)
	}
	return ids
}

func TestEarliestResetPicksSoonestAnthropicReset(t *testing.T) {
	now := time.Now()
	auths := []*Auth{
		resetAuth("a", map[string]string{"Anthropic-Ratelimit-Unified-Reset": epoch(now.Add(3 * time.Hour))}, now),
		resetAuth("b", map[string]string{"Anthropic-Ratelimit-Unified-Reset": epoch(now.Add(30 * time.Minute))}, now),
		resetAuth("c", map[string]string{"Anthropic-Ratelimit-Unified-Reset": epoch(now.Add(2 * time.Hour))}, now),
	}
	for _, id := range pickIDs(t, &EarliestResetSelector{}, auths, 4) {
		if id != "b" {
			t.Fatalf("picked %q, want b every time", id)
		}
	}
}

func TestEarliestResetUnknownSortsLastAndPastIsUnknown(t *testing.T) {
	now := time.Now()
	auths := []*Auth{
		resetAuth("a", nil, time.Time{}),
		resetAuth("b", map[string]string{"Anthropic-Ratelimit-Unified-Reset": epoch(now.Add(-time.Hour))}, now),
		resetAuth("c", map[string]string{"Anthropic-Ratelimit-Unified-5h-Reset": epoch(now.Add(4 * time.Hour))}, now),
	}
	if got := pickIDs(t, &EarliestResetSelector{}, auths, 1)[0]; got != "c" {
		t.Fatalf("picked %q, want c", got)
	}
}

func TestEarliestResetCodexRelativeAndAbsolute(t *testing.T) {
	now := time.Now()
	auths := []*Auth{
		resetAuth("a", map[string]string{"X-Codex-Primary-Reset-After-Seconds": "7200", "X-Codex-Secondary-Reset-After-Seconds": "600000"}, now),
		resetAuth("b", map[string]string{"X-Codex-Primary-Reset-After-Seconds": "5400", "X-Codex-Secondary-Reset-At": epoch(now.Add(10 * time.Minute))}, now),
		resetAuth("c", map[string]string{"X-Codex-Primary-Reset-After-Seconds": "900"}, now.Add(-time.Hour)),
	}
	auths[0].Provider, auths[1].Provider, auths[2].Provider = "codex", "codex", "codex"
	got, err := (&EarliestResetSelector{}).Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	// c observed an hour ago with 900s left: already rolled over, so unknown. b wins on the secondary reset-at.
	if got.ID != "b" {
		t.Fatalf("picked %q, want b", got.ID)
	}
}

func TestEarliestResetSkipsCooledCredentials(t *testing.T) {
	now := time.Now()
	cooled := resetAuth("a", map[string]string{"Anthropic-Ratelimit-Unified-Reset": epoch(now.Add(time.Minute))}, now)
	cooled.Quota.Exceeded = true
	cooled.Quota.Reason = "credential_quota"
	cooled.Quota.NextRecoverAt = now.Add(time.Hour)
	auths := []*Auth{
		cooled,
		resetAuth("b", map[string]string{"Anthropic-Ratelimit-Unified-Reset": epoch(now.Add(5 * time.Hour))}, now),
	}
	if got := pickIDs(t, &EarliestResetSelector{}, auths, 1)[0]; got != "b" {
		t.Fatalf("picked %q, want b (a is cooled down)", got)
	}
}

func TestEarliestResetTiesRoundRobin(t *testing.T) {
	now := time.Now()
	same := epoch(now.Add(time.Hour))
	auths := []*Auth{
		resetAuth("a", map[string]string{"Anthropic-Ratelimit-Unified-Reset": same}, now),
		resetAuth("b", map[string]string{"Anthropic-Ratelimit-Unified-Reset": same}, now),
		resetAuth("c", map[string]string{"Anthropic-Ratelimit-Unified-Reset": epoch(now.Add(2 * time.Hour))}, now),
	}
	got := pickIDs(t, &EarliestResetSelector{}, auths, 4)
	want := []string{"a", "b", "a", "b"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("picks = %v, want %v", got, want)
		}
	}
}

func TestEarliestResetAllUnknownRoundRobin(t *testing.T) {
	auths := []*Auth{resetAuth("a", nil, time.Time{}), resetAuth("b", nil, time.Time{}), resetAuth("c", nil, time.Time{})}
	got := pickIDs(t, &EarliestResetSelector{}, auths, 4)
	want := []string{"a", "b", "c", "a"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("picks = %v, want %v", got, want)
		}
	}
}

func TestEarliestResetHonorsSessionAffinity(t *testing.T) {
	now := time.Now()
	auths := []*Auth{
		resetAuth("a", map[string]string{"Anthropic-Ratelimit-Unified-Reset": epoch(now.Add(3 * time.Hour))}, now),
		resetAuth("b", map[string]string{"Anthropic-Ratelimit-Unified-Reset": epoch(now.Add(time.Hour))}, now),
	}
	selector := NewSessionAffinitySelector(&EarliestResetSelector{})
	opts := cliproxyexecutor.Options{Headers: map[string][]string{"X-Claude-Code-Session-Id": {"session-1"}}}
	first, err := selector.Pick(context.Background(), "claude", "claude-sonnet", opts, auths)
	if err != nil || first.ID != "b" {
		t.Fatalf("first pick = %v, %v; want b", first, err)
	}
	// b's window now resets later than a's, but the bound session keeps b.
	auths[1].Quota.Signals = map[string]string{"Anthropic-Ratelimit-Unified-Reset": epoch(now.Add(9 * time.Hour))}
	second, err := selector.Pick(context.Background(), "claude", "claude-sonnet", opts, auths)
	if err != nil || second.ID != "b" {
		t.Fatalf("bound session pick = %v, %v; want b", second, err)
	}
	// A new session is not bound and picks the soonest reset, a.
	other := cliproxyexecutor.Options{Headers: map[string][]string{"X-Claude-Code-Session-Id": {"session-2"}}}
	third, err := selector.Pick(context.Background(), "claude", "claude-sonnet", other, auths)
	if err != nil || third.ID != "a" {
		t.Fatalf("new session pick = %v, %v; want a", third, err)
	}
}

func TestObserveResponseHeadersFeedsEarliestReset(t *testing.T) {
	now := time.Now()
	claude := &Auth{ID: "claude-1", Provider: "claude"}
	claude.Quota.ObserveResponseHeadersForProvider("claude", headerOf(
		"Anthropic-Ratelimit-Unified-Reset", epoch(now.Add(time.Hour)),
		"Anthropic-Ratelimit-Unified-5h-Reset", epoch(now.Add(20*time.Minute)),
		"Anthropic-Ratelimit-Unified-Overage-Reset", epoch(now.Add(10*time.Minute)),
	), now)
	got, ok := earliestKnownReset(claude, "", now)
	if !ok || got.Unix() != now.Add(20*time.Minute).Unix() {
		t.Fatalf("claude reset = %v, %v; want 5h reset (overage ignored)", got, ok)
	}

	codex := &Auth{ID: "codex-1", Provider: "codex"}
	codex.Quota.ObserveResponseHeadersForProvider("codex", headerOf(
		"x-codex-primary-reset-after-seconds", "1800",
		"x-codex-primary-used-percent", "40",
		"x-codex-secondary-reset-after-seconds", "86400",
	), now)
	got, ok = earliestKnownReset(codex, "", now)
	if !ok || got.Unix() != now.Add(30*time.Minute).Unix() {
		t.Fatalf("codex reset = %v, %v; want primary 30m", got, ok)
	}
}

func TestEarliestKnownResetUsesCooldown(t *testing.T) {
	now := time.Now()
	auth := &Auth{ID: "x", Quota: QuotaState{NextRecoverAt: now.Add(15 * time.Minute)}}
	got, ok := earliestKnownReset(auth, "", now)
	if !ok || !got.Equal(now.Add(15*time.Minute)) {
		t.Fatalf("cooldown reset = %v, %v", got, ok)
	}
}

func headerOf(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}
