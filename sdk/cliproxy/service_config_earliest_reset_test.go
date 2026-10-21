package cliproxy

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestEarliestResetStrategySelectsSelector(t *testing.T) {
	for _, name := range []string{"earliest-reset", "Earliest-Reset", "earliestreset", "er"} {
		cfg := &config.Config{}
		cfg.Routing.Strategy = name
		state := normalizedRoutingRuntimeState(cfg)
		if state.strategy != coreauth.RoutingStrategyEarliestReset {
			t.Fatalf("strategy %q normalized to %q", name, state.strategy)
		}
		if _, ok := newRoutingSelector(state).(*coreauth.EarliestResetSelector); !ok {
			t.Fatalf("strategy %q did not build EarliestResetSelector", name)
		}
		state.sessionAffinity = true
		if _, ok := newRoutingSelector(state).(*coreauth.SessionAffinitySelector); !ok {
			t.Fatalf("strategy %q with affinity did not build SessionAffinitySelector", name)
		}
	}
}
