# Reserve gate

The reserve gate keeps the last share of the Claude Max logins' weekly quota for automations. Exempt API keys are never gated.

**The rule.** The Max logins keep one shared *reserve*, not one each. Its size follows the time left to the *next* weekly reset of any Max login (from the latest readings), because that reset brings fresh capacity:

| Time to the next reset of any Max login | Reserve | Meaning |
|---|---|---|
| more than `near-reset-hours` (6 h) | 1 - `threshold` (5%) | automations keep the last 5% of one login |
| `final-release-hours` (1 h) to `near-reset-hours` (6 h) | 1 - `near-reset-threshold` (1%) | automations keep the last 1% |
| `final-release-hours` (1 h) or less | 0 | nothing is kept; every caller may use what is left |
| no known reset | 1 - `threshold` (5%) | |

The reserve is placed on the logins that reset last (a login with no known reset counts as last): each takes up to its own headroom (1 - its 7-day utilization) until the reserve is placed. A login is *held* when all its headroom is reserve. So the login that resets first is spent first, and the logins that reset later keep the reserve until fresh capacity arrives. With one Max login this is a per-login limit of 95%, 99% and 100%. A `threshold` above `near-reset-threshold` wins in the near-reset window.

Example: the work login has 2% left and resets in 9 h; the general login has 4% left and resets in 26 h. The reserve is 5%: the general login holds 4% and the work login 1%, so ordinary callers may use 1% of the work login. From 6 h before the work login's reset the reserve is 1%, all on the general login, so ordinary callers may use the rest of the work login and 3% of the general login. In the last hour nothing is kept. Once the work login resets it is fresh and holds the reserve, so the general login's last 4% is free.

The reserve follows the clock: a held login is released at the step that frees it, without a new reading. A reading whose reset has passed counts as a fresh window (all headroom). A login cooling down after a real upstream 429 on the shared window has no headroom, whatever its reading says. A login with no reading yet takes no part and is not held.

- While at least one Max login is not held, that is while the logins together have more than the reserve left, nothing is blocked. Ordinary (non-exempt) callers may use any login, including a held one, up to its real upstream limit. The routing strategy picks as usual (pace spends the login that resets first).
- Only when every Max login is held (the logins have no more than the reserve left) does the gate act on ordinary callers. This is the *trigger*. While it is on:
  - **New prompt** (anything that is not a running task): refused, HTTP 429 with the reserve message and `Retry-After` set to the earliest release among the held logins (the next reserve step that frees one, or the next reset). Before it refuses, the proxy reads the real usage of the Max logins once (see [Quota reset recovery](#quota-reset-recovery)); the text and headers are under [Refusals](#refusals).
  - **Running task** (the request hands tool results back to the model): served for `wind-down-minutes` after the trigger turned on, in the same credential order as an exempt caller (Max logins up to their real limits, then credit keys once every Max login is at a real limit). The proxy appends one text block to the end of the last user message, after the tool results: "Quota reserve reached. Finish the current step, write a short status of what is done and what is left, and end your turn. Do not start new tool calls or subagents." The system prompt and every earlier byte stay as they were, so the prompt cache prefix is not broken.
  - **Running task after the wind-down**: refused like a new prompt.
- When the trigger turns off (a weekly reset, a login reads lower, or the reserve steps down), everything is served again, the note stops and the wind-down clock is cleared, so the next trigger starts a fresh wind-down.

"Max login" means a Claude subscription (OAuth) login that is not disabled and has no `fallback-when`. A Claude API key (`claude-api-key`) is never a Max login, whatever its priority or `fallback-when`: the credit expiry drain moves an expiring credit key to `priority: 10` and removes its `fallback-when`, and such a key must not count as a login with room. While the trigger is on, a Claude credential that is neither a Max login nor a credit key with `fallback-when` (such a drained key) is blocked for new prompts like a held login, and serves running tasks during the wind-down (it is first in priority) with the note. With no Max login at all there is no reserve and nothing is refused. A disabled login never counts as having room. A login with no reading yet counts as having room. A login cooling down after a real upstream 429 on the shared window does not count as having room.

**Running task or new prompt.** The gate reads the request as the client sent it:

- Claude Messages: the last message is a user message with a `tool_result` block (text blocks after it do not matter). A client continues a turn after a tool call this way.
- OpenAI Chat Completions: the last message has role `tool` (or the legacy `function`).
- OpenAI Responses: the last `input` item is a `*_call_output` item (`function_call_output`, `custom_tool_call_output`, ...).
- Gemini: the last content has a `functionResponse` part.
- Any other format, an empty or unparsable body: a new prompt.

The note is added by the Claude executor, after translation, to the Claude Messages body sent upstream, so it applies to callers of every format on the Claude route. It is not added when a credit key with `fallback-when` serves the request: that key serves only once every Max login is at a real limit, and then it serves new prompts too. The Responses compaction path and `count_tokens` get no note.

**Wind-down clock.** The proxy keeps only the time the trigger was first seen on, in memory. It is checked on every ordinary caller's request and after every Claude response (exempt traffic included), and it is cleared when either sees the trigger off or the gate is disabled. A proxy restart, or a reload that disables and re-enables the gate, restarts the clock: running tasks then get a fresh wind-down. A config reload that keeps the gate on does not touch it. The manual `hold` does not stop the clock.

Paid credit keys (`fallback-when`) are not part of the reserve. They serve every caller under their own rule only: once every Max login is really at an upstream limit. A reserve hold is not such a limit. So when every Max login is held and only credit keys are left, ordinary callers' new prompts get the reserve 429, not the credit key. When a credit key answers Anthropic's "credit balance is too low", the proxy reports a credential-wide 402: the key cools down and the request moves on.

`hold: true` is a manual pause: every Claude credential, credit keys included, is refused to ordinary callers with 429 and no `Retry-After`, running tasks included (no wind-down). Codex (GPT) routes are unaffected. The block hot-reloads.

Under Home dispatch the control center owns the credential list, so the gate cannot see the pool: each login Home offers keeps its own reserve (a pool of one, so 95%, 99% or 100% by its own reset). A held login Home offers is passed over and Home is asked again. Ordinary callers are refused once Home has no unheld Max login left. Credit keys stay blocked there, since Home does not apply `fallback-when`. The trigger and the wind-down are not known under Home: running tasks are gated like new prompts there.

## Config

```yaml
routing:
  reserve-gate:
    enabled: true            # master switch (false: gate off, nobody refused)
    hold: false              # true: manually refuse all non-exempt Claude requests
    threshold: 0.95          # the shared reserve is 1 - threshold while the next reset of any Max login is far
    near-reset-hours: 6      # from this many hours before the next reset the reserve is 1 - near-reset-threshold (0-168; 0 off)
    near-reset-threshold: 0.99
    final-release-hours: 1   # from this many hours before the next reset no reserve is kept (0-168; 0 off; at most near-reset-hours)
    exempt-api-keys: [automation-key]
    wind-down-minutes: 15    # running tasks are served this long after the trigger; 0 refuses them at once (0-1440)
    state-file: ""           # optional; default <auth-dir>/reserve-gate.state
```

Any other key under `routing.reserve-gate` is rejected when the config loads, so a typo cannot silently change the reserve. That includes the removed keys `resume-below`, `release-before-reset`, `weekly-relax-before-reset`, `weekly-relax-threshold` and `weekly-final-release-before-reset`: delete them from the config before deploying this build.

Anthropic no longer reports the 5-hour window, so the gate reads only the 7-day window. The model-scoped Fable window (`7d_oi`) is not part of the reserve.

These are admission limits based on the latest reported utilization, not token budgets. Requests already in flight and delayed upstream readings can overshoot the remaining share.

## State across restarts

The gate keeps, per account and window (`7d`), the last reading it saw (utilization, reset, observed time) and whether the window is held. It writes them to the state file whenever a utilization, a reset or a hold changes, and reads the file when the gate starts. A restart therefore keeps every known reading: the pool is decided from them before any new response arrives.

- File: `state-file`, or `reserve-gate.state` in `auth-dir` when unset. It is not a `.json` file, so the credential loader ignores it. Written by atomic replace, mode 0600. It holds account IDs, numbers and times; no tokens or keys. With neither `auth-dir` nor `state-file` set the gate logs a warning and keeps state in memory only.
- Partial responses: a response whose headers lack one window's utilization does not erase that window's known reading. Only a newer reading of the same window replaces it.
- Reset: a reading whose reset time has passed counts as reset, so the window has all its headroom. Such readings are dropped when the file is read.
- No reading: a login with no reading at all (never seen, or seen only before the state file existed) is not held and counts as having room. Its first response fills the book.
- Missing or unreadable file: the gate starts empty and logs the path (`starting empty`); the proxy keeps running.
- Older files: `5h` entries and fields this build does not know are ignored when the file is read and dropped on the next write.
- `enabled: false`: the file is neither read nor written.

## Quota reset recovery

A cooled login gets no traffic until its recorded reset, and the reserve reads usage only from response headers. So when the owner resets a limit on Anthropic's side, the proxy would not learn it. Two things close that gap.

### Usage read

When no Max login can serve the caller, the proxy reads the real usage of every Max login before it gives up on them. This happens at two points only:

- before a 429 refusal: a reserve hold refusal (not the manual `hold`, which no reading lifts) or a cooldown refusal on a route that includes Claude;
- before a credit key with `fallback-when` serves the request.

It never happens on the normal path, where a Max login serves. The upstream's own 429, passed on in the request that hit the limit, is not such a point either: that answer is fresh. Under Home dispatch nothing is read.

- Request: `GET https://api.anthropic.com/api/oauth/usage?cedar_ember=1&skip_spend=1` with `Authorization: Bearer <the login's own access token>` and `anthropic-beta: oauth-2025-04-20`. The request leaves by the transport the host supplies for the login, if it supplies one; otherwise through the login's `proxy-url`, and if that is empty the global `proxy-url`. The body gives `seven_day.utilization` in percent (73.0 is 73%), `five_hour.utilization`, and `limits[]` with `kind` `weekly_all` and `weekly_scoped`, each with `resets_at`. The weekly value is `seven_day`; a body without it gives the weekly value in its `weekly_all` limit.
- Limits: at most one read per login in 60 seconds, whatever its outcome (a failed read counts). Requests that arrive while a read is in flight wait for it and share its result. A read ends after 3 seconds.
- Effect: the weekly value replaces the login's reserve reading (a value without a reset is a fresh window). The reading is dated at the start of the read, so a reading from a response that arrived during the read stays. If every limit that cools the login now has room, the proxy clears the cooldown and selects again, so the login serves the same request.
- Proof: a cooldown is lifted only on proof. A weekly all-model cooldown needs the weekly value below 100%. A 5-hour or model-scoped cooldown needs the weekly value below 100% and its own value in the body and below 100%. The limit of a cooldown is the one named by the headers of the 429 that recorded it; a later response of the login does not rename it. A cooldown whose 429 named no limit stays. So does a cooldown that began or changed after the read started: the report is older than it. The body can carry `five_hour: null`. A 5-hour cooldown then has no proven room and stays until its own reset.
- Lift: the lift is `ResetQuota`, the same reset as the management cooldown reset below. It clears every cooldown of the login, also one that has another cause than quota. The proxy checks the proof again in the same step as the reset, so a 429 that lands just before the reset keeps its cooldown.
- Failure: a failed, slow or unparsable read changes nothing. A value of the wrong type or a reset that is not an RFC 3339 time, in any window the proxy knows (`seven_day`, `five_hour`, a `weekly_all` or `weekly_scoped` limit) and used or not, makes the whole body unparsable. So does a limit whose `kind` is not a string. A weekly reset that is not in the future fails the read too: it is not a report of the running window. A `proxy-url` (the login's or the global one) that cannot be used fails the read before a request is sent. The refusal or the credit key fallback then goes ahead as before. The log line names the login by its auth index, never by token or email.

**Timeout exception.** `AGENTS.md` allows timeouts only during credential acquisition. The usage read is a separate control call made before a refusal or a fallback, not part of an upstream model request, and it must not delay the caller. It therefore carries a 3-second timeout; `AGENTS.md` lists it with the other exceptions.

### Cooldown reset

`ResetQuota` (the management cooldown reset: `routing/cooldown/reset`, or the older `reset-quota`) also drops the login's reserve reading and its stored limit signals. Both describe the window before the reset; a stale reading would keep the login held. The management reset alone is therefore enough. The login counts as having room until its next response or usage read fills the book again.

### Refusals

A 429 that the proxy itself sends for a Claude limit names the limit and the reset. The text is one sentence, then a countdown:

| Limit | Sentence |
|---|---|
| weekly all-model limit of the Max logins | `Weekly limit. Next reset Fri 20 Nov 17:00. A Fable reset does not lift it.` |
| weekly model-scoped limit | `Weekly model limit. Next reset Fri 20 Nov 17:00.` |
| 5-hour limit | `Five-hour limit. Next reset Sun 15 Nov 21:00.` |
| reserve hold | `Quota reserve hold: the last share of the weekly quota is kept for automations. Next release Fri 20 Nov 17:00. A Fable reset does not lift it.` |
| cooldown with no known limit | `Rate limit. Next retry Sun 15 Nov 21:00.` |

- The time is the proxy host's local time. For a real limit it is the end of the cooldown the proxy took from the upstream response (its reset header), of the login that serves again first. For a reserve hold it is the earliest release.
- After the sentence comes ` (reset after 119h 59m 30s)`, the time left, for a client that reads the reset time from the text.
- When every Max login is at a real limit, an ordinary caller's refusal names that real limit, not the reserve: the reserve is then not what refuses the caller. The error stays a reserve hold inside the proxy, so nothing is retried.
- The manual `hold` keeps its text and has no reset.
- Headers: `Retry-After` (seconds), `anthropic-ratelimit-unified-status: rejected`, `anthropic-ratelimit-unified-reset` (Unix seconds) and `anthropic-ratelimit-unified-representative-claim` (`seven_day` for the weekly limit and the reserve, `seven_day_overage_included` for a model-scoped limit, `five_hour`; absent when the limit is not known). Refusal headers reach the client whatever `passthrough-headers` says. A cooldown refusal with no cooled Max login behind it follows upstream: `Retry-After` always, the unified headers only with `passthrough-headers: true`.

Note: the text carries the reset for a client that reads it there, the headers for a client that reads headers. Which of them a client uses depends on the client and its version; the tests of this repository do not cover that.

### Credit key responses

Anthropic sends the unified limit headers only for a subscription login. A response a Claude API key served (a credit key) therefore carried none, and a client that meters the weekly limit from headers kept its last value. Such a response now carries the headers that describe the Max logins: `anthropic-ratelimit-unified-status: allowed`, `-representative-claim: seven_day`, `-reset`, `-7d-utilization` and `-7d-reset`, from the Max login with the most room (a login at a real limit counts as full). Headers that already carry a unified status, responses of other credentials and a pool with no known reset stay as they were.

- The status is always `allowed`: a served response must not read as a limit event, so the proxy never sends `rejected` on it.
- Clients receive response headers only with `passthrough-headers: true`.
- A client that ignores these headers is not fed by this. The quota state file is the source for tools on this host.

### Quota state file

The proxy writes what it knows to a small file for other tools on the host.

- Path: `$HOME/.local/state/cliproxyapi/quota-state.json`; `CLIPROXY_QUOTA_STATE` overrides it. Written by atomic replace (temp file, rename), mode 0600, folder 0700.
- When: after every Claude response, at every dead end, after a usage read and a cooldown reset, and from a 30-second ticker; the file is rewritten when its content changed or it is 4 minutes old, so it is never 5 minutes old while the proxy runs. A reader treats a missing, unparsable or older-than-15-minutes file as unknown.
- With no Max login nothing is written. A test binary writes no file unless `CLIPROXY_QUOTA_STATE` is set. Without a home directory and without the variable nothing is written either; the log says so once.
- It holds no token, key, email or account name. A login is named by its auth index (a hash).

```json
{
  "schema": "cliproxy.quota_state.v1",
  "written_at": "2026-11-15T10:00:31Z",
  "interactive": {"route": "max", "since": "2026-11-15T10:51:42Z", "limit": "", "retry_at": null, "wind_down_until": null, "message": ""},
  "automation": {"route": "max", "since": "2026-11-15T10:51:42Z", "limit": "", "retry_at": null, "wind_down_until": null, "message": ""},
  "reserve": {"active": false, "size": 0.05, "on": ["8c6f5a61f61a5ab4"]},
  "logins": [
    {"auth_index": "8c6f5a61f61a5ab4", "state": "ok", "limit": "", "utilization": 0.01,
     "resets_at": "2026-11-20T16:00:00Z", "read_at": "2026-11-15T10:51:52Z", "read_from": "headers"}
  ]
}
```

- `interactive` is a caller whose key is not on `exempt-api-keys`; `automation` is an exempt caller. Each says what a new prompt gets now.
- `route`: `max` (a Max login serves), `credit` (a credit key serves) or `refused` (HTTP 429). `since`: when the route last changed, as far as this process saw.
- `limit`: `weekly_all`, `weekly_scoped`, `five_hour`, `reserve` or empty. `retry_at`: when the route is expected to return to `max`.
- `wind_down_until`: set while running tasks are still served after new prompts are refused.
- `message`: the refusal's sentence without the countdown; empty when the route is `max`.
- `reserve`: `active` is the trigger (every Max login is held), `size` the reserve as a share of one login's weekly quota (0 with the gate off), `on` the logins that carry it.
- `logins[].state`: `ok`, `held` (all its headroom is reserve) or `cooled` (a live cooldown after a real limit; `limit` names it, a model-scoped limit included). `utilization` and `resets_at` are the newest weekly reading (null without one, or once its reset has passed); `read_from` is `headers` or `usage`.

## Safe enable order

1. Deploy the binary with no `routing.reserve-gate` block (gate off).
2. Add `exempt-api-keys` for the automation clients.
3. Set `enabled: true`.
4. Try `hold: true` briefly, confirm a non-exempt key gets 429 and an exempt key 200, then set it back to `false`.

## How to check

```sh
go test ./internal/config ./sdk/cliproxy/auth ./sdk/api/handlers ./sdk/cliproxy -run 'ReserveGate|ReserveWindDown|ReserveRequest|ReserveTrigger' -count=1
go test ./internal/runtime/executor -run ReserveWindDown -count=1
go test ./sdk/cliproxy/auth ./sdk/api/handlers -run 'Quota|Refusal|PoolHeaders' -count=1
bash scripts/reserve-gate-smoke/smoke.sh
bash scripts/reserve-gate-smoke/winddown_smoke.sh <new dir>
```

The quota tests use a controllable clock and fake logins. A test binary never calls the real usage endpoint: without a test URL the read fails by design.

Max logins are subscription (OAuth) logins, and an OAuth login always talks to `https://api.anthropic.com`. `winddown_smoke.sh` therefore builds the checkout for Linux and runs it in a `golang` container with no network (`--network none`): `api.anthropic.com` resolves to the container's loopback, where `winddown_smoke.py` serves a fake HTTPS upstream with a certificate from a throwaway CA the proxy trusts through `SSL_CERT_FILE`. Two fake OAuth login files and a fake credit key (`fallback-when: limit`) stand in. Scenarios, each on a fresh proxy: both logins held (new prompt 429, running task served by a login with the note at the end of the last user message, exempt served, OpenAI-format running task served with the note); after the wind-down (`wind-down-minutes: 1`) running task 429, exempt served; both at a real limit (running task served by the credit key, no note); a drained credit key at priority 10 without `fallback-when` plus both held (new prompt 429; running task served by the drained key with the note); one login with room (new prompt and running task served, no note); restart (the held readings come back from the state file); the shared reserve (one login spent while the later one holds the reserve, the pool at the reserve with `Retry-After` at the 1% step, the near-reset and final-hour steps, explicit keys); the usage read before the credit key (both logins at a real limit: a failed read, HTTP 500, changes nothing; no second read within 60 seconds; after 60 seconds the fake upstream reports one login as reset, its cooldown is lifted and it serves with no manual step; no read on the normal path); the usage read before a refusal (no credit key: the 429 names the weekly limit and its reset in text and headers, the quota state file carries the same sentence, and the reset login serves after the next read). The fake upstream serves the usage report at `GET /api/oauth/usage` and counts the reads per login. The two usage scenarios each wait about one minute. Every 429 a scenario receives is kept in `refusals.jsonl` in its run folder.

`smoke.sh` runs on the host with fake API keys only. Since API keys are not Max logins, it checks that a `claude-api-key` at 0.97 is never reserve-held, plus the manual hold, hot reload, the Codex route and the state file across a restart.

The smoke script builds the checkout, runs it against a fake loopback upstream with fake keys, and asserts the 429/200 behaviour above through HTTP, including hot-reload of `hold` and `enabled: false`, and a restart that keeps the known hold from the state file. It writes only under `build/reserve-gate-smoke/` and exits non-zero on failure.

## CI note

The upstream PR workflow `.github/workflows/pr-test-build.yml` runs only `go build`, not tests. Four `go test ./...` failures already exist on base b18b4e4e and upstream main 90c447d3, unrelated to the gate:

- TestStartOAuthV8ParallelSessionsWithDifferentProxiesOutOfOrderCallbacks
- TestClaudeExecutorManagerRetriesSilentTLSHandshakeThroughConnectProxy
- TestDevinApplyPatchRejectionCarriesLogDiagnostic
- TestResponsesSteeringFullDuplexIntegration

Run the full suite with `bash scripts/test-in-container.sh`. It uses the Go version in `go.mod` inside a Linux container. On a developer Mac, local port scanners probe the tests' loopback servers and break four network tests, and Go 1.27 changes JSON error wording that one test checks.
