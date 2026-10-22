package auth

import (
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// FallbackWhenModelLimit is the AttributeFallbackWhen value for a credential that
// serves a model only while every higher-priority credential supporting that model
// is blocked for it by a model-scoped limit.
const FallbackWhenModelLimit = "model-limit"

// FallbackWhenLimit is the AttributeFallbackWhen value for a credential that serves a
// model only while every higher-priority credential supporting that model is blocked
// for it by a quota cooldown of either scope: model-scoped (Fable weekly window) or
// credential-wide (shared 5h or 7d window).
const FallbackWhenLimit = "limit"

// authFallbackWhen returns the canonical fallback-when mode of auth, or "" when auth is
// an ordinary credential.
func authFallbackWhen(auth *Auth) string {
	if auth == nil {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(authAttribute(auth, AttributeFallbackWhen))) {
	case FallbackWhenModelLimit:
		return FallbackWhenModelLimit
	case FallbackWhenLimit:
		return FallbackWhenLimit
	default:
		return ""
	}
}

func authFallbackWhenModelLimit(auth *Auth) bool {
	return authFallbackWhen(auth) != ""
}

// filterConditionalFallbacks drops the conditional fallbacks in auths that are not
// eligible for routeModel. It returns auths unchanged when none is present. The caller
// must hold m.mu.
func (m *Manager) filterConditionalFallbacks(auths []*Auth, routeModel string, now time.Time) []*Auth {
	hasFallback := false
	for _, candidate := range auths {
		if authFallbackWhenModelLimit(candidate) {
			hasFallback = true
			break
		}
	}
	if !hasFallback {
		return auths
	}
	filtered := make([]*Auth, 0, len(auths))
	for _, candidate := range auths {
		if authFallbackWhenModelLimit(candidate) && !m.conditionalFallbackEligible(candidate, routeModel, now) {
			continue
		}
		filtered = append(filtered, candidate)
	}
	return filtered
}

// conditionalFallbackEligible reports whether fallback may serve routeModel: at least one
// higher-priority credential of the same provider must apply, and every one that does
// must be blocked for routeModel by a quota limit. For "model-limit" only a model-scoped
// limit counts and a credential-wide limit (shared 5h or 7d window) makes the fallback
// ineligible; for "limit" either scope counts. A disabled or unauthorized credential, an
// unblocked credential, or an unrelated failure make the fallback ineligible in both.
func (m *Manager) conditionalFallbackEligible(fallback *Auth, routeModel string, now time.Time) bool {
	if strings.TrimSpace(routeModel) == "" {
		return false
	}
	mode := authFallbackWhen(fallback)
	if mode == "" {
		return false
	}
	provider := canonicalSchedulingProvider(executorKeyFromAuth(fallback))
	priority := authPriority(fallback)
	registryRef := registry.GetGlobalRegistry()
	peers := 0
	for _, peer := range m.auths {
		if peer == nil || peer.ID == fallback.ID || authPriority(peer) <= priority {
			continue
		}
		if canonicalSchedulingProvider(executorKeyFromAuth(peer)) != provider {
			continue
		}
		disabled := peer.Disabled || peer.Status == StatusDisabled
		if !m.authSupportsRouteModel(registryRef, peer, routeModel) {
			// A disabled credential is often unregistered, so its model support is unknown
			// and it still vetoes the fallback. Only a registered list that lacks the
			// model proves the peer unrelated.
			registered, _ := registryRef.GetModelsAndEpochForClient(peer.ID)
			if !disabled || len(registered) > 0 {
				continue
			}
		}
		peers++
		if disabled {
			return false
		}
		peerModel := m.selectionModelForAuth(peer, routeModel)
		if mode == FallbackWhenLimit {
			if !blockedByAnyQuota(peer, peerModel, now) {
				return false
			}
		} else if !blockedOnlyByModelQuota(peer, peerModel, now) {
			return false
		}
	}
	return peers > 0
}

// blockedOnlyByModelQuota reports whether auth is blocked for model by a live
// model-scoped quota cooldown and by nothing credential-wide.
func blockedOnlyByModelQuota(auth *Auth, model string, now time.Time) bool {
	if auth == nil || auth.Disabled || auth.Status == StatusDisabled || hasUnauthorizedAuthFailure(auth) {
		return false
	}
	if exp, ok := auth.AccessTokenExpirationTime(); ok && !exp.IsZero() && !exp.After(now) {
		return false
	}
	if auth.Quota.Exceeded && auth.Quota.Reason == "credential_quota" && auth.Quota.NextRecoverAt.After(now) {
		return false
	}
	modelKey := canonicalModelKey(model)
	found := false
	for stateModel, state := range auth.ModelStates {
		if state == nil || canonicalModelKey(stateModel) != modelKey {
			continue
		}
		if state.Status == StatusDisabled {
			return false
		}
		blocked, reason, _ := availabilityBlock(state.Unavailable, state.Quota.Exceeded, state.NextRetryAfter, state.Quota.NextRecoverAt, now)
		if !blocked {
			continue
		}
		if reason != blockReasonCooldown || state.Quota.Reason != "quota" || quotaSignalsShowSharedWindowRejected(state.Quota.Signals) {
			return false
		}
		found = true
	}
	return found
}

// blockedByAnyQuota reports whether auth is blocked for model by a live quota cooldown
// of either scope (credential-wide or model-scoped) and by nothing else: a model state
// blocked for a non-quota reason, a disabled model, an unauthorized credential or an
// expired token all make it false.
func blockedByAnyQuota(auth *Auth, model string, now time.Time) bool {
	if auth == nil || auth.Disabled || auth.Status == StatusDisabled || hasUnauthorizedAuthFailure(auth) {
		return false
	}
	if exp, ok := auth.AccessTokenExpirationTime(); ok && !exp.IsZero() && !exp.After(now) {
		return false
	}
	blocked := auth.Quota.Exceeded && auth.Quota.Reason == "credential_quota" && auth.Quota.NextRecoverAt.After(now)
	modelKey := canonicalModelKey(model)
	for stateModel, state := range auth.ModelStates {
		if state == nil || canonicalModelKey(stateModel) != modelKey {
			continue
		}
		if state.Status == StatusDisabled {
			return false
		}
		stateBlocked, reason, _ := availabilityBlock(state.Unavailable, state.Quota.Exceeded, state.NextRetryAfter, state.Quota.NextRecoverAt, now)
		if !stateBlocked {
			continue
		}
		if reason != blockReasonCooldown || (state.Quota.Reason != "quota" && state.Quota.Reason != "credential_quota") {
			return false
		}
		blocked = true
	}
	return blocked
}

// quotaSignalsShowSharedWindowRejected reports whether the response that produced a
// cooldown declared the shared 5h or 7d window rejected, which is never a model-scoped
// limit even if it was recorded against a single model.
func quotaSignalsShowSharedWindowRejected(signals map[string]string) bool {
	for name, value := range signals {
		lower := strings.ToLower(name)
		if lower != "anthropic-ratelimit-unified-5h-status" && lower != "anthropic-ratelimit-unified-7d-status" {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(value), "rejected") {
			return true
		}
	}
	return false
}
