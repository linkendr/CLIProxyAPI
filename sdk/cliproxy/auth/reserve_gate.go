package auth

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	cliproxysession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	log "github.com/sirupsen/logrus"
)

// reserveWindows are the account-wide Anthropic usage windows the gate reads. Anthropic
// reports only the 7-day window now. The model-scoped 7d_oi window is not one of them:
// it does not consume the shared quota.
var reserveWindows = [...]string{"7d"}

// reserveGate is routing.reserve-gate compiled once per config snapshot. A nil gate
// means the feature is disabled.
type reserveGate struct {
	hold      bool
	threshold float64
	// nearReset and nearThreshold raise a login's limit once its known reset is at most
	// nearReset away; finalRelease raises it to 1 once its reset is at most that far away.
	nearReset     time.Duration
	nearThreshold float64
	finalRelease  time.Duration
	// windDown is how long after the trigger turned on running tasks are still served.
	windDown time.Duration
	// exempt holds session.CallerScope hashes of the exempt client keys, the same
	// value the request carries in CallerScopeMetadataKey. Raw keys are not kept.
	exempt map[string]struct{}
}

func compileReserveGate(cfg internalconfig.ReserveGateConfig) *reserveGate {
	if !cfg.Enabled {
		return nil
	}
	gate := &reserveGate{
		hold: cfg.Hold, threshold: cfg.EffectiveThreshold(), windDown: cfg.EffectiveWindDown(),
		nearReset: cfg.EffectiveNearReset(), nearThreshold: math.Max(cfg.EffectiveThreshold(), cfg.EffectiveNearResetThreshold()),
		finalRelease: cfg.EffectiveFinalRelease(), exempt: make(map[string]struct{}, len(cfg.ExemptAPIKeys)),
	}
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

// reserveHold is the hold state of one window of one account.
type reserveHold struct {
	held  bool
	until time.Time // when the hold lifts without a new reading (a limit step or the reset); zero if unknown
}

// limit returns the utilization at or above which a login whose window resets at resetAt
// is held: 1 in the final release, nearThreshold near the reset, threshold otherwise and
// when the reset is unknown.
func (g *reserveGate) limit(resetAt, now time.Time) float64 {
	if resetAt.IsZero() {
		return g.threshold
	}
	left := resetAt.Sub(now)
	switch {
	case g.finalRelease > 0 && left <= g.finalRelease:
		return 1
	case g.nearReset > 0 && left <= g.nearReset:
		return g.nearThreshold
	default:
		return g.threshold
	}
}

// reserveEpsilon absorbs float error when a share is compared with a headroom: both are
// differences of readings that carry two decimals.
const reserveEpsilon = 1e-9

// reservePoolEntry is one Max login's window in the shared reserve.
type reservePoolEntry struct {
	id       string
	headroom float64   // 1 - utilization, at least 0; 0 at a real limit
	resetAt  time.Time // zero if unknown
}

// reserveAt returns the shared reserve when the next reset of any Max login is at next:
// 1 - limit, so a pool of one login is held exactly at its own limit.
func (g *reserveGate) reserveAt(next, now time.Time) float64 {
	return 1 - g.limit(next, now)
}

// placeReserve places reserve on the entries that reset last (an unknown reset counts as
// latest), each taking up to its headroom, and reports per entry whether all its headroom
// is reserve: held.
func placeReserve(entries []reservePoolEntry, reserve float64) []bool {
	order := make([]int, len(entries))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(x, y int) bool {
		a, b := entries[order[x]], entries[order[y]]
		switch {
		case a.resetAt.Equal(b.resetAt):
			return a.id < b.id
		case a.resetAt.IsZero() || b.resetAt.IsZero():
			return a.resetAt.IsZero()
		default:
			return a.resetAt.After(b.resetAt)
		}
	})
	held := make([]bool, len(entries))
	left := reserve
	for _, i := range order {
		share := math.Min(entries[i].headroom, math.Max(left, 0))
		held[i] = entries[i].headroom <= share+reserveEpsilon
		left -= share
	}
	return held
}

// poolHolds decides the hold of every entry of one window. The reserve follows the time
// left to the next known reset of any entry, since that reset brings fresh capacity. A
// held entry is released at the first limit step that frees it, else at that next reset;
// its release is unknown when no entry has a known reset.
func (g *reserveGate) poolHolds(entries []reservePoolEntry, now time.Time) []reserveHold {
	var next time.Time
	for _, entry := range entries {
		if !entry.resetAt.IsZero() && (next.IsZero() || entry.resetAt.Before(next)) {
			next = entry.resetAt
		}
	}
	held := placeReserve(entries, g.reserveAt(next, now))
	var steps []time.Time
	if !next.IsZero() {
		for _, before := range []time.Duration{g.nearReset, g.finalRelease} {
			if step := next.Add(-before); before > 0 && step.After(now) {
				steps = append(steps, step)
			}
		}
	}
	holds := make([]reserveHold, len(entries))
	for i := range entries {
		if !held[i] {
			continue
		}
		holds[i] = reserveHold{held: true, until: next}
		for _, step := range steps {
			if !placeReserve(entries, g.reserveAt(next, step))[i] {
				holds[i].until = step
				break
			}
		}
	}
	return holds
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
// hold. A response that lacks a window's utilization does not erase the known reading
// of that window. With a state file set, every change of a reading's utilization or
// reset, or of a hold, is written to it, and it is read when the gate starts, so a
// restart keeps known readings and holds. An account with no reading at all is not
// held: its first response fills the book.
type reserveHoldBook struct {
	mu       sync.Mutex
	readings map[reserveKey]reserveReading
	holds    map[reserveKey]reserveHold
	// dirty reports a reading or hold change the state file does not have yet.
	dirty bool
	// path is the state file; empty means the book is kept in memory only.
	path string
	// saveMu orders state file writes so the last write carries the newest book.
	saveMu sync.Mutex
}

// mergeLocked merges a live reading into the book and returns the newest known reading
// of key. The newer of the live and the known reading wins. The caller holds b.mu.
func (b *reserveHoldBook) mergeLocked(key reserveKey, live reserveReading, liveOK bool) (reserveReading, bool) {
	reading, ok := b.readings[key]
	if !liveOK || (ok && live.observedAt.Before(reading.observedAt)) {
		return reading, ok
	}
	if !ok || live.utilization != reading.utilization || !live.resetAt.Equal(reading.resetAt) {
		b.dirty = true
	}
	if b.readings == nil {
		b.readings = make(map[reserveKey]reserveReading)
	}
	b.readings[key] = live
	return live, true
}

// merge merges a live reading of one account window into the book.
func (b *reserveHoldBook) merge(authID, window string, live reserveReading) {
	b.mu.Lock()
	b.mergeLocked(reserveKey{authID: authID, window: window}, live, true)
	b.mu.Unlock()
}

// evaluate merges the live readings of logins into the book and decides the hold of each
// login from the shared reserve of their windows (poolHolds, one pool per window). A
// login with neither a reading nor a real limit takes no part and is not held. A login
// at a real limit has no headroom whatever its reading says; a reading whose reset has
// passed describes a window that already rolled over, so it has all its headroom.
// Transitions are logged so an operator can see when a login reaches and leaves the
// reserve, and a change is written to the state file.
func (b *reserveHoldBook) evaluate(gate *reserveGate, now time.Time, logins []reserveLogin) map[string]reserveAccount {
	type transition struct {
		key     reserveKey
		reading reserveReading
		held    bool
	}
	var transitions []transition
	accounts := make(map[string]reserveAccount, len(logins))
	for _, login := range logins {
		accounts[login.id] = reserveAccount{}
	}
	unknown := make(map[string]bool)
	b.mu.Lock()
	for index, window := range reserveWindows {
		var keys []reserveKey
		var readings []reserveReading
		var entries []reservePoolEntry
		for i := range logins {
			login := &logins[i]
			key := reserveKey{authID: login.id, window: window}
			reading, ok := b.mergeLocked(key, login.readings[index], login.ok[index])
			if !ok && !login.atLimit {
				if _, held := b.holds[key]; held {
					delete(b.holds, key)
					b.dirty = true
				}
				continue
			}
			entry := reservePoolEntry{id: login.id, headroom: math.Max(0, 1-reading.utilization), resetAt: reading.resetAt}
			if !entry.resetAt.IsZero() && !entry.resetAt.After(now) {
				entry.headroom, entry.resetAt = 1, time.Time{}
			}
			if login.atLimit {
				entry.headroom = 0
			}
			keys, readings, entries = append(keys, key), append(readings, reading), append(entries, entry)
		}
		for i, next := range gate.poolHolds(entries, now) {
			key := keys[i]
			prev := b.holds[key]
			if next.held {
				if b.holds == nil {
					b.holds = make(map[reserveKey]reserveHold)
				}
				b.holds[key] = next
			} else {
				delete(b.holds, key)
			}
			if next.held != prev.held || !next.until.Equal(prev.until) {
				b.dirty = true
			}
			if next.held != prev.held {
				transitions = append(transitions, transition{key: key, reading: readings[i], held: next.held})
			}
			if !next.held {
				continue
			}
			account := accounts[key.authID]
			account.held = true
			if next.until.IsZero() {
				unknown[key.authID] = true
			} else if next.until.After(account.until) {
				account.until = next.until
			}
			accounts[key.authID] = account
		}
	}
	persist := b.dirty && b.path != ""
	b.dirty = b.dirty && !persist
	b.mu.Unlock()
	for id := range unknown {
		account := accounts[id]
		account.until = time.Time{}
		accounts[id] = account
	}
	for _, change := range transitions {
		state := "released"
		if change.held {
			state = "held"
		}
		log.Infof("reserve gate: %s %s window %s (utilization %.2f, resets %s); all its headroom is shared reserve, and non-exempt callers are refused only while every Max login is held", change.key.authID, change.key.window, state, change.reading.utilization, formatReserveReset(change.reading.resetAt))
	}
	if persist {
		b.save(now)
	}
	return accounts
}

// recordReserveReadings merges the readings of a response into the book as soon as the
// response is recorded, so a reading seen only on exempt traffic is kept, and written
// to the state file, before any request of another caller looks at the account. It
// also records the reserve trigger, so the wind-down starts when the response that
// held the last Max login arrives, whoever sent it. The caller must not hold m.mu.
func (m *Manager) recordReserveReadings(auth *Auth) {
	gate := m.reserveGate.Load()
	if gate == nil || auth == nil || !isClaudeAuth(auth) {
		return
	}
	for _, window := range reserveWindows {
		if reading, ok := latestReserveReading(auth, window); ok {
			m.reserveHolds.merge(auth.ID, window, reading)
		}
	}
	m.observeReserveTrigger(gate, time.Now())
}

// reserveLogin is one Max login (a Claude subscription login that is not disabled) as
// reserveCheckFor saw it.
type reserveLogin struct {
	id string
	// readings are the live readings per window of reserveWindows; ok[i] reports one.
	readings [len(reserveWindows)]reserveReading
	ok       [len(reserveWindows)]bool
	// atLimit reports a live credential-wide quota cooldown: the login has no room left
	// whatever its reading says.
	atLimit bool
}

// reserveAccount is the evaluated hold of one account: until is when it is expected to
// be released (the latest release among its held windows), zero if unknown.
type reserveAccount struct {
	held  bool
	until time.Time
}

// reserveCheck applies the gate to one request of a caller that is not exempt.
//
// The reserve is shared: one amount for all Max logins, placed on the logins that reset
// last (poolHolds). A login is held when all its headroom is reserve, and a held Max
// login is kept from the caller only while every Max login is held or at a real limit,
// that is while the logins together have no more than the reserve left. The Max logins are captured by reserveCheckFor and
// evaluated against the book once, on first use, so every candidate of the request is
// judged by the same view.
type reserveCheck struct {
	gate   *reserveGate
	book   *reserveHoldBook
	now    time.Time
	logins []reserveLogin

	once     sync.Once
	accounts map[string]reserveAccount
	// everyHeld is the reserve trigger: there is at least one Max login and none has
	// room, each is held or at a real limit.
	everyHeld bool
}

// reserveCheckFor returns the check for a request, or nil when the gate does not apply:
// the gate is disabled, the caller is exempt, or the request is a running task within
// the wind-down. A wind-down request is marked with ReserveWindDownMetadataKey in
// opts.Metadata (a stale mark from an earlier attempt is removed), so the Claude
// executor adds the wrap-up note. It takes m.mu (read) to capture the Max logins, so the
// caller must not hold m.mu; every pick path calls it before taking the lock.
//
// While the trigger is on, a caller that is not exempt gets:
//   - a new prompt: refused (every Max login held, every other Claude credential without
//     fallback-when blocked; credit keys only under their own fallback-when rule);
//   - a running task, for windDown after the trigger turned on: served like an exempt
//     caller (Max logins up to their real limits, then credit keys at a real limit);
//   - a running task after that: refused like a new prompt.
func (m *Manager) reserveCheckFor(opts cliproxyexecutor.Options) *reserveCheck {
	delete(opts.Metadata, ReserveWindDownMetadataKey)
	check := m.reserveCallerCheck(opts.Metadata)
	if check == nil || check.gate.hold {
		return check
	}
	check.logins = m.reserveMaxLogins(check.now)
	check.evaluate()
	since := m.reserveTrigger.observe(check.everyHeld, check.now, check.gate.windDown)
	if !check.everyHeld || check.gate.windDown <= 0 || !check.now.Before(since.Add(check.gate.windDown)) {
		return check
	}
	if !reserveRequestIsRunningTask(opts.SourceFormat, opts.OriginalRequest) || opts.Metadata == nil {
		return check
	}
	opts.Metadata[ReserveWindDownMetadataKey] = true
	return nil
}

// reserveCallerCheck returns an unevaluated check for a caller that is not exempt, or nil
// when the gate is disabled or the caller is exempt.
func (m *Manager) reserveCallerCheck(meta map[string]any) *reserveCheck {
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

// reserveMaxLogins captures the readings and real-limit state of every Max login. It
// takes m.mu (read), so the caller must not hold it.
func (m *Manager) reserveMaxLogins(now time.Time) []reserveLogin {
	m.mu.RLock()
	defer m.mu.RUnlock()
	logins := make([]reserveLogin, 0, len(m.auths))
	for _, auth := range m.auths {
		if !isReserveMaxLogin(auth) {
			continue
		}
		login := reserveLoginOf(auth)
		login.atLimit = reserveAtRealLimit(auth, now)
		logins = append(logins, login)
	}
	return logins
}

// reserveLoginOf returns auth with its live readings, not marked at a real limit.
func reserveLoginOf(auth *Auth) reserveLogin {
	login := reserveLogin{id: auth.ID}
	for index, window := range reserveWindows {
		login.readings[index], login.ok[index] = latestReserveReading(auth, window)
	}
	return login
}

// isReserveMaxLogin reports whether auth is a Max login for the shared reserve: a Claude
// subscription (OAuth) login that is not a credit key (fallback-when) and not disabled.
// A Claude API key never is one, whatever its priority or fallback-when: the credit
// expiry drain removes fallback-when from a key it spends first, and that key must not
// count as a login with room. A disabled login never counts as having room.
func isReserveMaxLogin(auth *Auth) bool {
	return auth != nil && !auth.Disabled && auth.Status != StatusDisabled && isClaudeAuth(auth) &&
		auth.AuthKind() == AuthKindOAuth && authFallbackWhen(auth) == ""
}

// reserveAtRealLimit reports a live credential-wide quota cooldown on auth, the state an
// upstream 429 on the shared window leaves.
func reserveAtRealLimit(auth *Auth, now time.Time) bool {
	return auth.Quota.Exceeded && auth.Quota.Reason == "credential_quota" && auth.Quota.NextRecoverAt.After(now)
}

func isClaudeAuth(auth *Auth) bool {
	return canonicalSchedulingProvider(executorKeyFromAuth(auth)) == "claude"
}

// evaluate decides, once per check, the hold of every captured Max login from the
// shared reserve and whether any of them still has room. It takes only the book's lock.
func (c *reserveCheck) evaluate() {
	c.once.Do(func() {
		c.accounts = c.book.evaluate(c.gate, c.now, c.logins)
		room := false
		for _, login := range c.logins {
			if !c.accounts[login.id].held {
				room = true
			}
		}
		// With no Max login there is no reserve to keep.
		c.everyHeld = len(c.logins) > 0 && !room
	})
}

// accountHold returns the evaluated hold of auth: the captured one, or, for a credential
// the capture did not see, its own reserve as a pool of one.
func (c *reserveCheck) accountHold(auth *Auth) reserveAccount {
	c.evaluate()
	if account, ok := c.accounts[auth.ID]; ok {
		return account
	}
	return c.book.evaluate(c.gate, c.now, []reserveLogin{reserveLoginOf(auth)})[auth.ID]
}

// blocks reports whether the gate keeps auth from this request: every Claude credential
// under a manual hold; and, while every Max login is held or at a real limit, a held Max
// login and every Claude credential that is neither a Max login nor a credit key with
// fallback-when (an API key the credit expiry drain moved to the front). While one Max
// login still has room, nothing is blocked: the caller may use a held login up to its
// real limit. A paid credit key (fallback-when) is not blocked: its own fallback-when
// rule decides, and it serves only once every Max login is really at a limit. A reserve
// hold is not such a limit. Other providers are never blocked.
func (c *reserveCheck) blocks(auth *Auth) bool {
	if c == nil || auth == nil || !isClaudeAuth(auth) {
		return false
	}
	if c.gate.hold {
		return true
	}
	if authFallbackWhen(auth) != "" {
		return false
	}
	c.evaluate()
	if !c.everyHeld {
		return false
	}
	if !isReserveMaxLogin(auth) {
		return true
	}
	return c.accountHold(auth).held
}

// blocksHomeDispatch is blocks for a Home pick. Home owns the credential list, so the
// manager cannot see the other Max logins: each login Home picks keeps its own reserve
// as a pool of one, and the gate passes over a held login Home picked and asks Home again, so the caller is refused only once Home has no
// unheld Max login left. Home does not apply the fallback-when rule, so a credit key
// stays blocked there.
func (c *reserveCheck) blocksHomeDispatch(auth *Auth) bool {
	if c == nil || auth == nil || !isClaudeAuth(auth) {
		return false
	}
	if c.gate.hold || authFallbackWhen(auth) != "" {
		return true
	}
	return c.book.evaluate(c.gate, c.now, []reserveLogin{reserveLoginOf(auth)})[auth.ID].held
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
		if account := c.accountHold(auth); account.held && !account.until.IsZero() && (resetAt.IsZero() || account.until.Before(resetAt)) {
			resetAt = account.until
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
	// resetAt is the earliest time a held account is expected to be released (a limit step
	// or a reset); zero if unknown.
	resetAt time.Time
	now     time.Time
}

func (e *reserveHoldError) message() string {
	if e.manual {
		return "quota reserve hold: interactive Claude use is paused by the operator (routing.reserve-gate.hold) to preserve quota for automations"
	}
	message := "quota reserve hold: interactive Claude use is paused to preserve quota for automations"
	if !e.resetAt.IsZero() {
		message += "; available again at " + formatReserveReset(e.resetAt)
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
	// Home owns the credential list, so the trigger and the wind-down are not known here:
	// every pick keeps its own hold.
	reserve := m.reserveCallerCheck(opts.Metadata)
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
		if !reserve.blocksHomeDispatch(auth) {
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
