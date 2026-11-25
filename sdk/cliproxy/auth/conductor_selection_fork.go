package auth

import (
	"context"
	"errors"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// Fork-owned selection code. conductor_selection.go is upstream's busiest file; it keeps
// only one-line calls into this file, so upstream changes there do not touch fork lines.
//
// The scheduler fast path in pickNext and pickNextMixedOnce does not know the reserve
// gate: a request the gate would affect (reserveCheckFor is not nil and blocks a
// candidate) takes the legacy path, which filters the candidates with reserveSelection.

// usesPrevalidatedCandidates reports whether the selector receives manager-validated
// candidates, like the built-in selectors. The weekly-window selectors
// (EarliestResetSelector, PaceSelector) join them for candidate handling but not the
// scheduler fast path, which has no reset or pace ordering. They are not built-in
// selectors, so selectionArgForSelector still gives them the requested model, whose
// quota data they read.
func usesPrevalidatedCandidates(selector Selector) bool {
	if isWeeklyWindowSelector(selector) {
		return true
	}
	return isBuiltInSelector(selector)
}

// isWeeklyWindowSelector reports whether the selector ranks by the requested model's
// weekly quota windows.
func isWeeklyWindowSelector(selector Selector) bool {
	switch selector.(type) {
	case *EarliestResetSelector, *PaceSelector:
		return true
	default:
		return false
	}
}

// reserveSelection is the reserve gate's part of one legacy pick (pickNextLegacy,
// pickNextMixedLegacy). It keeps credit keys from a caller they are withheld from, keeps
// gated candidates out of the pool, and answers a pool the gate emptied with the hold
// refusal. For each candidate the pick calls excludes, then skips.
type reserveSelection struct {
	check          *reserveCheck
	creditWithheld bool
	gated          []*Auth

	// blocked is the gate's answer for the candidate excludes saw last.
	// reserveCheck.blocks can update the hold book, so it runs once per candidate.
	blocked bool
}

// beginReserveSelection evaluates the gate for one pick. It takes m.mu (read) through
// reserveCheckFor, so the caller must not hold m.mu.
func (m *Manager) beginReserveSelection(opts cliproxyexecutor.Options) *reserveSelection {
	check := m.reserveCheckFor(opts)
	return &reserveSelection{check: check, creditWithheld: m.reserveWithholdsCredit(opts.Metadata)}
}

func (r *reserveSelection) withholds(candidate *Auth) bool {
	return r.creditWithheld && isClaudeCreditKey(candidate)
}

// excludes reports whether candidate leaves the eligible pool used for pinned
// accounting: a withheld credit key or a gated candidate. Pinning does not shrink that
// pool; the reserve gate does, as it does for unpinned requests.
func (r *reserveSelection) excludes(candidate *Auth) bool {
	r.blocked = false
	if r.withholds(candidate) {
		return true
	}
	r.blocked = r.check.blocks(candidate)
	return r.blocked
}

// skips reports whether the pick must pass over candidate, the one excludes saw last.
// A gated candidate not yet tried is kept for the hold refusal.
func (r *reserveSelection) skips(candidate *Auth, used bool) bool {
	if r.withholds(candidate) {
		return true
	}
	if !r.blocked {
		return false
	}
	if !used {
		r.gated = append(r.gated, candidate)
	}
	return true
}

// emptyPoolError returns the hold refusal when the gate removed every candidate.
func (r *reserveSelection) emptyPoolError(candidates []*Auth) error {
	if len(candidates) > 0 || len(r.gated) == 0 {
		return nil
	}
	return r.check.holdError(r.gated)
}

// pickNextMixed selects the credential of one attempt. At a quota dead end, where no Max
// login can serve the caller, it first reads the real usage of the Max logins
// (refreshQuotaUsage) and selects once more if that lifted a cooldown or changed a
// reading: a limit the owner reset upstream is then seen without a manual step. A
// cooldown refusal behind a cooled Max login names the limit and its reset. Home owns
// the credentials under Home dispatch, so nothing is read there.
func (m *Manager) pickNextMixed(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, string, error) {
	auth, executor, provider, errPick := m.pickNextMixedOnce(ctx, providers, model, opts, tried)
	if m.HomeEnabled() || !quotaDeadEnd(auth, providers, errPick) {
		return auth, executor, provider, errPick
	}
	if m.refreshQuotaUsage(ctx) {
		auth, executor, provider, errPick = m.pickNextMixedOnce(ctx, providers, model, opts, tried)
	}
	now := time.Now()
	errPick = m.nameQuotaRefusal(errPick, model, now)
	m.syncQuotaState(now)
	return auth, executor, provider, errPick
}

// quotaDeadEnd reports whether a selection ended where no Max login can serve the
// caller: a reserve hold refusal (not the manual hold, which no reading lifts), a
// cooldown refusal on a route that includes Claude, or a Claude credit key
// (fallback-when) about to serve.
func quotaDeadEnd(selected *Auth, providers []string, errPick error) bool {
	if errPick == nil {
		return selected != nil && isClaudeAuth(selected) && authFallbackWhen(selected) != ""
	}
	var holdErr *reserveHoldError
	if errors.As(errPick, &holdErr) && holdErr != nil {
		return !holdErr.manual
	}
	var cooldownErr *modelCooldownError
	if !errors.As(errPick, &cooldownErr) || cooldownErr == nil {
		return false
	}
	for _, provider := range providers {
		if canonicalSchedulingProvider(provider) == "claude" {
			return true
		}
	}
	return false
}
