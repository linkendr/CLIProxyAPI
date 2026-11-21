# Client Retry-After cap

OpenCode retries a 429 up to 5 times and sleeps for exactly the `Retry-After` value each time. When a
Codex login hits its weekly limit, the proxy sends `Retry-After` of about 180000 seconds (50 hours).
The worker then shows "running" with no output for days.

## Config

```yaml
routing:
  cooldown:
    max-client-retry-after-seconds: 120   # 0 (default) disables the cap
```

The key is hot-reloadable.

## Scope

- Capped: the `Retry-After` header on `/v1/chat/completions`, `/v1/completions`, `/v1/responses`
  (POST, streaming error path, compact), the `headers.Retry-After` field of the Responses websocket
  error event, `/v1/alpha/search`, and the image, video and audio routes under `/v1`.
- Not capped: `/v1/messages`, `/v1/messages/count_tokens` and the Claude Code handlers. Claude Code
  relies on the real `Retry-After` and the `anthropic-ratelimit-*` headers.
- The JSON error body keeps the real `reset_seconds` and `reset_time`, so the client error text shows
  the true reset.
- Only whole-second values are capped. An HTTP-date value is passed through unchanged.

The cap sits in `ClientRetryAfterCapMiddleware` (`sdk/api/handlers/retry_after_cap.go`), which wraps the
response writer of each guarded route, so every code path that writes the header is covered.
