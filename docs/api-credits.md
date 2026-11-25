# API credits row

A Claude Max plan comes with a monthly Console API credit grant ($200). The management panel shows each Max login's quota, but Anthropic's subscription usage endpoint carries no data about that grant. This fork adds it.

When the panel reads a Claude login's usage through `POST /v8/management/requests/api-call` (`GET https://api.anthropic.com/api/oauth/usage`, HTTP 200), the proxy adds one key, `API credits`, to the returned body:

```json
"API credits": {"limit_dollars": 200, "used_dollars": 97.95, "remaining_dollars": 102.05, "utilization": 48.975, "resets_at": "2026-12-09T00:00:00+00:00"}
```

The stock panel draws any such object as a dollar row: the key as label, "$102.05 remaining", "Spent $97.95 / Limit $200.00", and the reset time. So the panel needs no change. `resets_at` is the grant's expiry.

## Source

The numbers come from the state file of the credit-expiry job (linkendr tools, `mac/cliproxy-credit-expiry`), which writes it every minute:

- Path: `$HOME/.local/state/cliproxyapi/credit-expiry/state.json`; the environment variable `CLIPROXY_API_CREDITS` names another file.
- The grant whose `account` equals the login's email (case-insensitive) is used. It needs `amount_usd` above 0, `spent_usd`, `remaining_usd` and `expires`.

No row is added when the file is missing, unreadable, not JSON, has a `checked_at` more than 15 minutes from now, has no grant for the login, or the grant lacks a number. The upstream body is returned as it came. The deprecated `/v0/management/api-call` route is unchanged.

Code: `internal/api/handlers/management/api_credits_fork.go`, called once from `APICall`.
