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

func TestNextReserveHoldReleaseBeforeReset(t *testing.T) {
	now := time.Unix(1_900_000_000, 0)
	const threshold, resume = 0.95, 0.90
	for name, tt := range map[string]struct {
		utilization float64
		resetIn     time.Duration // zero: reading without a reset
		prevHeld    bool
		release     time.Duration
		wantHeld    bool
	}{
		"above threshold, reset within release window: released": {utilization: 0.97, resetIn: 30 * time.Minute, release: time.Hour},
		"above threshold, reset exactly at the window: released": {utilization: 0.97, resetIn: time.Hour, release: time.Hour},
		"above threshold, reset outside release window: held":    {utilization: 0.97, resetIn: 2 * time.Hour, release: time.Hour, wantHeld: true},
		"hysteresis hold, reset within release window: released": {utilization: 0.92, resetIn: 30 * time.Minute, prevHeld: true, release: time.Hour},
		"hysteresis hold, reset outside release window: held":    {utilization: 0.92, resetIn: 3 * time.Hour, prevHeld: true, release: time.Hour, wantHeld: true},
		"release 0 keeps the old behavior":                       {utilization: 0.97, resetIn: time.Minute, wantHeld: true},
		"release 0 keeps the old hysteresis":                     {utilization: 0.92, resetIn: time.Minute, prevHeld: true, wantHeld: true},
		"unknown reset is never released":                        {utilization: 0.97, release: time.Hour, wantHeld: true},
		"below threshold stays unheld":                           {utilization: 0.50, resetIn: 30 * time.Minute, release: time.Hour},
	} {
		t.Run(name, func(t *testing.T) {
			reading := reserveReading{utilization: tt.utilization, observedAt: now}
			if tt.resetIn != 0 {
				reading.resetAt = now.Add(tt.resetIn)
			}
			prev := reserveHold{held: tt.prevHeld, until: reading.resetAt}
			got := nextReserveHold(prev, reading, true, threshold, resume, tt.release, now)
			if got.held != tt.wantHeld {
				t.Fatalf("held = %v, want %v", got.held, tt.wantHeld)
			}
		})
	}
}

func releaseGate(window string) internalconfig.ReserveGateConfig {
	return reserveGateOn(func(g *internalconfig.ReserveGateConfig) { g.ReleaseBeforeReset = window })
}

func TestReserveGateReleaseBeforeResetFreesHeldWindow(t *testing.T) {
	forEachReserveRig(t, releaseGate("1h"), func(t *testing.T, rig *reserveRig) {
		// Outside the release window the 5h hold applies as before.
		rig.observe(t, rig.a, "5h", 0.97, time.Now().Add(3*time.Hour))
		if seen := rig.picked(t, reserveTestUserKey, 4); seen[rig.a] != 0 {
			t.Fatalf("picks = %v, want %s held (reset in 3h, release-before-reset 1h)", seen, rig.a)
		}
		// The same window, now 30 minutes from its reset: released for everyone.
		rig.observe(t, rig.a, "5h", 0.97, time.Now().Add(30*time.Minute))
		rig.observe(t, rig.b, "5h", 0.97, time.Now().Add(3*time.Hour))
		if seen := rig.picked(t, reserveTestUserKey, 4); seen[rig.a] != 4 {
			t.Fatalf("picks = %v, want only %s (released 30m before reset; %s held)", seen, rig.a, rig.b)
		}
		// 7d windows release the same way.
		rig.observe(t, rig.a, "7d", 0.99, time.Now().Add(20*time.Minute))
		if seen := rig.picked(t, reserveTestUserKey, 4); seen[rig.a] != 4 {
			t.Fatalf("picks = %v, want %s (7d window released before reset)", seen, rig.a)
		}
	})
}

func TestReserveGateReleaseBeforeResetDefaultOff(t *testing.T) {
	forEachReserveRig(t, reserveGateOn(nil), func(t *testing.T, rig *reserveRig) {
		rig.observe(t, rig.a, "5h", 0.97, time.Now().Add(5*time.Minute))
		if seen := rig.picked(t, reserveTestUserKey, 4); seen[rig.a] != 0 {
			t.Fatalf("picks = %v, want %s held up to its reset without release-before-reset", seen, rig.a)
		}
	})
}

func TestReserveGateReleaseBeforeResetKeepsManualHold(t *testing.T) {
	gate := releaseGate("1h")
	gate.Hold = true
	forEachReserveRig(t, gate, func(t *testing.T, rig *reserveRig) {
		rig.observe(t, rig.a, "5h", 0.97, time.Now().Add(10*time.Minute))
		rig.observe(t, rig.b, "5h", 0.10, time.Now().Add(10*time.Minute))
		_, err := rig.pick(reserveTestUserKey, "s1", reserveTestClaudeModel)
		if hold := requireReserveHold(t, err); !hold.manual {
			t.Fatal("manual hold error not marked manual")
		}
	})
}

// A hold persisted by an older build (same state file format) is released after a
// restart once its window is within release-before-reset, and the release is logged.
func TestReserveGateReleaseBeforeResetPersistedHold(t *testing.T) {
	authDir := t.TempDir()
	prefix := reserveStatePrefix()
	reset := time.Now().Add(30 * time.Minute).Unix()
	state := fmt.Sprintf(`{"version":1,"accounts":{%q:{"5h":{"utilization":0.97,"reset_at":%d,"observed_at":%q,"held":true,"held_until":%d}}}}`,
		prefix+"a", reset, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), reset)
	path := filepath.Join(authDir, internalconfig.DefaultReserveGateStateFileName)
	if errWrite := os.WriteFile(path, []byte(state), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	hook := logtest.NewGlobal()
	defer hook.Reset()
	cfg := reserveStateConfig(authDir)
	cfg.Routing.ReserveGate.ReleaseBeforeReset = "1h"
	rig := newReserveRigWithConfig(t, &EarliestResetSelector{}, false, cfg, prefix)
	rig.manager.Remove(WithSkipPersist(context.Background()), rig.b)
	auth, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel)
	if err != nil || auth.ID != rig.a {
		t.Fatalf("pick = %v, %v; want %s released 30m before its reset", auth, err, rig.a)
	}
	logged := false
	for _, entry := range hook.AllEntries() {
		if strings.Contains(entry.Message, rig.a+" 5h window released before reset") {
			logged = true
		}
	}
	if !logged {
		t.Fatal("no log line for the release before reset")
	}
	file, errRead := readReserveStateFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if entry := file.Accounts[rig.a]["5h"]; entry.Held || entry.Utilization != 0.97 {
		t.Fatalf("persisted 5h entry = %+v, want the reading kept and the hold cleared", entry)
	}
}
