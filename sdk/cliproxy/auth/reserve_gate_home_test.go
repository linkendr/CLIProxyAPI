package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/home"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executionregistry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// reserveHomeDispatcher stands in for the Home control center: for a model it hands out
// the first of its credentials the request did not exclude, in order.
type reserveHomeDispatcher struct {
	byModel map[string][]Auth
	pops    atomic.Int64
}

func (d *reserveHomeDispatcher) HeartbeatOK() bool { return true }

func (d *reserveHomeDispatcher) RPopAuth(ctx context.Context, model, sessionID string, headers http.Header, count int) ([]byte, error) {
	return d.RPopAuthWithConstraints(ctx, model, sessionID, headers, count, nil, "")
}

func (d *reserveHomeDispatcher) RPopAuthWithConstraints(_ context.Context, model, _ string, _ http.Header, _ int, excluded []string, _ string) ([]byte, error) {
	d.pops.Add(1)
	skip := make(map[string]struct{}, len(excluded))
	for _, id := range excluded {
		skip[id] = struct{}{}
	}
	for _, auth := range d.byModel[model] {
		if _, ok := skip[auth.ID]; !ok {
			return json.Marshal(homeAuthDispatchResponse{Auth: auth})
		}
	}
	return nil, home.ErrAuthNotFound
}

func (d *reserveHomeDispatcher) AbortAmbiguousDispatch() {}

// reserveHomeAuth is a Claude OAuth account as Home sends it, with Home's quota snapshot.
func reserveHomeAuth(id string, utilization5h float64, reset time.Time) Auth {
	auth := Auth{ID: id, Provider: "claude", Status: StatusActive, Attributes: map[string]string{"auth_kind": "oauth"}}
	if utilization5h >= 0 {
		auth.Quota = QuotaState{ObservedAt: time.Now(), Signals: map[string]string{
			"anthropic-ratelimit-unified-5h-utilization": strconv.FormatFloat(utilization5h, 'f', 2, 64),
			"anthropic-ratelimit-unified-5h-reset":       strconv.FormatInt(reset.Unix(), 10),
		}}
	}
	return auth
}

func newReserveHomeManager(t *testing.T, gate internalconfig.ReserveGateConfig, dispatcher *reserveHomeDispatcher) (*Manager, *reserveRig) {
	t.Helper()
	rig := &reserveRig{}
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}, Routing: internalconfig.RoutingConfig{ReserveGate: gate}})
	manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
	manager.RegisterExecutor(&reserveTestExecutor{id: "claude", rig: rig})
	manager.RegisterExecutor(&reserveTestExecutor{id: "codex", rig: rig})
	return manager, rig
}

// Under Home the control center picks the credential, so the gate applies to its pick:
// a non-exempt caller is moved off a held account, the exempt caller is not.
func TestReserveGateHomeDispatchSkipsHeldAccount(t *testing.T) {
	reset := time.Now().Add(time.Hour)
	dispatcher := &reserveHomeDispatcher{byModel: map[string][]Auth{
		reserveTestClaudeModel: {reserveHomeAuth("home-held", 0.97, reset), reserveHomeAuth("home-ok", 0.40, reset)},
	}}
	manager, _ := newReserveHomeManager(t, reserveGateOn(nil), dispatcher)
	ctx := context.Background()
	req := cliproxyexecutor.Request{Model: reserveTestClaudeModel}

	resp, err := manager.Execute(ctx, []string{"claude"}, req, reserveOpts(reserveTestUserKey, ""))
	if err != nil || string(resp.Payload) != "home-ok" {
		t.Fatalf("non-exempt Home Execute = %q, %v; want home-ok", resp.Payload, err)
	}
	resp, err = manager.Execute(ctx, []string{"claude"}, req, reserveOpts(reserveTestExemptKey, ""))
	if err != nil || string(resp.Payload) != "home-held" {
		t.Fatalf("exempt Home Execute = %q, %v; want home-held", resp.Payload, err)
	}
}

// When Home has only held Claude accounts, a non-exempt caller gets the reserve 429 on
// every execution path and no upstream call is made.
func TestReserveGateHomeDispatchAllHeldReturns429(t *testing.T) {
	reset := time.Now().Add(90 * time.Minute).Truncate(time.Second)
	dispatcher := &reserveHomeDispatcher{byModel: map[string][]Auth{
		reserveTestClaudeModel: {reserveHomeAuth("home-held", 0.97, reset)},
	}}
	manager, rig := newReserveHomeManager(t, reserveGateOn(nil), dispatcher)
	ctx := context.Background()
	req := cliproxyexecutor.Request{Model: reserveTestClaudeModel}

	_, errExec := manager.Execute(ctx, []string{"claude"}, req, reserveOpts(reserveTestUserKey, ""))
	hold := requireReserveHold(t, errExec)
	if got := hold.Headers().Get("Retry-After"); got == "" {
		t.Fatal("Home reserve hold has no Retry-After")
	}
	_, errStream := manager.ExecuteStream(ctx, []string{"claude"}, req, reserveOpts(reserveTestUserKey, ""))
	requireReserveHold(t, errStream)
	_, errCount := manager.ExecuteCount(ctx, []string{"claude"}, req, reserveOpts(reserveTestUserKey, ""))
	requireReserveHold(t, errCount)
	if calls := rig.calls.Load(); calls != 0 {
		t.Fatalf("executor called %d times for held Home requests", calls)
	}
	resp, err := manager.Execute(ctx, []string{"claude"}, req, reserveOpts(reserveTestExemptKey, ""))
	if err != nil || string(resp.Payload) != "home-held" {
		t.Fatalf("exempt Home Execute = %q, %v; want home-held", resp.Payload, err)
	}
}

// A manual hold refuses every Home-dispatched Claude credential for non-exempt callers
// and leaves Codex alone.
func TestReserveGateHomeManualHoldLeavesCodex(t *testing.T) {
	dispatcher := &reserveHomeDispatcher{byModel: map[string][]Auth{
		reserveTestClaudeModel: {reserveHomeAuth("home-a", -1, time.Time{}), reserveHomeAuth("home-b", -1, time.Time{})},
		reserveTestGPTModel:    {{ID: "home-codex", Provider: "codex", Status: StatusActive, Attributes: map[string]string{"auth_kind": "oauth"}}},
	}}
	manager, _ := newReserveHomeManager(t, reserveGateOn(func(g *internalconfig.ReserveGateConfig) { g.Hold = true }), dispatcher)
	ctx := context.Background()

	_, err := manager.Execute(ctx, []string{"claude"}, cliproxyexecutor.Request{Model: reserveTestClaudeModel}, reserveOpts(reserveTestUserKey, ""))
	if hold := requireReserveHold(t, err); !hold.manual || hold.Headers().Get("Retry-After") != "" {
		t.Fatalf("Home manual hold = %+v, want manual without Retry-After", hold)
	}
	resp, err := manager.Execute(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: reserveTestGPTModel}, reserveOpts(reserveTestUserKey, ""))
	if err != nil || string(resp.Payload) != "home-codex" {
		t.Fatalf("non-exempt Home codex Execute = %q, %v; want home-codex", resp.Payload, err)
	}
	resp, err = manager.Execute(ctx, []string{"claude"}, cliproxyexecutor.Request{Model: reserveTestClaudeModel}, reserveOpts(reserveTestExemptKey, ""))
	if err != nil || string(resp.Payload) != "home-a" {
		t.Fatalf("exempt Home Execute = %q, %v; want home-a", resp.Payload, err)
	}
}
