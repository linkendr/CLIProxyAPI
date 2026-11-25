package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
)

func TestClientRetryAfterCapRoutesAreRegistered(t *testing.T) {
	server := newTestServerWithConfig(t, &config.Config{})
	registered := make(map[string]bool)
	for _, route := range server.engine.Routes() {
		registered[route.Path] = true
	}
	for path := range clientRetryAfterCapRoutes {
		if !registered[path] {
			t.Errorf("capped route %s is not registered", path)
		}
	}
}

func TestClientRetryAfterCapMiddlewareCapsListedRoutesOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handlers.SetClientRetryAfterCapSeconds(120)
	t.Cleanup(func() { handlers.SetClientRetryAfterCapSeconds(0) })

	engine := gin.New()
	v1 := engine.Group("/v1")
	v1.Use(clientRetryAfterCapMiddleware())
	refuse := func(c *gin.Context) {
		c.Header("Retry-After", "3600")
		c.String(http.StatusTooManyRequests, "rate limited")
	}
	v1.POST("/chat/completions", refuse)
	v1.GET("/videos/:request_id", refuse)
	v1.POST("/messages", refuse)

	for _, tc := range []struct {
		method, path, want string
	}{
		{http.MethodPost, "/v1/chat/completions", "120"},
		{http.MethodGet, "/v1/videos/abc", "120"},
		{http.MethodPost, "/v1/messages", "3600"},
	} {
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, nil))
		if got := recorder.Header().Get("Retry-After"); got != tc.want {
			t.Errorf("%s %s Retry-After = %q, want %q", tc.method, tc.path, got, tc.want)
		}
	}
}
