package auth

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// The manager must hand the requested model to PaceSelector, otherwise the model-scoped
// 7d_oi readings in ModelStates are skipped and the pick ignores them.
func TestManagerPickNextPaceUsesRequestedModelWindow(t *testing.T) {
	const model = "claude-fable-pace-test"
	now := time.Now()
	// "a-oi-spent" has the better 7d pace, but its 7d_oi window for model is nearly spent.
	authSpent := paceAuth("a-oi-spent", pw(0.10, 100), model, pw(0.95, 100), now)
	authFresh := paceAuth("b-fresh", pw(0.40, 100), "", nil, now)

	manager := NewManager(nil, NewPaceSelector(24*time.Hour, 0.10), nil)
	manager.RegisterExecutor(&refreshMockExecutor{id: "claude"})
	for _, a := range []*Auth{authSpent, authFresh} {
		registry.GetGlobalRegistry().RegisterClient(a.ID, "claude", []*registry.ModelInfo{{ID: model}})
		id := a.ID
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
		if _, err := manager.Register(context.Background(), a); err != nil {
			t.Fatalf("Register(%s) error = %v", a.ID, err)
		}
	}

	for i := 0; i < 3; i++ {
		picked, _, err := manager.pickNext(context.Background(), "claude", model, cliproxyexecutor.Options{}, nil)
		if err != nil {
			t.Fatalf("pickNext() error = %v", err)
		}
		if picked.ID != authFresh.ID {
			t.Fatalf("pickNext() #%d = %s, want %s (7d_oi of the requested model counts)", i, picked.ID, authFresh.ID)
		}
	}
}
