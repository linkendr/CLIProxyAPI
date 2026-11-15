package auth

import (
	"math"
	"net/http"
	"strconv"
	"time"
)

// withMaxPoolHeaders returns the response headers of a Claude API key (a credit key)
// with the unified limit headers that describe the Max logins: the weekly usage and
// reset of the Max login with the most room, a login at a real limit counting as full.
// Anthropic sends these headers only for a subscription login, so a client that meters
// the weekly limit from them would otherwise keep its last value while a credit key
// serves. The status is always "allowed": the response was served, and Claude Code
// treats any other status as a limit event. Headers of another credential, headers
// that already carry a unified status, and a pool with no known reset are returned
// unchanged. Clients receive response headers only with passthrough-headers.
func (m *Manager) withMaxPoolHeaders(auth *Auth, headers http.Header, now time.Time) http.Header {
	const prefix = "Anthropic-Ratelimit-Unified-"
	if m == nil || auth == nil || !isClaudeAuth(auth) || auth.AuthKind() == AuthKindOAuth || headers.Get(prefix+"Status") != "" {
		return headers
	}
	doc, ok := m.buildQuotaState(now)
	if !ok {
		return headers
	}
	utilization, reset, found := 0.0, time.Time{}, false
	for _, login := range doc.Logins {
		used, resetAt := 1.0, login.cooledUntil
		if login.ResetsAt != nil {
			resetAt = *login.ResetsAt
		}
		switch {
		case login.State == quotaLoginCooled:
		case login.Utilization != nil:
			used = math.Min(1, *login.Utilization)
		default:
			continue
		}
		if resetAt.IsZero() {
			continue
		}
		if !found || used < utilization || (used == utilization && resetAt.Before(reset)) {
			utilization, reset, found = used, resetAt, true
		}
	}
	if !found {
		return headers
	}
	out := headers.Clone()
	if out == nil {
		out = make(http.Header)
	}
	resetUnix := strconv.FormatInt(reset.Unix(), 10)
	out.Set(prefix+"Status", "allowed")
	out.Set(prefix+"Representative-Claim", quotaRefusalClaim(quotaLimitWeeklyAll))
	out.Set(prefix+"Reset", resetUnix)
	out.Set(prefix+"7d-Utilization", strconv.FormatFloat(math.Round(utilization*100)/100, 'f', -1, 64))
	out.Set(prefix+"7d-Reset", resetUnix)
	return out
}
