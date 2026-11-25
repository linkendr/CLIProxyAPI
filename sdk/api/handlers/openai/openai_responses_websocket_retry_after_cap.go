package openai

import (
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
)

// capWebsocketRetryAfter applies the configured client Retry-After cap to a websocket error header.
func capWebsocketRetryAfter(key, value string) string {
	if http.CanonicalHeaderKey(key) != "Retry-After" {
		return value
	}
	return handlers.CapClientRetryAfter(value)
}
