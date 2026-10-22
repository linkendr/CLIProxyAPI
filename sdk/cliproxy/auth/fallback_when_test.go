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
	if withOption {
		keyAttrs[AttributeFallbackWhen] = FallbackWhenModelLimit
	}
	for _, entry := range []struct {
		auth   *Auth
		models []string
	}{
		{&Auth{ID: rig.oauthID, Provider: provider, Status: StatusActive, Disabled: oauthDisabled}, []string{fallbackTestFable, fallbackTestOpus}},
		{&Auth{ID: rig.keyID, Provider: provider, Status: StatusActive, Attributes: keyAttrs}, []string{fallbackTestFable}},
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
	for selectorName, selector := range fallbackSelectors() {
		for _, mixed := range []bool{false, true} {
			name := selectorName + "-single"
			if mixed {
				name = selectorName + "-mixed"
			}
			t.Run(name, func(t *testing.T) {
				run(t, newFallbackTestRig(t, name, selector, mixed, withOption, oauthDisabled))
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
	eligible := manager.modelLimitFallbackEligible(key, fallbackTestFable, time.Now())
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
