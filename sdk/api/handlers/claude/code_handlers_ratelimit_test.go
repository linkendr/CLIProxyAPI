package claude

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

type upstreamRateLimitTestErr struct{ headers http.Header }

func (e upstreamRateLimitTestErr) Error() string                         { return "rate limited" }
func (e upstreamRateLimitTestErr) UpstreamRateLimitHeaders() http.Header { return e.headers }

func TestWriteErrorResponseEchoesAllowlistedRateLimitHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	h := &ClaudeCodeAPIHandler{BaseAPIHandler: &handlers.BaseAPIHandler{Cfg: &sdkconfig.SDKConfig{}}}
	h.WriteErrorResponse(c, &interfaces.ErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error: upstreamRateLimitTestErr{headers: coreauth.FilterUpstreamRateLimitHeaders(http.Header{
			"Retry-After":                        {"30"},
			"Anthropic-Ratelimit-Unified-Status": {"rejected"},
			"Anthropic-Ratelimit-Unified-Reset":  {"1790000000"},
			"Set-Cookie":                         {"a=b"},
		})},
	})

	if got := recorder.Header().Get("Anthropic-Ratelimit-Unified-Reset"); got != "1790000000" {
		t.Fatalf("unified-reset = %q", got)
	}
	if got := recorder.Header().Get("Retry-After"); got != "30" {
		t.Fatalf("Retry-After = %q", got)
	}
	if recorder.Header().Get("Set-Cookie") != "" {
		t.Fatal("Set-Cookie must not be echoed")
	}
}

func TestToClaudeErrorKeepsCooldownResetFields(t *testing.T) {
	h := &ClaudeCodeAPIHandler{}
	cooldown := coreauth.NewModelCooldownError("claude-haiku-5-5", "claude", 90*time.Minute)
	resp := h.toClaudeError(&interfaces.ErrorMessage{StatusCode: http.StatusTooManyRequests, Error: cooldown})

	body, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	if got := ojson.GetBytes(body, "error.type").String(); got != "rate_limit_error" {
		t.Fatalf("error.type = %q", got)
	}
	if got := ojson.GetBytes(body, "error.reset_seconds"); got.Int() != 5400 {
		t.Fatalf("error.reset_seconds = %s, want 5400", got.Raw)
	}
	if got := ojson.GetBytes(body, "error.reset_time").String(); got == "" {
		t.Fatal("error.reset_time dropped")
	}
	plain := h.toClaudeError(&interfaces.ErrorMessage{StatusCode: http.StatusTooManyRequests, Error: errors.New("slow down")})
	plainBody, _ := json.Marshal(plain)
	if ojson.GetBytes(plainBody, "error.reset_seconds").Exists() {
		t.Fatal("reset_seconds must be omitted when absent")
	}
}
