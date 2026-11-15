package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// quotaStateTestFile points the quota state file of this test at a new directory.
func quotaStateTestFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state", "quota-state.json")
	t.Setenv(quotaStateEnv, path)
	return path
}

// readQuotaStateFile reads the file as a reader of the contract does: every field of
// the schema, and none besides. It decodes with the standard library on purpose: the
// readers are not this proxy, and ojson has no decoder that rejects unknown fields.
func readQuotaStateFile(t *testing.T, path string) (quotaStateDoc, string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("quota state file: %v", err)
	}
	var doc quotaStateDoc
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if errDecode := decoder.Decode(&doc); errDecode != nil {
		t.Fatalf("quota state file does not parse: %v\n%s", errDecode, data)
	}
	return doc, string(data)
}

func jsonKeys(t *testing.T, value any) []string {
	t.Helper()
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%v is not a JSON object", value)
	}
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (r *reserveRig) index(id string) string {
	r.manager.mu.RLock()
	defer r.manager.mu.RUnlock()
	return quotaAuthIndex(r.manager.auths[id])
}

func loginOf(t *testing.T, doc quotaStateDoc, index string) quotaStateLogin {
	t.Helper()
	for _, login := range doc.Logins {
		if login.AuthIndex == index {
			return login
		}
	}
	t.Fatalf("no login %s in %+v", index, doc.Logins)
	return quotaStateLogin{}
}

// The file as the contract states it: path from the variable, mode 0600, written by
// rename, every field present, and nothing that identifies an account.
func TestQuotaStateFileContract(t *testing.T) {
	path := quotaStateTestFile(t)
	rig := newReserveRig(t, &RoundRobinSelector{}, true, reserveGateOn(nil))
	rig.setAccessTokens(t)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("state file before any response: %v", err)
	}
	reset := time.Now().Add(100 * time.Hour).Truncate(time.Second)
	// A response of a Max login is a change: the file is written with it.
	markClaudeResult(rig.manager, rig.a, unifiedHeaders(map[string]float64{"7d": 0.01}, reset))

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("mode = %o, want 600", mode)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 || entries[0].Name() != "quota-state.json" {
		t.Fatalf("directory holds %v, want the state file alone (no temp file left)", entries)
	}

	doc, raw := readQuotaStateFile(t, path)
	var generic map[string]any
	if errJSON := json.Unmarshal([]byte(raw), &generic); errJSON != nil {
		t.Fatal(errJSON)
	}
	routeKeys := []string{"limit", "message", "retry_at", "route", "since", "wind_down_until"}
	for what, got := range map[string][]string{
		"file":        jsonKeys(t, generic),
		"interactive": jsonKeys(t, generic["interactive"]),
		"automation":  jsonKeys(t, generic["automation"]),
		"reserve":     jsonKeys(t, generic["reserve"]),
		"login":       jsonKeys(t, generic["logins"].([]any)[1]),
	} {
		want := map[string][]string{
			"file":        {"automation", "interactive", "logins", "reserve", "schema", "written_at"},
			"interactive": routeKeys,
			"automation":  routeKeys,
			"reserve":     {"active", "on", "size"},
			"login":       {"auth_index", "limit", "read_at", "read_from", "resets_at", "state", "utilization"},
		}[what]
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s fields = %v, want %v", what, got, want)
		}
	}
	if !strings.Contains(raw, `"written_at": "`+doc.WrittenAt.UTC().Format("2006-01-02T15:04:05Z")+`"`) {
		t.Fatalf("written_at is not a UTC timestamp to the second:\n%s", raw)
	}
	if doc.Schema != "cliproxy.quota_state.v1" || time.Since(doc.WrittenAt) > time.Minute || time.Since(doc.WrittenAt) < -time.Minute {
		t.Fatalf("schema %q written_at %s", doc.Schema, doc.WrittenAt)
	}
	for name, route := range map[string]quotaStateRoute{"interactive": doc.Interactive, "automation": doc.Automation} {
		if route.Route != "max" || route.Limit != "" || route.RetryAt != nil || route.WindDownUntil != nil || route.Message != "" || route.Since.IsZero() {
			t.Fatalf("%s = %+v, want the max route with nothing else set", name, route)
		}
	}
	a, b := rig.index(rig.a), rig.index(rig.b)
	if a == "" || b == "" || a == b {
		t.Fatalf("auth indexes %q, %q", a, b)
	}
	if len(doc.Logins) != 2 || doc.Logins[0].AuthIndex >= doc.Logins[1].AuthIndex {
		t.Fatalf("logins = %+v, want the two Max logins by auth_index", doc.Logins)
	}
	read := loginOf(t, doc, a)
	if read.State != "ok" || read.Limit != "" || read.Utilization == nil || *read.Utilization != 0.01 ||
		read.ResetsAt == nil || !read.ResetsAt.Equal(reset) || read.ReadAt == nil || time.Since(*read.ReadAt) > time.Minute || read.ReadFrom != "headers" {
		t.Fatalf("login with a reading = %+v", read)
	}
	if unread := loginOf(t, doc, b); unread.State != "ok" || unread.Utilization != nil || unread.ResetsAt != nil || unread.ReadAt != nil || unread.ReadFrom != "" {
		t.Fatalf("login without a reading = %+v", unread)
	}
	if doc.Reserve.Active || doc.Reserve.Size != 0.05 || !reflect.DeepEqual(doc.Reserve.On, []string{a}) {
		t.Fatalf("reserve = %+v, want 0.05 on %s, not active", doc.Reserve, a)
	}
	for _, secret := range []string{rig.a, rig.b, rig.credit, rig.codex, "token-of-", "access_token", "owner@example.com", "api_key", reserveTestExemptKey, reserveTestUserKey} {
		if strings.Contains(raw, secret) {
			t.Fatalf("state file contains %q:\n%s", secret, raw)
		}
	}
}

// Without the variable a test binary writes no file, so a test run cannot replace the
// file of the proxy that runs on the same machine.
func TestQuotaStatePathInTestBinary(t *testing.T) {
	t.Setenv(quotaStateEnv, "")
	if path := quotaStatePath(); path != "" {
		t.Fatalf("path in a test binary = %q, want none", path)
	}
	t.Setenv(quotaStateEnv, " /somewhere/quota-state.json ")
	if path := quotaStatePath(); path != "/somewhere/quota-state.json" {
		t.Fatalf("path from the variable = %q", path)
	}
}

// What a new prompt gets, per kind of caller, in each state of the Max logins.
func TestQuotaStateRoutes(t *testing.T) {
	reset := time.Now().Add(48 * time.Hour).Truncate(time.Second)
	t.Run("credit key serves at a real limit", func(t *testing.T) {
		path := quotaStateTestFile(t)
		rig := newReserveRig(t, &RoundRobinSelector{}, true, reserveGateOn(nil))
		rig.limitAt(t, rig.a, reset)
		rig.limitAt(t, rig.b, reset)
		doc, _ := readQuotaStateFile(t, path)
		first := rig.auth(rig.a).Quota.NextRecoverAt
		sentence := quotaRefusalSentence(quotaLimitWeeklyAll, first, time.Local)
		for name, route := range map[string]quotaStateRoute{"interactive": doc.Interactive, "automation": doc.Automation} {
			if route.Route != "credit" || route.Limit != "weekly_all" || route.RetryAt == nil || !route.RetryAt.Equal(first.Truncate(time.Second)) ||
				route.Message != sentence || route.WindDownUntil != nil {
				t.Fatalf("%s = %+v, want credit until %s with %q", name, route, first, sentence)
			}
		}
		for _, login := range doc.Logins {
			if login.State != "cooled" || login.Limit != "weekly_all" || login.Utilization == nil || *login.Utilization != 1 || login.ResetsAt == nil || !login.ResetsAt.Equal(reset) {
				t.Fatalf("login = %+v, want cooled at the weekly limit", login)
			}
		}

		// Without the credit key the same state is a refusal, in the refusal's own words.
		rig.disable(t, rig.credit)
		for _, key := range []string{reserveTestUserKey, reserveTestExemptKey} {
			_, err := rig.pick(key, "", reserveTestClaudeModel)
			if err == nil || !strings.Contains(err.Error(), sentence+" (reset after ") {
				t.Fatalf("refusal for %q = %v, want the sentence %q and the countdown", key, err, sentence)
			}
		}
		doc, _ = readQuotaStateFile(t, path)
		if doc.Interactive.Route != "refused" || doc.Automation.Route != "refused" || doc.Interactive.Message != sentence || doc.Automation.Message != sentence ||
			doc.Interactive.Limit != "weekly_all" || doc.Automation.RetryAt == nil {
			t.Fatalf("routes without the credit key = %+v / %+v", doc.Interactive, doc.Automation)
		}
	})

	t.Run("reserve hold refuses the interactive caller only", func(t *testing.T) {
		path := quotaStateTestFile(t)
		rig := newReserveRig(t, &RoundRobinSelector{}, true, reserveGateOn(nil))
		rig.observe(t, rig.a, "7d", 0.98, reset)
		rig.observe(t, rig.b, "7d", 0.99, reset)
		_, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel)
		hold := requireReserveHold(t, err)
		doc, _ := readQuotaStateFile(t, path)
		sentence := quotaRefusalSentence(quotaLimitReserve, hold.resetAt, time.Local)
		got := doc.Interactive
		if got.Route != "refused" || got.Limit != "reserve" || got.RetryAt == nil || !got.RetryAt.Equal(hold.resetAt) || got.Message != sentence ||
			!strings.HasPrefix(hold.message(), got.Message+" (reset after ") {
			t.Fatalf("interactive = %+v, want the refusal %q", got, hold.message())
		}
		windDown := time.Duration(internalconfig.DefaultReserveGateWindDownMinutes) * time.Minute
		if got.WindDownUntil == nil || time.Until(*got.WindDownUntil) > windDown || time.Until(*got.WindDownUntil) < windDown-time.Minute {
			t.Fatalf("wind_down_until = %v, want %s after the reserve trigger turned on", got.WindDownUntil, windDown)
		}
		if doc.Automation.Route != "max" || doc.Automation.Message != "" || doc.Automation.RetryAt != nil {
			t.Fatalf("automation = %+v, want max", doc.Automation)
		}
		if !doc.Reserve.Active || doc.Reserve.Size != 0.05 || len(doc.Reserve.On) == 0 {
			t.Fatalf("reserve = %+v, want it active", doc.Reserve)
		}
		for _, login := range doc.Logins {
			if login.State != "held" || login.Limit != "reserve" {
				t.Fatalf("login = %+v, want held", login)
			}
		}

		// Past the wind-down the field is empty again.
		rig.manager.syncQuotaState(time.Now().Add(windDown + time.Minute))
		if doc, _ = readQuotaStateFile(t, path); doc.Interactive.Route != "refused" || doc.Interactive.WindDownUntil != nil {
			t.Fatalf("interactive after the wind-down = %+v", doc.Interactive)
		}
	})

	t.Run("manual hold", func(t *testing.T) {
		path := quotaStateTestFile(t)
		rig := newReserveRig(t, &RoundRobinSelector{}, true, reserveGateOn(func(gate *internalconfig.ReserveGateConfig) { gate.Hold = true }))
		rig.observe(t, rig.a, "7d", 0.10, reset)
		_, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel)
		rig.manager.syncQuotaState(time.Now())
		doc, _ := readQuotaStateFile(t, path)
		got := doc.Interactive
		if err == nil || got.Route != "refused" || got.Limit != "reserve" || got.RetryAt != nil || got.Message == "" || !strings.Contains(err.Error(), got.Message) {
			t.Fatalf("interactive = %+v, refusal %v", got, err)
		}
		if doc.Automation.Route != "max" {
			t.Fatalf("automation = %+v, want max", doc.Automation)
		}
	})

	t.Run("gate off", func(t *testing.T) {
		path := quotaStateTestFile(t)
		rig := newReserveRig(t, &RoundRobinSelector{}, true, internalconfig.ReserveGateConfig{})
		rig.observe(t, rig.a, "7d", 0.99, reset)
		rig.observe(t, rig.b, "7d", 0.99, reset)
		rig.manager.syncQuotaState(time.Now())
		doc, _ := readQuotaStateFile(t, path)
		if doc.Interactive.Route != "max" || doc.Automation.Route != "max" || doc.Reserve.Active || doc.Reserve.Size != 0 || len(doc.Reserve.On) != 0 {
			t.Fatalf("with the gate off: %+v / %+v / %+v", doc.Interactive, doc.Automation, doc.Reserve)
		}
		for _, login := range doc.Logins {
			if login.State != "ok" {
				t.Fatalf("login = %+v, want ok", login)
			}
		}
	})

	t.Run("one model limited on one login", func(t *testing.T) {
		path := quotaStateTestFile(t)
		rig := newReserveRig(t, &RoundRobinSelector{}, true, reserveGateOn(nil))
		headers := unifiedHeaders(map[string]float64{"7d": 0.30}, reset)
		headers.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		headers.Set("Anthropic-Ratelimit-Unified-7d_oi-Status", "rejected")
		rig.limitModel(t, rig.a, headers)
		doc, _ := readQuotaStateFile(t, path)
		if login := loginOf(t, doc, rig.index(rig.a)); login.State != "cooled" || login.Limit != "weekly_scoped" || login.Utilization == nil || *login.Utilization != 0.30 {
			t.Fatalf("login = %+v, want cooled by the model's weekly limit", login)
		}
		if doc.Interactive.Route != "max" || doc.Automation.Route != "max" {
			t.Fatalf("routes = %+v / %+v, want max: the other login serves", doc.Interactive, doc.Automation)
		}
	})
}

// A reading the usage read brought is marked so, and a reading of a window that has
// rolled over is shown as unknown.
func TestQuotaStateReadings(t *testing.T) {
	path := quotaStateTestFile(t)
	rig := newReserveRig(t, &RoundRobinSelector{}, true, reserveGateOn(nil))
	usage := installQuotaUsageFake(rig)
	rig.limitAt(t, rig.a, time.Now().Add(48*time.Hour))
	rig.observe(t, rig.b, "7d", 0.60, time.Now().Add(-time.Minute))
	rig.manager.syncQuotaState(time.Now())
	doc, _ := readQuotaStateFile(t, path)
	if login := loginOf(t, doc, rig.index(rig.b)); login.Utilization != nil || login.ResetsAt != nil || login.ReadFrom != "" {
		t.Fatalf("login whose window rolled over = %+v, want no reading", login)
	}

	rig.disable(t, rig.b)
	fresh := time.Now().Add(167 * time.Hour).Truncate(time.Second)
	usage.report(0.02, fresh, rig.a)
	auth, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel)
	if err != nil || auth.ID != rig.a {
		t.Fatalf("pick = %v, %v; want the login the upstream reports as reset", auth, err)
	}
	doc, _ = readQuotaStateFile(t, path)
	if len(doc.Logins) != 1 {
		t.Fatalf("logins = %+v, want the enabled Max login alone", doc.Logins)
	}
	login := doc.Logins[0]
	if login.State != "ok" || login.Utilization == nil || *login.Utilization != 0.02 || login.ResetsAt == nil || !login.ResetsAt.Equal(fresh) || login.ReadFrom != "usage" {
		t.Fatalf("login after the usage read = %+v", login)
	}
	if doc.Interactive.Route != "max" || doc.Automation.Route != "max" {
		t.Fatalf("routes after the usage read = %+v / %+v", doc.Interactive, doc.Automation)
	}
}

// The file is written when its content changes and, unchanged, once it is 4 minutes
// old; since keeps the time a route last changed.
func TestQuotaStateWriteSchedule(t *testing.T) {
	path := quotaStateTestFile(t)
	rig := newReserveRig(t, &RoundRobinSelector{}, true, reserveGateOn(nil))
	reset := time.Now().Add(100 * time.Hour).Truncate(time.Second)
	base := time.Now().Add(time.Hour).Truncate(time.Second)
	rig.observe(t, rig.a, "7d", 0.10, reset)
	rig.manager.syncQuotaState(base)
	first, _ := readQuotaStateFile(t, path)
	if !first.WrittenAt.Equal(base) {
		t.Fatalf("written_at = %s, want %s", first.WrittenAt, base)
	}

	// Unchanged content: no write before the file is due.
	rig.manager.syncQuotaState(base.Add(quotaStateRefresh - time.Second))
	if doc, _ := readQuotaStateFile(t, path); !doc.WrittenAt.Equal(base) {
		t.Fatalf("an unchanged file was written again at %s", doc.WrittenAt)
	}
	rig.manager.syncQuotaState(base.Add(quotaStateRefresh))
	due, _ := readQuotaStateFile(t, path)
	if !due.WrittenAt.Equal(base.Add(quotaStateRefresh)) {
		t.Fatalf("written_at = %s, want the file written again once it is %s old", due.WrittenAt, quotaStateRefresh)
	}
	if !due.Interactive.Since.Equal(first.Interactive.Since) || !due.Automation.Since.Equal(first.Automation.Since) {
		t.Fatalf("since moved without a route change: %s -> %s", first.Interactive.Since, due.Interactive.Since)
	}

	// A reading that differs is a change: the file is written at once.
	changed := base.Add(quotaStateRefresh + time.Second)
	rig.observeAt(t, rig.a, "", "7d", 0.11, reset, time.Now())
	rig.manager.syncQuotaState(changed)
	doc, _ := readQuotaStateFile(t, path)
	if login := loginOf(t, doc, rig.index(rig.a)); login.Utilization == nil || *login.Utilization != 0.11 {
		t.Fatalf("login = %+v, want the new reading", login)
	}
	if !doc.Interactive.Since.Equal(first.Interactive.Since) {
		t.Fatalf("since moved on a reading change")
	}

	// A route change moves since of that route only.
	rig.observeAt(t, rig.a, "", "7d", 0.99, reset, time.Now())
	rig.observeAt(t, rig.b, "", "7d", 0.99, reset, time.Now())
	later := changed.Add(time.Minute)
	rig.manager.syncQuotaState(later)
	doc, _ = readQuotaStateFile(t, path)
	if doc.Interactive.Route != "refused" || doc.Interactive.Since.Before(first.Interactive.Since.Add(time.Second)) {
		t.Fatalf("interactive = %+v, want refused with a new since (was %s)", doc.Interactive, first.Interactive.Since)
	}
	if doc.Automation.Route != "max" || !doc.Automation.Since.Equal(first.Automation.Since) {
		t.Fatalf("automation = %+v, want max since %s", doc.Automation, first.Automation.Since)
	}
}

// A path that cannot be written changes nothing else: requests are served as before.
func TestQuotaStateUnwritablePath(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(quotaStateEnv, filepath.Join(blocker, "quota-state.json"))
	rig := newReserveRig(t, &RoundRobinSelector{}, true, reserveGateOn(nil))
	rig.observe(t, rig.a, "7d", 0.10, time.Now().Add(100*time.Hour))
	auth, err := rig.pick(reserveTestUserKey, "", reserveTestClaudeModel)
	requireMaxLogin(t, rig, auth, err)
}

// With no Max login there is nothing to describe: no file.
func TestQuotaStateNoMaxLogin(t *testing.T) {
	path := quotaStateTestFile(t)
	rig := newReserveRig(t, &RoundRobinSelector{}, true, reserveGateOn(nil))
	rig.disable(t, rig.a)
	rig.disable(t, rig.b)
	rig.manager.syncQuotaState(time.Now())
	if _, ok := rig.manager.buildQuotaState(time.Now()); ok {
		t.Fatal("a state was built with no Max login")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("state file with no Max login: %v", err)
	}
}

// While the proxy runs the file is never 5 minutes old, with no traffic at all.
func TestQuotaStateTickerKeepsFileFresh(t *testing.T) {
	path := quotaStateTestFile(t)
	rig := newReserveRig(t, &RoundRobinSelector{}, true, reserveGateOn(nil))
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			rig.manager.runQuotaStateTicker(ctx)
			close(done)
		}()
		synctest.Wait()
		doc, _ := readQuotaStateFile(t, path)
		if !doc.WrittenAt.Equal(time.Now()) {
			t.Fatalf("written_at = %s, want the file written at start (%s)", doc.WrittenAt, time.Now())
		}
		writes := map[time.Time]bool{doc.WrittenAt: true}
		for elapsed := time.Duration(0); elapsed < 20*time.Minute; elapsed += quotaStateTick {
			time.Sleep(quotaStateTick) // the bubble's clock, not the wall clock
			synctest.Wait()
			doc, _ = readQuotaStateFile(t, path)
			if age := time.Since(doc.WrittenAt); age >= 5*time.Minute {
				t.Fatalf("after %s the file is %s old", elapsed+quotaStateTick, age)
			}
			writes[doc.WrittenAt] = true
		}
		if len(writes) != 6 {
			t.Fatalf("writes in 20 idle minutes = %d, want 6 (one at start, then one per %s)", len(writes), quotaStateRefresh)
		}
		cancel()
		<-done
	})
}
