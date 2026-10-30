# Reserve gate

The reserve gate keeps a share of Claude subscription quota free for automations. When the upstream 5h usage reading reaches `threshold`, requests from API keys that are not exempt get HTTP 429 (reserve message plus `Retry-After`) until the reading drops below `resume-below`. Exempt keys are never gated. `hold: true` is a manual pause: non-exempt requests get 429 without `Retry-After`. Codex (GPT) routes are unaffected. The block hot-reloads.

## Config

```yaml
routing:
  reserve-gate:
    enabled: true            # master switch (false: gate off, nobody refused)
    hold: false              # true: manually refuse all non-exempt Claude requests
    threshold: 0.95          # start refusing at this 5h usage fraction
    resume-below: 0.90       # stop refusing once usage is below this
    exempt-api-keys: [automation-key]
    state-file: ""           # optional; default <auth-dir>/reserve-gate.state
```

## State across restarts

The gate keeps, per account and window (`5h`, `7d`), the last reading it saw (utilization, reset, observed time) and whether the window is held. It writes them to the state file whenever a utilization, a reset or a hold changes, and reads the file when the gate starts. A restart therefore keeps every known hold: an account last seen at 0.97 stays held for non-exempt callers before any new response arrives.

- File: `state-file`, or `reserve-gate.state` in `auth-dir` when unset. It is not a `.json` file, so the credential loader ignores it. Written by atomic replace, mode 0600. It holds account IDs, numbers and times; no tokens or keys. With neither `auth-dir` nor `state-file` set the gate logs a warning and keeps state in memory only.
- Partial responses: a response whose headers lack one window's utilization does not erase that window's known reading. Only a newer reading of the same window replaces it.
- Reset: a reading whose reset time has passed counts as reset, so the window does not hold. Such readings are dropped when the file is read.
- No reading: an account with no reading at all (never seen, or seen only before the state file existed) is eligible. Its first response fills the book. This keeps a new account usable; the cost is that requests admitted before that first response are not gated.
- Missing or unreadable file: the gate starts empty and logs the path (`starting empty`); the proxy keeps running.
- `enabled: false`: the file is neither read nor written.

## Safe enable order

1. Deploy the binary with no `routing.reserve-gate` block (gate off).
2. Add `exempt-api-keys` for the automation clients.
3. Set `enabled: true`.
4. Try `hold: true` briefly, confirm a non-exempt key gets 429 and an exempt key 200, then set it back to `false`.

## How to check

```sh
go test ./internal/config ./sdk/cliproxy/auth ./sdk/api/handlers ./sdk/cliproxy -run ReserveGate -count=1
bash scripts/reserve-gate-smoke/smoke.sh
```

The smoke script builds the checkout, runs it against a fake loopback upstream with fake keys, and asserts the 429/200 behaviour above through HTTP, including hot-reload of `hold` and `enabled: false`, and a restart that keeps the known hold from the state file. It writes only under `build/reserve-gate-smoke/` and exits non-zero on failure.

## CI note

The upstream PR workflow `.github/workflows/pr-test-build.yml` runs only `go build`, not tests. Four `go test ./...` failures already exist on base b18b4e4e and upstream main 90c447d3, unrelated to the gate:

- TestStartOAuthV8ParallelSessionsWithDifferentProxiesOutOfOrderCallbacks
- TestClaudeExecutorManagerRetriesSilentTLSHandshakeThroughConnectProxy
- TestDevinApplyPatchRejectionCarriesLogDiagnostic
- TestResponsesSteeringFullDuplexIntegration

Run the full suite with `bash scripts/test-in-container.sh`. It uses the Go version in `go.mod` inside a Linux container. On a developer Mac, local port scanners probe the tests' loopback servers and break four network tests, and Go 1.27 changes JSON error wording that one test checks.
