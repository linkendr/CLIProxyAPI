package handlers

import (
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/gin-gonic/gin"
)

var clientRetryAfterCapSeconds atomic.Int64

// SetClientRetryAfterCapSeconds sets the upper bound, in seconds, for the Retry-After header
// sent on OpenAI-compatible responses. Values <= 0 disable the cap.
func SetClientRetryAfterCapSeconds(seconds int) {
	if seconds < 0 {
		seconds = 0
	}
	clientRetryAfterCapSeconds.Store(int64(seconds))
}

// CapRetryAfterValue returns the Retry-After value limited to maxSeconds. A maxSeconds <= 0
// disables the cap. Values that are not whole seconds (such as HTTP dates) are returned unchanged.
func CapRetryAfterValue(value string, maxSeconds int64) string {
	if maxSeconds <= 0 {
		return value
	}
	seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || seconds <= maxSeconds {
		return value
	}
	return strconv.FormatInt(maxSeconds, 10)
}

// CapClientRetryAfter applies the configured Retry-After cap to value.
func CapClientRetryAfter(value string) string {
	return CapRetryAfterValue(value, clientRetryAfterCapSeconds.Load())
}

// ClientRetryAfterCapMiddleware caps Retry-After on every response of the routes it guards,
// whichever code path writes the header. Use it on OpenAI-compatible routes only: Claude
// clients rely on the real Retry-After value.
func ClientRetryAfterCapMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if clientRetryAfterCapSeconds.Load() > 0 {
			c.Writer = &retryAfterCapWriter{ResponseWriter: c.Writer}
		}
		c.Next()
	}
}

type retryAfterCapWriter struct {
	gin.ResponseWriter
}

func (w *retryAfterCapWriter) capHeader() {
	if w.ResponseWriter.Written() {
		return
	}
	header := w.ResponseWriter.Header()
	values := header.Values("Retry-After")
	if len(values) == 0 {
		return
	}
	capped := make([]string, len(values))
	for i, value := range values {
		capped[i] = CapClientRetryAfter(value)
	}
	header["Retry-After"] = capped
}

func (w *retryAfterCapWriter) WriteHeaderNow() {
	w.capHeader()
	w.ResponseWriter.WriteHeaderNow()
}

func (w *retryAfterCapWriter) Write(data []byte) (int, error) {
	w.capHeader()
	return w.ResponseWriter.Write(data)
}

func (w *retryAfterCapWriter) WriteString(s string) (int, error) {
	w.capHeader()
	return w.ResponseWriter.WriteString(s)
}

func (w *retryAfterCapWriter) Flush() {
	w.capHeader()
	w.ResponseWriter.Flush()
}

var _ http.Flusher = (*retryAfterCapWriter)(nil)
