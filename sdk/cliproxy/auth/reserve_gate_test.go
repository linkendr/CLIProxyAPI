package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	cliproxysession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
)

const (
	reserveTestClaudeModel = "reserve-gate-claude-model"
	reserveTestGPTModel    = "reserve-gate-gpt-model"
	reserveTestExemptKey   = "reserve-gate-automation-key"
	reserveTestUserKey     = "reserve-gate-interactive-key"
)

var reserveRigSeq atomic.Int64

// reserveRig holds two Claude OAuth accounts (a, b), a paid Claude credit key with
// fallback-when: limit (priority -10) and one Codex account.
type reserveRig struct {
	manager *Manager
	a, b    string
	credit  string
	codex   string
	mixed   bool
	calls   atomic.Int64
}

type reserveTestExecutor struct {
	id  string
	rig *reserveRig
}

func (e *reserveTestExecutor) Identifier() string { return e.id }

func (e *reserveTestExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.rig.calls.Add(1)
	return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
}

func (e *reserveTestExecutor) ExecuteStream(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.rig.calls.Add(1)
	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	ch <- cliproxyexecutor.StreamChunk{Payload: []byte(auth.ID)}
	close(ch)
	return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
}

func (e *reserveTestExecutor) CountTokens(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.rig.calls.Add(1)
	return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
}

func (e *reserveTestExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) { return auth, nil }

func (e *reserveTestExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func reserveGateOn(mutate func(*internalconfig.ReserveGateConfig)) internalconfig.ReserveGateConfig {
	gate := internalconfig.ReserveGateConfig{Enabled: true, ExemptAPIKeys: []string{reserveTestExemptKey}}
	if mutate != nil {
		mutate(&gate)
	}
	return gate
}

func newReserveRig(t *testing.T, selector Selector, mixed bool, gate internalconfig.ReserveGateConfig) *reserveRig {
	t.Helper()
	prefix := fmt.Sprintf("reserve-%d-", reserveRigSeq.Add(1))
	return newReserveRigWithConfig(t, selector, mixed, &internalconfig.Config{Routing: internalconfig.RoutingConfig{ReserveGate: gate}}, prefix)
}

// newReserveRigWithConfig builds a rig whose credential IDs start with prefix, so a
// second rig with the same prefix and config stands in for the proxy after a restart.
func newReserveRigWithConfig(t *testing.T, selector Selector, mixed bool, cfg *internalconfig.Config, prefix string) *reserveRig {
	t.Helper()
	if stopper, ok := selector.(interface{ Stop() }); ok {
		t.Cleanup(stopper.Stop)
	}
	rig := &reserveRig{a: prefix + "a", b: prefix + "b", credit: prefix + "credit", codex: prefix + "codex", mixed: mixed}
	manager := NewManager(nil, selector, nil)
	manager.SetConfig(cfg)
	manager.RegisterExecutor(&reserveTestExecutor{id: "claude", rig: rig})
	manager.RegisterExecutor(&reserveTestExecutor{id: "codex", rig: rig})
	rig.manager = manager
	for _, entry := range []struct {
		auth  *Auth
		model string
	}{
		{&Auth{ID: rig.a, Provider: "claude", Status: StatusActive, Attributes: map[string]string{"auth_kind": "oauth"}}, reserveTestClaudeModel},
		{&Auth{ID: rig.b, Provider: "claude", Status: StatusActive, Attributes: map[string]string{"auth_kind": "oauth"}}, reserveTestClaudeModel},
		{&Auth{ID: rig.credit, Provider: "claude", Status: StatusActive, Attributes: map[string]string{
			"api_key": "credit", "priority": "-10", AttributeFallbackWhen: FallbackWhenLimit,
		}}, reserveTestClaudeModel},
		{&Auth{ID: rig.codex, Provider: "codex", Status: StatusActive, Attributes: map[string]string{"auth_kind": "oauth"}}, reserveTestGPTModel},
	} {
		if _, err := manager.Register(WithSkipPersist(context.Background()), entry.auth); err != nil {
			t.Fatalf("Register(%s): %v", entry.auth.ID, err)
		}
		registry.GetGlobalRegistry().RegisterClient(entry.auth.ID, entry.auth.Provider, []*registry.ModelInfo{{ID: entry.model}})
		id := entry.auth.ID
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	return rig
}

func reserveOpts(clientKey, session string) cliproxyexecutor.Options {
	meta := map[string]any{}
	if clientKey != "" {
		meta[cliproxyexecutor.CallerScopeMetadataKey] = cliproxysession.CallerScope(clientKey)
	}
	if session != "" {
		meta[cliproxyexecutor.DerivedSessionIDMetadataKey] = session
	}
	return cliproxyexecutor.Options{Metadata: meta}
}

func (r *reserveRig) pick(clientKey, session, model string) (*Auth, error) {
	ctx := context.Background()
	if r.mixed {
		provider := "claude"
		if model == reserveTestGPTModel {
			provider = "codex"
		}
		auth, _, _, err := r.manager.pickNextMixed(ctx, []string{provider}, model, reserveOpts(clientKey, session), nil)
		return auth, err
	}
	provider := "claude"
	if model == reserveTestGPTModel {
		provider = "codex"
	}
	auth, _, err := r.manager.pickNext(ctx, provider, model, reserveOpts(clientKey, session), nil)
	return auth, err
}

// picked returns the set of credentials picked over n requests without a session.
func (r *reserveRig) picked(t *testing.T, clientKey string, n int) map[string]int {
	t.Helper()
	seen := map[string]int{}
	for i := 0; i < n; i++ {
		auth, err := r.pick(clientKey, "", reserveTestClaudeModel)
		if err != nil {
			t.Fatalf("pick #%d for %q: %v", i, clientKey, err)
		}
		seen[auth.ID]++
	}
	return seen
}

// observe records a unified-window reading on the credential-level snapshot, as the
// result path does for every upstream response.
func (r *reserveRig) observe(t *testing.T, authID, window string, utilization float64, reset time.Time) {
	t.Helper()
	r.observeAt(t, authID, "", window, utilization, reset, time.Now())
}

// observeAt records a reading on the credential snapshot (model "") or one model's snapshot.
func (r *reserveRig) observeAt(t *testing.T, authID, model, window string, utilization float64, reset, observedAt time.Time) {
	t.Helper()
	r.manager.mu.Lock()
	defer r.manager.mu.Unlock()
	auth := r.manager.auths[authID]
	if auth == nil {
		t.Fatalf("unknown auth %s", authID)
	}
	quota := &auth.Quota
	if model != "" {
		if auth.ModelStates == nil {
			auth.ModelStates = map[string]*ModelState{}
		}
		state := auth.ModelStates[model]
		if state == nil {
			state = &ModelState{Status: StatusActive}
			auth.ModelStates[model] = state
		}
		quota = &state.Quota
	}
	quota.ObservedAt = observedAt
	quota.Signals = map[string]string{
		"Anthropic-Ratelimit-Unified-" + window + "-Utilization": strconv.FormatFloat(utilization, 'f', 2, 64),
		"Anthropic-Ratelimit-Unified-" + window + "-Reset":       strconv.FormatInt(reset.Unix(), 10),
		"Anthropic-Ratelimit-Unified-" + window + "-Status":      "allowed",
	}
}

func (r *reserveRig) coolDown(t *testing.T, authIDs ...string) {
	t.Helper()
	for _, id := range authIDs {
		retry := 3 * time.Hour
		r.manager.MarkResult(context.Background(), Result{
			AuthID:          id,
			Provider:        "claude",
			Model:           reserveTestClaudeModel,
			Success:         false,
			RetryAfter:      &retry,
			CredentialScope: true,
			Error:           &Error{HTTPStatus: http.StatusTooManyRequests, Message: "limit"},
		})
	}
}

// limitAt records an upstream 429 on the shared 7d window, as the executor result path
// does: the response carries the rejected 7d reading and the credential cools down.
func (r *reserveRig) limitAt(t *testing.T, authID string, reset time.Time) {
	t.Helper()
	headers := unifiedHeaders(map[string]float64{"7d": 1.0}, reset)
	headers.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
	headers.Set("Anthropic-Ratelimit-Unified-7d-Status", "rejected")
	ctx := internallogging.WithResponseHeadersHolder(context.Background())
	internallogging.SetResponseHeaders(ctx, headers)
	retry := 3 * time.Hour
	r.manager.MarkResult(ctx, Result{
		AuthID:          authID,
		Provider:        "claude",
		Model:           reserveTestClaudeModel,
		Success:         false,
		RetryAfter:      &retry,
		CredentialScope: true,
		Error:           &Error{HTTPStatus: http.StatusTooManyRequests, Message: "limit"},
	})
}

// disable turns one credential off the way an operator does.
func (r *reserveRig) disable(t *testing.T, authID string) {
	t.Helper()
	r.manager.mu.RLock()
	auth := r.manager.auths[authID].Clone()
	r.manager.mu.RUnlock()
	auth.Disabled = true
	auth.Status = StatusDisabled
	if _, err := r.manager.Update(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Update(%s): %v", authID, err)
	}
}

// requireRetryAfterNear checks the hold's Retry-After against an expected reset.
func requireRetryAfterNear(t *testing.T, err error, reset time.Time) {
	t.Helper()
	retryAfter, errParse := strconv.Atoi(SafeResponseHeaders(err).Get("Retry-After"))
	want := int(time.Until(reset).Seconds())
	if errParse != nil || retryAfter < want-60 || retryAfter > want+60 {
		t.Fatalf("Retry-After = %q, want about %d seconds (reset %s)", SafeResponseHeaders(err).Get("Retry-After"), want, reset.UTC().Format(time.RFC3339))
	}
	if local := reset.In(time.Local).Format(quotaRefusalTimeLayout); !strings.Contains(err.Error(), local) {
		t.Fatalf("message %s does not name the earliest reset in local time (%s)", err.Error(), local)
	}
	requireRefusalCountdownNear(t, err.Error(), reset)
}

// resetAfterPattern reads the countdown at the end of a refusal message.
var resetAfterPattern = regexp.MustCompile(`\(reset after (?:(\d+)h ?)?(?:(\d+)m ?)?(?:(\d+)s)?\)`)

// requireRefusalCountdownNear checks that message carries the countdown
// and that it ends at reset.
func requireRefusalCountdownNear(t *testing.T, message string, reset time.Time) {
	t.Helper()
	match := resetAfterPattern.FindStringSubmatch(message)
	if match == nil {
		t.Fatalf("message %s has no (reset after ...) countdown", message)
	}
	var seconds int
	for index, unit := range []int{3600, 60, 1} {
		value, _ := strconv.Atoi(match[index+1])
		seconds += value * unit
	}
	if want := int(time.Until(reset).Seconds()); seconds < want-60 || seconds > want+60 {
		t.Fatalf("countdown %q = %d seconds, want about %d", match[0], seconds, want)
	}
}

func reserveSelectors() map[string]func() Selector {
	return map[string]func() Selector{
		// Built-in selector: pickNext and pickNextMixed take the scheduler fast path.
		"round-robin-fast-path": func() Selector { return &RoundRobinSelector{} },
		"earliest-reset":        func() Selector { return &EarliestResetSelector{} },
		"pace":                  func() Selector { return NewPaceSelector(24*time.Hour, 0.10) },
		"affinity": func() Selector {
			return NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &EarliestResetSelector{}, TTL: time.Hour})
		},
		"affinity-quota-rebind": func() Selector {
			return NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &RoundRobinSelector{}, TTL: time.Hour, RebindOn: SessionAffinityRebindOnQuota})
		},
	}
}

// forEachReserveRig runs every case on the single-provider and mixed pick paths with
// each selector family, including the scheduler fast path.
func forEachReserveRig(t *testing.T, gate internalconfig.ReserveGateConfig, run func(t *testing.T, rig *reserveRig)) {
	t.Helper()
	for name, selector := range reserveSelectors() {
		for _, mixed := range []bool{false, true} {
			label := name + "-single"
			if mixed {
				label = name + "-mixed"
			}
			t.Run(label, func(t *testing.T) {
				run(t, newReserveRig(t, selector(), mixed, gate))
			})
		}
	}
}

func requireReserveHold(t *testing.T, err error) *reserveHoldError {
	t.Helper()
	var hold *reserveHoldError
	if !errors.As(err, &hold) || hold == nil {
		t.Fatalf("error = %v (%T), want reserve hold error", err, err)
	}
	if got := statusCodeFromError(err); got != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", got)
	}
	var body struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if errJSON := json.Unmarshal([]byte(err.Error()), &body); errJSON != nil {
		t.Fatalf("error body %q is not JSON: %v", err.Error(), errJSON)
	}
	if body.Type != "error" || body.Error.Type != "rate_limit_error" || !strings.HasPrefix(strings.ToLower(body.Error.Message), "quota reserve hold:") {
		t.Fatalf("error body = %s, want Anthropic rate_limit_error with a quota reserve hold message", err.Error())
	}
	if strings.Contains(body.Error.Message, "cooling down") {
		t.Fatalf("message %q reads as a cooldown", body.Error.Message)
	}
	return hold
}

func TestReserveGateDisabledLeavesSelectionUnchanged(t *testing.T) {
	forEachReserveRig(t, internalconfig.ReserveGateConfig{}, func(t *testing.T, rig *reserveRig) {
		reset := time.Now().Add(2 * time.Hour)
		rig.observe(t, rig.a, "7d", 0.99, reset)
		rig.observe(t, rig.b, "7d", 0.99, reset)
		auth, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel)
		if err != nil || auth == nil {
			t.Fatalf("pick = %v, %v; want a credential with the gate disabled", auth, err)
		}
	})
}

// Case 1: the reserve is shared. While one Max login still has room, a held login is
// not blocked for an ordinary caller: it may be drained up to its real limit, and the
// other login stays usable too.
func TestReserveGateSharedOneHeldLoginStillServes(t *testing.T) {
	forEachReserveRig(t, reserveGateOn(nil), func(t *testing.T, rig *reserveRig) {
		// a resets last, so its 3% is all reserve: a is held.
		rig.observe(t, rig.a, "7d", 0.97, time.Now().Add(6*24*time.Hour))
		rig.observe(t, rig.b, "7d", 0.30, time.Now().Add(7*time.Hour))
		check := rig.manager.reserveCheckFor(reserveOpts(reserveTestUserKey, ""))
		rig.manager.mu.RLock()
		blockedA, blockedB := check.blocks(rig.manager.auths[rig.a]), check.blocks(rig.manager.auths[rig.b])
		rig.manager.mu.RUnlock()
		if blockedA || blockedB {
			t.Fatalf("blocks(a held 0.97) = %v, blocks(b 0.30) = %v; want neither blocked while b has room", blockedA, blockedB)
		}
		if !check.accountHold(&Auth{ID: rig.a}).held {
			t.Fatal("a at 0.97 is not recorded as held")
		}
		// The reset-ranking selectors spend b first; the pinned picks below show a served.
		if seen := rig.picked(t, reserveTestUserKey, 8); seen[rig.a]+seen[rig.b] != 8 {
			t.Fatalf("non-exempt picks = %v, want only Max logins", seen)
		}
		for _, target := range []string{rig.a, rig.b} {
			opts := reserveOpts(reserveTestUserKey, "")
			opts.Metadata[cliproxyexecutor.PinnedAuthMetadataKey] = target
			auth, _, err := rig.manager.pickNext(context.Background(), "claude", reserveTestClaudeModel, opts, nil)
			if err != nil || auth.ID != target {
				t.Fatalf("pinned pick of %s = %v, %v; want it served", target, auth, err)
			}
		}
	})
}

// Case 2: every Max login held: 90 minutes before b's reset the reserve is 1%, and the
// logins have only that left. The ordinary caller is blocked from both and gets the
// reserve 429 with Retry-After at the earliest release (the final hour before b's reset,
// when no reserve is kept); the exempt caller is served.
func TestReserveGateAllHeldReturns429WithRetryAfter(t *testing.T) {
	forEachReserveRig(t, reserveGateOn(nil), func(t *testing.T, rig *reserveRig) {
		soon := time.Now().Add(90 * time.Minute).Truncate(time.Second)
		rig.observe(t, rig.a, "7d", 0.99, time.Now().Add(72*time.Hour))
		rig.observe(t, rig.b, "7d", 1.00, soon)
		_, err := rig.pick(reserveTestUserKey, "s1", reserveTestClaudeModel)
		hold := requireReserveHold(t, err)
		if !isRequestStopError(err) {
			t.Fatal("reserve hold error must stop the request, not retry it")
		}
		requireRetryAfterNear(t, err, soon.Add(-time.Hour))
		if hold.resetAt.IsZero() {
			t.Fatal("hold error has no reset time")
		}
		// The exempt caller still gets both held accounts.
		if seen := rig.picked(t, reserveTestExemptKey, 4); seen[rig.a]+seen[rig.b] != 4 {
			t.Fatalf("exempt picks = %v, want the held OAuth accounts", seen)
		}
	})
}

// Case 3: one Max login at its real limit (a 429 on the shared window, cooling down) and
// the other only held: the ordinary caller gets the reserve 429, not the credit key, and
// Retry-After names the earliest release, when the reserve drops to 1% 6 h before the
// limited login's reset. The exempt caller gets the held login.
func TestReserveGateOneLimitedOneHeldReturnsReserve429(t *testing.T) {
	forEachReserveRig(t, reserveGateOn(nil), func(t *testing.T, rig *reserveRig) {
		limitReset := time.Now().Add(30 * time.Hour).Truncate(time.Second)
		heldReset := time.Now().Add(48 * time.Hour).Truncate(time.Second)
		rig.limitAt(t, rig.a, limitReset)
		rig.observe(t, rig.b, "7d", 0.96, heldReset)
		_, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel)
		requireReserveHold(t, err)
		requireRetryAfterNear(t, err, limitReset.Add(-6*time.Hour))
		auth, errExempt := rig.pick(reserveTestExemptKey, "", reserveTestClaudeModel)
		if errExempt != nil || auth.ID != rig.b {
			t.Fatalf("exempt pick = %v, %v; want held %s", auth, errExempt, rig.b)
		}
	})
}

// A Max login cooling down at a real limit has no room even when its 429 carried no
// reading: the other, held login is not drained for the ordinary caller, and the credit
// key does not serve either.
func TestReserveGateLimitedLoginWithoutReadingHasNoRoom(t *testing.T) {
	forEachReserveRig(t, reserveGateOn(nil), func(t *testing.T, rig *reserveRig) {
		rig.coolDown(t, rig.a)
		rig.observe(t, rig.b, "7d", 0.97, time.Now().Add(48*time.Hour))
		if auth, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel); err == nil {
			t.Fatalf("pick = %s, want no credential (b held, a at its limit, credit key ineligible)", auth.ID)
		}
	})
}

// Case 4: both Max logins really at their limit: everyone, the ordinary caller included,
// gets the credit key. A reserve hold alone never lets the credit key serve.
func TestReserveGateCreditKeyServesNonExemptCallerOnlyAtRealLimit(t *testing.T) {
	forEachReserveRig(t, reserveGateOn(nil), func(t *testing.T, rig *reserveRig) {
		rig.observe(t, rig.a, "7d", 0.98, time.Now().Add(48*time.Hour))
		rig.observe(t, rig.b, "7d", 0.99, time.Now().Add(25*time.Hour))
		_, err := rig.pick(reserveTestUserKey, "s0", reserveTestClaudeModel)
		requireReserveHold(t, err)
		rig.limitAt(t, rig.a, time.Now().Add(48*time.Hour))
		rig.limitAt(t, rig.b, time.Now().Add(time.Hour))
		for _, key := range []string{reserveTestExemptKey, reserveTestUserKey} {
			auth, errPick := rig.pick(key, "s-"+key, reserveTestClaudeModel)
			if errPick != nil || auth.ID != rig.credit {
				t.Fatalf("pick for %q with both accounts limited = %v, %v; want credit key %s", key, auth, errPick, rig.credit)
			}
		}
	})
}

// The same with cooldowns that carried no reading.
func TestReserveGateCreditKeyServesWhenBothCoolWithoutReadings(t *testing.T) {
	forEachReserveRig(t, reserveGateOn(nil), func(t *testing.T, rig *reserveRig) {
		rig.coolDown(t, rig.a, rig.b)
		auth, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel)
		if err != nil || auth.ID != rig.credit {
			t.Fatalf("pick = %v, %v; want credit key %s", auth, err, rig.credit)
		}
	})
}

// Case 5: a disabled Max login does not count as having room, so the only enabled,
// held login is blocked for the ordinary caller.
func TestReserveGateDisabledLoginDoesNotCountAsRoom(t *testing.T) {
	forEachReserveRig(t, reserveGateOn(nil), func(t *testing.T, rig *reserveRig) {
		reset := time.Now().Add(29 * time.Hour).Truncate(time.Second)
		rig.observe(t, rig.a, "7d", 0.97, reset)
		rig.disable(t, rig.b)
		_, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel)
		requireReserveHold(t, err)
		// At 97% a is released when its limit rises to 99%, 6 h before its reset.
		requireRetryAfterNear(t, err, reset.Add(-6*time.Hour))
		if seen := rig.picked(t, reserveTestExemptKey, 3); seen[rig.a] != 3 {
			t.Fatalf("exempt picks = %v, want %s", seen, rig.a)
		}
	})
}

// The pool holds at the reserve and releases as soon as it has more: there is no resume
// level. b has nothing left, so a alone decides whether anything has room.
func TestReserveGateHoldsAtThresholdWithoutHysteresis(t *testing.T) {
	forEachReserveRig(t, reserveGateOn(nil), func(t *testing.T, rig *reserveRig) {
		reset := time.Now().Add(30 * time.Hour)
		rig.observe(t, rig.b, "7d", 1.00, reset)
		served := func() bool {
			_, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel)
			return err == nil
		}
		rig.observe(t, rig.a, "7d", 0.94, reset)
		if !served() {
			t.Fatal("0.94 must leave room")
		}
		rig.observe(t, rig.a, "7d", 0.95, reset)
		if served() {
			t.Fatal("0.95 (threshold) must hold, and with b empty nothing has room")
		}
		rig.observe(t, rig.a, "7d", 0.94, reset)
		if !served() {
			t.Fatal("0.94 after a hold must release it (no resume-below)")
		}
	})
}

func TestReserveGatePassedResetReleases(t *testing.T) {
	forEachReserveRig(t, reserveGateOn(nil), func(t *testing.T, rig *reserveRig) {
		rig.observe(t, rig.b, "7d", 0.98, time.Now().Add(48*time.Hour))
		rig.observe(t, rig.a, "7d", 0.98, time.Now().Add(25*time.Hour))
		if _, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel); err == nil {
			t.Fatal("both at 0.98 must hold")
		}
		// a's window rolled over: its stale reading no longer holds, so a has room.
		rig.observe(t, rig.a, "7d", 0.97, time.Now().Add(-time.Second))
		if _, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel); err != nil {
			t.Fatalf("a reading whose reset has passed must not hold: %v", err)
		}
		rig.observe(t, rig.a, "7d", 0.10, time.Now().Add(7*24*time.Hour))
		if _, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel); err != nil {
			t.Fatalf("a's new window must have room: %v", err)
		}
	})
}

func TestReserveGateNoSignalsIsEligible(t *testing.T) {
	forEachReserveRig(t, reserveGateOn(nil), func(t *testing.T, rig *reserveRig) {
		auth, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel)
		if err != nil || (auth.ID != rig.a && auth.ID != rig.b) {
			t.Fatalf("pick = %v, %v; want an OAuth account with no readings yet", auth, err)
		}
	})
}

// A login with no reading counts as having room, so a held login stays usable.
func TestReserveGateLoginWithoutReadingCountsAsRoom(t *testing.T) {
	forEachReserveRig(t, reserveGateOn(nil), func(t *testing.T, rig *reserveRig) {
		rig.observe(t, rig.a, "7d", 0.99, time.Now().Add(5*time.Hour))
		if seen := rig.picked(t, reserveTestUserKey, 6); seen[rig.a]+seen[rig.b] != 6 {
			t.Fatalf("non-exempt picks = %v, want Max logins served", seen)
		}
	})
}

// Case 6: a manual hold still refuses every Claude credential for ordinary callers,
// even with every Max login well below threshold.
func TestReserveGateManualHold(t *testing.T) {
	forEachReserveRig(t, reserveGateOn(func(g *internalconfig.ReserveGateConfig) { g.Hold = true }), func(t *testing.T, rig *reserveRig) {
		rig.observe(t, rig.a, "7d", 0.10, time.Now().Add(48*time.Hour))
		rig.observe(t, rig.b, "7d", 0.20, time.Now().Add(48*time.Hour))
		_, err := rig.pick(reserveTestUserKey, "s1", reserveTestClaudeModel)
		hold := requireReserveHold(t, err)
		if !hold.manual {
			t.Fatal("manual hold error not marked manual")
		}
		if got := SafeResponseHeaders(err).Get("Retry-After"); got != "" {
			t.Fatalf("manual hold Retry-After = %q, want none", got)
		}
		// No caller key at all is not exempt either.
		if _, errAnon := rig.pick("", "", reserveTestClaudeModel); errAnon == nil {
			t.Fatal("caller without a key bypassed the manual hold")
		}
		if seen := rig.picked(t, reserveTestExemptKey, 4); seen[rig.a]+seen[rig.b] != 4 {
			t.Fatalf("exempt picks = %v during manual hold, want OAuth accounts", seen)
		}
		auth, errGPT := rig.pick(reserveTestUserKey, "s1", reserveTestGPTModel)
		if errGPT != nil || auth.ID != rig.codex {
			t.Fatalf("codex pick during hold = %v, %v; want %s", auth, errGPT, rig.codex)
		}
	})
}

func TestReserveGateManualHoldBlocksCreditKey(t *testing.T) {
	forEachReserveRig(t, reserveGateOn(func(g *internalconfig.ReserveGateConfig) { g.Hold = true }), func(t *testing.T, rig *reserveRig) {
		rig.coolDown(t, rig.a, rig.b)
		_, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel)
		if hold := requireReserveHold(t, err); !hold.manual {
			t.Fatal("manual hold bypassed")
		}
	})
}

func TestReserveGateLeavesCodexUnaffected(t *testing.T) {
	forEachReserveRig(t, reserveGateOn(nil), func(t *testing.T, rig *reserveRig) {
		rig.observe(t, rig.a, "7d", 0.99, time.Now().Add(time.Hour))
		rig.observe(t, rig.b, "7d", 0.99, time.Now().Add(time.Hour))
		auth, err := rig.pick(reserveTestUserKey, "", reserveTestGPTModel)
		if err != nil || auth.ID != rig.codex {
			t.Fatalf("codex pick = %v, %v; want %s", auth, err, rig.codex)
		}
	})
}

// A session bound to a login that becomes held keeps being served while the other login
// has room, and is refused once both are held (4% left, below the 5% reserve).
func TestReserveGateSessionBoundToHeldAccount(t *testing.T) {
	forEachReserveRig(t, reserveGateOn(nil), func(t *testing.T, rig *reserveRig) {
		first, err := rig.pick(reserveTestUserKey, "bound-session", reserveTestClaudeModel)
		if err != nil {
			t.Fatal(err)
		}
		other := rig.a
		if first.ID == rig.a {
			other = rig.b
		}
		rig.observe(t, first.ID, "7d", 0.98, time.Now().Add(25*time.Hour))
		for i := 0; i < 3; i++ {
			auth, errPick := rig.pick(reserveTestUserKey, "bound-session", reserveTestClaudeModel)
			if errPick != nil || (auth.ID != first.ID && auth.ID != other) {
				t.Fatalf("pick #%d for a session bound to held %s = %v, %v; want a Max login", i, first.ID, auth, errPick)
			}
		}
		rig.observe(t, other, "7d", 0.98, time.Now().Add(26*time.Hour))
		_, errPick := rig.pick(reserveTestUserKey, "bound-session", reserveTestClaudeModel)
		requireReserveHold(t, errPick)
	})
}

// Every upstream response replaces the credential snapshot, so a model whose response
// lacked unified headers can leave the older reading only in a model snapshot. The gate
// takes, per window, the newest snapshot that carries that window's utilization. b has
// nothing left, so a's reading alone decides.
func TestReserveGateUsesFreshestReadingAcrossModelSnapshots(t *testing.T) {
	rig := newReserveRig(t, &EarliestResetSelector{}, true, reserveGateOn(nil))
	now := time.Now()
	reset := now.Add(26 * time.Hour)
	rig.observe(t, rig.b, "7d", 1.00, now.Add(48*time.Hour))
	rig.observeAt(t, rig.a, "", "7d", 0.50, reset, now.Add(-10*time.Minute))
	rig.observeAt(t, rig.a, "other-model", "7d", 0.97, reset, now.Add(-time.Minute))
	if _, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel); err == nil {
		t.Fatal("newer 0.97 model reading must hold the account")
	}
	rig.observeAt(t, rig.a, "", "7d", 0.50, reset, now)
	rig.observeAt(t, rig.a, "other-model", "7d", 0.97, reset, now.Add(-time.Hour))
	if _, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel); err != nil {
		t.Fatalf("newer 0.50 credential reading must release the account: %v", err)
	}
}

// Execute, ExecuteStream and ExecuteCount all stop with the hold error and never reach
// the executor; the exempt caller and Codex still run.
func TestReserveGateAppliesToExecuteStreamAndCount(t *testing.T) {
	for name, selector := range reserveSelectors() {
		t.Run(name, func(t *testing.T) {
			rig := newReserveRig(t, selector(), true, reserveGateOn(nil))
			rig.observe(t, rig.a, "7d", 0.99, time.Now().Add(25*time.Hour))
			rig.observe(t, rig.b, "7d", 0.99, time.Now().Add(24*time.Hour))
			ctx := context.Background()
			req := cliproxyexecutor.Request{Model: reserveTestClaudeModel}
			user := reserveOpts(reserveTestUserKey, "exec-session")
			_, errExec := rig.manager.Execute(ctx, []string{"claude"}, req, user)
			requireReserveHold(t, errExec)
			_, errStream := rig.manager.ExecuteStream(ctx, []string{"claude"}, req, reserveOpts(reserveTestUserKey, "exec-session"))
			requireReserveHold(t, errStream)
			_, errCount := rig.manager.ExecuteCount(ctx, []string{"claude"}, req, reserveOpts(reserveTestUserKey, "exec-session"))
			requireReserveHold(t, errCount)
			if calls := rig.calls.Load(); calls != 0 {
				t.Fatalf("executor called %d times for held requests", calls)
			}
			resp, errExempt := rig.manager.Execute(ctx, []string{"claude"}, req, reserveOpts(reserveTestExemptKey, "exec-session"))
			if errExempt != nil || (string(resp.Payload) != rig.a && string(resp.Payload) != rig.b) {
				t.Fatalf("exempt Execute = %q, %v; want a held OAuth account", resp.Payload, errExempt)
			}
			resp, errGPT := rig.manager.Execute(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: reserveTestGPTModel}, reserveOpts(reserveTestUserKey, ""))
			if errGPT != nil || string(resp.Payload) != rig.codex {
				t.Fatalf("codex Execute = %q, %v", resp.Payload, errGPT)
			}
		})
	}
}

// SelectAuth (legacy single-provider pick used by websocket and helper paths) is gated too.
func TestReserveGateAppliesToSelectAuth(t *testing.T) {
	rig := newReserveRig(t, &RoundRobinSelector{}, false, reserveGateOn(func(g *internalconfig.ReserveGateConfig) { g.Hold = true }))
	_, err := rig.manager.SelectAuth(context.Background(), "claude", reserveTestClaudeModel, reserveOpts(reserveTestUserKey, ""))
	requireReserveHold(t, err)
	if _, errExempt := rig.manager.SelectAuth(context.Background(), "claude", reserveTestClaudeModel, reserveOpts(reserveTestExemptKey, "")); errExempt != nil {
		t.Fatalf("exempt SelectAuth: %v", errExempt)
	}
}

func TestReserveGateFastPathIsTaken(t *testing.T) {
	rig := newReserveRig(t, &RoundRobinSelector{}, true, reserveGateOn(nil))
	if !rig.manager.useSchedulerFastPath() {
		t.Fatal("round-robin rig does not exercise the scheduler fast path")
	}
}

func TestReserveGateConfigReloadTogglesGate(t *testing.T) {
	rig := newReserveRig(t, &RoundRobinSelector{}, true, internalconfig.ReserveGateConfig{})
	if _, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel); err != nil {
		t.Fatalf("gate off: %v", err)
	}
	rig.manager.SetConfig(&internalconfig.Config{Routing: internalconfig.RoutingConfig{ReserveGate: reserveGateOn(func(g *internalconfig.ReserveGateConfig) { g.Hold = true })}})
	_, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel)
	requireReserveHold(t, err)
	rig.manager.SetConfig(&internalconfig.Config{})
	if _, err = rig.pick(reserveTestUserKey, "", reserveTestClaudeModel); err != nil {
		t.Fatalf("gate off again: %v", err)
	}
}

// Without a credit key nothing else sends the round-robin rig off the scheduler fast
// path, so this pins the gate's own detour: one held login does not leave the fast path
// or the rotation, and an all-held pool gets the hold error, on single and mixed picks.
func TestReserveGateOnSchedulerFastPathWithoutCreditKey(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprintf("mixed=%v", mixed), func(t *testing.T) {
			rig := newReserveRig(t, &RoundRobinSelector{}, mixed, reserveGateOn(nil))
			rig.manager.Remove(WithSkipPersist(context.Background()), rig.credit)
			if !rig.manager.useSchedulerFastPath() {
				t.Fatal("rig does not use the scheduler fast path")
			}
			rig.manager.mu.RLock()
			for _, auth := range rig.manager.auths {
				if auth != nil && rig.manager.routeAwareSelectionRequired(auth, reserveTestClaudeModel) {
					rig.manager.mu.RUnlock()
					t.Fatalf("%s forces the legacy path on its own", auth.ID)
				}
			}
			rig.manager.mu.RUnlock()
			if seen := rig.picked(t, reserveTestUserKey, 4); seen[rig.a]+seen[rig.b] != 4 || seen[rig.a] == 0 || seen[rig.b] == 0 {
				t.Fatalf("picks with nothing held = %v, want both accounts", seen)
			}
			rig.observe(t, rig.a, "7d", 0.98, time.Now().Add(25*time.Hour))
			if seen := rig.picked(t, reserveTestUserKey, 6); seen[rig.a] == 0 || seen[rig.b] == 0 {
				t.Fatalf("non-exempt picks with only %s held = %v, want both in rotation", rig.a, seen)
			}
			rig.observe(t, rig.b, "7d", 0.98, time.Now().Add(24*time.Hour))
			_, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel)
			requireReserveHold(t, err)
			if seen := rig.picked(t, reserveTestExemptKey, 4); seen[rig.a] == 0 || seen[rig.b] == 0 {
				t.Fatalf("exempt picks = %v, want both held accounts", seen)
			}
		})
	}
}

// A pinned request accounts against the same eligible pool an unpinned request sees,
// so a login the reserve gate blocks must not enter the weighted accounting pool. With
// the shared reserve the gate blocks a Max login only while every Max login is held, so
// the pinned credential that still serves is one of another provider on a mixed route.
func TestReserveGateExcludesHeldPeerFromPinnedAccounting(t *testing.T) {
	weighted := &WeightedRoundRobinSelector{}
	rig := newReserveRig(t, weighted, true, reserveGateOn(nil))
	rig.disable(t, rig.credit)
	registry.GetGlobalRegistry().RegisterClient(rig.codex, "codex", []*registry.ModelInfo{{ID: reserveTestGPTModel}, {ID: reserveTestClaudeModel}})
	rig.observe(t, rig.a, "7d", 0.99, time.Now().Add(25*time.Hour))
	rig.observe(t, rig.b, "7d", 0.99, time.Now().Add(24*time.Hour))
	opts := reserveOpts(reserveTestUserKey, "")
	opts.Metadata[cliproxyexecutor.PinnedAuthMetadataKey] = rig.codex
	picked, _, _, err := rig.manager.pickNextMixedLegacy(context.Background(), []string{"claude", "codex"}, reserveTestClaudeModel, opts, nil)
	if err != nil || picked == nil || picked.ID != rig.codex {
		t.Fatalf("pinned pick = %v, %v, want %s", picked, err, rig.codex)
	}
	route := "mixed:" + reserveTestClaudeModel
	weightedStateForTest(t, weighted, route, rig.codex)
	held := map[string]int64{rig.a: 1, rig.b: 1, rig.codex: 1}
	if weighted.states[weightedPoolStateKey(route, held)] != nil {
		t.Fatalf("pinned accounting pool included the reserve-held %s and %s", rig.a, rig.b)
	}
}
