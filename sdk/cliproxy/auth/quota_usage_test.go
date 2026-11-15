package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
)

// quotaUsageFake stands in for Anthropic's usage endpoint: a report or an error per
// login, a count of the reads, and the clock the read limit runs on. The clock starts at
// the real time, so a usage reading is never older than a header reading of the test.
type quotaUsageFake struct {
	mu      sync.Mutex
	now     time.Time
	reads   map[string]int
	reports map[string]quotaUsageReport
	errs    map[string]error
}

func installQuotaUsageFake(rig *reserveRig) *quotaUsageFake {
	fake := &quotaUsageFake{now: time.Now(), reads: map[string]int{}, reports: map[string]quotaUsageReport{}, errs: map[string]error{}}
	rig.manager.quotaUsage.now = fake.clock
	rig.manager.quotaUsage.read = fake.read
	return fake
}

func (f *quotaUsageFake) clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *quotaUsageFake) advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
}

func (f *quotaUsageFake) read(_ context.Context, auth *Auth) (quotaUsageReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads[auth.ID]++
	if err := f.errs[auth.ID]; err != nil {
		return quotaUsageReport{}, err
	}
	report, ok := f.reports[auth.ID]
	if !ok {
		return quotaUsageReport{}, errors.New("no report")
	}
	return report, nil
}

// report sets what the fake upstream answers for the logins: weekly usage and reset.
func (f *quotaUsageFake) report(weekly float64, reset time.Time, authIDs ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range authIDs {
		delete(f.errs, id)
		f.reports[id] = quotaUsageReport{weeklyAll: weekly, weeklyReset: reset}
	}
}

func (f *quotaUsageFake) fail(err error, authIDs ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range authIDs {
		f.errs[id] = err
	}
}

func (f *quotaUsageFake) count(authID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads[authID]
}

// forEachQuotaRig runs a case with each selector family on the pick path every request
// takes (pickNextMixed), where the usage read is wired.
func forEachQuotaRig(t *testing.T, gate internalconfig.ReserveGateConfig, run func(t *testing.T, rig *reserveRig, usage *quotaUsageFake)) {
	t.Helper()
	for name, selector := range reserveSelectors() {
		t.Run(name, func(t *testing.T) {
			rig := newReserveRig(t, selector(), true, gate)
			run(t, rig, installQuotaUsageFake(rig))
		})
	}
}

// limitModel records an upstream 429 that limits one model only, as the executor result
// path does for a rejection of a model-scoped window.
func (r *reserveRig) limitModel(t *testing.T, authID string, headers http.Header) {
	t.Helper()
	ctx := internallogging.WithResponseHeadersHolder(context.Background())
	internallogging.SetResponseHeaders(ctx, headers)
	retry := 3 * time.Hour
	r.manager.MarkResult(ctx, Result{
		AuthID: authID, Provider: "claude", Model: reserveTestClaudeModel, Success: false, RetryAfter: &retry,
		Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "limit"},
	})
}

func (r *reserveRig) cooled(authID string) bool {
	r.manager.mu.RLock()
	defer r.manager.mu.RUnlock()
	_, cooled := quotaCooldownOf(r.manager.auths[authID], "", time.Now())
	return cooled
}

func requireMaxLogin(t *testing.T, rig *reserveRig, auth *Auth, err error) {
	t.Helper()
	if err != nil || auth == nil || (auth.ID != rig.a && auth.ID != rig.b) {
		t.Fatalf("pick = %v, %v; want a Max login", auth, err)
	}
}

func TestParseQuotaUsage(t *testing.T) {
	report, err := parseQuotaUsage([]byte(`{
		"five_hour": {"utilization": 12.5, "resets_at": "2026-11-15T15:00:00.123456+00:00"},
		"seven_day": {"utilization": 73.0, "resets_at": "2026-11-20T16:00:00.987654+00:00"},
		"limits": [
			{"kind": "weekly_all", "percent": 74, "resets_at": "2026-11-21T16:00:00Z"},
			{"kind": "weekly_scoped", "percent": 20},
			{"kind": "weekly_scoped", "percent": 101.5},
			{"kind": "other", "percent": 500}
		]}`))
	if err != nil {
		t.Fatal(err)
	}
	want := quotaUsageReport{
		weeklyAll: 0.73, weeklyReset: time.Date(2026, 11, 20, 16, 0, 0, 0, time.UTC),
		weeklyScoped: 1.015, scopedKnown: true, fiveHour: 0.125, fiveHourKnown: true,
	}
	if !report.weeklyReset.Equal(want.weeklyReset) {
		t.Fatalf("weekly reset = %s, want %s", report.weeklyReset, want.weeklyReset)
	}
	report.weeklyReset = want.weeklyReset
	if report != want {
		t.Fatalf("report = %+v, want %+v", report, want)
	}

	// Without seven_day, the weekly_all entry of limits carries the weekly value.
	report, err = parseQuotaUsage([]byte(`{"five_hour": null, "limits": [{"kind": "weekly_all", "percent": 4, "resets_at": "2026-11-21T16:00:00Z"}]}`))
	if err != nil || report.weeklyAll != 0.04 || !report.weeklyReset.Equal(time.Date(2026, 11, 21, 16, 0, 0, 0, time.UTC)) || report.fiveHourKnown || report.scopedKnown {
		t.Fatalf("report from limits = %+v, %v", report, err)
	}
	// A fresh window has no reset yet.
	report, err = parseQuotaUsage([]byte(`{"seven_day": {"utilization": 0, "resets_at": null}}`))
	if err != nil || report.weeklyAll != 0 || !report.weeklyReset.IsZero() {
		t.Fatalf("report of a fresh window = %+v, %v", report, err)
	}

	for name, body := range map[string]string{
		"not JSON":                  `<html>busy</html>`,
		"empty object":              `{}`,
		"no weekly value":           `{"five_hour": {"utilization": 3}, "limits": [{"kind": "weekly_scoped", "percent": 3}]}`,
		"null utilization":          `{"seven_day": {"utilization": null}}`,
		"negative":                  `{"seven_day": {"utilization": -1}}`,
		"wrong type":                `{"seven_day": {"utilization": "73"}}`,
		"error from gateway":        `{"type": "error", "error": {"type": "authentication_error", "message": "expired"}}`,
		"window not an object":      `{"seven_day": "soon"}`,
		"malformed reset":           `{"seven_day": {"utilization": 4, "resets_at": "tomorrow"}}`,
		"empty reset":               `{"seven_day": {"utilization": 4, "resets_at": ""}}`,
		"reset not a string":        `{"seven_day": {"utilization": 4, "resets_at": 1795000000}}`,
		"malformed reset in limits": `{"limits": [{"kind": "weekly_all", "percent": 4, "resets_at": "tomorrow"}]}`,
		"limits not an array":       `{"seven_day": {"utilization": 4}, "limits": {"kind": "weekly_all"}}`,
		"limit not an object":       `{"seven_day": {"utilization": 4}, "limits": [4]}`,
		"percent not a number":      `{"seven_day": {"utilization": 4}, "limits": [{"kind": "weekly_scoped", "percent": "4"}]}`,
		"malformed five-hour reset": `{"seven_day": {"utilization": 4}, "five_hour": {"utilization": 10, "resets_at": "tomorrow"}}`,
		"malformed scoped reset":    `{"seven_day": {"utilization": 4}, "limits": [{"kind": "weekly_scoped", "percent": 10, "resets_at": "tomorrow"}]}`,
		"malformed unused reset":    `{"seven_day": {"utilization": 4}, "limits": [{"kind": "weekly_all", "percent": 4, "resets_at": "tomorrow"}]}`,
		"reset without a percent":   `{"seven_day": {"utilization": 4}, "five_hour": {"utilization": null, "resets_at": 5}}`,
	} {
		if _, errParse := parseQuotaUsage([]byte(body)); errParse == nil {
			t.Errorf("%s: parsed without error", name)
		}
	}
}

// A body with the keys of a full usage answer: no five-hour window, integer percents, and
// many keys the parser does not know. The values are invented.
func TestParseQuotaUsageRealBodyShape(t *testing.T) {
	report, err := parseQuotaUsage([]byte(`{
		"five_hour": null,
		"seven_day": {
			"utilization": 18.0, "resets_at": "2026-11-20T16:00:00.321426+00:00",
			"limit_dollars": null, "used_dollars": null, "remaining_dollars": null, "locked_reason": null
		},
		"seven_day_oauth_apps": null, "seven_day_opus": null, "seven_day_sonnet": null,
		"seven_day_cowork": null, "seven_day_omelette": null, "tangelo": null,
		"iguana_necktie": null, "omelette_promotional": null, "nimbus_quill": null,
		"cinder_cove": null, "copper_kite": null, "brass_thimble": null, "harbor_lantern": null,
		"wattle_ember": null, "promo_credits": null, "amber_ladder": null, "granite_teacup": null,
		"amber_cistern": null, "amber_cistern_offer": null, "juniper_tide": null,
		"starry_waterfall": null, "amber_gauge": null, "pace": null, "indicator": null,
		"extra_usage": null, "spend": null,
		"cedar_ember": {
			"eligible": false, "ineligible_reason": "no_plan", "at_limit": false,
			"next_grant_id": null, "weekly_resets_at": null, "cooldown_until": null, "event_props": null
		},
		"limits": [
			{
				"kind": "weekly_all", "group": "weekly", "percent": 18, "severity": "normal",
				"resets_at": "2026-11-20T16:00:00.321426+00:00", "scope": null, "is_active": false
			},
			{
				"kind": "weekly_scoped", "group": "weekly", "percent": 33, "severity": "normal",
				"resets_at": "2026-11-20T16:00:00.321579+00:00",
				"scope": {"model": {"id": null, "display_name": "Fable"}, "surface": null},
				"is_active": true
			}
		],
		"member_dashboard_available": false,
		"seven_day_breakdown": {
			"as_of": "2026-11-15T12:00:00.123456+00:00",
			"window_started_at": "2026-11-13T16:00:00.321426+00:00",
			"rows": [
				{"key": "claude_code", "display_name": "Claude Code", "percent": 100},
				{"key": "chat", "display_name": "Chats", "percent": 0},
				{"key": "cowork", "display_name": "Cowork", "percent": 0},
				{"key": "other", "display_name": "Other", "percent": 0}
			]
		},
		"weekly_scoped_shares": [
			{"limit_index": 1, "used_percent_of_weekly": 17, "allowance_percent_of_weekly": 50}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if report.weeklyAll != 0.18 {
		t.Errorf("weekly fraction = %v, want 0.18", report.weeklyAll)
	}
	if want := time.Date(2026, 11, 20, 16, 0, 0, 0, time.UTC); !report.weeklyReset.Equal(want) {
		t.Errorf("weekly reset = %s, want %s", report.weeklyReset, want)
	}
	if !report.scopedKnown || report.weeklyScoped != 0.33 {
		t.Errorf("scoped fraction = %v (known %t), want 0.33", report.weeklyScoped, report.scopedKnown)
	}
	if report.fiveHourKnown {
		t.Errorf("five-hour window = %v, want unknown", report.fiveHour)
	}
}

// The usage read never runs while a Max login can serve the caller.
func TestQuotaUsageNotReadOnNormalPath(t *testing.T) {
	forEachQuotaRig(t, reserveGateOn(nil), func(t *testing.T, rig *reserveRig, usage *quotaUsageFake) {
		usage.report(0.10, time.Now().Add(100*time.Hour), rig.a, rig.b)
		rig.observe(t, rig.a, "7d", 0.50, time.Now().Add(100*time.Hour))
		rig.limitAt(t, rig.b, time.Now().Add(50*time.Hour))
		for _, key := range []string{reserveTestUserKey, reserveTestExemptKey} {
			for i := 0; i < 3; i++ {
				auth, err := rig.pick(key, "", reserveTestClaudeModel)
				if err != nil || auth.ID != rig.a {
					t.Fatalf("pick for %q = %v, %v; want %s", key, auth, err, rig.a)
				}
			}
		}
		auth, err := rig.pick(reserveTestUserKey, "", reserveTestGPTModel)
		if err != nil || auth.ID != rig.codex {
			t.Fatalf("codex pick = %v, %v", auth, err)
		}
		if got := usage.count(rig.a) + usage.count(rig.b); got != 0 {
			t.Fatalf("usage reads = %d, want none while a Max login serves", got)
		}
		if !rig.cooled(rig.b) {
			t.Fatal("the limited login lost its cooldown without a usage read")
		}
	})
}

// Before the credit key serves, the proxy reads the real usage. Logins the upstream
// reports as reset are served again with no manual step.
func TestQuotaUsageLiftsCooldownBeforeCreditFallback(t *testing.T) {
	forEachQuotaRig(t, reserveGateOn(nil), func(t *testing.T, rig *reserveRig, usage *quotaUsageFake) {
		rig.limitAt(t, rig.a, time.Now().Add(48*time.Hour))
		rig.limitAt(t, rig.b, time.Now().Add(72*time.Hour))
		usage.report(1.0, time.Now().Add(48*time.Hour), rig.a, rig.b)
		auth, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel)
		if err != nil || auth.ID != rig.credit {
			t.Fatalf("pick with both logins still at their limit = %v, %v; want credit key", auth, err)
		}
		if usage.count(rig.a) != 1 || usage.count(rig.b) != 1 {
			t.Fatalf("usage reads = %d, %d; want one per login before the credit key serves", usage.count(rig.a), usage.count(rig.b))
		}

		// The owner resets the limit upstream; a minute later the next request sees it.
		usage.report(0.02, time.Now().Add(160*time.Hour), rig.a, rig.b)
		usage.advance(quotaUsageMinInterval)
		for _, key := range []string{reserveTestUserKey, reserveTestExemptKey} {
			auth, err = rig.pick(key, "", reserveTestClaudeModel)
			requireMaxLogin(t, rig, auth, err)
		}
		if rig.cooled(rig.a) || rig.cooled(rig.b) {
			t.Fatal("a login the upstream reports as reset is still cooled")
		}
		if usage.count(rig.a) != 2 || usage.count(rig.b) != 2 {
			t.Fatalf("usage reads = %d, %d; want two per login", usage.count(rig.a), usage.count(rig.b))
		}
	})
}

// The same before a cooldown refusal, where no credit key can serve.
func TestQuotaUsageLiftsCooldownBeforeRefusal(t *testing.T) {
	forEachQuotaRig(t, internalconfig.ReserveGateConfig{}, func(t *testing.T, rig *reserveRig, usage *quotaUsageFake) {
		rig.disable(t, rig.credit)
		rig.limitAt(t, rig.a, time.Now().Add(48*time.Hour))
		rig.limitAt(t, rig.b, time.Now().Add(72*time.Hour))
		usage.report(1.0, time.Now().Add(48*time.Hour), rig.a, rig.b)
		_, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel)
		var cooldown *modelCooldownError
		if !errors.As(err, &cooldown) || statusCodeFromError(err) != http.StatusTooManyRequests {
			t.Fatalf("pick with both logins at their limit = %v, want a cooldown refusal", err)
		}

		// Only one login was reset: it serves, the other stays cooled.
		usage.report(0.02, time.Time{}, rig.b)
		usage.advance(quotaUsageMinInterval)
		auth, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel)
		if err != nil || auth.ID != rig.b {
			t.Fatalf("pick after the upstream reset %s = %v, %v", rig.b, auth, err)
		}
		if !rig.cooled(rig.a) || rig.cooled(rig.b) {
			t.Fatalf("cooled a = %v, b = %v; want only a", rig.cooled(rig.a), rig.cooled(rig.b))
		}
	})
}

// Before a reserve hold refusal, a fresh reading replaces the one the hold rests on.
func TestQuotaUsageReplacesReadingBeforeReserveRefusal(t *testing.T) {
	forEachQuotaRig(t, reserveGateOn(nil), func(t *testing.T, rig *reserveRig, usage *quotaUsageFake) {
		reset := time.Now().Add(48 * time.Hour).Truncate(time.Second)
		rig.observe(t, rig.a, "7d", 0.98, reset)
		rig.observe(t, rig.b, "7d", 0.99, reset)
		usage.report(0.99, reset, rig.a, rig.b)
		_, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel)
		requireReserveHold(t, err)
		if usage.count(rig.a) != 1 || usage.count(rig.b) != 1 {
			t.Fatalf("usage reads = %d, %d; want one per login before the refusal", usage.count(rig.a), usage.count(rig.b))
		}

		fresh := time.Now().Add(167 * time.Hour).Truncate(time.Second)
		usage.report(0.03, fresh, rig.a)
		usage.advance(quotaUsageMinInterval)
		auth, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel)
		if err != nil || auth.ID != rig.a {
			t.Fatalf("pick after the upstream reset %s = %v, %v", rig.a, auth, err)
		}
		reading, ok := rig.manager.reserveHolds.reading(rig.a, "7d")
		if !ok || reading.utilization != 0.03 || !reading.resetAt.Equal(fresh) || reading.source != reserveReadingFromUsage {
			t.Fatalf("book reading = %+v, %v; want the usage reading", reading, ok)
		}
	})
}

// The manual hold is the operator's switch: no reading lifts it, so nothing is read.
func TestQuotaUsageNotReadUnderManualHold(t *testing.T) {
	gate := reserveGateOn(func(gate *internalconfig.ReserveGateConfig) { gate.Hold = true })
	forEachQuotaRig(t, gate, func(t *testing.T, rig *reserveRig, usage *quotaUsageFake) {
		usage.report(0.02, time.Now().Add(100*time.Hour), rig.a, rig.b)
		if _, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel); err == nil || !strings.Contains(err.Error(), "paused by the operator") {
			t.Fatalf("pick under the manual hold = %v", err)
		}
		if got := usage.count(rig.a) + usage.count(rig.b); got != 0 {
			t.Fatalf("usage reads = %d, want none under the manual hold", got)
		}
	})
}

// A failed, slow or unparsable read changes nothing: the cooldown, the readings and the
// outcome of the request stay as they were.
func TestQuotaUsageFailedReadChangesNothing(t *testing.T) {
	cases := map[string]func(t *testing.T, rig *reserveRig, usage *quotaUsageFake){
		"error": func(_ *testing.T, rig *reserveRig, usage *quotaUsageFake) {
			usage.fail(errors.New("usage endpoint answered HTTP 500"), rig.a, rig.b)
		},
		"slow": func(_ *testing.T, rig *reserveRig, _ *quotaUsageFake) {
			rig.manager.quotaUsage.timeout = time.Millisecond
			rig.manager.quotaUsage.read = func(ctx context.Context, _ *Auth) (quotaUsageReport, error) {
				<-ctx.Done()
				return quotaUsageReport{}, ctx.Err()
			}
		},
		"unparsable": func(t *testing.T, rig *reserveRig, _ *quotaUsageFake) {
			rig.serveUsage(t, `{"seven_day": "soon"}`)
		},
		// The body reports room, but its reset is not a time: the report is refused whole.
		"malformed reset": func(t *testing.T, rig *reserveRig, _ *quotaUsageFake) {
			rig.serveUsage(t, `{"seven_day": {"utilization": 4.0, "resets_at": "tomorrow"}}`)
		},
		// The body reports room in a weekly window that already ended: it is not a report of
		// the running window, so it proves nothing.
		"weekly reset in the past": func(t *testing.T, rig *reserveRig, _ *quotaUsageFake) {
			rig.serveUsage(t, `{"seven_day": {"utilization": 4.0, "resets_at": "2020-01-01T00:00:00Z"}}`)
		},
		// The endpoint would report room, but the configured proxy cannot be used: the read
		// fails and no request leaves by another route.
		"unusable proxy-url": func(t *testing.T, rig *reserveRig, _ *quotaUsageFake) {
			requests := rig.serveUsage(t, `{"seven_day": {"utilization": 4.0, "resets_at": "2099-01-01T00:00:00Z"}}`)
			cfg, _ := rig.manager.runtimeConfig.Load().(*internalconfig.Config)
			cfg.ProxyURL = "ftp://proxy.invalid:1"
			t.Cleanup(func() {
				if n := requests.Load(); n != 0 {
					t.Errorf("%d usage requests left without the configured proxy, want 0", n)
				}
			})
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			rig := newReserveRig(t, &RoundRobinSelector{}, true, reserveGateOn(nil))
			usage := installQuotaUsageFake(rig)
			reset := time.Now().Add(48 * time.Hour).Truncate(time.Second)
			rig.limitAt(t, rig.a, reset)
			rig.limitAt(t, rig.b, reset)
			before, _ := rig.manager.reserveHolds.reading(rig.a, "7d")
			setup(t, rig, usage)

			auth, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel)
			if err != nil || auth.ID != rig.credit {
				t.Fatalf("pick = %v, %v; want the credit key as before", auth, err)
			}
			if !rig.cooled(rig.a) || !rig.cooled(rig.b) {
				t.Fatal("a failed read cleared a cooldown")
			}
			if after, _ := rig.manager.reserveHolds.reading(rig.a, "7d"); after != before {
				t.Fatalf("book reading changed from %+v to %+v", before, after)
			}

			// Without the credit key the refusal stands too. The exempt caller is asked: the
			// reserve gate never holds it, so its refusal is the cooldown itself.
			rig.disable(t, rig.credit)
			usage.advance(quotaUsageMinInterval)
			_, err = rig.pick(reserveTestExemptKey, "", reserveTestClaudeModel)
			var cooldown *modelCooldownError
			if !errors.As(err, &cooldown) {
				t.Fatalf("pick without the credit key = %v, want the cooldown refusal as before", err)
			}
			if !rig.cooled(rig.a) || !rig.cooled(rig.b) {
				t.Fatal("a failed read before the refusal cleared a cooldown")
			}
		})
	}
}

// serveUsage makes the real read answer every login with body, and counts the requests.
func (r *reserveRig) serveUsage(t *testing.T, body string) *atomic.Int32 {
	t.Helper()
	requests := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	r.setAccessTokens(t)
	r.manager.quotaUsage.url = server.URL
	r.manager.quotaUsage.read = nil
	return requests
}

// A proxy-url that cannot be used fails the read by name, for the login's own setting and
// for the global one, before any request is sent.
func TestQuotaUsageReadFailsOnUnusableProxy(t *testing.T) {
	for name, tc := range map[string]struct {
		login, global, want string
	}{
		"login proxy-url":  {"ftp://proxy.invalid:1", "", "login proxy-url transport: "},
		"global proxy-url": {"", "ftp://proxy.invalid:1", "proxy-url transport: "},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newReserveRig(t, &RoundRobinSelector{}, true, reserveGateOn(nil))
			requests := rig.serveUsage(t, `{"seven_day": {"utilization": 4.0}}`)
			cfg, _ := rig.manager.runtimeConfig.Load().(*internalconfig.Config)
			cfg.ProxyURL = tc.global
			auth := &Auth{ID: rig.a, ProxyURL: tc.login, Metadata: map[string]any{"access_token": "t"}}
			_, err := rig.manager.readQuotaUsage(context.Background(), auth)
			if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
				t.Fatalf("read = %v, want an error starting %q", err, tc.want)
			}
			if n := requests.Load(); n != 0 {
				t.Fatalf("%d usage requests sent, want 0", n)
			}
			cfg.ProxyURL = ""
			auth.ProxyURL = ""
			if report, errRead := rig.manager.readQuotaUsage(context.Background(), auth); errRead != nil || report.weeklyAll != 0.04 || requests.Load() != 1 {
				t.Fatalf("read without a proxy-url = %+v, %v after %d requests; want 4%% from one request", report, errRead, requests.Load())
			}
		})
	}
}

// setAccessTokens gives the Max logins the token the usage read sends.
func (r *reserveRig) setAccessTokens(t *testing.T) {
	t.Helper()
	r.manager.mu.Lock()
	defer r.manager.mu.Unlock()
	for _, id := range []string{r.a, r.b} {
		r.manager.auths[id].Metadata = map[string]any{"access_token": "token-of-" + id, "email": "owner@example.com"}
	}
}

// The read itself: the login's own token, the beta header, and the body applied.
func TestQuotaUsageReadSendsLoginToken(t *testing.T) {
	rig := newReserveRig(t, &RoundRobinSelector{}, true, reserveGateOn(nil))
	var mu sync.Mutex
	seen := map[string]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.Header.Get("Authorization")] = r.Method + " " + r.URL.RequestURI() + " " + r.Header.Get("anthropic-beta")
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer token-of-"+rig.a {
			http.Error(w, `{"type":"error"}`, http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"five_hour": null, "seven_day": {"utilization": 4.0, "resets_at": "2099-01-01T00:00:00Z"}}`))
	}))
	t.Cleanup(server.Close)
	rig.setAccessTokens(t)
	rig.manager.quotaUsage.url = server.URL + "/api/oauth/usage?cedar_ember=1&skip_spend=1"
	rig.limitAt(t, rig.a, time.Now().Add(48*time.Hour))
	rig.limitAt(t, rig.b, time.Now().Add(48*time.Hour))

	auth, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel)
	if err != nil || auth.ID != rig.a {
		t.Fatalf("pick = %v, %v; want %s, the login the upstream reports as reset", auth, err, rig.a)
	}
	if !rig.cooled(rig.b) {
		t.Fatal("the login whose read failed (HTTP 500) lost its cooldown")
	}
	want := "GET /api/oauth/usage?cedar_ember=1&skip_spend=1 oauth-2025-04-20"
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen["Bearer token-of-"+rig.a] != want || seen["Bearer token-of-"+rig.b] != want {
		t.Fatalf("usage requests = %v, want one %q per login with its own token", seen, want)
	}
}

// A test binary never reaches the real endpoint, and a login without a token is not read.
func TestQuotaUsageReadRefusedWithoutOverride(t *testing.T) {
	rig := newReserveRig(t, &RoundRobinSelector{}, true, reserveGateOn(nil))
	if _, err := rig.manager.readQuotaUsage(context.Background(), &Auth{ID: rig.a}); err == nil || !strings.Contains(err.Error(), "no access token") {
		t.Fatalf("read without a token = %v", err)
	}
	if _, err := rig.manager.readQuotaUsage(context.Background(), &Auth{ID: rig.a, Metadata: map[string]any{"access_token": "t"}}); err == nil || !strings.Contains(err.Error(), "off in a test binary") {
		t.Fatalf("read of the real endpoint from a test = %v", err)
	}
}

// At most one read per login per 60 seconds, whatever its outcome.
func TestQuotaUsageReadLimit(t *testing.T) {
	rig := newReserveRig(t, &RoundRobinSelector{}, true, reserveGateOn(nil))
	usage := installQuotaUsageFake(rig)
	rig.limitAt(t, rig.a, time.Now().Add(48*time.Hour))
	rig.limitAt(t, rig.b, time.Now().Add(48*time.Hour))
	usage.report(1.0, time.Now().Add(48*time.Hour), rig.a)
	usage.fail(errors.New("down"), rig.b)

	requireReads := func(want int) {
		t.Helper()
		if usage.count(rig.a) != want || usage.count(rig.b) != want {
			t.Fatalf("usage reads = %d, %d; want %d per login", usage.count(rig.a), usage.count(rig.b), want)
		}
	}
	for i := 0; i < 5; i++ {
		if auth, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel); err != nil || auth.ID != rig.credit {
			t.Fatalf("pick #%d = %v, %v", i, auth, err)
		}
	}
	requireReads(1)
	usage.advance(quotaUsageMinInterval - time.Second)
	_, _ = rig.pick(reserveTestUserKey, "", reserveTestClaudeModel)
	requireReads(1)
	usage.advance(time.Second)
	_, _ = rig.pick(reserveTestExemptKey, "", reserveTestClaudeModel)
	_, _ = rig.pick(reserveTestUserKey, "", reserveTestClaudeModel)
	requireReads(2)

	// The upstream reset inside the 60 seconds is seen by the first request after them.
	usage.report(0.01, time.Time{}, rig.a, rig.b)
	if auth, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel); err != nil || auth.ID != rig.credit {
		t.Fatalf("pick inside the 60 seconds = %v, %v; want the credit key", auth, err)
	}
	requireReads(2)
	usage.advance(quotaUsageMinInterval)
	auth, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel)
	requireMaxLogin(t, rig, auth, err)
	requireReads(3)
}

// Concurrent requests share one in-flight read and its outcome.
func TestQuotaUsageConcurrentRequestsShareOneRead(t *testing.T) {
	rig := newReserveRig(t, &RoundRobinSelector{}, true, reserveGateOn(nil))
	rig.observe(t, rig.a, "7d", 0.99, time.Now().Add(48*time.Hour))
	rig.manager.mu.RLock()
	login := rig.manager.auths[rig.a].Clone()
	rig.manager.mu.RUnlock()

	synctest.Test(t, func(t *testing.T) {
		const requests = 8
		reads := 0
		release := make(chan struct{})
		rig.manager.quotaUsage.now = time.Now
		rig.manager.quotaUsage.read = func(context.Context, *Auth) (quotaUsageReport, error) {
			reads++
			<-release
			return quotaUsageReport{weeklyAll: 0.05}, nil
		}
		results := make(chan bool, requests+1)
		for i := 0; i < requests; i++ {
			go func() { results <- rig.manager.refreshQuotaUsageOf(context.Background(), login) }()
		}
		// A request that gives up does not end the shared read.
		gone, cancel := context.WithCancel(context.Background())
		go func() { results <- rig.manager.refreshQuotaUsageOf(gone, login) }()
		synctest.Wait()
		cancel()
		synctest.Wait()
		if reads != 1 || len(results) > 1 {
			t.Fatalf("with the read in flight: reads = %d, returned = %d; want 1 read and at most the cancelled request back", reads, len(results))
		}
		close(release)
		applied := 0
		for i := 0; i < requests+1; i++ {
			if <-results {
				applied++
			}
		}
		if applied < requests || reads != 1 {
			t.Fatalf("requests that saw the fresh reading = %d of %d, reads = %d; want all to share one read", applied, requests, reads)
		}
		// Inside the 60 seconds a new request reads nothing.
		if rig.manager.refreshQuotaUsageOf(context.Background(), login) || reads != 1 {
			t.Fatalf("a request right after the read: reads = %d, want no new read", reads)
		}
		time.Sleep(quotaUsageMinInterval) // the bubble's clock, not the wall clock
		go func() { results <- rig.manager.refreshQuotaUsageOf(context.Background(), login) }()
		synctest.Wait()
		if reads != 2 {
			t.Fatalf("reads after 60 seconds = %d, want 2", reads)
		}
		<-results
	})
}

// A cooldown is lifted only when the limit behind it has proven room. The usage body
// may not carry the 5-hour window or the model-scoped weekly limit.
func TestQuotaUsageLiftsOnlyTheLimitWithRoom(t *testing.T) {
	reset := time.Now().Add(48 * time.Hour)
	sharedRejection := func(window string) http.Header {
		headers := unifiedHeaders(map[string]float64{"7d": 0.40}, reset)
		headers.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		headers.Set("Anthropic-Ratelimit-Unified-"+window+"-Status", "rejected")
		return headers
	}
	room := quotaUsageReport{weeklyAll: 0.40}
	cases := []struct {
		name   string
		cool   func(t *testing.T, rig *reserveRig)
		report quotaUsageReport
		lifted bool
	}{
		{"weekly limit, weekly room", func(t *testing.T, rig *reserveRig) { rig.limitAt(t, rig.a, reset) }, room, true},
		{"weekly limit, still full", func(t *testing.T, rig *reserveRig) { rig.limitAt(t, rig.a, reset) }, quotaUsageReport{weeklyAll: 1}, false},
		{"weekly limit, over full", func(t *testing.T, rig *reserveRig) { rig.limitAt(t, rig.a, reset) }, quotaUsageReport{weeklyAll: 1.02}, false},
		{"cooldown without headers names no limit", func(t *testing.T, rig *reserveRig) { rig.coolDown(t, rig.a) }, room, false},
		{"5-hour limit, window not reported", func(t *testing.T, rig *reserveRig) { rig.limitWith(t, rig.a, sharedRejection("5h")) }, room, false},
		{"5-hour limit, window full", func(t *testing.T, rig *reserveRig) { rig.limitWith(t, rig.a, sharedRejection("5h")) },
			quotaUsageReport{weeklyAll: 0.40, fiveHour: 1, fiveHourKnown: true}, false},
		{"5-hour limit, window has room", func(t *testing.T, rig *reserveRig) { rig.limitWith(t, rig.a, sharedRejection("5h")) },
			quotaUsageReport{weeklyAll: 0.40, fiveHour: 0.10, fiveHourKnown: true}, true},
		{"model limit, scoped limit not reported", func(t *testing.T, rig *reserveRig) { rig.limitModel(t, rig.a, sharedRejection("7d_oi")) }, room, false},
		{"model limit, scoped limit full", func(t *testing.T, rig *reserveRig) { rig.limitModel(t, rig.a, sharedRejection("7d_oi")) },
			quotaUsageReport{weeklyAll: 0.40, weeklyScoped: 1, scopedKnown: true}, false},
		{"model limit, scoped limit has room", func(t *testing.T, rig *reserveRig) { rig.limitModel(t, rig.a, sharedRejection("7d_oi")) },
			quotaUsageReport{weeklyAll: 0.40, weeklyScoped: 0.30, scopedKnown: true}, true},
		{"model cooldown that names no limit", func(t *testing.T, rig *reserveRig) { rig.limitModel(t, rig.a, http.Header{}) }, room, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newReserveRig(t, &RoundRobinSelector{}, true, internalconfig.ReserveGateConfig{})
			tc.cool(t, rig)
			if !rig.cooled(rig.a) {
				t.Fatal("setup left the login without a quota cooldown")
			}
			now := time.Now()
			rig.manager.applyQuotaUsage(context.Background(), rig.a, tc.report, rig.manager.quotaUsageStart(rig.a, now), now)
			if got := !rig.cooled(rig.a); got != tc.lifted {
				t.Fatalf("cooldown lifted = %v, want %v", got, tc.lifted)
			}
		})
	}
}

// A response that completes after the 429 replaces the header snapshot of the login. The
// cooldown keeps the headers it was recorded with, so it is still a 5-hour cooldown and
// a report with weekly room alone does not lift it.
func TestQuotaUsageLateResponseDoesNotRenameCooldown(t *testing.T) {
	forEachQuotaRig(t, internalconfig.ReserveGateConfig{}, func(t *testing.T, rig *reserveRig, usage *quotaUsageFake) {
		rig.disable(t, rig.b)
		rig.disable(t, rig.credit)
		reset := time.Now().Add(48 * time.Hour).Truncate(time.Second)
		rejected := unifiedHeaders(map[string]float64{"7d": 0.40}, reset)
		rejected.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		rejected.Set("Anthropic-Ratelimit-Unified-5h-Status", "rejected")
		rig.limitWith(t, rig.a, rejected)
		rig.manager.mu.Lock()
		late := &rig.manager.auths[rig.a].Quota
		late.Signals = map[string]string{"anthropic-ratelimit-unified-status": "allowed"}
		rig.manager.mu.Unlock()

		usage.report(0.40, reset, rig.a)
		_, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel)
		var cooldown *modelCooldownError
		if !errors.As(err, &cooldown) {
			t.Fatalf("pick = %v, want the cooldown refusal", err)
		}
		if usage.count(rig.a) != 1 || !rig.cooled(rig.a) {
			t.Fatalf("usage reads = %d, cooled = %v; want one read and the 5-hour cooldown kept", usage.count(rig.a), rig.cooled(rig.a))
		}
		if !strings.Contains(err.Error(), "Five-hour limit.") {
			t.Fatalf("refusal = %q, want it to name the five-hour limit", err.Error())
		}
	})
}

// A 429 that arrives while the usage read is under way is newer than the report: the
// report does not lift its cooldown or replace its reading. The next read does.
func TestQuotaUsageLimitDuringReadKeepsCooldown(t *testing.T) {
	forEachQuotaRig(t, reserveGateOn(nil), func(t *testing.T, rig *reserveRig, usage *quotaUsageFake) {
		rig.disable(t, rig.b)
		rig.disable(t, rig.credit)
		rig.limitAt(t, rig.a, time.Now().Add(48*time.Hour).Truncate(time.Second))
		during := time.Now().Add(160 * time.Hour).Truncate(time.Second)
		usage.report(0.02, during, rig.a)
		rig.manager.quotaUsage.read = func(ctx context.Context, auth *Auth) (quotaUsageReport, error) {
			rig.limitAt(t, auth.ID, during)
			return usage.read(ctx, auth)
		}

		_, err := rig.pick(reserveTestExemptKey, "", reserveTestClaudeModel)
		var cooldown *modelCooldownError
		if !errors.As(err, &cooldown) {
			t.Fatalf("pick = %v, want the cooldown refusal: the 429 is newer than the report", err)
		}
		if !rig.cooled(rig.a) {
			t.Fatal("a report older than the 429 cleared its cooldown")
		}
		reading, ok := rig.manager.reserveHolds.reading(rig.a, "7d")
		if !ok || reading.utilization != 1 || !reading.resetAt.Equal(during) || reading.source == reserveReadingFromUsage {
			t.Fatalf("book reading = %+v, %v; want the header reading of the 429", reading, ok)
		}

		rig.manager.quotaUsage.read = usage.read
		usage.advance(quotaUsageMinInterval)
		auth, err := rig.pick(reserveTestExemptKey, "", reserveTestClaudeModel)
		if err != nil || auth.ID != rig.a {
			t.Fatalf("pick after an undisturbed read = %v, %v; want %s", auth, err, rig.a)
		}
	})
}

// usageWindowCase is one window of a usage answer that the parser knows.
type usageWindowCase struct {
	path    string // the path the parser names in an error
	percent string // the key of the percent
	kind    string // the kind of a limits entry; empty for a top-level window
}

var usageWindowCases = []usageWindowCase{
	{"seven_day", "utilization", ""},
	{"five_hour", "utilization", ""},
	{"limits.0", "percent", quotaLimitWeeklyAll},
	{"limits.1", "percent", quotaLimitWeeklyScoped},
}

const usageValidReset = `"2099-01-01T00:00:00Z"`

func usageWindowJSON(w usageWindowCase, percent, reset string) string {
	kind := ""
	if w.kind != "" {
		kind = fmt.Sprintf(`"kind": %q, `, w.kind)
	}
	return fmt.Sprintf(`{%s%q: %s, "resets_at": %s}`, kind, w.percent, percent, reset)
}

// usageBody is a usage answer that reports room (10%) in every window the parser knows.
// replace maps the path of a window, or "limits", to the JSON written in its place.
func usageBody(replace map[string]string) string {
	windows := make([]string, len(usageWindowCases))
	for i, w := range usageWindowCases {
		windows[i] = usageWindowJSON(w, "10", usageValidReset)
		if other, replaced := replace[w.path]; replaced {
			windows[i] = other
		}
	}
	limits := "[" + windows[2] + ", " + windows[3] + "]"
	if other, replaced := replace["limits"]; replaced {
		limits = other
	}
	return fmt.Sprintf(`{"seven_day": %s, "five_hour": %s, "limits": %s}`, windows[0], windows[1], limits)
}

// malformedUsageBodies is every body that differs from usageBody(nil) by one value of the
// wrong form: the body, limits, and for each window the parser knows the window itself,
// its kind, its percent and its reset, the reset with and without a percent beside it.
// The key is the path of that value, then the value.
func malformedUsageBodies() map[string]string {
	bodies := map[string]string{}
	for _, bad := range []string{`<html>busy</html>`, ``, `null`, `4`, `"soon"`, `true`, `[]`} {
		bodies["body = "+bad] = bad
	}
	for _, bad := range []string{`4`, `"soon"`, `true`, `{}`} {
		bodies["limits = "+bad] = usageBody(map[string]string{"limits": bad})
	}
	for _, w := range usageWindowCases {
		for _, bad := range []string{`4`, `"soon"`, `true`, `[]`} {
			bodies[w.path+" = "+bad] = usageBody(map[string]string{w.path: bad})
		}
		// A kind that is a string the parser does not know is another limit, not an error.
		for _, bad := range []string{`4`, `true`, `{}`, `[]`} {
			if w.kind != "" {
				entry := fmt.Sprintf(`{"kind": %s, "percent": 10, "resets_at": %s}`, bad, usageValidReset)
				bodies[w.path+".kind = "+bad] = usageBody(map[string]string{w.path: entry})
			}
		}
		for _, bad := range []string{`"4"`, `true`, `{}`, `[4]`, `-1`} {
			bodies[w.path+"."+w.percent+" = "+bad] = usageBody(map[string]string{w.path: usageWindowJSON(w, bad, usageValidReset)})
		}
		for _, bad := range []string{`"tomorrow"`, `""`, `"2099-01-01"`, `1795000000`, `true`, `{}`, `[]`} {
			bodies[w.path+".resets_at = "+bad] = usageBody(map[string]string{w.path: usageWindowJSON(w, "10", bad)})
			bodies[w.path+".resets_at = "+bad+", no percent"] = usageBody(map[string]string{w.path: usageWindowJSON(w, "null", bad)})
		}
	}
	return bodies
}

// The body the table is built on parses, and every body of the table fails on the value
// it replaced.
func TestParseQuotaUsageRejectsEveryMalformedValue(t *testing.T) {
	report, err := parseQuotaUsage([]byte(usageBody(nil)))
	want := quotaUsageReport{weeklyAll: 0.10, weeklyScoped: 0.10, scopedKnown: true, fiveHour: 0.10, fiveHourKnown: true}
	want.weeklyReset = report.weeklyReset
	if err != nil || report != want || report.weeklyReset.IsZero() {
		t.Fatalf("valid body = %+v, %v; want %+v", report, err, want)
	}
	for name, body := range malformedUsageBodies() {
		_, errParse := parseQuotaUsage([]byte(body))
		if errParse == nil {
			t.Errorf("%s: parsed without error", name)
			continue
		}
		path, _, _ := strings.Cut(name, " = ")
		if path != "body" && !strings.Contains(errParse.Error(), path) {
			t.Errorf("%s: error %q does not name %s", name, errParse, path)
		}
	}
}

// A body that reports room in every window lifts a five-hour cooldown and a model
// cooldown. Every body of the malformed table lifts neither: the credit key serves, as
// without a read.
func TestQuotaUsageMalformedValueKeepsCooldown(t *testing.T) {
	rejection := func(window string) http.Header {
		headers := unifiedHeaders(map[string]float64{"7d": 0.40}, time.Now().Add(48*time.Hour))
		headers.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		headers.Set("Anthropic-Ratelimit-Unified-"+window+"-Status", "rejected")
		return headers
	}
	for limit, cool := range map[string]func(t *testing.T, rig *reserveRig, id string){
		"5-hour limit": func(t *testing.T, rig *reserveRig, id string) { rig.limitWith(t, id, rejection("5h")) },
		"model limit":  func(t *testing.T, rig *reserveRig, id string) { rig.limitModel(t, id, rejection("7d_oi")) },
	} {
		pick := func(t *testing.T, body string) (*reserveRig, *Auth, error) {
			rig := newReserveRig(t, &RoundRobinSelector{}, true, reserveGateOn(nil))
			cool(t, rig, rig.a)
			cool(t, rig, rig.b)
			requests := rig.serveUsage(t, body)
			auth, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel)
			if requests.Load() == 0 {
				t.Fatal("no usage read before the fallback")
			}
			return rig, auth, err
		}
		t.Run(limit+", valid body", func(t *testing.T) {
			rig, auth, err := pick(t, usageBody(nil))
			requireMaxLogin(t, rig, auth, err)
		})
		for name, body := range malformedUsageBodies() {
			t.Run(limit+", "+name, func(t *testing.T) {
				rig, auth, err := pick(t, body)
				if err != nil || auth.ID != rig.credit {
					t.Fatalf("pick = %v, %v; want the credit key, as without a read", auth, err)
				}
				if !rig.cooled(rig.a) || !rig.cooled(rig.b) {
					t.Fatal("a malformed body cleared a cooldown")
				}
			})
		}
	}
}

// limitWith records an upstream 429 on a shared window with the given response headers.
func (r *reserveRig) limitWith(t *testing.T, authID string, headers http.Header) {
	t.Helper()
	ctx := internallogging.WithResponseHeadersHolder(context.Background())
	internallogging.SetResponseHeaders(ctx, headers)
	retry := 3 * time.Hour
	r.manager.MarkResult(ctx, Result{
		AuthID: authID, Provider: "claude", Model: reserveTestClaudeModel, Success: false, RetryAfter: &retry, CredentialScope: true,
		Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "limit"},
	})
}

// The condition of a guarded reset sees the login as it is when the reset would run. A
// refused reset changes nothing.
func TestResetQuotaIfRefusedKeepsCooldown(t *testing.T) {
	rig := newReserveRig(t, &RoundRobinSelector{}, true, reserveGateOn(nil))
	rig.limitAt(t, rig.a, time.Now().Add(48*time.Hour).Truncate(time.Second))
	before, _ := rig.manager.reserveHolds.reading(rig.a, "7d")
	seen := false
	auth, _, err := rig.manager.resetQuotaIf(context.Background(), rig.a, func(auth *Auth) bool {
		seen = auth.ID == rig.a && reserveAtRealLimit(auth, time.Now())
		return false
	})
	if err != nil || auth != nil || !seen {
		t.Fatalf("refused reset = %v, %v, saw the cooled login = %v; want no auth, no error, true", auth, err, seen)
	}
	if after, ok := rig.manager.reserveHolds.reading(rig.a, "7d"); !rig.cooled(rig.a) || !ok || after != before {
		t.Fatalf("after a refused reset: cooled = %v, reading = %+v, %v; want the cooldown and %+v", rig.cooled(rig.a), after, ok, before)
	}
	auth, _, err = rig.manager.resetQuotaIf(context.Background(), rig.a, func(*Auth) bool { return true })
	if err != nil || auth == nil || rig.cooled(rig.a) {
		t.Fatalf("allowed reset = %v, %v, cooled = %v; want the login without a cooldown", auth, err, rig.cooled(rig.a))
	}
}

// The management cooldown reset alone is enough: it drops the stale reserve reading and
// the header snapshot the reading came from, so the login is not held on old usage.
func TestResetQuotaDropsReserveReading(t *testing.T) {
	forEachQuotaRig(t, reserveGateOn(nil), func(t *testing.T, rig *reserveRig, usage *quotaUsageFake) {
		reset := time.Now().Add(48 * time.Hour).Truncate(time.Second)
		rig.limitAt(t, rig.a, reset)
		rig.disable(t, rig.b)
		rig.disable(t, rig.credit)
		if _, ok := rig.manager.reserveHolds.reading(rig.a, "7d"); !ok {
			t.Fatal("setup: the 429 left no reserve reading")
		}

		if _, _, err := rig.manager.ResetQuota(context.Background(), rig.a); err != nil {
			t.Fatal(err)
		}
		if reading, ok := rig.manager.reserveHolds.reading(rig.a, "7d"); ok {
			t.Fatalf("reserve reading after the reset = %+v, want none", reading)
		}
		rig.manager.mu.RLock()
		_, live := latestReserveReading(rig.manager.auths[rig.a], "7d")
		rig.manager.mu.RUnlock()
		if live {
			t.Fatal("the header snapshot of the old window survived the reset")
		}
		auth, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel)
		if err != nil || auth.ID != rig.a {
			t.Fatalf("interactive pick after the reset = %v, %v; want %s", auth, err, rig.a)
		}
		if got := usage.count(rig.a); got != 0 {
			t.Fatalf("usage reads = %d, want none: the reset alone is enough", got)
		}
	})
}
