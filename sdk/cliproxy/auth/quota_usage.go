package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

const (
	// quotaUsageURL is Anthropic's usage report of one subscription (OAuth) login.
	quotaUsageURL  = "https://api.anthropic.com/api/oauth/usage?cedar_ember=1&skip_spend=1"
	quotaUsageBeta = "oauth-2025-04-20"
	// quotaUsageTimeout bounds one usage read. The read is a control call made before a
	// refusal or a credit key fallback, not part of an upstream model request.
	quotaUsageTimeout = 3 * time.Second
	// quotaUsageMinInterval is the least time between two usage reads of one login,
	// whatever their outcome.
	quotaUsageMinInterval = 60 * time.Second
	quotaUsageMaxBody     = 1 << 20
)

// quotaUsageReport is one usage read of a Max login. Utilizations are fractions (0.73 is
// 73%).
type quotaUsageReport struct {
	weeklyAll   float64
	weeklyReset time.Time // zero if the report carried none
	// weeklyScoped is the highest model-scoped weekly limit; scopedKnown reports one.
	weeklyScoped float64
	scopedKnown  bool
	// fiveHour is the 5-hour window; fiveHourKnown reports one.
	fiveHour      float64
	fiveHourKnown bool
}

// parseQuotaUsage reads the usage body: seven_day.utilization in percent (73.0 is 73%),
// five_hour, and limits[] with kind weekly_all and weekly_scoped. A body without a weekly
// all-model value is an error, and so is a value of the wrong type or a reset that is not
// a time in any of these windows, used or not, and a kind that is not a string, so the
// caller changes nothing. A key that is absent or null is not known.
func parseQuotaUsage(data []byte) (quotaUsageReport, error) {
	if !ojson.ValidBytes(data) {
		return quotaUsageReport{}, errors.New("parse usage: body is not JSON")
	}
	body := ojson.ParseBytes(data)
	if !body.IsObject() {
		return quotaUsageReport{}, errors.New("parse usage: body is not a JSON object")
	}
	var report quotaUsageReport
	weekly, found, weeklyReset, errWeekly := quotaUsageWindow(body, "seven_day", "utilization")
	if errWeekly != nil {
		return quotaUsageReport{}, errWeekly
	}
	if found {
		report.weeklyAll, report.weeklyReset = weekly, weeklyReset
	}
	limits := body.Get("limits")
	if limits.Exists() && limits.Type != ojson.Null && !limits.IsArray() {
		return quotaUsageReport{}, errors.New("parse usage: limits is not an array")
	}
	for i, limit := range limits.Array() {
		name := fmt.Sprintf("limits.%d", i)
		if !limit.IsObject() {
			return quotaUsageReport{}, fmt.Errorf("parse usage: %s is not an object", name)
		}
		kindValue := limit.Get("kind")
		if kindValue.Exists() && kindValue.Type != ojson.Null && kindValue.Type != ojson.String {
			return quotaUsageReport{}, fmt.Errorf("parse usage: %s.kind is not a string", name)
		}
		kind := kindValue.String()
		if kind != quotaLimitWeeklyAll && kind != quotaLimitWeeklyScoped {
			continue
		}
		fraction, known, reset, errLimit := quotaUsageWindow(body, name, "percent")
		if errLimit != nil {
			return quotaUsageReport{}, errLimit
		}
		if !known {
			continue
		}
		switch kind {
		case quotaLimitWeeklyAll:
			if !found {
				report.weeklyAll, report.weeklyReset, found = fraction, reset, true
			}
		case quotaLimitWeeklyScoped:
			if !report.scopedKnown || fraction > report.weeklyScoped {
				report.weeklyScoped, report.scopedKnown = fraction, true
			}
		}
	}
	if !found {
		return quotaUsageReport{}, errors.New("parse usage: no weekly all-model utilization")
	}
	fiveHour, fiveHourKnown, _, errFiveHour := quotaUsageWindow(body, "five_hour", "utilization")
	if errFiveHour != nil {
		return quotaUsageReport{}, errFiveHour
	}
	report.fiveHour, report.fiveHourKnown = fiveHour, fiveHourKnown
	return report, nil
}

// quotaUsageWindow reads the percent key of the window object as a fraction, and its
// reset. A window that is absent or null is not known. A window that is not an object,
// or whose percent or reset has the wrong form, is an error, with or without a percent.
func quotaUsageWindow(body ojson.Result, window, key string) (float64, bool, time.Time, error) {
	object := body.Get(window)
	if !object.Exists() || object.Type == ojson.Null {
		return 0, false, time.Time{}, nil
	}
	if !object.IsObject() {
		return 0, false, time.Time{}, fmt.Errorf("parse usage: %s is not an object", window)
	}
	fraction, known, errNumber := quotaUsageNumber(object.Get(key), window+"."+key)
	if errNumber != nil {
		return 0, false, time.Time{}, errNumber
	}
	reset, errReset := quotaUsageTime(object.Get("resets_at"), window+".resets_at")
	if errReset != nil {
		return 0, false, time.Time{}, errReset
	}
	return fraction, known, reset, nil
}

// quotaUsageNumber reads a percent as a fraction. Absent or null is not known; any other
// value that is not a number of 0 or more is an error.
func quotaUsageNumber(value ojson.Result, name string) (float64, bool, error) {
	if !value.Exists() || value.Type == ojson.Null {
		return 0, false, nil
	}
	if value.Type != ojson.Number {
		return 0, false, fmt.Errorf("parse usage: %s is not a number", name)
	}
	fraction := value.Float() / 100
	if math.IsNaN(fraction) || math.IsInf(fraction, 0) || fraction < 0 {
		return 0, false, fmt.Errorf("parse usage: %s is out of range", name)
	}
	return fraction, true, nil
}

// quotaUsageTime reads a reset time. Absent or null is the zero time (a fresh window has
// no reset yet); any other value that is not an RFC 3339 time is an error.
func quotaUsageTime(value ojson.Result, name string) (time.Time, error) {
	if !value.Exists() || value.Type == ojson.Null {
		return time.Time{}, nil
	}
	if value.Type != ojson.String {
		return time.Time{}, fmt.Errorf("parse usage: %s is not a string", name)
	}
	parsed, errParse := time.Parse(time.RFC3339, value.String())
	if errParse != nil {
		return time.Time{}, fmt.Errorf("parse usage: %s: %w", name, errParse)
	}
	return parsed.Truncate(time.Second), nil
}

// quotaUsageCall is one in-flight usage read of one login. applied is set before done is
// closed.
type quotaUsageCall struct {
	done    chan struct{}
	applied bool
}

type quotaUsageLogin struct {
	last     time.Time // start of the last read; zero before the first
	inflight *quotaUsageCall
}

// quotaUsageReader limits the usage reads: at most one per login per
// quotaUsageMinInterval, and concurrent requests share one in-flight read. The zero
// value is ready. now, read and timeout are replaced by tests.
type quotaUsageReader struct {
	mu     sync.Mutex
	logins map[string]*quotaUsageLogin

	now     func() time.Time
	read    func(ctx context.Context, auth *Auth) (quotaUsageReport, error)
	timeout time.Duration
	// url overrides quotaUsageURL.
	url string
}

func (r *quotaUsageReader) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// refreshQuotaUsage reads the real usage of every Max login and applies it. It is called
// only when no Max login can serve the caller: before a refusal and before a credit key
// serves. It reports whether a read was applied, so the caller selects again. The caller
// must not hold m.mu.
func (m *Manager) refreshQuotaUsage(ctx context.Context) bool {
	m.mu.RLock()
	logins := make([]*Auth, 0, len(m.auths))
	for _, auth := range m.auths {
		if isReserveMaxLogin(auth) {
			logins = append(logins, auth.Clone())
		}
	}
	m.mu.RUnlock()
	if len(logins) == 0 {
		return false
	}
	results := make(chan bool, len(logins))
	for _, login := range logins {
		go func(login *Auth) {
			results <- m.refreshQuotaUsageOf(ctx, login)
		}(login)
	}
	applied := false
	for range logins {
		if <-results {
			applied = true
		}
	}
	return applied
}

// refreshQuotaUsageOf reads and applies the usage of one login within the read limits.
// A request that finds a read in flight waits for it and shares its outcome; a request
// within quotaUsageMinInterval of the last read changes nothing.
func (m *Manager) refreshQuotaUsageOf(ctx context.Context, auth *Auth) bool {
	reader := &m.quotaUsage
	now := reader.clock()
	reader.mu.Lock()
	entry := reader.logins[auth.ID]
	if entry == nil {
		entry = &quotaUsageLogin{}
		if reader.logins == nil {
			reader.logins = make(map[string]*quotaUsageLogin)
		}
		reader.logins[auth.ID] = entry
	}
	if call := entry.inflight; call != nil {
		reader.mu.Unlock()
		select {
		case <-call.done:
			return call.applied
		case <-ctx.Done():
			return false
		}
	}
	if !entry.last.IsZero() && now.Sub(entry.last) < quotaUsageMinInterval {
		reader.mu.Unlock()
		return false
	}
	call := &quotaUsageCall{done: make(chan struct{})}
	entry.last, entry.inflight = now, call
	read, timeout := reader.read, reader.timeout
	reader.mu.Unlock()
	started := m.quotaUsageStart(auth.ID, now)
	if read == nil {
		read = m.readQuotaUsage
	}
	if timeout <= 0 {
		timeout = quotaUsageTimeout
	}

	// The read is shared, so one caller's cancellation must not end it.
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	report, errRead := read(readCtx, auth)
	cancel()
	if errRead == nil {
		errRead = report.checkAt(now)
	}
	if errRead != nil {
		log.Warnf("quota usage: read for login %s failed, nothing changed: %v", quotaAuthIndex(auth), errRead)
	} else {
		call.applied = m.applyQuotaUsage(context.WithoutCancel(ctx), auth.ID, report, started, reader.clock())
	}
	reader.mu.Lock()
	entry.inflight = nil
	reader.mu.Unlock()
	close(call.done)
	return call.applied
}

// readQuotaUsage performs the usage read of one login with its own access token.
func (m *Manager) readQuotaUsage(ctx context.Context, auth *Auth) (quotaUsageReport, error) {
	token, _ := auth.Metadata["access_token"].(string)
	token = strings.TrimSpace(token)
	if token == "" {
		return quotaUsageReport{}, errors.New("login has no access token")
	}
	url := m.quotaUsage.url
	if url == "" {
		if testing.Testing() {
			// A test never reaches the real endpoint; it sets url or read.
			return quotaUsageReport{}, errors.New("usage read is off in a test binary")
		}
		url = quotaUsageURL
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if errReq != nil {
		return quotaUsageReport{}, errReq
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", quotaUsageBeta)
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: quotaUsageTimeout}
	transport, errTransport := m.quotaUsageTransport(auth)
	if errTransport != nil {
		return quotaUsageReport{}, errTransport
	}
	if transport != nil {
		client.Transport = transport
	}
	resp, errDo := client.Do(req)
	if errDo != nil {
		// A client error can quote the request; keep the token out of the log.
		return quotaUsageReport{}, errors.New(strings.ReplaceAll(errDo.Error(), token, "[REDACTED]"))
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debugf("quota usage: close response body: %v", errClose)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return quotaUsageReport{}, fmt.Errorf("usage endpoint answered HTTP %d", resp.StatusCode)
	}
	data, errBody := io.ReadAll(io.LimitReader(resp.Body, quotaUsageMaxBody+1))
	if errBody != nil {
		return quotaUsageReport{}, errBody
	}
	if len(data) > quotaUsageMaxBody {
		return quotaUsageReport{}, errors.New("usage body too large")
	}
	return parseQuotaUsage(data)
}

// quotaUsageTransport returns the transport of the login's own proxy-url, else of the
// global proxy-url, else nil for the default transport. A proxy-url that cannot be used is
// an error: the read must not leave through another route than the configured one.
func (m *Manager) quotaUsageTransport(auth *Auth) (http.RoundTripper, error) {
	if transport := m.roundTripperFor(auth); transport != nil {
		return transport, nil
	}
	proxyURL, name := "", "login proxy-url"
	if auth != nil {
		proxyURL = strings.TrimSpace(auth.ProxyURL)
	}
	if proxyURL == "" {
		if cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config); cfg != nil {
			proxyURL, name = strings.TrimSpace(cfg.ProxyURL), "proxy-url"
		}
	}
	if proxyURL == "" {
		return nil, nil
	}
	transport, _, errBuild := proxyutil.BuildHTTPTransport(proxyURL)
	if errBuild != nil {
		return nil, fmt.Errorf("%s transport: %w", name, errBuild)
	}
	if transport == nil {
		// The setting says to inherit the environment, which the default transport does.
		return nil, nil
	}
	// The transport serves this one read; do not keep its connection.
	transport.DisableKeepAlives = true
	return transport, nil
}

// quotaUsageStart is what the proxy knew of a login when a usage read of it started.
type quotaUsageStart struct {
	at        time.Time
	cooldowns []quotaCooldown
}

// quotaUsageStart records the live quota cooldowns of a login at the start of a read.
func (m *Manager) quotaUsageStart(authID string, now time.Time) quotaUsageStart {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return quotaUsageStart{at: now, cooldowns: quotaCooldownsOf(m.auths[authID], "", now)}
}

// had reports whether cooldown was live, with the same end, when the read started.
func (s quotaUsageStart) had(cooldown quotaCooldown) bool {
	return slices.ContainsFunc(s.cooldowns, func(known quotaCooldown) bool {
		return known.limit == cooldown.limit && known.until.Equal(cooldown.until)
	})
}

// checkAt rejects a report whose weekly reset is not after at, the start of the read: a
// fresh report of a running window names a reset to come, and one that does not is not
// evidence of room.
func (r quotaUsageReport) checkAt(at time.Time) error {
	if !r.weeklyReset.IsZero() && !r.weeklyReset.After(at) {
		return errors.New("usage report: the weekly reset is not in the future")
	}
	return nil
}

// applyQuotaUsage applies a usage read to one login: when every limit that cools the
// login has room again it clears the cooldown, and the weekly reading replaces the
// login's reserve reading. started is the state at the start of the read; the reading is
// dated there, so a header reading that arrived during the read stays the newer one. The
// lift is the reset the owner can run by hand (ResetQuota): it clears every cooldown of
// the login, also one that has another cause than quota. It reports whether that lifted
// a cooldown or changed the reading, so a new selection can differ. The caller must not
// hold m.mu.
func (m *Manager) applyQuotaUsage(ctx context.Context, authID string, report quotaUsageReport, started quotaUsageStart, now time.Time) bool {
	m.mu.RLock()
	auth := m.auths[authID]
	known := isReserveMaxLogin(auth)
	index := quotaAuthIndex(auth)
	m.mu.RUnlock()
	if !known {
		return false
	}
	reset, _, errReset := m.resetQuotaIf(ctx, authID, func(auth *Auth) bool {
		return quotaUsageLiftsCooldown(auth, report, started, now)
	})
	lift := errReset == nil && reset != nil
	switch {
	case errReset != nil:
		log.Warnf("quota usage: login %s has room again but the cooldown reset failed: %v", index, errReset)
	case lift:
		log.Infof("quota usage: login %s has room again (weekly utilization %.2f); cooldown cleared", index, report.weeklyAll)
	}
	reading := reserveReading{utilization: report.weeklyAll, resetAt: report.weeklyReset, observedAt: started.at, source: reserveReadingFromUsage}
	changed := false
	for _, window := range reserveWindows {
		if m.reserveHolds.merge(authID, window, reading) {
			changed = true
		}
	}
	if gate := m.reserveGate.Load(); gate != nil {
		m.observeReserveTrigger(gate, now)
	}
	m.syncQuotaState(now)
	return lift || changed
}

// quotaUsageLiftsCooldown reports whether auth has a live quota cooldown and the usage
// report shows room for every limit that cools it. A limit the report does not carry
// (the 5-hour window, a model-scoped weekly limit) or a cooldown whose limit is not
// known has no proven room and stays. So does a cooldown that began or changed after the
// read started: the report is older than the response behind it.
func quotaUsageLiftsCooldown(auth *Auth, report quotaUsageReport, started quotaUsageStart, now time.Time) bool {
	cooldowns := quotaCooldownsOf(auth, "", now)
	for _, cooldown := range cooldowns {
		if !started.had(cooldown) || !report.hasRoom(cooldown.limit) {
			return false
		}
	}
	return len(cooldowns) > 0
}

// hasRoom reports whether the report shows limit below 100%.
func (r quotaUsageReport) hasRoom(limit string) bool {
	switch limit {
	case quotaLimitWeeklyAll:
		return r.weeklyAll < 1
	case quotaLimitWeeklyScoped:
		return r.weeklyAll < 1 && r.scopedKnown && r.weeklyScoped < 1
	case quotaLimitFiveHour:
		return r.weeklyAll < 1 && r.fiveHourKnown && r.fiveHour < 1
	default:
		return false
	}
}

// quotaAuthIndex is auth's stable index (a hash, so it names no account) without
// assigning it, for use under a read lock.
func quotaAuthIndex(auth *Auth) string {
	if auth == nil {
		return ""
	}
	if index := strings.TrimSpace(auth.Index); index != "" {
		return index
	}
	if seed := auth.indexSeed(); seed != "" {
		return stableAuthIndex(seed)
	}
	return ""
}
