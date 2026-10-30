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
```

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

The smoke script builds the checkout, runs it against a fake loopback upstream with fake keys, and asserts the 429/200 behaviour above through HTTP, including hot-reload of `hold` and `enabled: false`. It writes only under `build/reserve-gate-smoke/` and exits non-zero on failure.

## CI note

The upstream PR workflow `.github/workflows/pr-test-build.yml` runs only `go build`, not tests. Four `go test ./...` failures already exist on base b18b4e4e and upstream main 90c447d3, unrelated to the gate:

- TestStartOAuthV8ParallelSessionsWithDifferentProxiesOutOfOrderCallbacks
- TestClaudeExecutorManagerRetriesSilentTLSHandshakeThroughConnectProxy
- TestDevinApplyPatchRejectionCarriesLogDiagnostic
- TestResponsesSteeringFullDuplexIntegration
