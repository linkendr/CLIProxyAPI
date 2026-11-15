package auth

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestQuotaRefusalSentence(t *testing.T) {
	zone := time.FixedZone("CET", 3600)
	reset := time.Date(2026, 11, 20, 16, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		limit string
		at    time.Time
		want  string
	}{
		{quotaLimitWeeklyAll, reset, "Weekly limit. Next reset Fri 20 Nov 17:00. A Fable reset does not lift it."},
		{quotaLimitWeeklyScoped, reset, "Weekly model limit. Next reset Fri 20 Nov 17:00."},
		{quotaLimitFiveHour, reset, "Five-hour limit. Next reset Fri 20 Nov 17:00."},
		{quotaLimitReserve, reset, "Quota reserve hold: the last share of the weekly quota is kept for automations. Next release Fri 20 Nov 17:00. A Fable reset does not lift it."},
		{"", reset, "Rate limit. Next retry Fri 20 Nov 17:00."},
		{quotaLimitWeeklyAll, time.Time{}, "Weekly limit. Next reset unknown. A Fable reset does not lift it."},
		{"", time.Time{}, "Rate limit. Next retry unknown."},
	} {
		if got := quotaRefusalSentence(tc.limit, tc.at, zone); got != tc.want {
			t.Errorf("sentence(%q) = %q, want %q", tc.limit, got, tc.want)
		}
	}
	// Without a zone the reset is in the machine's local time.
	want := "Five-hour limit. Next reset " + reset.In(time.Local).Format("Mon 2 Jan 15:04") + "."
	if got := quotaRefusalSentence(quotaLimitFiveHour, reset, nil); got != want {
		t.Errorf("sentence in local time = %q, want %q", got, want)
	}
}

// T3 Code derives resetAt of a failed turn from this countdown and nothing else, with
// the pattern below (t3ResetAfterPattern is the one T3 0.1.9 uses).
func TestQuotaRefusalCountdownMatchesT3Pattern(t *testing.T) {
	now := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		left time.Duration
		want string
	}{
		{5 * time.Second, " (reset after 0h 0m 5s)"},
		{3*time.Hour + 4*time.Minute + 5*time.Second, " (reset after 3h 4m 5s)"},
		{125*time.Hour + 59*time.Minute, " (reset after 125h 59m 0s)"},
		{1500 * time.Millisecond, " (reset after 0h 0m 2s)"},
		{0, " (reset after 0h 0m 1s)"},
		{-time.Hour, " (reset after 0h 0m 1s)"},
	} {
		got := quotaRefusalCountdown(now.Add(tc.left), now)
		if got != tc.want {
			t.Errorf("countdown(%s) = %q, want %q", tc.left, got, tc.want)
			continue
		}
		match := t3ResetAfterPattern.FindStringSubmatch("Weekly limit." + got)
		if match == nil {
			t.Errorf("countdown %q does not match T3's pattern", got)
			continue
		}
		hours, _ := strconv.Atoi(match[1])
		minutes, _ := strconv.Atoi(match[2])
		seconds, _ := strconv.Atoi(match[3])
		read := time.Duration(hours)*time.Hour + time.Duration(minutes)*time.Minute + time.Duration(seconds)*time.Second
		if wantRead := max(tc.left, 0); read < wantRead || read >= wantRead+time.Second+time.Nanosecond {
			t.Errorf("T3 reads %s from %q, want %s rounded up to a second", read, got, tc.left)
		}
	}
	if got := quotaRefusalCountdown(time.Time{}, now); got != "" {
		t.Errorf("countdown of an unknown reset = %q, want none", got)
	}
}

func TestQuotaLimitFromSignals(t *testing.T) {
	const prefix = "Anthropic-Ratelimit-Unified-"
	for _, tc := range []struct {
		name    string
		signals map[string]string
		want    string
	}{
		{"none", nil, ""},
		{"allowed", map[string]string{prefix + "Status": "allowed", prefix + "Representative-Claim": "seven_day"}, ""},
		{"window status wins over the claim", map[string]string{prefix + "Status": "rejected", prefix + "5h-Status": "rejected", prefix + "Representative-Claim": "seven_day"}, quotaLimitFiveHour},
		{"7d window", map[string]string{prefix + "7d-Status": " Rejected "}, quotaLimitWeeklyAll},
		{"scoped window", map[string]string{"anthropic-ratelimit-unified-7d_oi-status": "rejected"}, quotaLimitWeeklyScoped},
		{"claim five_hour", map[string]string{prefix + "Status": "rejected", prefix + "Representative-Claim": "five_hour"}, quotaLimitFiveHour},
		{"claim seven_day", map[string]string{prefix + "Status": "rejected", prefix + "Representative-Claim": "seven_day"}, quotaLimitWeeklyAll},
		{"claim of one model", map[string]string{prefix + "Status": "rejected", prefix + "Representative-Claim": "seven_day_opus"}, quotaLimitWeeklyScoped},
		{"rejected without a claim", map[string]string{prefix + "Status": "rejected"}, ""},
	} {
		if got := quotaLimitFromSignals(tc.signals); got != tc.want {
			t.Errorf("%s: limit = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A cooldown refusal names the limit that cooled the Max login and its reset in local
// time, ends with the countdown T3 reads, and carries the headers Claude Code turns
// into a rate_limit_event with resetsAt.
func TestCooldownRefusalNamesLimitAndReset(t *testing.T) {
	const prefix = "Anthropic-Ratelimit-Unified-"
	reset := time.Now().Add(100 * time.Hour).Truncate(time.Second)
	rejection := func(window string) http.Header {
		headers := unifiedHeaders(map[string]float64{window: 1.0}, reset)
		headers.Set(prefix+"Status", "rejected")
		headers.Set(prefix+window+"-Status", "rejected")
		return headers
	}
	for _, tc := range []struct {
		name  string
		cool  func(t *testing.T, rig *reserveRig, authID string)
		limit string
		claim string
	}{
		{"weekly limit", func(t *testing.T, rig *reserveRig, id string) { rig.limitAt(t, id, reset) }, quotaLimitWeeklyAll, "seven_day"},
		{"cooldown without headers", func(t *testing.T, rig *reserveRig, id string) { rig.coolDown(t, id) }, quotaLimitWeeklyAll, "seven_day"},
		{"5-hour limit", func(t *testing.T, rig *reserveRig, id string) { rig.limitWith(t, id, rejection("5h")) }, quotaLimitFiveHour, "five_hour"},
		{"model limit", func(t *testing.T, rig *reserveRig, id string) { rig.limitModel(t, id, rejection("7d_oi")) }, quotaLimitWeeklyScoped, "seven_day_overage_included"},
		{"model cooldown that names no limit", func(t *testing.T, rig *reserveRig, id string) { rig.limitModel(t, id, http.Header{}) }, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newReserveRig(t, &RoundRobinSelector{}, true, internalconfig.ReserveGateConfig{})
			rig.disable(t, rig.credit)
			tc.cool(t, rig, rig.a)
			tc.cool(t, rig, rig.b)
			rig.manager.mu.RLock()
			cooldown, cooled := quotaCooldownOf(rig.manager.auths[rig.a], reserveTestClaudeModel, time.Now())
			rig.manager.mu.RUnlock()
			if !cooled || cooldown.limit != tc.limit {
				t.Fatalf("cooldown = %+v, %v; want limit %q", cooldown, cooled, tc.limit)
			}

			_, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel)
			var refusal *modelCooldownError
			if !errors.As(err, &refusal) || refusal.StatusCode() != http.StatusTooManyRequests {
				t.Fatalf("pick = %v, want a cooldown refusal", err)
			}
			var body struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if errJSON := json.Unmarshal([]byte(err.Error()), &body); errJSON != nil || body.Error.Code != "model_cooldown" {
				t.Fatalf("refusal body = %s (%v)", err.Error(), errJSON)
			}
			sentence := quotaRefusalSentence(tc.limit, cooldown.until, time.Local)
			if !strings.HasPrefix(body.Error.Message, sentence+" (reset after ") {
				t.Fatalf("message = %q, want it to start with %q and the countdown", body.Error.Message, sentence)
			}
			if !strings.Contains(sentence, cooldown.until.In(time.Local).Format("Mon 2 Jan 15:04")) {
				t.Fatalf("sentence %q lacks the local reset time", sentence)
			}
			if fable := strings.Contains(body.Error.Message, "A Fable reset does not lift it."); fable != (tc.limit == quotaLimitWeeklyAll) {
				t.Fatalf("message %q: Fable note present = %v", body.Error.Message, fable)
			}
			requireRefusalCountdownNear(t, body.Error.Message, cooldown.until)

			headers := refusal.Headers()
			if got := headers.Get("anthropic-ratelimit-unified-status"); got != "rejected" {
				t.Fatalf("unified status = %q", got)
			}
			if got := headers.Get("anthropic-ratelimit-unified-reset"); got != strconv.FormatInt(cooldown.until.Unix(), 10) {
				t.Fatalf("unified reset = %q, want %d", got, cooldown.until.Unix())
			}
			if got := headers.Get("anthropic-ratelimit-unified-representative-claim"); got != tc.claim {
				t.Fatalf("representative claim = %q, want %q", got, tc.claim)
			}
			if seconds, errAtoi := strconv.Atoi(headers.Get("Retry-After")); errAtoi != nil || seconds < 1 {
				t.Fatalf("Retry-After = %q", headers.Get("Retry-After"))
			}
		})
	}
}

// The refusal names the Max login that serves again first. The cooldown ends at the
// retry time the executor took from the upstream response.
func TestCooldownRefusalNamesEarliestReset(t *testing.T) {
	rig := newReserveRig(t, &RoundRobinSelector{}, true, internalconfig.ReserveGateConfig{})
	rig.disable(t, rig.credit)
	for id, retry := range map[string]time.Duration{rig.a: 90 * time.Hour, rig.b: 30 * time.Hour} {
		rig.manager.MarkResult(t.Context(), Result{
			AuthID: id, Provider: "claude", Model: reserveTestClaudeModel, Success: false, RetryAfter: &retry, CredentialScope: true,
			Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "limit"},
		})
	}
	rig.manager.mu.RLock()
	early, _ := quotaCooldownOf(rig.manager.auths[rig.b], reserveTestClaudeModel, time.Now())
	rig.manager.mu.RUnlock()
	if left := time.Until(early.until); left < 29*time.Hour || left > 30*time.Hour {
		t.Fatalf("cooldown of %s ends in %s, want 30h", rig.b, left)
	}
	_, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel)
	var refusal *modelCooldownError
	if !errors.As(err, &refusal) {
		t.Fatalf("pick = %v, want a cooldown refusal", err)
	}
	if got := refusal.Headers().Get("anthropic-ratelimit-unified-reset"); got != strconv.FormatInt(early.until.Unix(), 10) {
		t.Fatalf("unified reset = %q, want the earlier reset %d", got, early.until.Unix())
	}
	if !strings.Contains(err.Error(), "Next reset "+early.until.In(time.Local).Format("Mon 2 Jan 15:04")+".") {
		t.Fatalf("refusal %q lacks the earlier reset", err.Error())
	}
}

// A cooldown with no Max login behind it keeps the text it had.
func TestCooldownRefusalOfOtherProviderUnchanged(t *testing.T) {
	rig := newReserveRig(t, &RoundRobinSelector{}, true, reserveGateOn(nil))
	retry := time.Hour
	rig.manager.MarkResult(t.Context(), Result{
		AuthID: rig.codex, Provider: "codex", Model: reserveTestGPTModel, Success: false, RetryAfter: &retry,
		Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "limit"},
	})
	_, err := rig.pick(reserveTestUserKey, "", reserveTestGPTModel)
	var refusal *modelCooldownError
	if !errors.As(err, &refusal) {
		t.Fatalf("pick = %v, want a cooldown refusal", err)
	}
	if !strings.Contains(err.Error(), "All credentials for model "+reserveTestGPTModel+" are cooling down") {
		t.Fatalf("refusal = %q, want the unchanged text", err.Error())
	}
	if got := refusal.Headers().Get("anthropic-ratelimit-unified-representative-claim"); got != "" {
		t.Fatalf("representative claim = %q, want none", got)
	}
}

// With every Max login at a real limit and no credit key, the reserve gate still stops
// the interactive caller's request, but the refusal names the real limit: the reserve
// is not what refuses the caller.
func TestReserveRefusalNamesRealLimitWhenEveryLoginIsLimited(t *testing.T) {
	rig := newReserveRig(t, &RoundRobinSelector{}, true, reserveGateOn(nil))
	rig.disable(t, rig.credit)
	rig.limitAt(t, rig.a, time.Now().Add(48*time.Hour))
	rig.limitAt(t, rig.b, time.Now().Add(72*time.Hour))
	rig.manager.mu.RLock()
	first, _ := quotaCooldownOf(rig.manager.auths[rig.a], reserveTestClaudeModel, time.Now())
	rig.manager.mu.RUnlock()

	_, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel)
	var hold *reserveHoldError
	if !errors.As(err, &hold) || !isRequestStopError(err) || hold.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("pick = %v, want the request-stopping 429 of the gate", err)
	}
	sentence := quotaRefusalSentence(quotaLimitWeeklyAll, first.until, time.Local)
	if !strings.HasPrefix(hold.message(), sentence+" (reset after ") {
		t.Fatalf("message = %q, want it to start with %q", hold.message(), sentence)
	}
	headers := hold.Headers()
	if got := headers.Get("anthropic-ratelimit-unified-reset"); got != strconv.FormatInt(first.until.Unix(), 10) {
		t.Fatalf("unified reset = %q, want %d", got, first.until.Unix())
	}
	if got := headers.Get("anthropic-ratelimit-unified-representative-claim"); got != "seven_day" {
		t.Fatalf("representative claim = %q", got)
	}

	// The exempt caller is refused with the same sentence.
	_, errExempt := rig.pick(reserveTestExemptKey, "", reserveTestClaudeModel)
	if errExempt == nil || !strings.Contains(errExempt.Error(), sentence) {
		t.Fatalf("exempt pick = %v, want the same sentence %q", errExempt, sentence)
	}
}
