package executor

import (
	"net/http"
	"reflect"
)

// statusErrRateLimit is the fork's part of statusErr, embedded as its first field so
// upstream's field list stays as upstream has it. Go requires methods on statusErr to
// sit in this package, so this file is in the executor directory, not helps/.
type statusErrRateLimit struct {
	// rateLimitHeaders holds the allowlisted upstream 429 headers (Retry-After
	// and anthropic-ratelimit-*) that the handler layer may echo to the client.
	rateLimitHeaders http.Header
}

// UpstreamRateLimitHeaders returns the allowlisted upstream rate-limit headers.
func (e statusErrRateLimit) UpstreamRateLimitHeaders() http.Header { return e.rateLimitHeaders }

// Is compares by value: the rateLimitHeaders map makes statusErr incomparable
// with ==, which would otherwise break errors.Is against a wrapped statusErr cause.
func (e statusErr) Is(target error) bool {
	t, ok := target.(statusErr)
	return ok && reflect.DeepEqual(e, t)
}
