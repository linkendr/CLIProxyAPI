package openai

// Regression test: the Retry-After cap writer must keep forwarding the transport FlushError.

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
)

func TestRetryAfterCapKeepsFlushError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, capSeconds := range []int{0, 120} {
		name := "cap_off"
		if capSeconds > 0 {
			name = "cap_on"
		}
		t.Run(name, func(t *testing.T) {
			handlers.SetClientRetryAfterCapSeconds(capSeconds)
			t.Cleanup(func() { handlers.SetClientRetryAfterCapSeconds(0) })
			router := gin.New()
			var got error
			router.POST("/v1/responses", handlers.ClientRetryAfterCapMiddleware(), func(c *gin.Context) {
				c.Header("Content-Type", "text/event-stream")
				got = flushResponsesSSE(c.Writer)
			})
			// The client is gone: the transport's FlushError reports a closed pipe.
			writer := &issue6332Writer{ResponseRecorder: httptest.NewRecorder(), cancel: func() {}, failFlush: true}
			router.ServeHTTP(writer, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"stream":true}`)))
			if !errors.Is(got, io.ErrClosedPipe) {
				t.Fatalf("flushResponsesSSE error = %v, want io.ErrClosedPipe (transport error hidden)", got)
			}
		})
	}
}
