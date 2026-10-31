package auth

import (
	"context"
	"strconv"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const (
	paceTestModel = "claude-sonnet"
	paceTestFable = "claude-fable"
)

var paceTestNow = time.Unix(1_900_000_000, 0)

// paceWindow is one weekly reading: utilization and hours until reset (negative: passed).
type paceWindow struct {
	utilization float64
	resetHours  float64
}

func paceSignals(window string, w paceWindow, now time.Time) map[string]string {
	reset := now.Add(time.Duration(w.resetHours * float64(time.Hour)))
	return map[string]string{
		"Anthropic-Ratelimit-Unified-" + window + "-Utilization": strconv.FormatFloat(w.utilization, 'f', -1, 64),
		"Anthropic-Ratelimit-Unified-" + window + "-Reset":       epoch(reset),
	}
}

// paceAuth builds a Claude credential with a 7d reading on the credential snapshot and,
// when oi is set, a 7d_oi reading on the state of oiModel.
func paceAuth(id string, weekly *paceWindow, oiModel string, oi *paceWindow, now time.Time) *Auth {
	auth := &Auth{ID: id, Provider: "claude"}
	if weekly != nil {
		auth.Quota = QuotaState{Signals: paceSignals("7d", *weekly, now), ObservedAt: now}
	}
	if oi != nil {
		auth.ModelStates = map[string]*ModelState{
			oiModel: {Quota: QuotaState{Signals: paceSignals("7d_oi", *oi, now), ObservedAt: now}},
		}
	}
	return auth
}

func pw(utilization, resetHours float64) *paceWindow {
	return &paceWindow{utilization: utilization, resetHours: resetHours}
}

func newTestPaceSelector(override time.Duration, margin float64, clock *time.Time) *PaceSelector {
	s := NewPaceSelector(override, margin)
	s.now = func() time.Time { return *clock }
	return s
}

func pacePick(t *testing.T, s *PaceSelector, model string, auths []*Auth) string {
	t.Helper()
	got, err := s.Pick(context.Background(), "claude", model, cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	return got.ID
}

func TestPaceSelectorFirstPick(t *testing.T) {
	now := paceTestNow
	newer := func(a *Auth, model string, w paceWindow, observed time.Time) *Auth {
		a.ModelStates = map[string]*ModelState{model: {Quota: QuotaState{Signals: paceSignals("7d", w, now), ObservedAt: observed}}}
		return a
	}
	for name, tt := range map[string]struct {
		override time.Duration
		model    string
		auths    []*Auth
		want     string
	}{
		"highest remaining per hour wins": {
			auths: []*Auth{
				paceAuth("a", pw(0.50, 72), "", nil, now),  // 0.50/72  = 0.0069
				paceAuth("b", pw(0.20, 120), "", nil, now), // 0.80/120 = 0.0067
				paceAuth("c", pw(0.90, 48), "", nil, now),  // 0.10/48  = 0.0021
			},
			want: "a",
		},
		"fable uses the lower of 7d and 7d_oi": {
			model: paceTestFable,
			auths: []*Auth{
				paceAuth("a", pw(0.10, 100), paceTestFable, pw(0.90, 100), now), // min(0.009, 0.001)
				paceAuth("b", pw(0.50, 100), "", nil, now),                      // 0.005
			},
			want: "b",
		},
		"7d_oi of another model does not count": {
			model: paceTestModel,
			auths: []*Auth{
				paceAuth("a", pw(0.10, 100), paceTestFable, pw(0.90, 100), now), // 0.009 for sonnet
				paceAuth("b", pw(0.50, 100), "", nil, now),                      // 0.005
			},
			want: "a",
		},
		"7d_oi reset within override puts the credential in the override": {
			model: paceTestFable,
			auths: []*Auth{
				paceAuth("a", pw(0.10, 100), paceTestFable, pw(0.50, 10), now),
				paceAuth("b", pw(0.00, 100), "", nil, now),
			},
			want: "a",
		},
		"reset override beats a higher pace": {
			auths: []*Auth{
				paceAuth("a", pw(0.90, 20), "", nil, now),  // 0.005, resets in 20h
				paceAuth("b", pw(0.00, 100), "", nil, now), // 0.010
			},
			want: "a",
		},
		"two in the override: earlier reset wins": {
			auths: []*Auth{
				paceAuth("a", pw(0.10, 20), "", nil, now),
				paceAuth("b", pw(0.80, 10), "", nil, now),
				paceAuth("c", pw(0.00, 100), "", nil, now),
			},
			want: "b",
		},
		"override tie on the reset second: higher pace wins": {
			auths: []*Auth{
				paceAuth("a", pw(0.50, 10), "", nil, now),
				paceAuth("b", pw(0.20, 10), "", nil, now),
			},
			want: "b",
		},
		"override skips a credential with no quota left": {
			auths: []*Auth{
				paceAuth("a", pw(1.00, 5), "", nil, now),
				paceAuth("b", pw(0.50, 100), "", nil, now),
			},
			want: "b",
		},
		"override-hours 0 turns the override off": {
			override: -1,
			auths: []*Auth{
				paceAuth("a", pw(0.90, 20), "", nil, now),
				paceAuth("b", pw(0.00, 100), "", nil, now),
			},
			want: "b",
		},
		"expired 7d reset is ignored, 7d_oi still counts": {
			model: paceTestFable,
			auths: []*Auth{
				paceAuth("a", pw(0.99, -1), paceTestFable, pw(0.00, 100), now), // 7d rolled over; 0.010
				paceAuth("b", pw(0.50, 100), "", nil, now),                     // 0.005
			},
			want: "a",
		},
		"newest 7d reading wins across snapshots": {
			model: paceTestModel,
			auths: []*Auth{
				// Credential snapshot says 0% an hour ago; the model snapshot says 90% now.
				newer(paceAuth("a", pw(0.00, 100), "", nil, now.Add(-time.Hour)), paceTestModel, paceWindow{0.90, 100}, now),
				paceAuth("b", pw(0.50, 100), "", nil, now),
			},
			want: "b",
		},
	} {
		t.Run(name, func(t *testing.T) {
			override := tt.override
			switch override {
			case 0:
				override = 24 * time.Hour
			case -1:
				override = 0
			}
			model := tt.model
			if model == "" {
				model = paceTestModel
			}
			clock := now
			if got := pacePick(t, newTestPaceSelector(override, 0.10, &clock), model, tt.auths); got != tt.want {
				t.Fatalf("picked %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPaceSelectorSwitchMargin(t *testing.T) {
	now := paceTestNow
	for name, tt := range map[string]struct {
		margin  float64
		bUtil   float64 // b's new utilization; a stays at 0.50 (pace 0.0050)
		wantSec string
	}{
		"within margin keeps the previous pick":  {margin: 0.10, bUtil: 0.46, wantSec: "a"}, // 0.0054 < 0.0050*1.1
		"beyond margin switches":                 {margin: 0.10, bUtil: 0.40, wantSec: "b"}, // 0.0060 > 0.0055
		"margin 0 switches to any better pace":   {margin: 0, bUtil: 0.46, wantSec: "b"},
		"equal pace keeps the previous pick too": {margin: 0, bUtil: 0.50, wantSec: "a"},
	} {
		t.Run(name, func(t *testing.T) {
			clock := now
			s := newTestPaceSelector(24*time.Hour, tt.margin, &clock)
			auths := []*Auth{
				paceAuth("a", pw(0.50, 100), "", nil, now),
				paceAuth("b", pw(0.60, 100), "", nil, now),
			}
			if got := pacePick(t, s, paceTestModel, auths); got != "a" {
				t.Fatalf("first pick = %q, want a", got)
			}
			auths[1].Quota.Signals = paceSignals("7d", paceWindow{tt.bUtil, 100}, now)
			for i := 0; i < 2; i++ {
				if got := pacePick(t, s, paceTestModel, auths); got != tt.wantSec {
					t.Fatalf("pick #%d after b changed = %q, want %q", i+2, got, tt.wantSec)
				}
			}
		})
	}
}

func TestPaceSelectorMarginIsPerModel(t *testing.T) {
	clock := paceTestNow
	s := newTestPaceSelector(24*time.Hour, 0.10, &clock)
	auths := []*Auth{
		paceAuth("a", pw(0.50, 100), "", nil, clock),
		paceAuth("b", pw(0.60, 100), "", nil, clock),
	}
	if got := pacePick(t, s, paceTestModel, auths); got != "a" {
		t.Fatalf("first pick = %q, want a", got)
	}
	auths[1].Quota.Signals = paceSignals("7d", paceWindow{0.46, 100}, clock)
	// Another model has no previous pick, so it takes the best pace.
	if got := pacePick(t, s, "claude-other", auths); got != "b" {
		t.Fatalf("other model pick = %q, want b", got)
	}
}

func TestPaceSelectorUnknownProbedOnceThenRankedBehind(t *testing.T) {
	clock := paceTestNow
	s := newTestPaceSelector(24*time.Hour, 0.10, &clock)
	auths := []*Auth{
		paceAuth("a-known", pw(0.50, 100), "", nil, clock),
		// Only a 5h reading: no weekly window known, so a probe.
		{ID: "b-unknown", Provider: "claude", Quota: QuotaState{Signals: paceSignals("5h", paceWindow{0.1, 2}, clock), ObservedAt: clock}},
		// Weekly reset has passed: the window rolled over, so unknown too.
		paceAuth("c-expired", pw(0.10, -1), "", nil, clock),
		paceAuth("d-known", pw(0.80, 100), "", nil, clock),
	}
	var got []string
	for i := 0; i < 4; i++ {
		got = append(got, pacePick(t, s, paceTestModel, auths))
	}
	want := []string{"b-unknown", "c-expired", "a-known", "a-known"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("picks = %v, want %v (probe each unknown once, then pace order)", got, want)
		}
	}
	// The probe expires: the still-unknown credentials are probed once more.
	clock = clock.Add(earliestResetProbeTTL + time.Second)
	got = got[:0]
	for i := 0; i < 3; i++ {
		got = append(got, pacePick(t, s, paceTestModel, auths))
	}
	want = []string{"b-unknown", "c-expired", "a-known"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("picks after ttl = %v, want %v", got, want)
		}
	}
}

// A probe does not count as the previous pick, so the switch margin still protects the
// credential picked before it.
func TestPaceSelectorProbeKeepsMarginPick(t *testing.T) {
	clock := paceTestNow
	s := newTestPaceSelector(24*time.Hour, 0.10, &clock)
	auths := []*Auth{
		paceAuth("a", pw(0.50, 100), "", nil, clock),
		paceAuth("b", pw(0.60, 100), "", nil, clock),
	}
	if got := pacePick(t, s, paceTestModel, auths); got != "a" {
		t.Fatalf("first pick = %q, want a", got)
	}
	auths[1].Quota.Signals = paceSignals("7d", paceWindow{0.46, 100}, clock) // within margin
	auths = append(auths, &Auth{ID: "c-new", Provider: "claude"})
	if got := pacePick(t, s, paceTestModel, auths); got != "c-new" {
		t.Fatalf("second pick = %q, want c-new (probe)", got)
	}
	if got := pacePick(t, s, paceTestModel, auths); got != "a" {
		t.Fatalf("third pick = %q, want a (kept within margin after the probe)", got)
	}
}

func TestPaceSelectorAllUnknownRoundRobin(t *testing.T) {
	clock := paceTestNow
	s := newTestPaceSelector(24*time.Hour, 0.10, &clock)
	auths := []*Auth{{ID: "a", Provider: "claude"}, {ID: "b", Provider: "claude"}, {ID: "c", Provider: "claude"}}
	var got []string
	for i := 0; i < 6; i++ {
		got = append(got, pacePick(t, s, paceTestModel, auths))
	}
	// Three probes, then rotation.
	want := []string{"a", "b", "c", "a", "b", "c"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("picks = %v, want %v", got, want)
		}
	}
}

func TestPaceSelectorOverrideTiesRotate(t *testing.T) {
	clock := paceTestNow
	s := newTestPaceSelector(24*time.Hour, 0.10, &clock)
	auths := []*Auth{
		paceAuth("a", pw(0.50, 10), "", nil, clock),
		paceAuth("b", pw(0.50, 10), "", nil, clock),
		paceAuth("c", pw(0.00, 100), "", nil, clock),
	}
	var got []string
	for i := 0; i < 4; i++ {
		got = append(got, pacePick(t, s, paceTestModel, auths))
	}
	want := []string{"a", "b", "a", "b"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("picks = %v, want %v", got, want)
		}
	}
}

func TestPaceSelectorSkipsCooledCredentials(t *testing.T) {
	clock := paceTestNow
	cooled := paceAuth("a", pw(0.00, 100), "", nil, clock)
	cooled.Quota.Exceeded = true
	cooled.Quota.Reason = "credential_quota"
	cooled.Quota.NextRecoverAt = clock.Add(time.Hour)
	auths := []*Auth{cooled, paceAuth("b", pw(0.90, 100), "", nil, clock)}
	if got := pacePick(t, newTestPaceSelector(24*time.Hour, 0.10, &clock), paceTestModel, auths); got != "b" {
		t.Fatalf("picked %q, want b (a is cooled down)", got)
	}
}

func TestWeeklyPaceValues(t *testing.T) {
	now := paceTestNow
	auth := paceAuth("a", pw(0.25, 50), paceTestFable, pw(0.50, 25), now)
	pace, reset, ok := weeklyPace(auth, paceTestFable, now)
	if !ok || pace != 0.75/50 || !reset.Equal(now.Add(25*time.Hour)) {
		t.Fatalf("weeklyPace = %v, %v, %v; want 0.015 (7d 0.75/50 = 0.015, 7d_oi 0.50/25 = 0.02), reset in 25h", pace, reset, ok)
	}
	over := paceAuth("b", pw(1.20, 50), "", nil, now)
	if pace, _, ok = weeklyPace(over, paceTestModel, now); !ok || pace != 0 {
		t.Fatalf("utilization above 1: pace = %v, %v; want 0", pace, ok)
	}
	if _, _, ok = weeklyPace(&Auth{ID: "c"}, paceTestModel, now); ok {
		t.Fatal("credential without readings has a pace")
	}
}
