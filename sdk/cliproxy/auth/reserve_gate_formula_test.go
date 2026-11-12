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

// The reserve follows the next reset of any login. b resets in 5 h, so the reserve is
// 1% and 6% left leaves room. With 1% left every login is held and automations keep it.
// In the final hour before b's reset no reserve is kept.
func TestReserveGatePoolLimitFollowsNextReset(t *testing.T) {
	forEachReserveRig(t, reserveGateOn(nil), func(t *testing.T, rig *reserveRig) {
		rig.observe(t, rig.a, "7d", 0.97, time.Now().Add(30*time.Hour))
		rig.observe(t, rig.b, "7d", 0.97, time.Now().Add(5*time.Hour))
		if seen := rig.picked(t, reserveTestUserKey, 4); seen[rig.a]+seen[rig.b] != 4 {
			t.Fatalf("ordinary picks = %v, want Max logins: 6%% left is above the 1%% reserve", seen)
		}

		bReset := time.Now().Add(5 * time.Hour)
		rig.observe(t, rig.a, "7d", 1.00, time.Now().Add(30*time.Hour))
		rig.observe(t, rig.b, "7d", 0.99, bReset)
		_, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel)
		requireReserveHold(t, err)
		// b is released for everyone one hour before its reset.
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
	reading := func(utilization float64) []reserveLogin {
		login := reserveLogin{id: "work"}
		login.readings[0] = reserveReading{utilization: utilization, resetAt: reset, observedAt: reset.Add(-30 * time.Hour)}
		login.ok[0] = true
		return []reserveLogin{login}
	}
	known := []reserveLogin{{id: "work"}}
	if hold := book.evaluate(gate, reset.Add(-7*time.Hour), reading(0.97))["work"]; !hold.held || !hold.until.Equal(reset.Add(-6*time.Hour)) {
		t.Fatalf("7 h before reset at 97%%: %+v, want held until the 99%% step 6 h before reset", hold)
	}
	if hold := book.evaluate(gate, reset.Add(-6*time.Hour), known)["work"]; hold.held {
		t.Fatalf("6 h before reset at 97%%: %+v, want released (limit 99%%)", hold)
	}
	if hold := book.evaluate(gate, reset.Add(-3*time.Hour), reading(0.99))["work"]; !hold.held || !hold.until.Equal(reset.Add(-time.Hour)) {
		t.Fatalf("3 h before reset at 99%%: %+v, want held until 1 h before reset", hold)
	}
	if hold := book.evaluate(gate, reset.Add(-time.Hour), known)["work"]; hold.held {
		t.Fatalf("1 h before reset at 99%%: %+v, want released (limit 100%%)", hold)
	}
}
