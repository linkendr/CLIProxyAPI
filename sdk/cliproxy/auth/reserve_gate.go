package auth

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	cliproxysession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	log "github.com/sirupsen/logrus"
)

// reserveWindows are the account-wide Anthropic usage windows the gate reads. The
// model-scoped 7d_oi window is not one of them: it does not consume the shared quota.
var reserveWindows = [...]string{"5h", "7d"}

// reserveGate is routing.reserve-gate compiled once per config snapshot. A nil gate
// means the feature is disabled.
type reserveGate struct {
	hold        bool
	threshold   float64
	resumeBelow float64
	// exempt holds session.CallerScope hashes of the exempt client keys, the same
	// value the request carries in CallerScopeMetadataKey. Raw keys are not kept.
	exempt map[string]struct{}
}

func compileReserveGate(cfg internalconfig.ReserveGateConfig) *reserveGate {
	if !cfg.Enabled {
		return nil
	}
	threshold, resumeBelow := cfg.Levels()
	gate := &reserveGate{hold: cfg.Hold, threshold: threshold, resumeBelow: resumeBelow, exempt: make(map[string]struct{}, len(cfg.ExemptAPIKeys))}
	for _, key := range cfg.ExemptAPIKeys {
		if scope := cliproxysession.CallerScope(key); scope != "" {
			gate.exempt[scope] = struct{}{}
		}
	}
	return gate
}

// reserveReading is the newest utilization reading of one window of one account.
type reserveReading struct {
	utilization float64
	resetAt     time.Time // zero if the response carried no reset
	observedAt  time.Time
}

// reserveHold is the hysteresis state of one window of one account.
type reserveHold struct {
	held  bool
	until time.Time // reset of the window the hold belongs to; zero if unknown
}

// reserveWindowSlack is how far a reading's reset may move past a held window's reset
// and still count as the same window. A new window starts after the old one ended, so
// its reset is hours later.
const reserveWindowSlack = time.Minute

// nextReserveHold decides whether a window is held, from its newest reading and the
// previous state. A reading whose reset has passed describes a window that already
// rolled over, so it never holds, and a previous hold whose window has reset, or that
// belongs to an earlier window than the reading, no longer counts. Otherwise a window
// holds at or above threshold and, once held, stays held until utilization falls below
// resumeBelow.
func nextReserveHold(prev reserveHold, reading reserveReading, ok bool, threshold, resumeBelow float64, now time.Time) reserveHold {
	if !ok || (!reading.resetAt.IsZero() && !reading.resetAt.After(now)) {
		return reserveHold{}
	}
	sameWindow := prev.until.IsZero() || reading.resetAt.IsZero() || !reading.resetAt.After(prev.until.Add(reserveWindowSlack))
	wasHeld := prev.held && sameWindow && (prev.until.IsZero() || prev.until.After(now))
	if reading.utilization >= threshold || (wasHeld && reading.utilization >= resumeBelow) {
		return reserveHold{held: true, until: reading.resetAt}
	}
	return reserveHold{}
}

// latestReserveReading returns the newest reading of window in the snapshots of auth.
// Every upstream response replaces the credential snapshot (auth.Quota) and the
// snapshot of the model it served, so the readings of the shared window can sit in
// either; the snapshot with the newest ObservedAt that carries the window's utilization
// wins, whatever model it came from. The hold book keeps a reading the snapshots lost.
func latestReserveReading(auth *Auth, window string) (reserveReading, bool) {
	var best reserveReading
	found := false
	consider := func(quota QuotaState) {
		reading, ok := reserveReadingFromSignals(quota, window)
		if ok && (!found || reading.observedAt.After(best.observedAt)) {
			best, found = reading, true
		}
	}
	consider(auth.Quota)
	for _, state := range auth.ModelStates {
		if state != nil {
			consider(state.Quota)
		}
	}
	return best, found
}

func reserveReadingFromSignals(quota QuotaState, window string) (reserveReading, bool) {
	utilizationName := "anthropic-ratelimit-unified-" + window + "-utilization"
	resetName := "anthropic-ratelimit-unified-" + window + "-reset"
	reading := reserveReading{observedAt: quota.ObservedAt}
	found := false
	for name, value := range quota.Signals {
		switch strings.ToLower(name) {
		case utilizationName:
			utilization, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if err != nil || math.IsNaN(utilization) || utilization < 0 {
				return reserveReading{}, false
			}
			reading.utilization, found = utilization, true
		case resetName:
			reading.resetAt = parseResetEpoch(value)
		}
	}
	return reading, found
}

// reserveKey names one usage window of one account.
type reserveKey struct {
	authID string
	window string
}

// reserveHoldBook keeps, per account and window, the gate's last known reading and its
// hysteresis state. A response that lacks a window's utilization does not erase the
// known reading of that window. With a state file set, every change of a reading's
// utilization or reset, or of a hold, is written to it, and it is read when the gate
// starts, so a restart keeps known holds. An account with no reading at all is
// eligible: its first response fills the book.
type reserveHoldBook struct {
	mu       sync.Mutex
	readings map[reserveKey]reserveReading
	holds    map[reserveKey]reserveHold
	// path is the state file; empty means the book is kept in memory only.
	path string
	// saveMu orders state file writes so the last write carries the newest book.
	saveMu sync.Mutex
}

// update merges the live reading of one account window into the book, advances its
// hold and returns it. The newer of the live and the known reading wins. Transitions
// are logged so an operator can see when interactive use of an account stops and resumes.
func (b *reserveHoldBook) update(authID, window string, live reserveReading, liveOK bool, gate *reserveGate, now time.Time) reserveHold {
	key := reserveKey{authID: authID, window: window}
	b.mu.Lock()
	reading, ok := b.readings[key]
	changed := false
	if liveOK && (!ok || !live.observedAt.Before(reading.observedAt)) {
		changed = !ok || live.utilization != reading.utilization || !live.resetAt.Equal(reading.resetAt)
		if b.readings == nil {
			b.readings = make(map[reserveKey]reserveReading)
		}
		b.readings[key] = live
		reading, ok = live, true
	}
	prev := b.holds[key]
	next := nextReserveHold(prev, reading, ok, gate.threshold, gate.resumeBelow, now)
	if next.held {
		if b.holds == nil {
			b.holds = make(map[reserveKey]reserveHold)
		}
		b.holds[key] = next
	} else {
		delete(b.holds, key)
	}
	changed = changed || next.held != prev.held || !next.until.Equal(prev.until)
	persist := changed && b.path != ""
	b.mu.Unlock()
	if next.held != prev.held {
		state := "released"
		if next.held {
			state = "held"
		}
		log.Infof("reserve gate: %s %s window %s for non-exempt callers (utilization %.2f, resets %s)", authID, window, state, reading.utilization, formatReserveReset(reading.resetAt))
	}
	if persist {
		b.save(now)
	}
	return next
}

// recordReserveReadings merges the readings of a response into the book as soon as the
// response is recorded, so a reading seen only on exempt traffic is kept, and written
// to the state file, before any request of another caller looks at the account.
func (m *Manager) recordReserveReadings(auth *Auth) {
	gate := m.reserveGate.Load()
	if gate == nil || auth == nil || !isClaudeAuth(auth) {
		return
	}
	now := time.Now()
	for _, window := range reserveWindows {
		if reading, ok := latestReserveReading(auth, window); ok {
			m.reserveHolds.update(auth.ID, window, reading, ok, gate, now)
		}
	}
}

// reserveCheck applies the gate to one request of a caller that is not exempt.
type reserveCheck struct {
	gate *reserveGate
	book *reserveHoldBook
	now  time.Time
}

// reserveCheckFor returns the check for a request, or nil when the gate is disabled
// or the caller is exempt.
func (m *Manager) reserveCheckFor(meta map[string]any) *reserveCheck {
	if m == nil {
		return nil
	}
	gate := m.reserveGate.Load()
	if gate == nil {
		return nil
	}
	scope, _ := meta[cliproxyexecutor.CallerScopeMetadataKey].(string)
	if _, exempt := gate.exempt[strings.TrimSpace(scope)]; exempt {
		return nil
	}
	return &reserveCheck{gate: gate, book: &m.reserveHolds, now: time.Now()}
}

func isClaudeAuth(auth *Auth) bool {
	return canonicalSchedulingProvider(executorKeyFromAuth(auth)) == "claude"
}

// blocks reports whether the gate keeps auth from this request: every Claude credential
// under a manual hold, a paid credit key (fallback-when) always, and an account with a
// held 5h or 7d window. Other providers are never blocked.
func (c *reserveCheck) blocks(auth *Auth) bool {
	if c == nil || auth == nil || !isClaudeAuth(auth) {
		return false
	}
	if c.gate.hold || authFallbackWhen(auth) != "" {
		return true
	}
	held, _ := c.accountHold(auth)
	return held
}

// accountHold evaluates both windows of auth. until is when the account is expected to
// be released (the latest reset among its held windows), zero if any held window has
// no known reset.
func (c *reserveCheck) accountHold(auth *Auth) (held bool, until time.Time) {
	unknown := false
	for _, window := range reserveWindows {
		reading, ok := latestReserveReading(auth, window)
		state := c.book.update(auth.ID, window, reading, ok, c.gate, c.now)
		if !state.held {
			continue
		}
		held = true
		if state.until.IsZero() {
			unknown = true
		} else if state.until.After(until) {
			until = state.until
		}
	}
	if unknown {
		until = time.Time{}
	}
	return held, until
}

// holdError builds the error for a request whose every candidate the gate removed.
// The reset is the earliest expected release among the held accounts; a manual hold
// and credit keys have none.
func (c *reserveCheck) holdError(gated []*Auth) error {
	if c.gate.hold {
		return &reserveHoldError{manual: true, now: c.now}
	}
	var resetAt time.Time
	for _, auth := range gated {
		if authFallbackWhen(auth) != "" {
			continue
		}
		if held, until := c.accountHold(auth); held && !until.IsZero() && (resetAt.IsZero() || until.Before(resetAt)) {
			resetAt = until
		}
	}
	return &reserveHoldError{resetAt: resetAt, now: c.now}
}

// reserveHoldError reports that routing.reserve-gate removed every Claude candidate
// for a caller that is not exempt. It is a 429 that stops the request: retrying or
// waiting inside the proxy would only hold the client's connection open.
type reserveHoldError struct {
	// manual is true for routing.reserve-gate.hold.
	manual bool
	// resetAt is the earliest time a held account is expected to be released; zero if unknown.
	resetAt time.Time
	now     time.Time
}

func (e *reserveHoldError) message() string {
	if e.manual {
		return "quota reserve hold: interactive Claude use is paused by the operator (routing.reserve-gate.hold) to preserve quota for automations"
	}
	message := "quota reserve hold: interactive Claude use is paused to preserve quota for automations"
	if !e.resetAt.IsZero() {
		message += "; resets at " + formatReserveReset(e.resetAt)
	}
	return message
}

// Error returns an Anthropic error body so Claude clients show the message as is.
func (e *reserveHoldError) Error() string {
	body, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": "rate_limit_error", "message": e.message()},
	})
	return string(body)
}

func (e *reserveHoldError) StatusCode() int { return http.StatusTooManyRequests }

func (e *reserveHoldError) IsRequestStop() bool { return true }

// Headers returns Retry-After and the unified rate-limit headers Claude Code reads to
// show when the limit resets. A manual hold has no reset, so it sends none.
func (e *reserveHoldError) Headers() http.Header {
	if e.resetAt.IsZero() {
		return nil
	}
	now := e.now
	if now.IsZero() {
		now = time.Now()
	}
	seconds := int64(math.Ceil(e.resetAt.Sub(now).Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	headers := make(http.Header)
	headers.Set("Retry-After", strconv.FormatInt(seconds, 10))
	headers.Set("anthropic-ratelimit-unified-status", "rejected")
	headers.Set("anthropic-ratelimit-unified-reset", strconv.FormatInt(e.resetAt.Unix(), 10))
	return headers
}

func formatReserveReset(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.UTC().Format(time.RFC3339)
}

// pickHomeDispatchSelection is every Home dispatch: Home picks the credential, and the
// reserve gate applies to its pick. A credential the gate keeps from the caller is
// released back to Home and excluded, and Home is asked again. When Home has nothing
// left after the gate removed at least one credential, the request gets the reserve
// hold error. Under Home the utilization readings are the quota snapshot Home sends
// with the credential, since results are reported to Home.
func (m *Manager) pickHomeDispatchSelection(ctx context.Context, model string, opts cliproxyexecutor.Options) (*HomeDispatchSelection, error) {
	reserve := m.reserveCheckFor(opts.Metadata)
	if reserve == nil {
		return m.dispatchHomeSelection(ctx, model, opts)
	}
	excluded := make(map[string]struct{})
	var gated []*Auth
	for {
		selection, errSelection := m.dispatchHomeSelection(ctx, model, withHomeExcludedAuthIDs(opts, excluded))
		if errSelection != nil {
			if len(gated) > 0 && homeDispatchExhausted(errSelection) {
				return nil, reserve.holdError(gated)
			}
			return nil, errSelection
		}
		auth := selection.CloneAuth()
		if !reserve.blocks(auth) {
			return selection, nil
		}
		if errEnd := m.endHomeSelectionBeforeRedispatch(ctx, selection, "reserve_gate"); errEnd != nil {
			return nil, errEnd
		}
		gated = append(gated, auth)
		id := strings.TrimSpace(auth.ID)
		if _, repeated := excluded[id]; repeated || id == "" {
			// Home ignored the exclusion; asking again would loop.
			return nil, reserve.holdError(gated)
		}
		excluded[id] = struct{}{}
		opts = withHomeAuthCount(opts, homeAuthCountFromMetadata(opts.Metadata)+1)
	}
}

// homeDispatchExhausted reports whether Home had no further credential for the request.
// A cooldown is not exhaustion: it passes through so the Home retry round can wait it out.
func homeDispatchExhausted(err error) bool {
	var authErr *Error
	return errors.As(err, &authErr) && authErr != nil && authErr.Code == "auth_not_found"
}
