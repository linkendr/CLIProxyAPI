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

// earliestResetProbeTTL bounds how often a credential with no known weekly reset is probed
// again for one provider and model. A probe is one routed request; if the upstream still
// sends no weekly reset header afterwards, the credential ranks behind known ones until
// this much time has passed.
const earliestResetProbeTTL = 10 * time.Minute

// EarliestResetSelector picks the available credential whose weekly usage window resets
// soonest, so quota that is about to expire is spent first and the other credentials
// stay fresh (use it or lose it). Only weekly windows rank: Claude 7d and the model-scoped
// 7d_oi window, and the Codex secondary window. The 5-hour and Codex primary windows, the
// generic Claude unified reset and cooldown deadlines do not rank. Credentials with the
// same weekly reset second rotate round-robin.
//
// A credential with no known upcoming weekly reset (new login, restart, or an upstream that
// sends no weekly header) is probed first: it is picked ahead of credentials with a known
// reset, so the response headers teach the snapshot its reset. Several unprobed credentials
// rotate. Each credential is probed at most once per earliestResetProbeTTL for a provider
// and model, so one that stays unknown does not keep winning. After that it ranks behind
// known credentials and rotates among other unknowns, and the TTL expiry probes it again.
//
// Reset times are read from the passive quota snapshot the manager already records on every
// upstream response (QuotaState.Signals and ObservedAt). The snapshot is in memory, so it is
// empty after a restart. The selector keeps only the probe times and the rotation state.
type EarliestResetSelector struct {
	mu sync.Mutex
	resetProbeState
	now func() time.Time
}

// resetProbeState is the per provider:model bookkeeping of the selectors that rank by
// weekly windows (EarliestResetSelector, PaceSelector): the last pick, for rotation, and
// when each credential with no known weekly reading was last probed. The owner's mutex
// guards it.
type resetProbeState struct {
	lastPicked map[string]string
	probedAt   map[string]time.Time
	maxKeys    int
}

// begin readies the maps for key and bounds their size: the rotation map is cleared when
// a new key would exceed maxKeys, and probe times older than earliestResetProbeTTL are
// dropped (all of them if that is not enough).
func (p *resetProbeState) begin(key string, now time.Time) {
	if p.lastPicked == nil {
		p.lastPicked = make(map[string]string)
	}
	if p.probedAt == nil {
		p.probedAt = make(map[string]time.Time)
	}
	limit := p.maxKeys
	if limit <= 0 {
		limit = 4096
	}
	if _, ok := p.lastPicked[key]; !ok && len(p.lastPicked) >= limit {
		p.lastPicked = make(map[string]string)
	}
	if len(p.probedAt) >= limit {
		for k, at := range p.probedAt {
			if now.Sub(at) >= earliestResetProbeTTL {
				delete(p.probedAt, k)
			}
		}
		if len(p.probedAt) >= limit {
			p.probedAt = make(map[string]time.Time)
		}
	}
}

// probedRecently reports whether auth was probed for key within earliestResetProbeTTL.
func (p *resetProbeState) probedRecently(key string, auth *Auth, now time.Time) bool {
	at, ok := p.probedAt[key+"|"+auth.ID]
	return ok && now.Sub(at) < earliestResetProbeTTL
}

// markProbed records that auth was probed for key at now.
func (p *resetProbeState) markProbed(key string, auth *Auth, now time.Time) {
	p.probedAt[key+"|"+auth.ID] = now
}

// Pick selects the available auth with the earliest known weekly reset, or an unprobed
// auth without one.
func (s *EarliestResetSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)

	key := provider + ":" + canonicalModelKey(model)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.begin(key, now)

	tied, probe := earliestResetCandidates(available, model, now, func(auth *Auth) bool {
		return s.probedRecently(key, auth, now)
	})
	picked := tied[successorIndex(tied, s.lastPicked[key])]
	s.lastPicked[key] = picked.ID
	if probe {
		s.markProbed(key, picked, now)
	}
	return picked, nil
}

// earliestResetCandidates returns the candidates to rotate among, sorted by ID, and whether
// they are probes. Order of preference: credentials with no known weekly reset that were
// not probed recently (probe true); then the credentials sharing the earliest known weekly
// reset second; then, when none has a known reset, every candidate.
func earliestResetCandidates(available []*Auth, model string, now time.Time, probedRecently func(*Auth) bool) ([]*Auth, bool) {
	var best int64
	hasBest := false
	var tied, unprobed, unknown []*Auth
	for _, auth := range available {
		reset, ok := earliestKnownReset(auth, model, now)
		if !ok {
			unknown = append(unknown, auth)
			if !probedRecently(auth) {
				unprobed = append(unprobed, auth)
			}
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
	probe := false
	switch {
	case len(unprobed) > 0:
		tied, probe = unprobed, true
	case !hasBest:
		tied = unknown
	}
	sort.SliceStable(tied, func(i, j int) bool { return tied[i].ID < tied[j].ID })
	return tied, probe
}

// earliestKnownReset returns the soonest future weekly reset time known for the credential:
// the provider weekly reset headers in the last quota snapshot (credential level and the
// requested model's state). Past times are ignored because they describe a window that has
// already rolled over.
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
			}
		}
	}
	return best, !best.IsZero()
}

func collectQuotaResets(quota QuotaState, consider func(time.Time)) {
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

// isEpochResetSignal matches the weekly window resets (epoch seconds): Anthropic 7d and the
// model-scoped 7d_oi, and the Codex secondary reset-at. The 5h, generic unified and Codex
// primary resets describe short windows and do not rank. Overage and additional-limit
// namespaces are excluded: they describe billing periods and side limits.
func isEpochResetSignal(name string) bool {
	switch name {
	case "anthropic-ratelimit-unified-7d-reset",
		"anthropic-ratelimit-unified-7d_oi-reset",
		"x-codex-secondary-reset-at":
		return true
	}
	return false
}

func isRelativeResetSignal(name string) bool {
	return name == "x-codex-secondary-reset-after-seconds"
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
