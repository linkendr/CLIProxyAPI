package auth

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// The manager must hand the requested model to EarliestResetSelector, otherwise the
// per-model reset data in ModelStates is skipped and the pick falls back to ID order.
func TestManagerPickNextEarliestResetUsesRequestedModelReset(t *testing.T) {
	const model = "claude-sonnet-reset-test"
	now := time.Now()
	perModel := func(id string, reset time.Time) *Auth {
		return &Auth{
			ID:       id,
			Provider: "claude",
			ModelStates: map[string]*ModelState{
				model: {Quota: QuotaState{
					Signals:    map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": epoch(reset)},
					ObservedAt: now,
				}},
			},
		}
	}
	// "a-late" sorts first by ID, so an ID-order fallback would wrongly pick it.
	authLate := perModel("a-late", now.Add(3*time.Hour))
	authSoon := perModel("b-soon", now.Add(20*time.Minute))

	manager := NewManager(nil, &EarliestResetSelector{}, nil)
	manager.RegisterExecutor(&refreshMockExecutor{id: "claude"})
	for _, a := range []*Auth{authLate, authSoon} {
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
		if picked.ID != authSoon.ID {
			t.Fatalf("pickNext() #%d = %s, want %s (soonest per-model reset)", i, picked.ID, authSoon.ID)
		}
	}
}
