package handlers_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/claude"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestCapRetryAfterValue(t *testing.T) {
	for _, tt := range []struct {
		name  string
		value string
		max   int64
		want  string
	}{
		{"no cap", "180000", 0, "180000"},
		{"negative cap is no cap", "180000", -5, "180000"},
		{"under cap", "30", 120, "30"},
		{"at cap", "120", 120, "120"},
		{"over cap", "180000", 120, "120"},
		{"padded value over cap", " 180000 ", 120, "120"},
		{"http date unchanged", "Wed, 21 Oct 2026 07:28:00 GMT", 120, "Wed, 21 Oct 2026 07:28:00 GMT"},
		{"empty unchanged", "", 120, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := handlers.CapRetryAfterValue(tt.value, tt.max); got != tt.want {
				t.Fatalf("CapRetryAfterValue(%q, %d) = %q, want %q", tt.value, tt.max, got, tt.want)
			}
		})
	}
}

func TestClientRetryAfterCapOpenAIVersusClaude(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handlers.SetClientRetryAfterCapSeconds(120)
	t.Cleanup(func() { handlers.SetClientRetryAfterCapSeconds(0) })

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	claudeHandler := claude.NewClaudeCodeAPIHandler(base)
	cooldown := func() *interfaces.ErrorMessage {
		return &interfaces.ErrorMessage{
			StatusCode: http.StatusTooManyRequests,
			Error:      coreauth.NewModelCooldownError("gpt-5", "codex", 50*time.Hour),
		}
	}

	router := gin.New()
	router.POST("/v1/chat/completions", handlers.ClientRetryAfterCapMiddleware(), func(c *gin.Context) {
		base.WriteErrorResponse(c, cooldown())
	})
	router.POST("/v1/responses-stream", handlers.ClientRetryAfterCapMiddleware(), func(c *gin.Context) {
		c.Header("Retry-After", "180000")
		c.Header("Content-Type", "text/event-stream")
		c.Writer.Flush()
	})
	router.POST("/v1/messages", func(c *gin.Context) {
		claudeHandler.WriteErrorResponse(c, cooldown())
	})

	post := func(path string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}")))
		return recorder
	}

	openai := post("/v1/chat/completions")
	if openai.Code != http.StatusTooManyRequests {
		t.Fatalf("openai status = %d, want 429", openai.Code)
	}
	if got := openai.Header().Get("Retry-After"); got != "120" {
		t.Fatalf("openai Retry-After = %q, want capped 120", got)
	}
	if !strings.Contains(openai.Body.String(), "180000") {
		t.Fatalf("openai body = %s, want the uncapped reset_seconds 180000", openai.Body.String())
	}

	if got := post("/v1/responses-stream").Header().Get("Retry-After"); got != "120" {
		t.Fatalf("streaming Retry-After = %q, want capped 120", got)
	}

	claudeResp := post("/v1/messages")
	if got := claudeResp.Header().Get("Retry-After"); got != "180000" {
		t.Fatalf("claude Retry-After = %q, want full 180000", got)
	}
}

func TestClientRetryAfterCapDisabledKeepsFullValue(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handlers.SetClientRetryAfterCapSeconds(0)
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	router := gin.New()
	router.POST("/v1/chat/completions", handlers.ClientRetryAfterCapMiddleware(), func(c *gin.Context) {
		base.WriteErrorResponse(c, &interfaces.ErrorMessage{
			StatusCode: http.StatusTooManyRequests,
			Error:      coreauth.NewModelCooldownError("gpt-5", "codex", 50*time.Hour),
		})
	})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}")))
	if got := recorder.Header().Get("Retry-After"); got != "180000" {
		t.Fatalf("Retry-After = %q, want full 180000 with the cap off", got)
	}
}
