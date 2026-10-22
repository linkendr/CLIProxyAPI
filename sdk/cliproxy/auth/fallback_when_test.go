package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const (
	fallbackTestFable = "fallback-fable-model"
	fallbackTestOpus  = "fallback-opus-model"
)

type fallbackTestRig struct {
	manager  *Manager
	provider string
	oauthID  string
	keyID    string
	pick     func(opts cliproxyexecutor.Options, model string) (*Auth, error)
}

// newFallbackTestRig registers an OAuth-like credential (priority 0, Fable and Opus) and a
// credit key (priority -10, Fable only). withOption controls the fallback-when attribute.
func newFallbackTestRig(t *testing.T, name string, selector func() Selector, mixed, withOption, oauthDisabled bool) *fallbackTestRig {
	t.Helper()
	mode := ""
	if withOption {
		mode = FallbackWhenModelLimit
	}
	return newFallbackTestRigMode(t, name, selector, mixed, mode, oauthDisabled, []string{fallbackTestFable})
}

// newFallbackTestRigMode is newFallbackTestRig with an explicit fallback-when mode ("" for
// none) and the models the credit key supports.
func newFallbackTestRigMode(t *testing.T, name string, selector func() Selector, mixed bool, mode string, oauthDisabled bool, keyModels []string) *fallbackTestRig {
	t.Helper()
	ctx := context.Background()
	provider := "fallback-" + name
	manager := NewManager(nil, nil, nil)
	sel := selector()
	if stopper, ok := sel.(interface{ Stop() }); ok {
		t.Cleanup(stopper.Stop)
	}
	manager.SetSelector(sel)
	manager.RegisterExecutor(schedulerTestExecutor{provider: provider})

	rig := &fallbackTestRig{manager: manager, provider: provider, oauthID: provider + "-oauth", keyID: provider + "-key"}
	keyAttrs := map[string]string{"priority": "-10"}
	if mode != "" {
		keyAttrs[AttributeFallbackWhen] = mode
	}
	for _, entry := range []struct {
		auth   *Auth
		models []string
	}{
		{&Auth{ID: rig.oauthID, Provider: provider, Status: StatusActive, Disabled: oauthDisabled}, []string{fallbackTestFable, fallbackTestOpus}},
		{&Auth{ID: rig.keyID, Provider: provider, Status: StatusActive, Attributes: keyAttrs}, keyModels},
	} {
		if _, err := manager.Register(WithSkipPersist(ctx), entry.auth); err != nil {
			t.Fatalf("Register(%s): %v", entry.auth.ID, err)
		}
		infos := make([]*registry.ModelInfo, 0, len(entry.models))
		for _, id := range entry.models {
			infos = append(infos, &registry.ModelInfo{ID: id})
		}
		registry.GetGlobalRegistry().RegisterClient(entry.auth.ID, provider, infos)
		id := entry.auth.ID
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	rig.pick = func(opts cliproxyexecutor.Options, model string) (*Auth, error) {
		if mixed {
			auth, _, _, err := manager.pickNextMixed(ctx, []string{provider}, model, opts, nil)
			return auth, err
		}
		auth, _, err := manager.pickNext(ctx, provider, model, opts, nil)
		return auth, err
	}
	return rig
}

func (r *fallbackTestRig) fail(model string, retry time.Duration, credentialScope bool) {
	r.manager.MarkResult(context.Background(), Result{
		AuthID:          r.oauthID,
		Provider:        r.provider,
		Model:           model,
		Success:         false,
		RetryAfter:      &retry,
		CredentialScope: credentialScope,
		Error:           &Error{HTTPStatus: http.StatusTooManyRequests, Message: "limit"},
	})
}

func (r *fallbackTestRig) expire(t *testing.T, model string) {
	t.Helper()
	r.manager.mu.Lock()
	defer r.manager.mu.Unlock()
	auth := r.manager.auths[r.oauthID]
	expired := time.Now().Add(-time.Second)
	for _, state := range auth.ModelStates {
		if state != nil {
			state.NextRetryAfter = expired
			state.Quota.NextRecoverAt = expired
		}
	}
	if auth.Quota.Exceeded {
		auth.Quota.NextRecoverAt = expired
		auth.NextRetryAfter = expired
	}
}

func fallbackSelectors() map[string]func() Selector {
	return map[string]func() Selector{
		"affinity": func() Selector {
			return NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &RoundRobinSelector{}, TTL: time.Hour})
		},
		"affinity-quota-rebind": func() Selector {
			return NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &RoundRobinSelector{}, TTL: time.Hour, RebindOn: SessionAffinityRebindOnQuota})
		},
		"earliest-reset-affinity": func() Selector {
			return NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &EarliestResetSelector{}, TTL: time.Hour})
		},
		"round-robin": func() Selector { return &RoundRobinSelector{} },
	}
}

func forEachFallbackRig(t *testing.T, withOption, oauthDisabled bool, run func(t *testing.T, rig *fallbackTestRig)) {
	t.Helper()
	mode := ""
	if withOption {
		mode = FallbackWhenModelLimit
	}
	forEachFallbackRigMode(t, mode, oauthDisabled, []string{fallbackTestFable}, run)
}

func forEachFallbackRigMode(t *testing.T, mode string, oauthDisabled bool, keyModels []string, run func(t *testing.T, rig *fallbackTestRig)) {
	t.Helper()
	for selectorName, selector := range fallbackSelectors() {
		for _, mixed := range []bool{false, true} {
			name := selectorName + "-single"
			if mixed {
				name = selectorName + "-mixed"
			}
			t.Run(name, func(t *testing.T) {
				run(t, newFallbackTestRigMode(t, name, selector, mixed, mode, oauthDisabled, keyModels))
			})
		}
	}
}

func fallbackSessionOpts(session string) cliproxyexecutor.Options {
	return cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.DerivedSessionIDMetadataKey: session}}
}

func requireFallbackPick(t *testing.T, rig *fallbackTestRig, session, model, wantID string) {
	t.Helper()
	got, err := rig.pick(fallbackSessionOpts(session), model)
	if err != nil || got == nil {
		t.Fatalf("pick(%s) = %v, %v; want %s", model, got, err, wantID)
	}
	if got.ID != wantID {
		t.Fatalf("pick(%s) = %s, want %s", model, got.ID, wantID)
	}
}

func requireFallbackCooldown(t *testing.T, rig *fallbackTestRig, session, model string) {
	t.Helper()
	got, err := rig.pick(fallbackSessionOpts(session), model)
	if err == nil {
		id := ""
		if got != nil {
			id = got.ID
		}
		t.Fatalf("pick(%s) selected %q, want a cooldown error", model, id)
	}
	var cooldown *modelCooldownError
	if !errors.As(err, &cooldown) {
		t.Fatalf("pick(%s) error = %v (%T), want modelCooldownError with a reset time", model, err, err)
	}
}

func TestFallbackWhenModelLimitServesKeyOnFableWeeklyLimit(t *testing.T) {
	forEachFallbackRig(t, true, false, func(t *testing.T, rig *fallbackTestRig) {
		requireFallbackPick(t, rig, "s1", fallbackTestFable, rig.oauthID)
		rig.fail(fallbackTestFable, 5*24*time.Hour, false)
		requireFallbackPick(t, rig, "s1", fallbackTestFable, rig.keyID)
		requireFallbackPick(t, rig, "s2", fallbackTestFable, rig.keyID)
		// Other models never reach the key: it does not support them, and OAuth still serves them.
		requireFallbackPick(t, rig, "s1", fallbackTestOpus, rig.oauthID)
	})
}

func TestFallbackWhenModelLimitIgnoredOnFiveHourLimit(t *testing.T) {
	forEachFallbackRig(t, true, false, func(t *testing.T, rig *fallbackTestRig) {
		rig.fail(fallbackTestFable, 4*time.Hour, true)
		requireFallbackCooldown(t, rig, "s1", fallbackTestFable)
		requireFallbackCooldown(t, rig, "s2", fallbackTestFable)
		requireFallbackCooldown(t, rig, "s1", fallbackTestOpus)
	})
}

func TestFallbackWhenModelLimitIgnoredOnSharedSevenDayLimit(t *testing.T) {
	forEachFallbackRig(t, true, false, func(t *testing.T, rig *fallbackTestRig) {
		rig.fail(fallbackTestFable, 6*24*time.Hour, true)
		requireFallbackCooldown(t, rig, "s1", fallbackTestFable)
		requireFallbackCooldown(t, rig, "s2", fallbackTestOpus)
	})
}

func TestFallbackWhenModelLimitIgnoredWhenHigherPriorityDisabled(t *testing.T) {
	forEachFallbackRig(t, true, true, func(t *testing.T, rig *fallbackTestRig) {
		got, err := rig.pick(fallbackSessionOpts("s1"), fallbackTestFable)
		if err == nil {
			t.Fatalf("pick selected %v with the only higher-priority credential disabled, want an error", got)
		}
	})
}

func TestFallbackWhenModelLimitRequiresAHigherPriorityCredential(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	key := &Auth{ID: "fallback-lone-key", Provider: "claude", Attributes: map[string]string{AttributeFallbackWhen: FallbackWhenModelLimit, "priority": "-10"}}
	manager.mu.Lock()
	manager.auths[key.ID] = key
	eligible := manager.conditionalFallbackEligible(key, fallbackTestFable, time.Now())
	manager.mu.Unlock()
	if eligible {
		t.Fatal("lone fallback credential is eligible without a higher-priority credential")
	}
}

func TestFallbackWhenModelLimitRebindsToOAuthOnRecovery(t *testing.T) {
	forEachFallbackRig(t, true, false, func(t *testing.T, rig *fallbackTestRig) {
		rig.fail(fallbackTestFable, 5*24*time.Hour, false)
		requireFallbackPick(t, rig, "s1", fallbackTestFable, rig.keyID)

		rig.expire(t, fallbackTestFable)
		// The session was bound to the key. Once OAuth recovers, the key is no longer eligible,
		// so the next request must go back to OAuth even though the key is itself healthy.
		requireFallbackPick(t, rig, "s1", fallbackTestFable, rig.oauthID)
		requireFallbackPick(t, rig, "s1", fallbackTestFable, rig.oauthID)
		requireFallbackPick(t, rig, "s2", fallbackTestFable, rig.oauthID)
	})
}

func TestFallbackWhenModelLimitAbsentKeepsPriorityFallback(t *testing.T) {
	forEachFallbackRig(t, false, false, func(t *testing.T, rig *fallbackTestRig) {
		rig.fail(fallbackTestFable, 4*time.Hour, true)
		requireFallbackPick(t, rig, "s1", fallbackTestFable, rig.keyID)
		rig.expire(t, fallbackTestFable)
		rig.fail(fallbackTestFable, 5*24*time.Hour, false)
		requireFallbackPick(t, rig, "s2", fallbackTestFable, rig.keyID)
	})
}

func TestBlockedOnlyByModelQuotaRejectsSharedWindowSignals(t *testing.T) {
	now := time.Now()
	state := func(signals map[string]string) *Auth {
		return &Auth{ID: "a", ModelStates: map[string]*ModelState{fallbackTestFable: {
			Unavailable:    true,
			NextRetryAfter: now.Add(time.Hour),
			Quota:          QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: now.Add(time.Hour), Signals: signals},
		}}}
	}
	if !blockedOnlyByModelQuota(state(map[string]string{"Anthropic-Ratelimit-Unified-7d_oi-Status": "rejected", "Anthropic-Ratelimit-Unified-5h-Status": "allowed"}), fallbackTestFable, now) {
		t.Fatal("7d_oi rejection with allowed 5h window should be a model-scoped limit")
	}
	for _, header := range []string{"Anthropic-Ratelimit-Unified-5h-Status", "Anthropic-Ratelimit-Unified-7d-Status"} {
		if blockedOnlyByModelQuota(state(map[string]string{header: "rejected"}), fallbackTestFable, now) {
			t.Fatalf("%s rejected must not count as a model-scoped limit", header)
		}
	}
	other := state(nil)
	other.ModelStates[fallbackTestFable].Quota.Reason = "credential_quota"
	if blockedOnlyByModelQuota(other, fallbackTestFable, now) {
		t.Fatal("a propagated credential_quota state must not count as model-scoped")
	}
	if blockedOnlyByModelQuota(state(nil), fallbackTestOpus, now) {
		t.Fatal("a block on another model must not count")
	}
}

func TestFallbackWhenModelLimitIgnoresUnrelatedDisabledPeer(t *testing.T) {
	forEachFallbackRig(t, true, false, func(t *testing.T, rig *fallbackTestRig) {
		unrelatedID := rig.provider + "-unrelated"
		unrelated := &Auth{ID: unrelatedID, Provider: rig.provider, Status: StatusActive, Disabled: true, Attributes: map[string]string{"priority": "5"}}
		if _, err := rig.manager.Register(WithSkipPersist(context.Background()), unrelated); err != nil {
			t.Fatalf("Register: %v", err)
		}
		registry.GetGlobalRegistry().RegisterClient(unrelatedID, rig.provider, []*registry.ModelInfo{{ID: "fallback-unrelated-model"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(unrelatedID) })

		rig.fail(fallbackTestFable, 5*24*time.Hour, false)
		requireFallbackPick(t, rig, "s1", fallbackTestFable, rig.keyID)
	})
}

var fallbackAllModels = []string{fallbackTestFable, fallbackTestOpus}

func TestFallbackWhenLimitServesKeyOnFiveHourLimitForAllModels(t *testing.T) {
	forEachFallbackRigMode(t, FallbackWhenLimit, false, fallbackAllModels, func(t *testing.T, rig *fallbackTestRig) {
		requireFallbackPick(t, rig, "s1", fallbackTestOpus, rig.oauthID)
		rig.fail(fallbackTestOpus, 4*time.Hour, true)
		requireFallbackPick(t, rig, "s1", fallbackTestOpus, rig.keyID)
		requireFallbackPick(t, rig, "s2", fallbackTestFable, rig.keyID)
		requireFallbackPick(t, rig, "s3", fallbackTestOpus, rig.keyID)
	})
}

func TestFallbackWhenLimitServesKeyOnSharedSevenDayLimit(t *testing.T) {
	forEachFallbackRigMode(t, FallbackWhenLimit, false, fallbackAllModels, func(t *testing.T, rig *fallbackTestRig) {
		rig.fail(fallbackTestFable, 6*24*time.Hour, true)
		requireFallbackPick(t, rig, "s1", fallbackTestFable, rig.keyID)
		requireFallbackPick(t, rig, "s2", fallbackTestOpus, rig.keyID)
	})
}

func TestFallbackWhenLimitServesKeyOnFableModelScopedLimit(t *testing.T) {
	forEachFallbackRigMode(t, FallbackWhenLimit, false, fallbackAllModels, func(t *testing.T, rig *fallbackTestRig) {
		rig.fail(fallbackTestFable, 5*24*time.Hour, false)
		requireFallbackPick(t, rig, "s1", fallbackTestFable, rig.keyID)
		// The Fable window does not block Opus, so Max keeps serving it.
		requireFallbackPick(t, rig, "s1", fallbackTestOpus, rig.oauthID)
	})
}

func TestFallbackWhenLimitIgnoredWhenHigherPriorityDisabled(t *testing.T) {
	forEachFallbackRigMode(t, FallbackWhenLimit, true, fallbackAllModels, func(t *testing.T, rig *fallbackTestRig) {
		if got, err := rig.pick(fallbackSessionOpts("s1"), fallbackTestOpus); err == nil {
			t.Fatalf("pick selected %v with the only higher-priority credential disabled, want an error", got)
		}
	})
}

func TestFallbackWhenLimitIgnoredWhenHigherPriorityHealthy(t *testing.T) {
	forEachFallbackRigMode(t, FallbackWhenLimit, false, fallbackAllModels, func(t *testing.T, rig *fallbackTestRig) {
		requireFallbackPick(t, rig, "s1", fallbackTestOpus, rig.oauthID)
		requireFallbackPick(t, rig, "s2", fallbackTestFable, rig.oauthID)
	})
}

func TestFallbackWhenLimitIgnoredOnNonQuotaFailure(t *testing.T) {
	forEachFallbackRigMode(t, FallbackWhenLimit, false, fallbackAllModels, func(t *testing.T, rig *fallbackTestRig) {
		retry := time.Hour
		rig.manager.MarkResult(context.Background(), Result{
			AuthID: rig.oauthID, Provider: rig.provider, Model: fallbackTestOpus, Success: false, RetryAfter: &retry,
			Error: &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "upstream down"},
		})
		got, err := rig.pick(fallbackSessionOpts("s1"), fallbackTestOpus)
		if err == nil && got != nil && got.ID == rig.keyID {
			t.Fatal("key served a model blocked by a non-quota failure")
		}
	})
}

func TestFallbackWhenLimitRebindsToOAuthOnRecovery(t *testing.T) {
	forEachFallbackRigMode(t, FallbackWhenLimit, false, fallbackAllModels, func(t *testing.T, rig *fallbackTestRig) {
		rig.fail(fallbackTestOpus, 4*time.Hour, true)
		requireFallbackPick(t, rig, "s1", fallbackTestOpus, rig.keyID)
		requireFallbackPick(t, rig, "s1", fallbackTestFable, rig.keyID)

		rig.expire(t, fallbackTestOpus)
		requireFallbackPick(t, rig, "s1", fallbackTestOpus, rig.oauthID)
		requireFallbackPick(t, rig, "s1", fallbackTestFable, rig.oauthID)
		requireFallbackPick(t, rig, "s2", fallbackTestOpus, rig.oauthID)
	})
}

func TestFallbackWhenLimitIgnoresUnrelatedDisabledPeer(t *testing.T) {
	forEachFallbackRigMode(t, FallbackWhenLimit, false, fallbackAllModels, func(t *testing.T, rig *fallbackTestRig) {
		unrelatedID := rig.provider + "-unrelated"
		unrelated := &Auth{ID: unrelatedID, Provider: rig.provider, Status: StatusActive, Disabled: true, Attributes: map[string]string{"priority": "5"}}
		if _, err := rig.manager.Register(WithSkipPersist(context.Background()), unrelated); err != nil {
			t.Fatalf("Register: %v", err)
		}
		registry.GetGlobalRegistry().RegisterClient(unrelatedID, rig.provider, []*registry.ModelInfo{{ID: "fallback-unrelated-model"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(unrelatedID) })

		rig.fail(fallbackTestOpus, 4*time.Hour, true)
		requireFallbackPick(t, rig, "s1", fallbackTestOpus, rig.keyID)
	})
}

func TestFallbackWhenModelLimitStillIgnoredOnFiveHourLimitWithAllModels(t *testing.T) {
	forEachFallbackRigMode(t, FallbackWhenModelLimit, false, fallbackAllModels, func(t *testing.T, rig *fallbackTestRig) {
		rig.fail(fallbackTestOpus, 4*time.Hour, true)
		requireFallbackCooldown(t, rig, "s1", fallbackTestOpus)
	})
}
