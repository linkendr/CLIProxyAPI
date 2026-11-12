package auth

import (
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// The limit of a login steps up as its own weekly reset comes near: the threshold while
// more than near-reset-hours remain, near-reset-threshold from then on, and 1 (nothing
// kept) in the final final-release-hours. A login with no known reset keeps the threshold.
func TestReserveGateLimitStepsBeforeReset(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	intPtr := func(v int) *int { return &v }
	for name, tt := range map[string]struct {
		cfg   internalconfig.ReserveGateConfig
		until time.Duration // reset - now; 0 means no known reset
		want  float64
	}{
		"no known reset":                 {internalconfig.ReserveGateConfig{Enabled: true}, 0, 0.95},
		"two days":                       {internalconfig.ReserveGateConfig{Enabled: true}, 48 * time.Hour, 0.95},
		"just over six hours":            {internalconfig.ReserveGateConfig{Enabled: true}, 6*time.Hour + time.Second, 0.95},
		"six hours":                      {internalconfig.ReserveGateConfig{Enabled: true}, 6 * time.Hour, 0.99},
		"two hours":                      {internalconfig.ReserveGateConfig{Enabled: true}, 2 * time.Hour, 0.99},
		"just over one hour":             {internalconfig.ReserveGateConfig{Enabled: true}, time.Hour + time.Second, 0.99},
		"one hour":                       {internalconfig.ReserveGateConfig{Enabled: true}, time.Hour, 1},
		"one minute":                     {internalconfig.ReserveGateConfig{Enabled: true}, time.Minute, 1},
		"higher threshold far out":       {internalconfig.ReserveGateConfig{Enabled: true, Threshold: 0.97}, 48 * time.Hour, 0.97},
		"threshold above near level":     {internalconfig.ReserveGateConfig{Enabled: true, Threshold: 1}, 3 * time.Hour, 1},
		"custom near window and level":   {internalconfig.ReserveGateConfig{Enabled: true, NearResetHours: intPtr(12), NearResetThreshold: 0.98}, 10 * time.Hour, 0.98},
		"custom final window":            {internalconfig.ReserveGateConfig{Enabled: true, FinalReleaseHours: intPtr(2)}, 90 * time.Minute, 1},
		"near window off":                {internalconfig.ReserveGateConfig{Enabled: true, NearResetHours: intPtr(0), FinalReleaseHours: intPtr(0)}, 30 * time.Minute, 0.95},
		"final window off, near applies": {internalconfig.ReserveGateConfig{Enabled: true, FinalReleaseHours: intPtr(0)}, 30 * time.Minute, 0.99},
	} {
		t.Run(name, func(t *testing.T) {
			gate := compileReserveGate(tt.cfg)
			var reset time.Time
			if tt.until > 0 {
				reset = now.Add(tt.until)
			}
			if got := gate.limit(reset, now); got != tt.want {
				t.Fatalf("limit(reset in %v) = %v, want %v", tt.until, got, tt.want)
			}
		})
	}
}

// Each login is judged by its own reset. a resets in 30 h and b in 5 h, both at 97%:
// b is below its 99% limit, so ordinary callers have room. At 99% b is held too and
// ordinary callers are refused while automations keep the last 1%. In b's final hour
// its limit is 100%, so ordinary callers may use what is left.
func TestReserveGatePerLoginLimitByOwnReset(t *testing.T) {
	forEachReserveRig(t, reserveGateOn(nil), func(t *testing.T, rig *reserveRig) {
		rig.observe(t, rig.a, "7d", 0.97, time.Now().Add(30*time.Hour))
		rig.observe(t, rig.b, "7d", 0.97, time.Now().Add(5*time.Hour))
		if seen := rig.picked(t, reserveTestUserKey, 4); seen[rig.a]+seen[rig.b] != 4 {
			t.Fatalf("ordinary picks = %v, want Max logins: b at 97%% is under its 99%% limit", seen)
		}

		bReset := time.Now().Add(5 * time.Hour)
		rig.observe(t, rig.b, "7d", 0.99, bReset)
		_, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel)
		requireReserveHold(t, err)
		// b is released for everyone one hour before its reset, before a's reset.
		requireRetryAfterNear(t, err, bReset.Add(-time.Hour))
		if seen := rig.picked(t, reserveTestExemptKey, 3); seen[rig.a]+seen[rig.b] != 3 {
			t.Fatalf("exempt picks = %v, want Max logins", seen)
		}

		rig.observe(t, rig.b, "7d", 0.99, time.Now().Add(50*time.Minute))
		if seen := rig.picked(t, reserveTestUserKey, 4); seen[rig.a]+seen[rig.b] != 4 {
			t.Fatalf("ordinary picks = %v, want Max logins: b is in its final hour", seen)
		}
	})
}

// A held login is released at the step, without a new reading: the limit follows the
// clock, not only the readings.
func TestReserveGateHoldLiftsWhenWindowOpens(t *testing.T) {
	gate := compileReserveGate(reserveGateOn(nil))
	var book reserveHoldBook
	reset := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	reading := reserveReading{utilization: 0.97, resetAt: reset, observedAt: reset.Add(-30 * time.Hour)}
	if hold := book.update("work", "7d", reading, true, gate, reset.Add(-7*time.Hour)); !hold.held || !hold.until.Equal(reset.Add(-6*time.Hour)) {
		t.Fatalf("7 h before reset at 97%%: %+v, want held until the 99%% step 6 h before reset", hold)
	}
	if hold := book.update("work", "7d", reserveReading{}, false, gate, reset.Add(-6*time.Hour)); hold.held {
		t.Fatalf("6 h before reset at 97%%: %+v, want released (limit 99%%)", hold)
	}
	reading.utilization = 0.99
	if hold := book.update("work", "7d", reading, true, gate, reset.Add(-3*time.Hour)); !hold.held || !hold.until.Equal(reset.Add(-time.Hour)) {
		t.Fatalf("3 h before reset at 99%%: %+v, want held until 1 h before reset", hold)
	}
	if hold := book.update("work", "7d", reserveReading{}, false, gate, reset.Add(-time.Hour)); hold.held {
		t.Fatalf("1 h before reset at 99%%: %+v, want released (limit 100%%)", hold)
	}
}
