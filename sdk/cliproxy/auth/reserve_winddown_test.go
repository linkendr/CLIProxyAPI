package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const (
	reserveTestNewPrompt   = `{"model":"m","messages":[{"role":"user","content":"start a task"}]}`
	reserveTestRunningTask = `{"model":"m","messages":[{"role":"user","content":"start a task"},{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}]}`
)

// reserveRequestOpts is reserveOpts for a request with a Claude Messages body.
func reserveRequestOpts(clientKey, body string) cliproxyexecutor.Options {
	opts := reserveOpts(clientKey, "")
	opts.SourceFormat = sdktranslator.FormatClaude
	opts.OriginalRequest = []byte(body)
	return opts
}

// pickWith picks on the rig's pick path with the given options and returns the options'
// metadata as the executor would see it.
func (r *reserveRig) pickWith(opts cliproxyexecutor.Options) (*Auth, map[string]any, error) {
	ctx := context.Background()
	if r.mixed {
		auth, _, _, err := r.manager.pickNextMixed(ctx, []string{"claude"}, reserveTestClaudeModel, opts, nil)
		return auth, opts.Metadata, err
	}
	auth, _, err := r.manager.pickNext(ctx, "claude", reserveTestClaudeModel, opts, nil)
	return auth, opts.Metadata, err
}

func windDownFlag(meta map[string]any) bool {
	flag, _ := meta[ReserveWindDownMetadataKey].(bool)
	return flag
}

func (r *reserveRig) bothHeld(t *testing.T) {
	t.Helper()
	r.observe(t, r.a, "7d", 0.98, time.Now().Add(48*time.Hour))
	r.observe(t, r.b, "7d", 0.99, time.Now().Add(72*time.Hour))
}

// addDrainedKey registers a Claude API key the way the credit expiry drain leaves one:
// priority 10 and no fallback-when.
func (r *reserveRig) addDrainedKey(t *testing.T) string {
	t.Helper()
	id := r.a + "-drained"
	auth := &Auth{ID: id, Provider: "claude", Status: StatusActive, Attributes: map[string]string{"api_key": "drained", "priority": "10"}}
	if _, err := r.manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register(%s): %v", id, err)
	}
	registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: reserveTestClaudeModel}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	return id
}

func TestReserveRequestIsRunningTask(t *testing.T) {
	cases := []struct {
		name    string
		format  sdktranslator.Format
		payload string
		want    bool
	}{
		{"claude text string", sdktranslator.FormatClaude, reserveTestNewPrompt, false},
		{"claude text blocks", sdktranslator.FormatClaude, `{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`, false},
		{"claude tool_result last", sdktranslator.FormatClaude, reserveTestRunningTask, true},
		{"claude tool_result then reminder text", sdktranslator.FormatClaude, `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"},{"type":"text","text":"<system-reminder>x</system-reminder>"}]}]}`, true},
		{"claude tool_result earlier, typed text last", sdktranslator.FormatClaude, `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]},{"role":"assistant","content":"done"},{"role":"user","content":"next task"}]}`, false},
		{"claude assistant prefill last", sdktranslator.FormatClaude, `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"{"}]}`, false},
		{"claude no messages", sdktranslator.FormatClaude, `{"messages":[]}`, false},
		{"openai tool last", sdktranslator.FormatOpenAI, `{"messages":[{"role":"user","content":"go"},{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c1","content":"ok"}]}`, true},
		{"openai user last", sdktranslator.FormatOpenAI, `{"messages":[{"role":"system","content":"s"},{"role":"user","content":"go"}]}`, false},
		{"responses function_call_output last", sdktranslator.FormatOpenAIResponse, `{"input":[{"role":"user","content":"go"},{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":"ok"}]}`, true},
		{"responses string input", sdktranslator.FormatOpenAIResponse, `{"input":"go"}`, false},
		{"responses message last", sdktranslator.FormatOpenAIResponse, `{"input":[{"type":"message","role":"user","content":"go"}]}`, false},
		{"gemini functionResponse last", sdktranslator.FormatGemini, `{"contents":[{"role":"user","parts":[{"text":"go"}]},{"role":"model","parts":[{"functionCall":{"name":"f"}}]},{"role":"user","parts":[{"functionResponse":{"name":"f","response":{}}}]}]}`, true},
		{"gemini text last", sdktranslator.FormatGemini, `{"contents":[{"role":"user","parts":[{"text":"go"}]}]}`, false},
		{"unknown format", sdktranslator.Format("interactions"), reserveTestRunningTask, false},
		{"invalid json", sdktranslator.FormatClaude, `{"messages":[`, false},
		{"empty", sdktranslator.FormatClaude, ``, false},
	}
	for _, tc := range cases {
		if got := reserveRequestIsRunningTask(tc.format, []byte(tc.payload)); got != tc.want {
			t.Errorf("%s: running task = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestReserveTriggerClockStartsAndClears(t *testing.T) {
	var clock reserveTriggerClock
	t0 := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	if since := clock.observe(false, t0, time.Minute); !since.IsZero() {
		t.Fatalf("off: since = %v, want zero", since)
	}
	if since := clock.observe(true, t0, time.Minute); !since.Equal(t0) {
		t.Fatalf("first on: since = %v, want %v", since, t0)
	}
	if since := clock.observe(true, t0.Add(10*time.Minute), time.Minute); !since.Equal(t0) {
		t.Fatalf("still on: since = %v, want the first start %v", since, t0)
	}
	if since := clock.observe(false, t0.Add(11*time.Minute), time.Minute); !since.IsZero() {
		t.Fatalf("off again: since = %v, want zero", since)
	}
	t1 := t0.Add(20 * time.Minute)
	if since := clock.observe(true, t1, time.Minute); !since.Equal(t1) {
		t.Fatalf("on again: since = %v, want a new start %v", since, t1)
	}
	clock.clear()
	if !clock.since.IsZero() {
		t.Fatal("clear left a start time")
	}
}

// Trigger on: a new prompt is refused, a running task is served by a Max login (the last
// share included) and marked for the wrap-up note, the exempt caller is served unmarked.
func TestReserveWindDownServesRunningTaskWithNote(t *testing.T) {
	forEachReserveRig(t, reserveGateOn(nil), func(t *testing.T, rig *reserveRig) {
		rig.bothHeld(t)
		_, meta, err := rig.pickWith(reserveRequestOpts(reserveTestUserKey, reserveTestNewPrompt))
		requireReserveHold(t, err)
		if windDownFlag(meta) {
			t.Fatal("refused new prompt is marked for the note")
		}
		auth, meta, err := rig.pickWith(reserveRequestOpts(reserveTestUserKey, reserveTestRunningTask))
		if err != nil || (auth.ID != rig.a && auth.ID != rig.b) {
			t.Fatalf("running task pick = %v, %v; want a held Max login", auth, err)
		}
		if !windDownFlag(meta) || !ReserveWindDownNoteApplies(auth, meta) {
			t.Fatalf("running task metadata %v not marked for the note on %s", meta, auth.ID)
		}
		auth, meta, err = rig.pickWith(reserveRequestOpts(reserveTestExemptKey, reserveTestNewPrompt))
		if err != nil || (auth.ID != rig.a && auth.ID != rig.b) || windDownFlag(meta) {
			t.Fatalf("exempt pick = %v, %v, marked %v; want a Max login, unmarked", auth, err, windDownFlag(meta))
		}
		if rig.manager.reserveTrigger.since.IsZero() {
			t.Fatal("trigger clock did not start")
		}
	})
}

// After the wind-down, running tasks of ordinary callers are refused too; the exempt
// caller is still served.
func TestReserveWindDownExpires(t *testing.T) {
	forEachReserveRig(t, reserveGateOn(nil), func(t *testing.T, rig *reserveRig) {
		rig.bothHeld(t)
		if _, _, err := rig.pickWith(reserveRequestOpts(reserveTestUserKey, reserveTestRunningTask)); err != nil {
			t.Fatalf("running task inside the wind-down: %v", err)
		}
		rig.manager.reserveTrigger.mu.Lock()
		rig.manager.reserveTrigger.since = time.Now().Add(-internalconfig.DefaultReserveGateWindDownMinutes*time.Minute - time.Second)
		rig.manager.reserveTrigger.mu.Unlock()
		_, meta, err := rig.pickWith(reserveRequestOpts(reserveTestUserKey, reserveTestRunningTask))
		requireReserveHold(t, err)
		if windDownFlag(meta) {
			t.Fatal("refused running task is marked for the note")
		}
		if auth, _, errExempt := rig.pickWith(reserveRequestOpts(reserveTestExemptKey, reserveTestRunningTask)); errExempt != nil || auth.ID == rig.credit {
			t.Fatalf("exempt pick after the wind-down = %v, %v; want a Max login", auth, errExempt)
		}
	})
}

// wind-down-minutes: 0 keeps the old behaviour: every request of an ordinary caller is
// refused once the trigger is on.
func TestReserveWindDownZeroRefusesRunningTasks(t *testing.T) {
	zero := 0
	forEachReserveRig(t, reserveGateOn(func(g *internalconfig.ReserveGateConfig) { g.WindDownMinutes = &zero }), func(t *testing.T, rig *reserveRig) {
		rig.bothHeld(t)
		_, _, err := rig.pickWith(reserveRequestOpts(reserveTestUserKey, reserveTestRunningTask))
		requireReserveHold(t, err)
	})
}

// A configured wind-down is used as given.
func TestReserveWindDownConfiguredLength(t *testing.T) {
	two := 2
	rig := newReserveRig(t, &RoundRobinSelector{}, false, reserveGateOn(func(g *internalconfig.ReserveGateConfig) { g.WindDownMinutes = &two }))
	rig.bothHeld(t)
	rig.manager.reserveTrigger.mu.Lock()
	rig.manager.reserveTrigger.since = time.Now().Add(-90 * time.Second)
	rig.manager.reserveTrigger.mu.Unlock()
	if _, _, err := rig.pickWith(reserveRequestOpts(reserveTestUserKey, reserveTestRunningTask)); err != nil {
		t.Fatalf("running task 90 s into a 2 min wind-down: %v", err)
	}
	rig.manager.reserveTrigger.mu.Lock()
	rig.manager.reserveTrigger.since = time.Now().Add(-121 * time.Second)
	rig.manager.reserveTrigger.mu.Unlock()
	_, _, err := rig.pickWith(reserveRequestOpts(reserveTestUserKey, reserveTestRunningTask))
	requireReserveHold(t, err)
}

// Trigger off (one login has room): everything is served, nothing is marked, and the
// clock is cleared, so the next trigger starts a fresh wind-down.
func TestReserveWindDownTriggerOffClearsClockAndNote(t *testing.T) {
	forEachReserveRig(t, reserveGateOn(nil), func(t *testing.T, rig *reserveRig) {
		rig.bothHeld(t)
		if _, _, err := rig.pickWith(reserveRequestOpts(reserveTestUserKey, reserveTestRunningTask)); err != nil {
			t.Fatalf("running task: %v", err)
		}
		started := rig.manager.reserveTrigger.since
		if started.IsZero() {
			t.Fatal("trigger clock did not start")
		}
		rig.observe(t, rig.b, "7d", 0.30, time.Now().Add(6*24*time.Hour))
		for _, body := range []string{reserveTestNewPrompt, reserveTestRunningTask} {
			opts := reserveRequestOpts(reserveTestUserKey, body)
			opts.Metadata[ReserveWindDownMetadataKey] = true // stale mark of an earlier attempt
			auth, meta, err := rig.pickWith(opts)
			if err != nil || auth.ID == rig.credit {
				t.Fatalf("pick with room = %v, %v; want a Max login", auth, err)
			}
			if windDownFlag(meta) {
				t.Fatal("request with room is marked for the note")
			}
		}
		if !rig.manager.reserveTrigger.since.IsZero() {
			t.Fatal("trigger clock not cleared once a login had room")
		}
		rig.observe(t, rig.b, "7d", 0.98, time.Now().Add(6*24*time.Hour))
		if _, _, err := rig.pickWith(reserveRequestOpts(reserveTestUserKey, reserveTestRunningTask)); err != nil {
			t.Fatalf("running task after a new trigger: %v", err)
		}
		// The clock was cleared above, so a start now is a new one; it cannot predate the first.
		if again := rig.manager.reserveTrigger.since; again.IsZero() || again.Before(started) {
			t.Fatalf("second trigger start %v, want a new start not before %v", again, started)
		}
	})
}

// The clock also starts from a recorded response (here exempt traffic), not only from an
// ordinary caller's request, and turning the gate off clears it.
func TestReserveWindDownClockStartsFromRecordedResponse(t *testing.T) {
	rig := newReserveRig(t, &RoundRobinSelector{}, false, reserveGateOn(nil))
	reset := time.Now().Add(48 * time.Hour)
	for _, id := range []string{rig.a, rig.b} {
		ctx := internallogging.WithResponseHeadersHolder(context.Background())
		internallogging.SetResponseHeaders(ctx, unifiedHeaders(map[string]float64{"7d": 0.98}, reset))
		rig.manager.MarkResult(ctx, Result{AuthID: id, Provider: "claude", Model: reserveTestClaudeModel, Success: true})
	}
	if rig.manager.reserveTrigger.since.IsZero() {
		t.Fatal("trigger clock did not start from the responses that held both logins")
	}
	rig.manager.SetConfig(&internalconfig.Config{})
	if !rig.manager.reserveTrigger.since.IsZero() {
		t.Fatal("disabling the gate did not clear the trigger clock")
	}
}

// Both Max logins at a real limit: the wind-down does not open the credit key to an
// ordinary caller. Its running task is refused like its new prompt; the exempt caller is
// served by the credit key.
func TestReserveWindDownBothLimitedKeepsCreditKeyForExemptCaller(t *testing.T) {
	forEachReserveRig(t, reserveGateOn(nil), func(t *testing.T, rig *reserveRig) {
		rig.limitAt(t, rig.a, time.Now().Add(48*time.Hour))
		rig.limitAt(t, rig.b, time.Now().Add(time.Hour))
		for _, body := range []string{reserveTestRunningTask, reserveTestNewPrompt} {
			auth, _, err := rig.pickWith(reserveRequestOpts(reserveTestUserKey, body))
			if err == nil || statusCodeFromError(err) != http.StatusTooManyRequests {
				t.Fatalf("ordinary pick with both limited = %v, %v; want a 429 and no credit key", auth, err)
			}
			auth, meta, err := rig.pickWith(reserveRequestOpts(reserveTestExemptKey, body))
			if err != nil || auth.ID != rig.credit {
				t.Fatalf("exempt pick with both limited = %v, %v; want credit key", auth, err)
			}
			if ReserveWindDownNoteApplies(auth, meta) {
				t.Fatal("credit key gets the note")
			}
		}
	})
}

// The manual hold still refuses everything of an ordinary caller, running tasks included.
func TestReserveWindDownManualHoldRefusesRunningTask(t *testing.T) {
	forEachReserveRig(t, reserveGateOn(func(g *internalconfig.ReserveGateConfig) { g.Hold = true }), func(t *testing.T, rig *reserveRig) {
		_, meta, err := rig.pickWith(reserveRequestOpts(reserveTestUserKey, reserveTestRunningTask))
		if hold := requireReserveHold(t, err); !hold.manual {
			t.Fatal("hold error is not the manual hold")
		}
		if windDownFlag(meta) {
			t.Fatal("manual hold marked the request for the note")
		}
	})
}

// Only subscription logins are Max logins; a Claude API key never is one, with or
// without fallback-when.
func TestReserveGateAPIKeyIsNeverMaxLogin(t *testing.T) {
	cases := []struct {
		name string
		auth *Auth
		want bool
	}{
		{"oauth login", &Auth{ID: "o", Provider: "claude", Attributes: map[string]string{"auth_kind": "oauth"}}, true},
		{"oauth login from metadata", &Auth{ID: "m", Provider: "claude", Metadata: map[string]any{"access_token": "x", "email": "a@example.com"}}, true},
		{"drained api key", &Auth{ID: "d", Provider: "claude", Attributes: map[string]string{"api_key": "k", "priority": "10", "auth_kind": "apikey"}}, false},
		{"api key without kind", &Auth{ID: "k", Provider: "claude", Attributes: map[string]string{"api_key": "k"}}, false},
		{"credit key", &Auth{ID: "c", Provider: "claude", Attributes: map[string]string{"api_key": "k", AttributeFallbackWhen: FallbackWhenLimit}}, false},
		{"disabled oauth login", &Auth{ID: "x", Provider: "claude", Disabled: true, Attributes: map[string]string{"auth_kind": "oauth"}}, false},
		{"codex login", &Auth{ID: "g", Provider: "codex", Attributes: map[string]string{"auth_kind": "oauth"}}, false},
	}
	for _, tc := range cases {
		if got := isReserveMaxLogin(tc.auth); got != tc.want {
			t.Errorf("%s: Max login = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The credit expiry drain moves a credit key to priority 10 and removes its fallback-when.
// It does not count as a Max login with room: with both logins held a new prompt is still
// refused. A running task is served inside the wind-down, by that key (it is first in
// priority), with the note; after the wind-down it is refused.
func TestReserveGateDrainedCreditKeyDoesNotDisableReserve(t *testing.T) {
	forEachReserveRig(t, reserveGateOn(nil), func(t *testing.T, rig *reserveRig) {
		drained := rig.addDrainedKey(t)
		rig.bothHeld(t)
		_, _, err := rig.pickWith(reserveRequestOpts(reserveTestUserKey, reserveTestNewPrompt))
		requireReserveHold(t, err)
		auth, meta, err := rig.pickWith(reserveRequestOpts(reserveTestUserKey, reserveTestRunningTask))
		if err != nil || auth.ID != drained {
			t.Fatalf("running task pick = %v, %v; want the drained key %s", auth, err, drained)
		}
		if !ReserveWindDownNoteApplies(auth, meta) {
			t.Fatal("running task on the drained key is not marked for the note")
		}
		rig.manager.reserveTrigger.mu.Lock()
		rig.manager.reserveTrigger.since = time.Now().Add(-time.Hour)
		rig.manager.reserveTrigger.mu.Unlock()
		_, _, err = rig.pickWith(reserveRequestOpts(reserveTestUserKey, reserveTestRunningTask))
		requireReserveHold(t, err)
		// With a login under the threshold nothing is blocked; the drained key serves first.
		rig.observe(t, rig.b, "7d", 0.30, time.Now().Add(6*24*time.Hour))
		auth, _, err = rig.pickWith(reserveRequestOpts(reserveTestUserKey, reserveTestNewPrompt))
		if err != nil || auth.ID != drained {
			t.Fatalf("new prompt with room = %v, %v; want the drained key %s", auth, err, drained)
		}
	})
}

// With only API keys there is no Max login and no reserve: nothing is refused.
func TestReserveGateNoMaxLoginNoReserve(t *testing.T) {
	rig := newReserveRig(t, &RoundRobinSelector{}, false, reserveGateOn(nil))
	drained := rig.addDrainedKey(t)
	rig.disable(t, rig.a)
	rig.disable(t, rig.b)
	rig.observe(t, drained, "7d", 0.99, time.Now().Add(time.Hour))
	auth, meta, err := rig.pickWith(reserveRequestOpts(reserveTestUserKey, reserveTestNewPrompt))
	if err != nil || auth.ID != drained || windDownFlag(meta) {
		t.Fatalf("pick = %v, %v, marked %v; want the API key, unmarked", auth, err, windDownFlag(meta))
	}
}

func TestReserveWindDownNoteApplies(t *testing.T) {
	marked := map[string]any{ReserveWindDownMetadataKey: true}
	cases := []struct {
		name string
		auth *Auth
		meta map[string]any
		want bool
	}{
		{"oauth login marked", &Auth{Provider: "claude", Attributes: map[string]string{"auth_kind": "oauth"}}, marked, true},
		{"drained api key marked", &Auth{Provider: "claude", Attributes: map[string]string{"api_key": "k", "priority": "10"}}, marked, true},
		{"credit key marked", &Auth{Provider: "claude", Attributes: map[string]string{"api_key": "k", AttributeFallbackWhen: FallbackWhenLimit}}, marked, false},
		{"codex marked", &Auth{Provider: "codex", Attributes: map[string]string{"auth_kind": "oauth"}}, marked, false},
		{"oauth login unmarked", &Auth{Provider: "claude", Attributes: map[string]string{"auth_kind": "oauth"}}, map[string]any{}, false},
		{"nil metadata", &Auth{Provider: "claude"}, nil, false},
	}
	for _, tc := range cases {
		if got := ReserveWindDownNoteApplies(tc.auth, tc.meta); got != tc.want {
			t.Errorf("%s: note = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// An OpenAI Chat Completions caller whose last message is a tool result is a running task.
func TestReserveWindDownOpenAIFormatRunningTask(t *testing.T) {
	rig := newReserveRig(t, &RoundRobinSelector{}, true, reserveGateOn(nil))
	rig.bothHeld(t)
	opts := reserveOpts(reserveTestUserKey, "")
	opts.SourceFormat = sdktranslator.FormatOpenAI
	opts.OriginalRequest = []byte(`{"messages":[{"role":"user","content":"go"},{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c1","content":"ok"}]}`)
	auth, meta, err := rig.pickWith(opts)
	if err != nil || auth.ID == rig.credit || !windDownFlag(meta) {
		t.Fatalf("openai running task = %v, %v, marked %v; want a Max login, marked", auth, err, windDownFlag(meta))
	}
	opts = reserveOpts(reserveTestUserKey, "")
	opts.SourceFormat = sdktranslator.FormatOpenAI
	opts.OriginalRequest = []byte(`{"messages":[{"role":"user","content":"go"}]}`)
	_, _, err = rig.pickWith(opts)
	requireReserveHold(t, err)
}
