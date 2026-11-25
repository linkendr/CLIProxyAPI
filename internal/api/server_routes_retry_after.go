package api

import (
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
)

// clientRetryAfterCapRoutes are the OpenAI-compatible /v1 routes whose Retry-After the
// client cap applies to (gin route patterns, every method). The Claude routes
// (/v1/messages), the model list and the Codex live routes are not on it: Claude clients
// rely on the real Retry-After value.
var clientRetryAfterCapRoutes = map[string]bool{
	"/v1/chat/completions":   true,
	"/v1/completions":        true,
	"/v1/images/generations": true,
	"/v1/images/edits":       true,
	"/v1/videos":             true,
	"/v1/videos/generations": true,
	"/v1/videos/edits":       true,
	"/v1/videos/extensions":  true,
	"/v1/videos/:request_id": true,
	"/v1/audio/speech":       true,
	"/v1/tts":                true,
	"/v1/responses":          true,
	"/v1/responses/compact":  true,
	"/v1/alpha/search":       true,
}

// clientRetryAfterCapMiddleware runs handlers.ClientRetryAfterCapMiddleware on the
// routes in clientRetryAfterCapRoutes. setupRoutes adds it to the /v1 group after the
// auth middleware, so it runs where a per-route middleware would.
func clientRetryAfterCapMiddleware() gin.HandlerFunc {
	capRetryAfter := handlers.ClientRetryAfterCapMiddleware()
	return func(c *gin.Context) {
		if clientRetryAfterCapRoutes[c.FullPath()] {
			capRetryAfter(c)
			return
		}
		c.Next()
	}
}
