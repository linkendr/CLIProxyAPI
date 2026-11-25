package auth

import (
	"context"
	"sync/atomic"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// Fork-owned quota state of the Manager: the reserve gate, its hold book and trigger,
// the on-demand usage reads and the quota state file. conductor.go and
// conductor_cooldown.go keep one-line calls into this file.

// managerQuota is the fork's part of Manager, embedded in its own block after
// runtimeConfig so upstream's field lines stay as upstream has them. Not at the top:
// upstream adds Manager fields there (inFlightTracker, 2026-11-15).
type managerQuota struct {
	// reserveGate is routing.reserve-gate compiled from runtimeConfig; nil when disabled.
	reserveGate atomic.Pointer[reserveGate]
	// reserveHolds keeps the reserve gate's readings and holds per account window.
	reserveHolds reserveHoldBook
	// reserveTrigger keeps when every Max login first became held (in memory only).
	reserveTrigger reserveTriggerClock
	// quotaUsage limits the on-demand usage reads of the Max logins.
	quotaUsage quotaUsageReader
	// quotaState writes the quota state file.
	quotaState quotaStateWriter
}

// applyReserveGateConfig compiles routing.reserve-gate from cfg and points the hold book
// at its state file. A disabled gate also clears the trigger clock.
func (m *Manager) applyReserveGateConfig(cfg *internalconfig.Config) {
	compiledReserveGate := compileReserveGate(cfg.Routing.ReserveGate)
	m.reserveGate.Store(compiledReserveGate)
	if compiledReserveGate == nil {
		m.reserveTrigger.clear()
	}
	m.reserveHolds.useStateFile(reserveStatePath(cfg), time.Now())
}

// ResetQuota clears quota/cooldown state for an auth and resumes registry routing. For a
// Claude credential it also drops the passive quota snapshot and the reserve gate's
// reading: both describe the window before the reset, and a stale reading would keep
// the login held.
//
// Its body is resetQuotaIf (conductor_cooldown.go): ResetQuota with a condition that is
// checked under m.mu, in the same critical section as the reset, so a cooldown recorded
// after the caller looked is not cleared. A nil due always resets. A reset that due
// refuses returns no auth. A reserve reading observed after keepAfter survives the reset
// (zero keeps none).
func (m *Manager) ResetQuota(ctx context.Context, authID string) (*Auth, []string, error) {
	return m.resetQuotaIf(ctx, authID, nil, time.Time{})
}

// clearReserveObservationsLocked drops the passive quota snapshot of a Claude credential
// and its models, and reports whether auth is one, so the reset also drops its reserve
// reading. The caller holds m.mu.
func clearReserveObservationsLocked(auth *Auth) bool {
	if !isClaudeAuth(auth) {
		return false
	}
	auth.Quota.ClearObservationSignals()
	for _, state := range auth.ModelStates {
		if state != nil {
			state.Quota.ClearObservationSignals()
		}
	}
	return true
}

// dropReserveReading runs after a reset released m.mu. For a Claude credential (drop) it
// drops the reserve reading now and re-observes the trigger; the returned function
// writes the quota state file, so the caller defers it.
func (m *Manager) dropReserveReading(drop bool, authID string, keepAfter, now time.Time) func() {
	if !drop {
		return func() {}
	}
	m.reserveHolds.drop(authID, keepAfter, now)
	if gate := m.reserveGate.Load(); gate != nil {
		m.observeReserveTrigger(gate, now)
	}
	return func() { m.syncQuotaState(now) }
}

// recordReserveResult feeds a MarkResult outcome to the reserve gate and the quota state
// file. authSnapshot is nil when the result named no known auth.
func (m *Manager) recordReserveResult(authSnapshot *Auth, result Result) {
	if authSnapshot != nil && !result.SkipQuotaObservation {
		m.recordReserveReadings(authSnapshot)
	}
	if authSnapshot != nil && isClaudeAuth(authSnapshot) {
		m.syncQuotaState(time.Now())
	}
}
