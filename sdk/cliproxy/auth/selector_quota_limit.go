package auth

import (
	"net/http"
	"strconv"
	"time"
)

// namedQuotaLimit is the fork's part of modelCooldownError, embedded as its first field
// so upstream's field list stays as upstream has it. limited is set when a cooled Max
// login is behind the refusal (nameQuotaRefusal): the message then names limit ("" if
// unknown) and its reset limitResetAt, as seen at now.
//
// A limited refusal keeps the headers of its named limit: its reset is the Max login's,
// not the route's, so Headers does not add the route's unified headers.
type namedQuotaLimit struct {
	limited      bool
	limit        string
	limitResetAt time.Time
	now          time.Time
}

// limitMessage replaces message with the named limit and its countdown for a limited
// refusal.
func (n namedQuotaLimit) limitMessage(message string) string {
	if !n.limited {
		return message
	}
	return quotaRefusalSentence(n.limit, n.limitResetAt, time.Local) + quotaRefusalCountdown(n.limitResetAt, n.now)
}

// setLimitHeaders adds, for a limited refusal, the unified headers a Claude Code session
// with a subscription login reads the reset from. They reach the client whatever
// passthrough-headers says.
func (e *modelCooldownError) setLimitHeaders(headers http.Header) {
	if e == nil || !e.limited {
		return
	}
	resetAt := e.limitResetAt
	if resetAt.IsZero() {
		resetAt = time.Now().Add(e.resetIn)
	}
	headers.Set("anthropic-ratelimit-unified-status", "rejected")
	headers.Set("anthropic-ratelimit-unified-reset", strconv.FormatInt(resetAt.Unix(), 10))
	if claim := quotaRefusalClaim(e.limit); claim != "" {
		headers.Set("anthropic-ratelimit-unified-representative-claim", claim)
	}
}
