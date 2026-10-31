package cliproxy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestPaceStrategySelectsSelector(t *testing.T) {
	for _, name := range []string{"pace", "Pace", " PACE "} {
		cfg := &config.Config{}
		cfg.Routing.Strategy = name
		state := normalizedRoutingRuntimeState(cfg)
		if state.strategy != coreauth.RoutingStrategyPace || state.paceOverride != 24*time.Hour || state.paceSwitchMargin != 0.10 {
			t.Fatalf("strategy %q normalized to %+v, want pace with defaults", name, state)
		}
		if _, ok := newRoutingSelector(state).(*coreauth.PaceSelector); !ok {
			t.Fatalf("strategy %q did not build PaceSelector", name)
		}
		state.sessionAffinity = true
		if _, ok := newRoutingSelector(state).(*coreauth.SessionAffinitySelector); !ok {
			t.Fatalf("strategy %q with affinity did not build SessionAffinitySelector", name)
		}
	}
	// routing.pace does not touch the state of another strategy, so editing it does not
	// rebuild (and reset) that strategy's selector.
	cfg := &config.Config{}
	cfg.Routing.Strategy = "earliest-reset"
	margin := 0.3
	cfg.Routing.Pace.SwitchMargin = &margin
	if state := normalizedRoutingRuntimeState(cfg); state.paceSwitchMargin != 0 || state.paceOverride != 0 {
		t.Fatalf("earliest-reset state carries pace settings: %+v", state)
	}
}

// Editing routing.pace in config.yaml reaches the routing state through the real watcher
// reload path, without a restart; an invalid edit is rejected and the running settings stay.
func TestPaceHotReloadFromConfigFile(t *testing.T) {
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.yaml")
	base := fmt.Sprintf("port: 18998\nauth-dir: %q\n", filepath.ToSlash(authDir))
	write := func(extra string) {
		t.Helper()
		if err := os.WriteFile(configPath, []byte(base+extra), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("routing:\n  strategy: earliest-reset\n")
	cfg, err := internalconfig.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewBuilder().WithConfig(cfg).WithConfigPath(configPath).Build()
	if err != nil {
		t.Fatal(err)
	}
	service.updateServerClientsContextFn = func(context.Context, *config.Config) bool { return true }
	watcherWrapper, err := service.watcherFactory(configPath, authDir, func(newCfg *config.Config) {
		service.applyWatcherConfigUpdate(newCfg)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = watcherWrapper.Stop() })
	service.setWatcher(watcherWrapper)
	watcherWrapper.SetConfig(service.currentConfig())
	service.applyConfigUpdate(service.currentConfig())

	applied := func() routingRuntimeState {
		t.Helper()
		if service.appliedRoutingState == nil {
			t.Fatal("no routing state applied")
		}
		return *service.appliedRoutingState
	}
	if got := applied().strategy; got != coreauth.RoutingStrategyEarliestReset {
		t.Fatalf("initial strategy = %q", got)
	}

	write("routing:\n  strategy: pace\n  pace:\n    override-hours: 12\n    switch-margin: 0.2\n")
	if !service.reloadConfigFromWatcher() {
		t.Fatal("reload did not run")
	}
	if got := applied(); got.strategy != coreauth.RoutingStrategyPace || got.paceOverride != 12*time.Hour || got.paceSwitchMargin != 0.2 {
		t.Fatalf("after switching to pace: %+v", got)
	}

	write("routing:\n  strategy: pace\n  pace:\n    override-hours: 6\n")
	service.reloadConfigFromWatcher()
	if got := applied(); got.paceOverride != 6*time.Hour || got.paceSwitchMargin != 0.10 {
		t.Fatalf("after editing override-hours: %+v", got)
	}

	write("routing:\n  strategy: pace\n  pace:\n    switch-margin: 10\n")
	service.reloadConfigFromWatcher()
	if got := applied(); got.paceOverride != 6*time.Hour || got.paceSwitchMargin != 0.10 {
		t.Fatalf("an invalid edit changed the running settings: %+v", got)
	}

	write("routing:\n  strategy: earliest-reset\n")
	service.reloadConfigFromWatcher()
	if got := applied().strategy; got != coreauth.RoutingStrategyEarliestReset {
		t.Fatalf("rollback strategy = %q", got)
	}
}
