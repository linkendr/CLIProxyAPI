package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

// reserveStateConfig is a gate whose state file sits in auth-dir, the default place.
func reserveStateConfig(authDir string) *internalconfig.Config {
	return &internalconfig.Config{AuthDir: authDir, Routing: internalconfig.RoutingConfig{ReserveGate: reserveGateOn(nil)}}
}

func reserveStatePrefix() string {
	return fmt.Sprintf("reserve-state-%d-", reserveRigSeq.Add(1))
}

// markClaudeResult records one upstream Claude response carrying headers, the way the
// executor result path does.
func markClaudeResult(manager *Manager, authID string, headers http.Header) {
	ctx := internallogging.WithResponseHeadersHolder(context.Background())
	internallogging.SetResponseHeaders(ctx, headers)
	manager.MarkResult(ctx, Result{AuthID: authID, Provider: "claude", Model: reserveTestClaudeModel, Success: true})
}

func unifiedHeaders(windows map[string]float64, reset time.Time) http.Header {
	headers := http.Header{}
	headers.Set("Anthropic-Ratelimit-Unified-Status", "allowed")
	for window, utilization := range windows {
		headers.Set("Anthropic-Ratelimit-Unified-"+window+"-Utilization", strconv.FormatFloat(utilization, 'f', 2, 64))
		headers.Set("Anthropic-Ratelimit-Unified-"+window+"-Reset", strconv.FormatInt(reset.Unix(), 10))
	}
	return headers
}

// A reading the proxy saw before a restart still holds the account after it: the
// non-exempt caller is refused and the exempt caller gets through. The reading was
// only ever seen on exempt traffic, so no non-exempt selection ran before the restart.
func TestReserveGateRestartKeepsKnownHold(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprintf("mixed=%v", mixed), func(t *testing.T) {
			authDir := t.TempDir()
			prefix := reserveStatePrefix()
			reset := time.Now().Add(2 * time.Hour)
			before := newReserveRigWithConfig(t, &RoundRobinSelector{}, mixed, reserveStateConfig(authDir), prefix)
			before.manager.Remove(WithSkipPersist(context.Background()), before.credit)
			markClaudeResult(before.manager, before.a, unifiedHeaders(map[string]float64{"7d": 0.97}, reset))
			markClaudeResult(before.manager, before.b, unifiedHeaders(map[string]float64{"7d": 0.96}, reset))

			path := filepath.Join(authDir, internalconfig.DefaultReserveGateStateFileName)
			info, errStat := os.Stat(path)
			if errStat != nil {
				t.Fatalf("state file: %v", errStat)
			}
			if mode := info.Mode().Perm(); mode != 0o600 {
				t.Fatalf("state file mode = %o, want 600", mode)
			}

			after := newReserveRigWithConfig(t, &RoundRobinSelector{}, mixed, reserveStateConfig(authDir), prefix)
			after.manager.Remove(WithSkipPersist(context.Background()), after.credit)
			_, err := after.pick(reserveTestUserKey, "", reserveTestClaudeModel)
			hold := requireReserveHold(t, err)
			if !hold.resetAt.Equal(reset.Truncate(time.Second)) {
				t.Fatalf("hold reset = %v, want %v", hold.resetAt, reset.Truncate(time.Second))
			}
			if seen := after.picked(t, reserveTestExemptKey, 4); seen[after.a]+seen[after.b] != 4 {
				t.Fatalf("exempt picks after restart = %v, want the held accounts", seen)
			}
		})
	}
}

// Case 8: a state file written by an older build, with 5h entries and fields this build
// does not know, loads: the 5h entries are ignored (they never hold), the 7d entries
// keep their holds, and the next write leaves the 5h entries out.
func TestReserveGateStateFileWithFiveHourEntriesLoads(t *testing.T) {
	authDir := t.TempDir()
	prefix := reserveStatePrefix()
	reset := time.Now().Add(20 * time.Hour).Unix()
	observed := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	write := func(bWindows string) string {
		state := fmt.Sprintf(`{"version":1,"updated_at":%q,"accounts":{
  %q:{"5h":{"utilization":0.10,"reset_at":%d,"observed_at":%q},
      "7d":{"utilization":0.97,"reset_at":%d,"observed_at":%q,"held":true,"held_until":%d,"retry_at":%d}},
  %q:{%s}}}`, observed, prefix+"a", reset, observed, reset, observed, reset, reset, prefix+"b", bWindows)
		path := filepath.Join(authDir, internalconfig.DefaultReserveGateStateFileName)
		if errWrite := os.WriteFile(path, []byte(state), 0o600); errWrite != nil {
			t.Fatal(errWrite)
		}
		return path
	}
	// b held only on 5h: ignored, so b has room and a (held on 7d) is not blocked.
	write(fmt.Sprintf(`"5h":{"utilization":0.99,"reset_at":%d,"observed_at":%q,"held":true,"held_until":%d}`, reset, observed, reset))
	rig := newReserveRigWithConfig(t, &EarliestResetSelector{}, false, reserveStateConfig(authDir), prefix)
	rig.manager.reserveHolds.mu.Lock()
	_, fiveHour := rig.manager.reserveHolds.readings[reserveKey{authID: rig.b, window: "5h"}]
	hold := rig.manager.reserveHolds.holds[reserveKey{authID: rig.a, window: "7d"}]
	rig.manager.reserveHolds.mu.Unlock()
	if fiveHour || !hold.held || hold.until.Unix() != reset {
		t.Fatalf("loaded book: b 5h kept = %v, a 7d hold = %+v; want 5h ignored and a held until %d", fiveHour, hold, reset)
	}
	if seen := rig.picked(t, reserveTestUserKey, 4); seen[rig.a]+seen[rig.b] != 4 {
		t.Fatalf("non-exempt picks = %v, want Max logins served while b has room", seen)
	}

	// b held on 7d too: after the restart both are held from the file alone.
	path := write(fmt.Sprintf(`"5h":{"utilization":0.10,"reset_at":%d,"observed_at":%q},"7d":{"utilization":0.96,"reset_at":%d,"observed_at":%q,"held":true,"held_until":%d}`, reset, observed, reset, observed, reset))
	after := newReserveRigWithConfig(t, &EarliestResetSelector{}, false, reserveStateConfig(authDir), prefix)
	_, err := after.pick(reserveTestUserKey, "", reserveTestClaudeModel)
	if hold := requireReserveHold(t, err); hold.resetAt.Unix() != reset {
		t.Fatalf("hold reset = %v, want %d", hold.resetAt, reset)
	}
	markClaudeResult(after.manager, after.a, unifiedHeaders(map[string]float64{"7d": 0.98}, time.Unix(reset, 0)))
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if strings.Contains(string(data), `"5h"`) || strings.Contains(string(data), "retry_at") {
		t.Fatalf("rewritten state file still carries old entries: %s", data)
	}
}

// A response whose headers lack the 7d utilization must not erase the known 7d reading.
func TestReserveGatePartialSnapshotKeepsKnownWindow(t *testing.T) {
	forEachReserveRig(t, reserveGateOn(nil), func(t *testing.T, rig *reserveRig) {
		reset := time.Now().Add(48 * time.Hour)
		markClaudeResult(rig.manager, rig.b, unifiedHeaders(map[string]float64{"7d": 0.97}, reset))
		markClaudeResult(rig.manager, rig.a, unifiedHeaders(map[string]float64{"7d": 0.97}, reset))
		if _, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel); err == nil {
			t.Fatal("both held on 7d must refuse the non-exempt caller")
		}
		// A response with only the model-scoped window does not erase a's 7d reading.
		markClaudeResult(rig.manager, rig.a, unifiedHeaders(map[string]float64{"7d_oi": 0.31}, reset))
		if _, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel); err == nil {
			t.Fatal("a response without the 7d reading released the hold")
		}
		// A newer 7d reading below threshold releases it.
		markClaudeResult(rig.manager, rig.a, unifiedHeaders(map[string]float64{"7d": 0.50}, reset))
		if seen := rig.picked(t, reserveTestUserKey, 4); seen[rig.a] == 0 {
			t.Fatalf("non-exempt picks after 7d 0.50 = %v, want %s released", seen, rig.a)
		}
	})
}

// A persisted reading whose window has reset counts as reset, held or not.
func TestReserveGatePersistedReadingPastResetIsEligible(t *testing.T) {
	authDir := t.TempDir()
	prefix := reserveStatePrefix()
	past := time.Now().Add(-time.Minute).Unix()
	state := fmt.Sprintf(`{"version":1,"accounts":{%q:{"7d":{"utilization":0.99,"reset_at":%d,"observed_at":%q,"held":true,"held_until":%d}}}}`,
		prefix+"a", past, time.Now().Add(-3*time.Hour).UTC().Format(time.RFC3339), past)
	path := filepath.Join(authDir, internalconfig.DefaultReserveGateStateFileName)
	if errWrite := os.WriteFile(path, []byte(state), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	rig := newReserveRigWithConfig(t, &EarliestResetSelector{}, false, reserveStateConfig(authDir), prefix)
	rig.manager.Remove(WithSkipPersist(context.Background()), rig.b)
	auth, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel)
	if err != nil || auth.ID != rig.a {
		t.Fatalf("pick = %v, %v; want %s, whose persisted window has reset", auth, err, rig.a)
	}
}

// An unreadable or missing state file starts the gate empty with a log line.
func TestReserveGateCorruptOrMissingStateFileStartsEmpty(t *testing.T) {
	for name, content := range map[string]*string{"missing": nil, "corrupt": ptrString("{not json"), "wrong-version": ptrString(`{"version":99}`)} {
		t.Run(name, func(t *testing.T) {
			authDir := t.TempDir()
			path := filepath.Join(authDir, internalconfig.DefaultReserveGateStateFileName)
			if content != nil {
				if errWrite := os.WriteFile(path, []byte(*content), 0o600); errWrite != nil {
					t.Fatal(errWrite)
				}
			}
			hook := logtest.NewGlobal()
			defer hook.Reset()
			rig := newReserveRigWithConfig(t, &EarliestResetSelector{}, false, reserveStateConfig(authDir), reserveStatePrefix())
			if seen := rig.picked(t, reserveTestUserKey, 4); seen[rig.a]+seen[rig.b] != 4 {
				t.Fatalf("non-exempt picks = %v, want both accounts", seen)
			}
			logged := false
			for _, entry := range hook.AllEntries() {
				if strings.Contains(entry.Message, "reserve gate: ") && strings.Contains(entry.Message, path) && strings.Contains(entry.Message, "starting empty") {
					logged = true
					if name != "missing" && entry.Level > log.WarnLevel {
						t.Fatalf("unreadable state logged at %s, want warning", entry.Level)
					}
				}
			}
			if !logged {
				t.Fatalf("no log line names %s and says it starts empty", path)
			}
		})
	}
}

// The state file holds readings and holds only: account IDs, numbers and times.
func TestReserveGateStateFileShape(t *testing.T) {
	authDir := t.TempDir()
	rig := newReserveRigWithConfig(t, &EarliestResetSelector{}, false, reserveStateConfig(authDir), reserveStatePrefix())
	reset := time.Now().Add(time.Hour).Truncate(time.Second)
	markClaudeResult(rig.manager, rig.a, unifiedHeaders(map[string]float64{"5h": 0.40, "7d": 0.97}, reset))
	data, errRead := os.ReadFile(filepath.Join(authDir, internalconfig.DefaultReserveGateStateFileName))
	if errRead != nil {
		t.Fatal(errRead)
	}
	var file struct {
		Version  int `json:"version"`
		Accounts map[string]map[string]struct {
			Utilization float64   `json:"utilization"`
			ResetAt     int64     `json:"reset_at"`
			ObservedAt  time.Time `json:"observed_at"`
			Held        bool      `json:"held"`
			HeldUntil   int64     `json:"held_until"`
		} `json:"accounts"`
	}
	if errJSON := json.Unmarshal(data, &file); errJSON != nil {
		t.Fatalf("state file %s: %v", data, errJSON)
	}
	windows := file.Accounts[rig.a]
	if file.Version != 1 || len(file.Accounts) != 1 || len(windows) != 1 {
		t.Fatalf("state file = %s, want version 1 with only the 7d window of %s", data, rig.a)
	}
	if w := windows["7d"]; w.Utilization != 0.97 || w.ResetAt != reset.Unix() || w.ObservedAt.IsZero() || !w.Held || w.HeldUntil != reset.Unix() {
		t.Fatalf("7d entry = %+v", w)
	}
}

// With the gate disabled nothing is read or written.
func TestReserveGateDisabledWritesNoStateFile(t *testing.T) {
	authDir := t.TempDir()
	cfg := &internalconfig.Config{AuthDir: authDir}
	rig := newReserveRigWithConfig(t, &EarliestResetSelector{}, false, cfg, reserveStatePrefix())
	markClaudeResult(rig.manager, rig.a, unifiedHeaders(map[string]float64{"7d": 0.99}, time.Now().Add(time.Hour)))
	rig.picked(t, reserveTestUserKey, 2)
	entries, errRead := os.ReadDir(authDir)
	if errRead != nil || len(entries) != 0 {
		t.Fatalf("auth dir entries = %v, %v; want none with the gate disabled", entries, errRead)
	}
}

// state-file overrides the default place.
func TestReserveGateStateFileOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "gate.state")
	cfg := reserveStateConfig(filepath.Join(dir, "auth"))
	cfg.Routing.ReserveGate.StateFile = path
	rig := newReserveRigWithConfig(t, &EarliestResetSelector{}, false, cfg, reserveStatePrefix())
	markClaudeResult(rig.manager, rig.a, unifiedHeaders(map[string]float64{"7d": 0.97}, time.Now().Add(time.Hour)))
	if _, errStat := os.Stat(path); errStat != nil {
		t.Fatalf("state-file %s: %v", path, errStat)
	}
	if _, errStat := os.Stat(filepath.Join(dir, "auth", internalconfig.DefaultReserveGateStateFileName)); !os.IsNotExist(errStat) {
		t.Fatalf("default state file written despite state-file: %v", errStat)
	}
}

func ptrString(s string) *string { return &s }
