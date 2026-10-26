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
		resetAuth("a", map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": epoch(now.Add(3 * time.Hour))}, now),
		resetAuth("b", map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": epoch(now.Add(30 * time.Minute))}, now),
		resetAuth("c", map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": epoch(now.Add(2 * time.Hour))}, now),
	}
	for _, id := range pickIDs(t, &EarliestResetSelector{}, auths, 4) {
		if id != "b" {
			t.Fatalf("picked %q, want b every time", id)
		}
	}
}

func TestEarliestResetRanksWeeklyNotFiveHour(t *testing.T) {
	now := time.Now()
	auths := []*Auth{
		resetAuth("a", map[string]string{
			"Anthropic-Ratelimit-Unified-5h-Reset": epoch(now.Add(20 * time.Minute)),
			"Anthropic-Ratelimit-Unified-7d-Reset": epoch(now.Add(5 * 24 * time.Hour)),
			"Anthropic-Ratelimit-Unified-Reset":    epoch(now.Add(20 * time.Minute)),
		}, now),
		resetAuth("b", map[string]string{
			"Anthropic-Ratelimit-Unified-5h-Reset": epoch(now.Add(3 * time.Hour)),
			"Anthropic-Ratelimit-Unified-7d-Reset": epoch(now.Add(24 * time.Hour)),
			"Anthropic-Ratelimit-Unified-Reset":    epoch(now.Add(3 * time.Hour)),
		}, now),
	}
	for _, id := range pickIDs(t, &EarliestResetSelector{}, auths, 3) {
		if id != "b" {
			t.Fatalf("picked %q, want b (weekly reset 1d beats 5d; 5h windows do not rank)", id)
		}
	}
}

func TestEarliestResetScopedWeeklyRanks(t *testing.T) {
	now := time.Now()
	auths := []*Auth{
		resetAuth("a", map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": epoch(now.Add(4 * 24 * time.Hour))}, now),
		resetAuth("b", map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Reset":    epoch(now.Add(6 * 24 * time.Hour)),
			"Anthropic-Ratelimit-Unified-7d_oi-Reset": epoch(now.Add(2 * 24 * time.Hour)),
		}, now),
	}
	if got := pickIDs(t, &EarliestResetSelector{}, auths, 1)[0]; got != "b" {
		t.Fatalf("picked %q, want b (scoped weekly window resets in 2d)", got)
	}
}

func TestEarliestResetUnknownProbedFirstOnce(t *testing.T) {
	now := time.Now()
	clock := now
	auths := []*Auth{
		resetAuth("a-known", map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": epoch(now.Add(time.Hour))}, now),
		// Only a 5h reset: no weekly reset known, so a probe.
		resetAuth("b-unknown", map[string]string{"Anthropic-Ratelimit-Unified-5h-Reset": epoch(now.Add(time.Minute))}, now),
		resetAuth("c-known", map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": epoch(now.Add(2 * time.Hour))}, now),
	}
	s := &EarliestResetSelector{now: func() time.Time { return clock }}
	got := pickIDs(t, s, auths, 3)
	want := []string{"b-unknown", "a-known", "a-known"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("picks = %v, want %v (probe once, then known ordering)", got, want)
		}
	}
	// The probe expires: the still-unknown credential is probed once more.
	clock = clock.Add(earliestResetProbeTTL + time.Second)
	auths[0].Quota.Signals = map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": epoch(clock.Add(time.Hour))}
	auths[2].Quota.Signals = map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": epoch(clock.Add(2 * time.Hour))}
	got = pickIDs(t, s, auths, 2)
	want = []string{"b-unknown", "a-known"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("picks after ttl = %v, want %v", got, want)
		}
	}
}

func TestEarliestResetUnknownLearnedStopsProbing(t *testing.T) {
	now := time.Now()
	auths := []*Auth{
		resetAuth("a-known", map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": epoch(now.Add(5 * 24 * time.Hour))}, now),
		resetAuth("b-new", nil, time.Time{}),
	}
	s := &EarliestResetSelector{}
	if got := pickIDs(t, s, auths, 1)[0]; got != "b-new" {
		t.Fatalf("first pick = %q, want b-new (probe)", got)
	}
	// The probe response taught b-new a weekly reset later than a-known's.
	auths[1].Quota = QuotaState{Signals: map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": epoch(now.Add(6 * 24 * time.Hour))}, ObservedAt: now}
	if got := pickIDs(t, s, auths, 1)[0]; got != "a-known" {
		t.Fatalf("second pick = %q, want a-known (normal ordering)", got)
	}
}

func TestEarliestResetMultipleUnknownsRotateOnce(t *testing.T) {
	now := time.Now()
	auths := []*Auth{
		resetAuth("a-known", map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": epoch(now.Add(time.Hour))}, now),
		resetAuth("b-new", nil, time.Time{}),
		resetAuth("c-new", nil, time.Time{}),
	}
	got := pickIDs(t, &EarliestResetSelector{}, auths, 4)
	want := []string{"b-new", "c-new", "a-known", "a-known"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("picks = %v, want %v", got, want)
		}
	}
}

func TestEarliestResetPastWeeklyIsUnknown(t *testing.T) {
	now := time.Now()
	auths := []*Auth{
		resetAuth("a", map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": epoch(now.Add(-time.Hour))}, now),
		resetAuth("c", map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": epoch(now.Add(4 * time.Hour))}, now),
	}
	if got := pickIDs(t, &EarliestResetSelector{}, auths, 1)[0]; got != "a" {
		t.Fatalf("picked %q, want a (rolled-over reset counts as unknown, probed first)", got)
	}
}

func TestEarliestResetCodexSecondaryRelativeAndAbsolute(t *testing.T) {
	now := time.Now()
	auths := []*Auth{
		resetAuth("a", map[string]string{"X-Codex-Primary-Reset-After-Seconds": "60", "X-Codex-Secondary-Reset-After-Seconds": "600000"}, now),
		resetAuth("b", map[string]string{"X-Codex-Primary-Reset-After-Seconds": "5400", "X-Codex-Secondary-Reset-At": epoch(now.Add(2 * 24 * time.Hour))}, now),
		resetAuth("c", map[string]string{"X-Codex-Secondary-Reset-After-Seconds": "900"}, now.Add(-time.Hour)),
		resetAuth("d", map[string]string{"X-Codex-Secondary-Reset-After-Seconds": "400000"}, now),
	}
	for _, a := range auths {
		a.Provider = "codex"
	}
	s := &EarliestResetSelector{}
	pick := func() string {
		got, err := s.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("Pick() error = %v", err)
		}
		return got.ID
	}
	// c observed an hour ago with 900s left: rolled over, so unknown and probed first.
	if got := pick(); got != "c" {
		t.Fatalf("first pick = %q, want c (probe)", got)
	}
	// d: 400000s (~4.6d) vs b: 2d vs a: ~6.9d. Primary resets (60s) never rank, so b wins.
	if got := pick(); got != "b" {
		t.Fatalf("second pick = %q, want b (soonest secondary)", got)
	}
}

func TestEarliestResetSkipsCooledCredentials(t *testing.T) {
	now := time.Now()
	cooled := resetAuth("a", map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": epoch(now.Add(time.Minute))}, now)
	cooled.Quota.Exceeded = true
	cooled.Quota.Reason = "credential_quota"
	cooled.Quota.NextRecoverAt = now.Add(time.Hour)
	auths := []*Auth{
		cooled,
		resetAuth("b", map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": epoch(now.Add(5 * time.Hour))}, now),
	}
	if got := pickIDs(t, &EarliestResetSelector{}, auths, 1)[0]; got != "b" {
		t.Fatalf("picked %q, want b (a is cooled down)", got)
	}
}

func TestEarliestResetTiesRoundRobin(t *testing.T) {
	now := time.Now()
	same := epoch(now.Add(time.Hour))
	auths := []*Auth{
		resetAuth("a", map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": same}, now),
		resetAuth("b", map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": same}, now),
		resetAuth("c", map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": epoch(now.Add(2 * time.Hour))}, now),
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
		resetAuth("a", map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": epoch(now.Add(3 * time.Hour))}, now),
		resetAuth("b", map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": epoch(now.Add(time.Hour))}, now),
	}
	selector := NewSessionAffinitySelector(&EarliestResetSelector{})
	opts := cliproxyexecutor.Options{Headers: map[string][]string{"X-Claude-Code-Session-Id": {"session-1"}}}
	first, err := selector.Pick(context.Background(), "claude", "claude-sonnet", opts, auths)
	if err != nil || first.ID != "b" {
		t.Fatalf("first pick = %v, %v; want b", first, err)
	}
	// b's window now resets later than a's, but the bound session keeps b.
	auths[1].Quota.Signals = map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": epoch(now.Add(9 * time.Hour))}
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
		"Anthropic-Ratelimit-Unified-7d-Reset", epoch(now.Add(72*time.Hour)),
		"Anthropic-Ratelimit-Unified-Overage-Reset", epoch(now.Add(10*time.Minute)),
	), now)
	got, ok := earliestKnownReset(claude, "", now)
	if !ok || got.Unix() != now.Add(72*time.Hour).Unix() {
		t.Fatalf("claude reset = %v, %v; want 7d reset (5h, unified and overage ignored)", got, ok)
	}

	codex := &Auth{ID: "codex-1", Provider: "codex"}
	codex.Quota.ObserveResponseHeadersForProvider("codex", headerOf(
		"x-codex-primary-reset-after-seconds", "1800",
		"x-codex-primary-used-percent", "40",
		"x-codex-secondary-reset-after-seconds", "86400",
	), now)
	got, ok = earliestKnownReset(codex, "", now)
	if !ok || got.Unix() != now.Add(24*time.Hour).Unix() {
		t.Fatalf("codex reset = %v, %v; want secondary 24h", got, ok)
	}
}

func TestEarliestKnownResetIgnoresCooldown(t *testing.T) {
	now := time.Now()
	auth := &Auth{ID: "x", Quota: QuotaState{NextRecoverAt: now.Add(15 * time.Minute)}}
	if got, ok := earliestKnownReset(auth, "", now); ok {
		t.Fatalf("cooldown deadline ranked as weekly reset: %v", got)
	}
}

func headerOf(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}
