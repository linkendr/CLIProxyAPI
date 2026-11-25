package openai

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestResponsesWebsocketErrorPayloadCapsRetryAfter(t *testing.T) {
	handlers.SetClientRetryAfterCapSeconds(120)
	t.Cleanup(func() { handlers.SetClientRetryAfterCapSeconds(0) })
	payload, errBuild := buildResponsesWebsocketErrorPayload(&interfaces.ErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error:      coreauth.NewModelCooldownError("gpt-5", "codex", 50*time.Hour),
	})
	if errBuild != nil {
		t.Fatalf("buildResponsesWebsocketErrorPayload() error = %v", errBuild)
	}
	if got := ojson.GetBytesNoCopy(payload, "headers.Retry-After").String(); got != "120" {
		t.Fatalf("headers.Retry-After = %q, want capped 120; payload=%s", got, payload)
	}
	if !strings.Contains(string(payload), "180000") {
		t.Fatalf("payload = %s, want uncapped reset_seconds 180000", payload)
	}
}
