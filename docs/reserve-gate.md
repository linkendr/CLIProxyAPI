# Reserve gate

The reserve gate keeps the last share of the Claude Max logins' weekly quota for automations. Exempt API keys are never gated.

**The rule.** A Max login is *held* once its latest 7-day usage reading reaches its *limit*. The limit follows the time left to that login's own weekly reset (from its latest reading):

| Time to the login's reset | Limit | Meaning |
|---|---|---|
| more than `near-reset-hours` (6 h) | `threshold` (95%) | the last 5% is kept for automations |
| `final-release-hours` (1 h) to `near-reset-hours` (6 h) | `near-reset-threshold` (99%) | automations keep the last 1% |
| `final-release-hours` (1 h) or less | 100% | nothing is kept; every caller may use what is left |
| no known reset | `threshold` (95%) | |

The limit follows the clock: a held login is released at the step without a new reading. A `threshold` above `near-reset-threshold` wins in the near-reset window. A reading whose reset has passed never holds. The reserve is shared across logins:

- While at least one Max login is not held, nothing is blocked. Ordinary (non-exempt) callers may use any login, including a held one, up to its real upstream limit. The routing strategy picks as usual (pace spends the login that resets first).
- Only when every Max login is held (or already at a real upstream limit) does the gate act on ordinary callers. This is the *trigger*. While it is on:
  - **New prompt** (anything that is not a running task): refused, HTTP 429 with the reserve message and `Retry-After` set to the earliest release among the held logins (the next limit step above its reading, or its reset).
  - **Running task** (the request hands tool results back to the model): served for `wind-down-minutes` after the trigger turned on, in the same credential order as an exempt caller (Max logins up to their real limits, then credit keys once every Max login is at a real limit). The proxy appends one text block to the end of the last user message, after the tool results: "Quota reserve reached. Finish the current step, write a short status of what is done and what is left, and end your turn. Do not start new tool calls or subagents." The system prompt and every earlier byte stay as they were, so the prompt cache prefix is not broken.
  - **Running task after the wind-down**: refused like a new prompt.
- When the trigger turns off (a weekly reset, a login reads below its limit, or a login's limit steps up past its reading), everything is served again, the note stops and the wind-down clock is cleared, so the next trigger starts a fresh wind-down.

"Max login" means a Claude subscription (OAuth) login that is not disabled and has no `fallback-when`. A Claude API key (`claude-api-key`) is never a Max login, whatever its priority or `fallback-when`: the credit expiry drain moves an expiring credit key to `priority: 10` and removes its `fallback-when`, and such a key must not count as a login with room. While the trigger is on, a Claude credential that is neither a Max login nor a credit key with `fallback-when` (such a drained key) is blocked for new prompts like a held login, and serves running tasks during the wind-down (it is first in priority) with the note. With no Max login at all there is no reserve and nothing is refused. A disabled login never counts as having room. A login with no reading yet counts as having room. A login cooling down after a real upstream 429 on the shared window does not count as having room.

**Running task or new prompt.** The gate reads the request as the client sent it:

- Claude Messages: the last message is a user message with a `tool_result` block (text blocks after it, such as Claude Code's reminders, do not matter). This is how Claude Code and the Agent SDK continue a turn.
- OpenAI Chat Completions: the last message has role `tool` (or the legacy `function`).
- OpenAI Responses: the last `input` item is a `*_call_output` item (`function_call_output`, `custom_tool_call_output`, ...).
- Gemini: the last content has a `functionResponse` part.
- Any other format, an empty or unparsable body: a new prompt.

The note is added by the Claude executor, after translation, to the Claude Messages body sent upstream, so it applies to callers of every format on the Claude route. It is not added when a credit key with `fallback-when` serves the request: that key serves only once every Max login is at a real limit, and then it serves new prompts too. The Responses compaction path and `count_tokens` get no note.

**Wind-down clock.** The proxy keeps only the time the trigger was first seen on, in memory. It is checked on every ordinary caller's request and after every Claude response (exempt traffic included), and it is cleared when either sees the trigger off or the gate is disabled. A proxy restart, or a reload that disables and re-enables the gate, restarts the clock: running tasks then get a fresh wind-down. A config reload that keeps the gate on does not touch it. The manual `hold` does not stop the clock.

Paid credit keys (`fallback-when`) are not part of the reserve. They serve every caller under their own rule only: once every Max login is really at an upstream limit. A reserve hold is not such a limit. So when every Max login is held and only credit keys are left, ordinary callers' new prompts get the reserve 429, not the credit key. When a credit key answers Anthropic's "credit balance is too low", the proxy reports a credential-wide 402: the key cools down and the request moves on.

`hold: true` is a manual pause: every Claude credential, credit keys included, is refused to ordinary callers with 429 and no `Retry-After`, running tasks included (no wind-down). Codex (GPT) routes are unaffected. The block hot-reloads.

Under Home dispatch the control center owns the credential list, so the gate applies to each pick: a held login Home offers is passed over and Home is asked again. Ordinary callers are refused once Home has no unheld Max login left. Credit keys stay blocked there, since Home does not apply `fallback-when`. The trigger and the wind-down are not known under Home: running tasks are gated like new prompts there.

## Config

```yaml
routing:
  reserve-gate:
    enabled: true            # master switch (false: gate off, nobody refused)
    hold: false              # true: manually refuse all non-exempt Claude requests
    threshold: 0.95          # a Max login is held at this 7d usage fraction while its reset is far
    near-reset-hours: 6      # from this many hours before a login's reset its limit is near-reset-threshold (0-168; 0 off)
    near-reset-threshold: 0.99
    final-release-hours: 1   # from this many hours before a login's reset its limit is 100% (0-168; 0 off; at most near-reset-hours)
    exempt-api-keys: [automation-key]
    wind-down-minutes: 15    # running tasks are served this long after the trigger; 0 refuses them at once (0-1440)
    state-file: ""           # optional; default <auth-dir>/reserve-gate.state
```

Any other key under `routing.reserve-gate` is rejected when the config loads, so a typo cannot silently change the reserve. That includes the removed keys `resume-below`, `release-before-reset`, `weekly-relax-before-reset`, `weekly-relax-threshold` and `weekly-final-release-before-reset`: delete them from the config before deploying this build.

Anthropic no longer reports the 5-hour window, so the gate reads only the 7-day window. The model-scoped Fable window (`7d_oi`) is not part of the reserve.

These are admission limits based on the latest reported utilization, not token budgets. Requests already in flight and delayed upstream readings can overshoot the remaining share.

## State across restarts

The gate keeps, per account and window (`7d`), the last reading it saw (utilization, reset, observed time) and whether the window is held. It writes them to the state file whenever a utilization, a reset or a hold changes, and reads the file when the gate starts. A restart therefore keeps every known hold: a login last seen at 0.97 counts as held before any new response arrives.

- File: `state-file`, or `reserve-gate.state` in `auth-dir` when unset. It is not a `.json` file, so the credential loader ignores it. Written by atomic replace, mode 0600. It holds account IDs, numbers and times; no tokens or keys. With neither `auth-dir` nor `state-file` set the gate logs a warning and keeps state in memory only.
- Partial responses: a response whose headers lack one window's utilization does not erase that window's known reading. Only a newer reading of the same window replaces it.
- Reset: a reading whose reset time has passed counts as reset, so the window does not hold. Such readings are dropped when the file is read.
- No reading: a login with no reading at all (never seen, or seen only before the state file existed) is not held and counts as having room. Its first response fills the book.
- Missing or unreadable file: the gate starts empty and logs the path (`starting empty`); the proxy keeps running.
- Older files: `5h` entries and fields this build does not know are ignored when the file is read and dropped on the next write.
- `enabled: false`: the file is neither read nor written.

## Safe enable order

1. Deploy the binary with no `routing.reserve-gate` block (gate off).
2. Add `exempt-api-keys` for the automation clients.
3. Set `enabled: true`.
4. Try `hold: true` briefly, confirm a non-exempt key gets 429 and an exempt key 200, then set it back to `false`.

## How to check

```sh
go test ./internal/config ./sdk/cliproxy/auth ./sdk/api/handlers ./sdk/cliproxy -run 'ReserveGate|ReserveWindDown|ReserveRequest|ReserveTrigger' -count=1
go test ./internal/runtime/executor -run ReserveWindDown -count=1
bash scripts/reserve-gate-smoke/smoke.sh
bash scripts/reserve-gate-smoke/winddown_smoke.sh <new dir>
```

Max logins are subscription (OAuth) logins, and an OAuth login always talks to `https://api.anthropic.com`. `winddown_smoke.sh` therefore builds the checkout for Linux and runs it in a `golang` container with no network (`--network none`): `api.anthropic.com` resolves to the container's loopback, where `winddown_smoke.py` serves a fake HTTPS upstream with a certificate from a throwaway CA the proxy trusts through `SSL_CERT_FILE`. Two fake OAuth login files and a fake credit key (`fallback-when: limit`) stand in. Scenarios, each on a fresh proxy: both logins held (new prompt 429, running task served by a login with the note at the end of the last user message, exempt served, OpenAI-format running task served with the note); after the wind-down (`wind-down-minutes: 1`) running task 429, exempt served; both at a real limit (running task served by the credit key, no note); a drained credit key at priority 10 without `fallback-when` plus both held (new prompt 429; running task served by the drained key with the note); one login with room (new prompt and running task served, no note); restart (the held readings come back from the state file).

`smoke.sh` runs on the host with fake API keys only. Since API keys are not Max logins, it checks that a `claude-api-key` at 0.97 is never reserve-held, plus the manual hold, hot reload, the Codex route and the state file across a restart.

The smoke script builds the checkout, runs it against a fake loopback upstream with fake keys, and asserts the 429/200 behaviour above through HTTP, including hot-reload of `hold` and `enabled: false`, and a restart that keeps the known hold from the state file. It writes only under `build/reserve-gate-smoke/` and exits non-zero on failure.

## CI note

The upstream PR workflow `.github/workflows/pr-test-build.yml` runs only `go build`, not tests. Four `go test ./...` failures already exist on base b18b4e4e and upstream main 90c447d3, unrelated to the gate:

- TestStartOAuthV8ParallelSessionsWithDifferentProxiesOutOfOrderCallbacks
- TestClaudeExecutorManagerRetriesSilentTLSHandshakeThroughConnectProxy
- TestDevinApplyPatchRejectionCarriesLogDiagnostic
- TestResponsesSteeringFullDuplexIntegration

Run the full suite with `bash scripts/test-in-container.sh`. It uses the Go version in `go.mod` inside a Linux container. On a developer Mac, local port scanners probe the tests' loopback servers and break four network tests, and Go 1.27 changes JSON error wording that one test checks.
