package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coresession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
)

type reserveGateResponseExecutor struct{}

func (reserveGateResponseExecutor) Identifier() string { return "claude" }

func (reserveGateResponseExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{Payload: []byte("{}")}, nil
}

func (reserveGateResponseExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return nil, nil
}

func (reserveGateResponseExecutor) Refresh(_ context.Context, a *coreauth.Auth) (*coreauth.Auth, error) {
	return a, nil
}

func (reserveGateResponseExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, nil
}

func (reserveGateResponseExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

// A request the reserve gate refuses reaches the client as a 429 with the Anthropic
// error body, the hold message and a Retry-After for the earliest release: at 99% with
// 3 h left, the login is released in its final hour, 2 h from now.
func TestReserveGateHoldReachesClientAs429(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const model = "reserve-gate-response-model"
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	manager.SetConfig(&internalconfig.Config{Routing: internalconfig.RoutingConfig{ReserveGate: internalconfig.ReserveGateConfig{Enabled: true}}})
	manager.RegisterExecutor(reserveGateResponseExecutor{})
	reset := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	auth := &coreauth.Auth{
		ID: "reserve-gate-response-oauth", Provider: "claude", Status: coreauth.StatusActive,
		Attributes: map[string]string{"auth_kind": "oauth"},
		Quota: coreauth.QuotaState{ObservedAt: time.Now(), Signals: map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Utilization": "0.99",
			"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(reset.Unix(), 10),
		}},
	}
	if _, err := manager.Register(coreauth.WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })

	opts := coreexecutor.Options{Metadata: map[string]any{coreexecutor.CallerScopeMetadataKey: coresession.CallerScope("interactive-key")}}
	_, err := manager.Execute(context.Background(), []string{"claude"}, coreexecutor.Request{Model: model}, opts)
	if err == nil {
		t.Fatal("held account served a non-exempt caller")
	}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	NewBaseAPIHandlers(nil, nil).WriteErrorResponse(c, ExecutionErrorMessage(err))

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body %s", recorder.Code, recorder.Body.String())
	}
	retryAfter, errParse := strconv.Atoi(recorder.Header().Get("Retry-After"))
	if errParse != nil || retryAfter < 7100 || retryAfter > 7201 {
		t.Fatalf("Retry-After = %q, want about two hours", recorder.Header().Get("Retry-After"))
	}
	var body struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if errJSON := json.Unmarshal(recorder.Body.Bytes(), &body); errJSON != nil {
		t.Fatalf("body %q: %v", recorder.Body.String(), errJSON)
	}
	release := reset.Add(-time.Hour).In(time.Local).Format("Mon 2 Jan 15:04")
	if body.Error.Type != "rate_limit_error" || !strings.HasPrefix(body.Error.Message, "Quota reserve hold:") || !strings.Contains(body.Error.Message, "Next release "+release+".") {
		t.Fatalf("body = %s, want rate_limit_error naming the hold and its release in local time (%s)", recorder.Body.String(), release)
	}
	// The message ends with the countdown to the release.
	if !regexp.MustCompile(`\(reset after (1h 59m \d+s|2h 0m 0s)\)$`).MatchString(body.Error.Message) {
		t.Fatalf("message %q does not end with the countdown to the release", body.Error.Message)
	}
	if got := recorder.Header().Get("Anthropic-Ratelimit-Unified-Representative-Claim"); got != "seven_day" {
		t.Fatalf("representative claim header = %q, want seven_day", got)
	}
}
