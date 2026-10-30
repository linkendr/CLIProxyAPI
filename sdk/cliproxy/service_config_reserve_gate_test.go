package cliproxy

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	cliproxysession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

type reserveReloadExecutor struct{}

func (reserveReloadExecutor) Identifier() string { return "claude" }

func (reserveReloadExecutor) Execute(_ context.Context, a *coreauth.Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{Payload: []byte(a.ID)}, nil
}

func (reserveReloadExecutor) ExecuteStream(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, &coreauth.Error{HTTPStatus: http.StatusNotImplemented, Message: "not implemented"}
}

func (reserveReloadExecutor) Refresh(_ context.Context, a *coreauth.Auth) (*coreauth.Auth, error) {
	return a, nil
}

func (reserveReloadExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (reserveReloadExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

// Editing config.yaml toggles the reserve gate through the real watcher reload path,
// without a restart.
func TestReserveGateHotReloadFromConfigFile(t *testing.T) {
	const model = "reserve-reload-model"
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.yaml")
	base := fmt.Sprintf("port: 18999\nauth-dir: %q\n", filepath.ToSlash(authDir))
	write := func(extra string) {
		t.Helper()
		if err := os.WriteFile(configPath, []byte(base+extra), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("")
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

	manager := service.coreManager
	manager.RegisterExecutor(reserveReloadExecutor{})
	auth := &coreauth.Auth{ID: "reserve-reload-oauth", Provider: "claude", Status: coreauth.StatusActive, Attributes: map[string]string{"auth_kind": "oauth"}}
	if _, err = manager.Register(coreauth.WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })

	opts := func(key string) cliproxyexecutor.Options {
		return cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: cliproxysession.CallerScope(key)}}
	}
	selectAs := func(key string) error {
		_, errSelect := manager.SelectAuth(context.Background(), "claude", model, opts(key))
		return errSelect
	}
	if err = selectAs("interactive"); err != nil {
		t.Fatalf("before reload: %v", err)
	}

	write("routing:\n  reserve-gate:\n    enabled: true\n    hold: true\n    exempt-api-keys: [automation]\n")
	if !service.reloadConfigFromWatcher() {
		t.Fatal("reload did not run")
	}
	err = selectAs("interactive")
	status := 0
	if coder, ok := err.(interface{ StatusCode() int }); ok {
		status = coder.StatusCode()
	}
	if err == nil || status != http.StatusTooManyRequests || !strings.Contains(err.Error(), "quota reserve hold") {
		t.Fatalf("after enabling hold: err = %v (status %d), want 429 quota reserve hold", err, status)
	}
	if err = selectAs("automation"); err != nil {
		t.Fatalf("exempt caller during hold: %v", err)
	}

	write("routing:\n  reserve-gate:\n    enabled: false\n")
	service.reloadConfigFromWatcher()
	if err = selectAs("interactive"); err != nil {
		t.Fatalf("after disabling the gate: %v", err)
	}
}
