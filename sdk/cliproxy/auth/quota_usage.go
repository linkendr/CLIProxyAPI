package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

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

type quotaUsageWindowBody struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    string   `json:"resets_at"`
}

type quotaUsageBody struct {
	FiveHour *quotaUsageWindowBody `json:"five_hour"`
	SevenDay *quotaUsageWindowBody `json:"seven_day"`
	Limits   []struct {
		Kind     string   `json:"kind"`
		Percent  *float64 `json:"percent"`
		ResetsAt string   `json:"resets_at"`
	} `json:"limits"`
}

// parseQuotaUsage reads the usage body: seven_day.utilization in percent (73.0 is 73%),
// and limits[] with kind weekly_all and weekly_scoped. A body without a weekly all-model
// value is an error, so the caller changes nothing.
func parseQuotaUsage(data []byte) (quotaUsageReport, error) {
	var body quotaUsageBody
	if errJSON := json.Unmarshal(data, &body); errJSON != nil {
		return quotaUsageReport{}, fmt.Errorf("parse usage: %w", errJSON)
	}
	var report quotaUsageReport
	found := false
	if body.SevenDay != nil && body.SevenDay.Utilization != nil {
		report.weeklyAll, found = *body.SevenDay.Utilization/100, true
		report.weeklyReset = parseQuotaUsageTime(body.SevenDay.ResetsAt)
	}
	for _, limit := range body.Limits {
		if limit.Percent == nil {
			continue
		}
		fraction := *limit.Percent / 100
		switch limit.Kind {
		case quotaLimitWeeklyAll:
			if !found {
				report.weeklyAll, found = fraction, true
				report.weeklyReset = parseQuotaUsageTime(limit.ResetsAt)
			}
		case quotaLimitWeeklyScoped:
			if validQuotaFraction(fraction) && (!report.scopedKnown || fraction > report.weeklyScoped) {
				report.weeklyScoped, report.scopedKnown = fraction, true
			}
		}
	}
	if !found || !validQuotaFraction(report.weeklyAll) {
		return quotaUsageReport{}, errors.New("parse usage: no weekly all-model utilization")
	}
	if body.FiveHour != nil && body.FiveHour.Utilization != nil {
		if fraction := *body.FiveHour.Utilization / 100; validQuotaFraction(fraction) {
			report.fiveHour, report.fiveHourKnown = fraction, true
		}
	}
	return report, nil
}

func validQuotaFraction(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

func parseQuotaUsageTime(value string) time.Time {
	parsed, errParse := time.Parse(time.RFC3339, strings.TrimSpace(value))
	if errParse != nil {
		return time.Time{}
	}
	return parsed.Truncate(time.Second)
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
	if errRead != nil {
		log.Warnf("quota usage: read for login %s failed, nothing changed: %v", quotaAuthIndex(auth), errRead)
	} else {
		call.applied = m.applyQuotaUsage(context.WithoutCancel(ctx), auth.ID, report, reader.clock())
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
	if transport := m.quotaUsageTransport(auth); transport != nil {
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

// quotaUsageTransport returns the login's proxy transport, else the global proxy-url
// transport, else nil for the default transport.
func (m *Manager) quotaUsageTransport(auth *Auth) http.RoundTripper {
	if transport := m.roundTripperFor(auth); transport != nil {
		return transport
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil || strings.TrimSpace(cfg.ProxyURL) == "" {
		return nil
	}
	transport, _, errBuild := proxyutil.BuildHTTPTransport(cfg.ProxyURL)
	if errBuild != nil || transport == nil {
		return nil
	}
	// The transport serves this one read; do not keep its connection.
	transport.DisableKeepAlives = true
	return transport
}

// applyQuotaUsage applies a fresh usage read to one login: when every limit that cools
// the login has room again it clears the cooldown (ResetQuota), and the fresh weekly
// reading replaces the login's reserve reading. It reports whether that lifted a
// cooldown or changed the reading, so a new selection can differ. The caller must not
// hold m.mu.
func (m *Manager) applyQuotaUsage(ctx context.Context, authID string, report quotaUsageReport, now time.Time) bool {
	m.mu.RLock()
	auth := m.auths[authID]
	known := isReserveMaxLogin(auth)
	lift := known && quotaUsageLiftsCooldown(auth, report, now)
	index := quotaAuthIndex(auth)
	m.mu.RUnlock()
	if !known {
		return false
	}
	if lift {
		if _, _, errReset := m.ResetQuota(ctx, authID); errReset != nil {
			log.Warnf("quota usage: login %s has room again but the cooldown reset failed: %v", index, errReset)
			lift = false
		} else {
			log.Infof("quota usage: login %s has room again (weekly utilization %.2f); cooldown cleared", index, report.weeklyAll)
		}
	}
	reading := reserveReading{utilization: report.weeklyAll, resetAt: report.weeklyReset, observedAt: now, source: reserveReadingFromUsage}
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
// known has no proven room and stays.
func quotaUsageLiftsCooldown(auth *Auth, report quotaUsageReport, now time.Time) bool {
	cooldowns := quotaCooldownsOf(auth, "", now)
	for _, cooldown := range cooldowns {
		if !report.hasRoom(cooldown.limit) {
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
