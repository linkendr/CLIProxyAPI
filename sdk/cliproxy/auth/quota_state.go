package auth

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	quotaStateSchema = "cliproxy.quota_state.v1"
	// quotaStateEnv overrides the path of the quota state file.
	quotaStateEnv = "CLIPROXY_QUOTA_STATE"
	// quotaStateRefresh is the age at which an unchanged file is written again. With
	// quotaStateTick it keeps the file younger than 5 minutes while the proxy runs.
	quotaStateRefresh = 4 * time.Minute
	quotaStateTick    = 30 * time.Second

	quotaRouteMax     = "max"     // a Max login serves a new prompt
	quotaRouteCredit  = "credit"  // a credit key (fallback-when) serves it
	quotaRouteRefused = "refused" // HTTP 429

	quotaLoginOK     = "ok"
	quotaLoginHeld   = "held"   // all its headroom is shared reserve
	quotaLoginCooled = "cooled" // a live quota cooldown after a real upstream limit
)

// quotaStateDoc is the quota state file: what a new prompt of each kind of caller gets
// now, the shared reserve, and every Max login's state and newest weekly reading. Other
// tools read it; it carries no token, key, email or account name.
type quotaStateDoc struct {
	Schema    string    `json:"schema"`
	WrittenAt time.Time `json:"written_at"`
	// Interactive is a caller whose key is not on the reserve exempt list; Automation
	// is an exempt caller.
	Interactive quotaStateRoute   `json:"interactive"`
	Automation  quotaStateRoute   `json:"automation"`
	Reserve     quotaStateReserve `json:"reserve"`
	Logins      []quotaStateLogin `json:"logins"`
}

type quotaStateRoute struct {
	Route string    `json:"route"`
	Since time.Time `json:"since"` // when Route last changed, as far as this process saw
	Limit string    `json:"limit"`
	// RetryAt is when Route is expected to return to max.
	RetryAt *time.Time `json:"retry_at"`
	// WindDownUntil is set while running tasks are still served after new prompts are
	// refused.
	WindDownUntil *time.Time `json:"wind_down_until"`
	// Message is the refusal's sentence without its countdown; empty when Route is max.
	Message string `json:"message"`
}

type quotaStateReserve struct {
	Active bool     `json:"active"` // the trigger: every Max login is held
	Size   float64  `json:"size"`   // share of one login's weekly quota; 0 when the gate is off
	On     []string `json:"on"`     // auth_index of the logins that carry the reserve
}

type quotaStateLogin struct {
	AuthIndex   string     `json:"auth_index"`
	State       string     `json:"state"`
	Limit       string     `json:"limit"`
	Utilization *float64   `json:"utilization"`
	ResetsAt    *time.Time `json:"resets_at"`
	ReadAt      *time.Time `json:"read_at"`
	ReadFrom    string     `json:"read_from"` // "headers" or "usage"; empty with no reading
	// cooledUntil is when a cooled login serves again; it is not written.
	cooledUntil time.Time
}

// quotaStateWriter writes the quota state file when its content changes and when the
// file is older than quotaStateRefresh.
type quotaStateWriter struct {
	mu sync.Mutex
	// last is the content last written, without written_at and read_at.
	last    string
	written time.Time
	// interactive and automation are the routes last built, kept for their since.
	interactive quotaStateRoute
	automation  quotaStateRoute
	failed      bool
}

// quotaStatePath returns the quota state file: CLIPROXY_QUOTA_STATE, else
// $HOME/.local/state/cliproxyapi/quota-state.json. Empty means no file is written. A
// test binary writes none unless the variable is set, so a test run cannot replace the
// file of the proxy running on the same machine.
func quotaStatePath() string {
	if path := strings.TrimSpace(os.Getenv(quotaStateEnv)); path != "" {
		return path
	}
	if testing.Testing() {
		return ""
	}
	home, errHome := os.UserHomeDir()
	if errHome != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "state", "cliproxyapi", "quota-state.json")
}

// syncQuotaState builds the quota state and writes the file if its content changed or
// the file is due. With no Max login there is nothing to describe and nothing is
// written. The caller must not hold m.mu.
func (m *Manager) syncQuotaState(now time.Time) {
	if m == nil {
		return
	}
	path := quotaStatePath()
	if path == "" {
		return
	}
	writer := &m.quotaState
	writer.mu.Lock()
	defer writer.mu.Unlock()
	doc, ok := m.buildQuotaState(now)
	if !ok {
		return
	}
	doc.Interactive.Since = quotaRouteSince(writer.interactive, doc.Interactive.Route, now)
	doc.Automation.Since = quotaRouteSince(writer.automation, doc.Automation.Route, now)
	writer.interactive, writer.automation = doc.Interactive, doc.Automation
	content := doc.content()
	if content == writer.last && now.Sub(writer.written) < quotaStateRefresh {
		return
	}
	doc.WrittenAt = now.UTC().Truncate(time.Second)
	data, errMarshal := json.MarshalIndent(doc, "", "  ")
	if errMarshal == nil {
		errMarshal = writeStateFileAtomic(path, append(data, '\n'))
	}
	if errMarshal != nil {
		if !writer.failed {
			log.Warnf("quota state: cannot write %s: %v", path, errMarshal)
		}
		writer.failed = true
		return
	}
	writer.failed = false
	writer.last, writer.written = content, now
}

// quotaRouteSince keeps the since of an unchanged route.
func quotaRouteSince(prev quotaStateRoute, route string, now time.Time) time.Time {
	if prev.Route == route && !prev.Since.IsZero() {
		return prev.Since
	}
	return now.UTC().Truncate(time.Second)
}

// content is the document without the fields that change on every write or response
// (written_at, read_at), for change detection.
func (d quotaStateDoc) content() string {
	d.WrittenAt = time.Time{}
	logins := make([]quotaStateLogin, len(d.Logins))
	copy(logins, d.Logins)
	for i := range logins {
		logins[i].ReadAt = nil
	}
	d.Logins = logins
	data, _ := json.Marshal(d)
	return string(data)
}

// runQuotaStateTicker keeps the quota state file fresh until ctx ends. Changes that
// only the clock brings (a cooldown that ends, a reserve step) reach the file here.
func (m *Manager) runQuotaStateTicker(ctx context.Context) {
	if quotaStatePath() == "" {
		return
	}
	m.syncQuotaState(time.Now())
	ticker := time.NewTicker(quotaStateTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			m.syncQuotaState(now)
		}
	}
}

// quotaStateTime returns t in UTC to the second, or nil for the zero time.
func quotaStateTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	utc := t.UTC().Truncate(time.Second)
	return &utc
}

// buildQuotaState describes the Max logins at now. It reports false when there is no
// Max login. It takes m.mu (read) and the reserve book's lock.
//
// A login is cooled while it has a live quota cooldown, credential-wide or of one model;
// with every login cooled, the routes describe the model that is limited. The
// interactive route differs from the automation route only through the reserve gate:
// the manual hold, or every login held while not every login is at a real limit.
func (m *Manager) buildQuotaState(now time.Time) (quotaStateDoc, bool) {
	type maxLogin struct {
		index    string
		reserve  reserveLogin
		cooldown quotaCooldown
		cooled   bool
	}
	var logins []maxLogin
	creditUsable := false
	m.mu.RLock()
	for _, auth := range m.auths {
		switch {
		case isReserveMaxLogin(auth):
			login := maxLogin{index: quotaAuthIndex(auth), reserve: reserveLoginOf(auth)}
			login.reserve.atLimit = reserveAtRealLimit(auth, now)
			login.cooldown, login.cooled = quotaCooldownOf(auth, "", now)
			logins = append(logins, login)
		case isClaudeAuth(auth) && authFallbackWhen(auth) != "" && quotaCreditKeyUsable(auth, now):
			creditUsable = true
		}
	}
	m.mu.RUnlock()
	if len(logins) == 0 {
		return quotaStateDoc{}, false
	}
	sort.Slice(logins, func(i, j int) bool {
		if logins[i].index != logins[j].index {
			return logins[i].index < logins[j].index
		}
		return logins[i].reserve.id < logins[j].reserve.id
	})

	gate := m.reserveGate.Load()
	var accounts map[string]reserveAccount
	everyHeld := false
	if gate != nil {
		reserveLogins := make([]reserveLogin, len(logins))
		for i, login := range logins {
			reserveLogins[i] = login.reserve
		}
		check := &reserveCheck{gate: gate, book: &m.reserveHolds, now: now, logins: reserveLogins}
		check.evaluate()
		accounts, everyHeld = check.accounts, check.everyHeld
	}

	doc := quotaStateDoc{Schema: quotaStateSchema, Reserve: quotaStateReserve{On: []string{}}, Logins: make([]quotaStateLogin, 0, len(logins))}
	const window = 0 // reserveWindows[0], the 7-day window
	var entries []reservePoolEntry
	var entryIndexes []string
	everyCooled := true
	var next quotaCooldown
	var release time.Time
	for _, login := range logins {
		reading, ok := login.reserve.readings[window], login.reserve.ok[window]
		if known, okKnown := m.reserveHolds.reading(login.reserve.id, reserveWindows[window]); okKnown && (!ok || !known.observedAt.Before(reading.observedAt)) {
			reading, ok = known, true
		}
		if ok || login.reserve.atLimit {
			entries = append(entries, reserveEntryOf(login.reserve.id, reading, login.reserve.atLimit, now))
			entryIndexes = append(entryIndexes, login.index)
		}
		entry := quotaStateLogin{AuthIndex: login.index, State: quotaLoginOK}
		account := accounts[login.reserve.id]
		switch {
		case login.cooled:
			entry.State, entry.Limit, entry.cooledUntil = quotaLoginCooled, login.cooldown.limit, login.cooldown.until
			if next.until.IsZero() || login.cooldown.until.Before(next.until) {
				next = login.cooldown
			}
		case account.held:
			entry.State, entry.Limit = quotaLoginHeld, quotaLimitReserve
		}
		if !login.cooled {
			everyCooled = false
		}
		if account.held && !account.until.IsZero() && (release.IsZero() || account.until.Before(release)) {
			release = account.until
		}
		// A reading whose reset has passed describes the window before it rolled over.
		if ok && (reading.resetAt.IsZero() || reading.resetAt.After(now)) {
			utilization := reading.utilization
			entry.Utilization, entry.ResetsAt, entry.ReadAt = &utilization, quotaStateTime(reading.resetAt), quotaStateTime(reading.observedAt)
			entry.ReadFrom = "headers"
			if reading.source == reserveReadingFromUsage {
				entry.ReadFrom = reserveReadingFromUsage
			}
		}
		doc.Logins = append(doc.Logins, entry)
	}

	if gate != nil {
		size := gate.reserveAt(nextReserveReset(entries), now)
		doc.Reserve.Active, doc.Reserve.Size = everyHeld, math.Round(size*1e4)/1e4
		_, shares := placeReserveShares(entries, size)
		for i, share := range shares {
			if share > reserveEpsilon {
				doc.Reserve.On = append(doc.Reserve.On, entryIndexes[i])
			}
		}
		sort.Strings(doc.Reserve.On)
	}

	doc.Automation = quotaStateRoute{Route: quotaRouteMax}
	if everyCooled {
		doc.Automation = quotaStateRoute{
			Route: quotaRouteRefused, Limit: next.limit, RetryAt: quotaStateTime(next.until),
			Message: quotaRefusalSentence(next.limit, next.until, time.Local),
		}
		if creditUsable {
			doc.Automation.Route = quotaRouteCredit
		}
	}
	doc.Interactive = doc.Automation
	switch {
	case gate == nil:
	case gate.hold:
		doc.Interactive = quotaStateRoute{Route: quotaRouteRefused, Limit: quotaLimitReserve, Message: (&reserveHoldError{manual: true}).message()}
	case everyHeld && !everyCooled:
		doc.Interactive = quotaStateRoute{
			Route: quotaRouteRefused, Limit: quotaLimitReserve, RetryAt: quotaStateTime(release),
			Message: quotaRefusalSentence(quotaLimitReserve, release, time.Local),
		}
		if since := m.reserveTrigger.start(); !since.IsZero() && gate.windDown > 0 && since.Add(gate.windDown).After(now) {
			doc.Interactive.WindDownUntil = quotaStateTime(since.Add(gate.windDown))
		}
	}
	return doc, true
}

// quotaCreditKeyUsable reports whether a credit key could serve now: it is not
// disabled, not unauthorized and not cooling down.
func quotaCreditKeyUsable(auth *Auth, now time.Time) bool {
	if auth.Disabled || auth.Status == StatusDisabled || hasUnauthorizedAuthFailure(auth) {
		return false
	}
	if auth.Unavailable && auth.NextRetryAfter.After(now) {
		return false
	}
	return !auth.Quota.Exceeded || !auth.Quota.NextRecoverAt.After(now)
}
