package auth

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

// The release before a reset is final-release-hours now (it replaced release-before-reset):
// in the final hour before the next weekly reset of any Max login no reserve is kept.

// finalReleaseGate is the gate with final-release-hours set to hours.
func finalReleaseGate(hours int) internalconfig.ReserveGateConfig {
	return reserveGateOn(func(g *internalconfig.ReserveGateConfig) { g.FinalReleaseHours = &hours })
}

// Outside the final hour the last 1% is kept, so the ordinary caller is refused. The same
// readings 30 minutes before the reset serve: nothing is kept any more.
func TestReserveGateFinalReleaseFreesHeldLogin(t *testing.T) {
	forEachReserveRig(t, reserveGateOn(nil), func(t *testing.T, rig *reserveRig) {
		rig.observe(t, rig.a, "7d", 0.99, time.Now().Add(72*time.Hour))
		rig.observe(t, rig.b, "7d", 1.00, time.Now().Add(90*time.Minute))
		_, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel)
		requireReserveHold(t, err)
		rig.observe(t, rig.b, "7d", 1.00, time.Now().Add(30*time.Minute))
		if seen := rig.picked(t, reserveTestUserKey, 4); seen[rig.a]+seen[rig.b] != 4 {
			t.Fatalf("picks = %v, want Max logins served 30m before the reset (final release 1h)", seen)
		}
		check := rig.manager.reserveCheckFor(reserveOpts(reserveTestUserKey, ""))
		if check.accountHold(&Auth{ID: rig.a}).held {
			t.Fatalf("%s at 0.99 is still held 30m before the reset", rig.a)
		}
	})
}

// A longer final release frees the held login earlier.
func TestReserveGateFinalReleaseCustomWindow(t *testing.T) {
	forEachReserveRig(t, finalReleaseGate(2), func(t *testing.T, rig *reserveRig) {
		rig.observe(t, rig.a, "7d", 0.99, time.Now().Add(72*time.Hour))
		rig.observe(t, rig.b, "7d", 1.00, time.Now().Add(90*time.Minute))
		if seen := rig.picked(t, reserveTestUserKey, 4); seen[rig.a]+seen[rig.b] != 4 {
			t.Fatalf("picks = %v, want Max logins served 90m before the reset (final release 2h)", seen)
		}
	})
}

// With final-release-hours 0 the last 1% is kept up to the reset itself.
func TestReserveGateFinalReleaseOffHoldsUpToReset(t *testing.T) {
	forEachReserveRig(t, finalReleaseGate(0), func(t *testing.T, rig *reserveRig) {
		soon := time.Now().Add(5 * time.Minute).Truncate(time.Second)
		rig.observe(t, rig.a, "7d", 0.99, time.Now().Add(72*time.Hour))
		rig.observe(t, rig.b, "7d", 1.00, soon)
		_, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel)
		requireReserveHold(t, err)
		requireRetryAfterNear(t, err, soon)
	})
}

// The final release lifts the reserve, not the manual hold.
func TestReserveGateFinalReleaseKeepsManualHold(t *testing.T) {
	gate := reserveGateOn(func(g *internalconfig.ReserveGateConfig) { g.Hold = true })
	forEachReserveRig(t, gate, func(t *testing.T, rig *reserveRig) {
		rig.observe(t, rig.a, "7d", 0.99, time.Now().Add(72*time.Hour))
		rig.observe(t, rig.b, "7d", 0.10, time.Now().Add(10*time.Minute))
		_, err := rig.pick(reserveTestUserKey, "s1", reserveTestClaudeModel)
		if hold := requireReserveHold(t, err); !hold.manual {
			t.Fatal("manual hold error not marked manual")
		}
	})
}

// A hold the proxy saved before a restart follows the clock after it: still held three
// hours before its reset, released inside the final hour without a new reading.
func TestReserveGateFinalReleasePersistedHold(t *testing.T) {
	for name, tt := range map[string]struct {
		resetIn  time.Duration
		wantHeld bool
	}{
		"three hours before the reset: held":        {3 * time.Hour, true},
		"thirty minutes before the reset: released": {30 * time.Minute, false},
	} {
		t.Run(name, func(t *testing.T) {
			authDir := t.TempDir()
			prefix := reserveStatePrefix()
			reset := time.Now().Add(tt.resetIn).Unix()
			state := fmt.Sprintf(`{"version":1,"accounts":{%q:{"7d":{"utilization":0.99,"reset_at":%d,"observed_at":%q,"held":true,"held_until":%d}}}}`,
				prefix+"a", reset, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), reset)
			path := filepath.Join(authDir, internalconfig.DefaultReserveGateStateFileName)
			if errWrite := os.WriteFile(path, []byte(state), 0o600); errWrite != nil {
				t.Fatal(errWrite)
			}
			hook := logtest.NewGlobal()
			defer hook.Reset()
			rig := newReserveRigWithConfig(t, &EarliestResetSelector{}, false, reserveStateConfig(authDir), prefix)
			rig.manager.Remove(WithSkipPersist(context.Background()), rig.b)
			auth, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel)
			released := false
			for _, entry := range hook.AllEntries() {
				if strings.Contains(entry.Message, "reserve gate: "+rig.a+" 7d window released") {
					released = true
				}
			}
			saved, errRead := readReserveStateFile(path)
			if errRead != nil {
				t.Fatalf("state file after the pick: %v", errRead)
			}
			window, kept := saved.Accounts[rig.a]["7d"]
			if !kept || window.Utilization != 0.99 || window.ResetAt != reset {
				t.Fatalf("saved reading = %+v (kept %v), want utilization 0.99 and reset %d kept", window, kept, reset)
			}
			if tt.wantHeld {
				requireReserveHold(t, err)
				if released || !window.Held || window.HeldUntil == 0 {
					t.Fatalf("release logged = %v, saved hold = %+v; want no release and the hold kept", released, window)
				}
				return
			}
			if err != nil || auth.ID != rig.a {
				t.Fatalf("pick = %v, %v; want %s released inside the final hour", auth, err, rig.a)
			}
			if !released {
				t.Fatalf("no release line logged for %s", rig.a)
			}
			if window.Held || window.HeldUntil != 0 {
				t.Fatalf("saved hold = %+v, want it cleared after the release", window)
			}
		})
	}
}
