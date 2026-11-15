package auth

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// The limits a refusal and the quota state file name.
const (
	quotaLimitWeeklyAll    = "weekly_all"    // the weekly limit shared by all models
	quotaLimitWeeklyScoped = "weekly_scoped" // a weekly limit of one model (Fable)
	quotaLimitFiveHour     = "five_hour"
	quotaLimitReserve      = "reserve" // the reserve gate, not an upstream limit
)

// quotaRefusalTimeLayout is how a refusal shows a reset: the local time, to the minute.
const quotaRefusalTimeLayout = "Mon 2 Jan 15:04"

// quotaRefusalSentence is the plain text of a refusal: the limit, when it resets in the
// zone's time, and, for a limit a Fable reset cannot lift, that it does not. limit ""
// is a cooldown whose limit is not known. The refusal and the quota state file use the
// same words.
func quotaRefusalSentence(limit string, at time.Time, zone *time.Location) string {
	name, event, note := "Rate limit.", "retry", ""
	switch limit {
	case quotaLimitWeeklyAll:
		name, event, note = "Weekly limit.", "reset", " A Fable reset does not lift it."
	case quotaLimitWeeklyScoped:
		name, event = "Weekly model limit.", "reset"
	case quotaLimitFiveHour:
		name, event = "Five-hour limit.", "reset"
	case quotaLimitReserve:
		name, event, note = "Quota reserve hold: the last share of the weekly quota is kept for automations.", "release", " A Fable reset does not lift it."
	}
	if at.IsZero() {
		return name + " Next " + event + " unknown." + note
	}
	if zone == nil {
		zone = time.Local
	}
	return name + " Next " + event + " " + at.In(zone).Format(quotaRefusalTimeLayout) + "." + note
}

// quotaRefusalCountdown is the suffix " (reset after 3h 4m 5s)" of a refusal message.
// T3 Code reads the reset time of a 429 from this text: Claude Code run with a gateway
// token ignores the unified rate-limit headers. Hours are not folded into days. It is
// empty when the reset is unknown.
func quotaRefusalCountdown(at, now time.Time) string {
	if at.IsZero() {
		return ""
	}
	seconds := int64(math.Ceil(at.Sub(now).Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	return fmt.Sprintf(" (reset after %dh %dm %ds)", seconds/3600, seconds%3600/60, seconds%60)
}

// quotaRefusalClaim is the anthropic-ratelimit-unified-representative-claim value of a
// refusal. A Claude Code session with a subscription login names the limit of a 429
// and its reset only when this header (or the overage status) is present. Empty for an
// unknown limit.
func quotaRefusalClaim(limit string) string {
	switch limit {
	case quotaLimitWeeklyAll, quotaLimitReserve:
		return "seven_day"
	case quotaLimitWeeklyScoped:
		return "seven_day_overage_included"
	case quotaLimitFiveHour:
		return "five_hour"
	default:
		return ""
	}
}

// quotaLimitFromSignals names the limit the response behind signals declared rejected,
// or "" when it declared none.
func quotaLimitFromSignals(signals map[string]string) string {
	var status, claim, fiveHour, sevenDay, scoped string
	for name, value := range signals {
		value = strings.ToLower(strings.TrimSpace(value))
		switch strings.ToLower(name) {
		case "anthropic-ratelimit-unified-status":
			status = value
		case "anthropic-ratelimit-unified-representative-claim":
			claim = value
		case "anthropic-ratelimit-unified-5h-status":
			fiveHour = value
		case "anthropic-ratelimit-unified-7d-status":
			sevenDay = value
		case "anthropic-ratelimit-unified-7d_oi-status":
			scoped = value
		}
	}
	const rejected = "rejected"
	switch {
	case fiveHour == rejected:
		return quotaLimitFiveHour
	case sevenDay == rejected:
		return quotaLimitWeeklyAll
	case scoped == rejected:
		return quotaLimitWeeklyScoped
	case status != rejected:
		return ""
	case claim == "five_hour":
		return quotaLimitFiveHour
	case claim == "seven_day":
		return quotaLimitWeeklyAll
	case strings.HasPrefix(claim, "seven_day_"):
		return quotaLimitWeeklyScoped
	default:
		return ""
	}
}

// quotaCooldown is one live quota cooldown of a login.
type quotaCooldown struct {
	limit string // "" if the response that cooled the login named none
	until time.Time
}

// quotaCooldownsOf returns the live quota cooldowns of auth: the credential-wide one (a
// shared window, so the weekly all-model limit unless the 5-hour window was rejected)
// and those of its model states. A non-empty model keeps only that model's state.
func quotaCooldownsOf(auth *Auth, model string, now time.Time) []quotaCooldown {
	if auth == nil {
		return nil
	}
	var cooldowns []quotaCooldown
	shared := ""
	if reserveAtRealLimit(auth, now) {
		if shared = quotaLimitFromSignals(auth.Quota.Signals); shared != quotaLimitFiveHour {
			shared = quotaLimitWeeklyAll
		}
		cooldowns = append(cooldowns, quotaCooldown{limit: shared, until: auth.Quota.NextRecoverAt})
	}
	modelKey := canonicalModelKey(model)
	for stateModel, state := range auth.ModelStates {
		if state == nil || !state.Quota.Exceeded || !state.Quota.NextRecoverAt.After(now) {
			continue
		}
		if model != "" && canonicalModelKey(stateModel) != modelKey {
			continue
		}
		limit := quotaLimitFromSignals(state.Quota.Signals)
		if limit == "" {
			// A state the credential-wide cooldown reached carries no rejection of its own.
			limit = shared
		}
		cooldowns = append(cooldowns, quotaCooldown{limit: limit, until: state.Quota.NextRecoverAt})
	}
	return cooldowns
}

// quotaCooldownOf returns the live quota cooldown of auth that ends last, the one that
// decides when the login serves again, and whether there is one.
func quotaCooldownOf(auth *Auth, model string, now time.Time) (quotaCooldown, bool) {
	var last quotaCooldown
	found := false
	for _, cooldown := range quotaCooldownsOf(auth, model, now) {
		if !found || cooldown.until.After(last.until) {
			last, found = cooldown, true
		}
	}
	return last, found
}

// nextQuotaCooldown returns, among the Max logins with a live quota cooldown for model,
// the cooldown of the login that serves again first, whether there is one, and whether
// every Max login has one. It takes m.mu (read), so the caller must not hold it.
func (m *Manager) nextQuotaCooldown(model string, now time.Time) (next quotaCooldown, found, every bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	every = true
	for _, auth := range m.auths {
		if !isReserveMaxLogin(auth) {
			continue
		}
		cooldown, cooled := quotaCooldownOf(auth, model, now)
		if !cooled {
			every = false
			continue
		}
		if !found || cooldown.until.Before(next.until) {
			next, found = cooldown, true
		}
	}
	return next, found, found && every
}

// nameQuotaRefusal makes a refusal name the limit of the Max login that serves again
// first and its reset: a cooldown refusal with a cooled Max login behind it, and a
// reserve hold refusal while every Max login is at a real limit (the reserve is then not
// what refuses the caller). Any other error is returned as it is.
func (m *Manager) nameQuotaRefusal(err error, model string, now time.Time) error {
	var cooldownErr *modelCooldownError
	var holdErr *reserveHoldError
	switch {
	case errors.As(err, &cooldownErr) && cooldownErr != nil:
		if cooldown, cooled, _ := m.nextQuotaCooldown(model, now); cooled {
			cooldownErr.limited, cooldownErr.limit, cooldownErr.limitResetAt, cooldownErr.now = true, cooldown.limit, cooldown.until, now
		}
	case errors.As(err, &holdErr) && holdErr != nil && !holdErr.manual:
		if cooldown, _, every := m.nextQuotaCooldown(model, now); every {
			holdErr.limited, holdErr.realLimit, holdErr.resetAt, holdErr.now = true, cooldown.limit, cooldown.until, now
		}
	}
	return err
}
