package auth

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// RoutingStrategyEarliestReset is the routing.strategy value that selects EarliestResetSelector.
const RoutingStrategyEarliestReset = "earliest-reset"

// EarliestResetSelector picks the available credential whose usage-limit window resets
// soonest, so quota that is about to reset is spent first and the other credentials stay
// fresh. Credentials without a known upcoming reset sort last, and credentials with the
// same reset second rotate round-robin.
//
// Reset times are read from the passive quota snapshot the manager already records on every
// upstream response (QuotaState.Signals and ObservedAt), plus active cooldown deadlines. The
// selector keeps no reset state of its own, so the snapshot is as fresh as the last response.
type EarliestResetSelector struct {
	mu         sync.Mutex
	lastPicked map[string]string
	maxKeys    int
}

// Pick selects the available auth with the earliest known reset time.
func (s *EarliestResetSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := time.Now()
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	tied := earliestResetCandidates(available, model, now)

	key := provider + ":" + canonicalModelKey(model)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastPicked == nil {
		s.lastPicked = make(map[string]string)
	}
	limit := s.maxKeys
	if limit <= 0 {
		limit = 4096
	}
	if _, ok := s.lastPicked[key]; !ok && len(s.lastPicked) >= limit {
		s.lastPicked = make(map[string]string)
	}
	picked := tied[successorIndex(tied, s.lastPicked[key])]
	s.lastPicked[key] = picked.ID
	return picked, nil
}

// earliestResetCandidates returns the candidates sharing the earliest known reset second,
// sorted by ID. When no candidate has a known reset, every candidate is returned.
func earliestResetCandidates(available []*Auth, model string, now time.Time) []*Auth {
	var best int64
	hasBest := false
	var tied []*Auth
	for _, auth := range available {
		reset, ok := earliestKnownReset(auth, model, now)
		if !ok {
			continue
		}
		sec := reset.Unix()
		switch {
		case !hasBest || sec < best:
			best, hasBest = sec, true
			tied = append(tied[:0], auth)
		case sec == best:
			tied = append(tied, auth)
		}
	}
	if !hasBest {
		tied = append(tied, available...)
	}
	sort.SliceStable(tied, func(i, j int) bool { return tied[i].ID < tied[j].ID })
	return tied
}

// earliestKnownReset returns the soonest future reset time known for the credential: the
// provider reset headers in the last quota snapshot (credential level and the requested
// model's state) and any active cooldown deadline. Past times are ignored because they
// describe a window that has already rolled over.
func earliestKnownReset(auth *Auth, model string, now time.Time) (time.Time, bool) {
	if auth == nil {
		return time.Time{}, false
	}
	var best time.Time
	consider := func(t time.Time) {
		if t.After(now) && (best.IsZero() || t.Before(best)) {
			best = t
		}
	}
	collectQuotaResets(auth.Quota, consider)
	if model != "" {
		modelKey := canonicalModelKey(model)
		for stateModel, state := range auth.ModelStates {
			if state != nil && canonicalModelKey(stateModel) == modelKey {
				collectQuotaResets(state.Quota, consider)
				consider(state.NextRetryAfter)
			}
		}
	}
	return best, !best.IsZero()
}

func collectQuotaResets(quota QuotaState, consider func(time.Time)) {
	consider(quota.NextRecoverAt)
	for name, value := range quota.Signals {
		name = strings.ToLower(name)
		switch {
		case isEpochResetSignal(name):
			consider(parseResetEpoch(value))
		case isRelativeResetSignal(name) && !quota.ObservedAt.IsZero():
			if seconds, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil && seconds >= 0 {
				consider(quota.ObservedAt.Add(time.Duration(seconds * float64(time.Second))))
			}
		}
	}
}

// isEpochResetSignal matches Anthropic unified window resets (epoch seconds) and the Codex
// primary and secondary reset-at headers. Overage and additional-limit namespaces are
// excluded: they describe billing periods and side limits, not the usage window.
func isEpochResetSignal(name string) bool {
	switch name {
	case "anthropic-ratelimit-unified-reset",
		"anthropic-ratelimit-unified-5h-reset",
		"anthropic-ratelimit-unified-7d-reset",
		"anthropic-ratelimit-unified-7d_oi-reset",
		"x-codex-primary-reset-at",
		"x-codex-secondary-reset-at":
		return true
	}
	return false
}

func isRelativeResetSignal(name string) bool {
	return name == "x-codex-primary-reset-after-seconds" || name == "x-codex-secondary-reset-after-seconds"
}

func parseResetEpoch(value string) time.Time {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseFloat(value, 64); err == nil && seconds > 0 {
		whole := int64(seconds)
		return time.Unix(whole, int64((seconds-float64(whole))*1e9))
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t
	}
	return time.Time{}
}
