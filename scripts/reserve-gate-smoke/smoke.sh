#!/usr/bin/env bash
# End-to-end HTTP smoke test of routing.reserve-gate in the CLIProxyAPI fork.
#
# Builds the head of the reserve-gate worktree the way
# ~/tools/mac/github-fork-sync/sync-cliproxyapi.sh builds, starts it on spare
# loopback ports with a throwaway config, fake client keys and a fake loopback
# upstream (no real credentials, never the real config, auth dir or port 8317),
# and asserts through real HTTP:
#   (a) after an upstream 5h reading of 0.97: non-exempt 429 with the reserve
#       message and Retry-After; exempt 200
#   (b) hot-reloaded hold: true: non-exempt 429 without Retry-After; exempt 200
#   (d) while held, a Codex (GPT) route is unaffected for the non-exempt key
#   (c) hot-reloaded enabled: false: both keys 200
# Prints PASS/FAIL per assertion and exits non-zero on any failure.
#
# All writes (binary, Go build cache, config, logs, hits) go under
# <repo>/build/reserve-gate-smoke/. Network: loopback only (the binary may attempt
# its own startup version check; that is outside this script's control).
#
# Usage: bash scripts/reserve-gate-smoke/smoke.sh   (run from anywhere in the repo)
set -uo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
WT=$(git -C "$HERE" rev-parse --show-toplevel)
RUN=$WT/build/reserve-gate-smoke
FAKE=$HERE/fake_upstream.py
export GOCACHE=$RUN/gocache
PY=$(command -v python3)
USER_KEY=smoke-user-key-not-secret
AUTO_KEY=smoke-auto-key-not-secret

failures=0
pass() { printf 'PASS %s\n' "$1"; }
fail() { printf 'FAIL %s\n' "$1"; failures=$((failures + 1)); }

pids=()
cleanup() {
    for pid in ${pids[@]+"${pids[@]}"}; do kill "$pid" 2>/dev/null; done
    for pid in ${pids[@]+"${pids[@]}"}; do wait "$pid" 2>/dev/null; done
}
trap cleanup EXIT

free_port() { "$PY" -I -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'; }

# --- build (same flags as sync-cliproxyapi.sh) ---------------------------------
head_sha=$(git -C "$WT" rev-parse HEAD)
dirty=$(git -C "$WT" status --short --untracked-files=no | wc -l | tr -d ' ')
commit=$(git -C "$WT" rev-parse --short HEAD)
tag=$(git -C "$WT" describe --tags --abbrev=0 --match 'v[0-9]*' HEAD 2>/dev/null || true)
version=${tag#v}; [[ -n "$version" ]] || version=0.0.0
build_date=$(git -C "$WT" show -s --format=%cI HEAD)
printf 'head %s (tracked changes: %s)\n' "$head_sha" "$dirty"
rm -rf "$RUN"; mkdir -p "$RUN/home" "$RUN/auth" "$RUN/bin" "$GOCACHE"
BIN=$RUN/bin/cli-proxy-api-${version}-${commit}
(cd "$WT" && CGO_ENABLED=1 go build -buildvcs=false \
    -ldflags="-s -w -X main.Version=${version} -X main.Commit=${commit} -X main.BuildDate=${build_date}" \
    -o "$BIN" ./cmd/server/) || { echo "FAIL build"; exit 1; }
printf 'binary %s sha256 %s\n' "$BIN" "$(shasum -a 256 "$BIN" | cut -d' ' -f1)"

PROXY_PORT=$(free_port); UP_PORT=$(free_port)
[[ "$PROXY_PORT" != 8317 && "$UP_PORT" != 8317 ]] || { echo "FAIL port choice"; exit 1; }
printf 'proxy 127.0.0.1:%s, fake upstream 127.0.0.1:%s\n' "$PROXY_PORT" "$UP_PORT"

write_config() { # $1 = reserve-gate body lines (indented 4)
    cat > "$RUN/config.yaml" <<EOF
config-version: 8
server:
  host: "127.0.0.1"
  port: ${PROXY_PORT}
access:
  api-keys: [${USER_KEY}, ${AUTO_KEY}]
oauth:
  auth-dir: "${RUN}/auth"
remote-management:
  disable-control-panel: true
  disable-auto-update-panel: true
routing:
  reserve-gate:
$1
    exempt-api-keys: [${AUTO_KEY}]
api-keys:
  claude:
    - name: smoke-fake-claude
      base-url: "http://127.0.0.1:${UP_PORT}"
      keys:
        - api-key: "sk-smoke-fake-upstream"
      models:
        - name: "claude-smoke"
  codex:
    - name: smoke-fake-codex
      base-url: "http://127.0.0.1:${UP_PORT}/codex"
      keys:
        - api-key: "sk-smoke-fake-codex"
      models:
        - name: "gpt-smoke"
EOF
}

reload_count() { grep -c 'config successfully reloaded' "$RUN/proxy.log" 2>/dev/null || true; }
reload_config() { # $1 = reserve-gate body lines; waits for the watcher to apply it
    # A write can race the watcher's own startup bookkeeping, so an unobserved write
    # is repeated with a comment that changes the file hash (at most 3 writes).
    local before attempt
    before=$(reload_count)
    for attempt in 1 2 3; do
        write_config "$1"
        printf '# reload attempt %s\n' "$attempt" >> "$RUN/config.yaml"
        for _ in $(seq 1 40); do
            if [[ $(reload_count) -gt $before ]]; then
                printf 'hot reload applied (write %s)\n' "$attempt"
                sleep 1; return 0
            fi
            sleep 0.25
        done
    done
    echo "FAIL hot reload not observed"; failures=$((failures + 1)); return 1
}

# req NAME KEY PATH BODY -> sets STATUS, files $RUN/NAME.{headers,body}
req() {
    STATUS=$(curl -sS -o "$RUN/$1.body" -D "$RUN/$1.headers" -w '%{http_code}' --max-time 30 \
        -H "Authorization: Bearer $2" -H 'content-type: application/json' -H 'anthropic-version: 2023-06-01' \
        -X POST "http://127.0.0.1:${PROXY_PORT}$3" --data "$4")
}
claude() { req "$1" "$2" /v1/messages '{"model":"claude-smoke","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}'; }
codex() { req "$1" "$2" /v1/responses '{"model":"gpt-smoke","input":"hi"}'; }
header() { grep -i "^$2:" "$RUN/$1.headers" | head -1 | cut -d' ' -f2- | tr -d '\r'; }

# --- start ---------------------------------------------------------------------
"$PY" -I "$FAKE" "$UP_PORT" 0.97 "$RUN/upstream.hits" > "$RUN/upstream.log" 2>&1 &
pids+=($!)
write_config "    enabled: true"
HOME=$RUN/home "$BIN" -config "$RUN/config.yaml" > "$RUN/proxy.log" 2>&1 &
pids+=($!)
for _ in $(seq 1 80); do
    curl -s -o /dev/null "http://127.0.0.1:${PROXY_PORT}/" && break
    sleep 0.25
done
for _ in $(seq 1 80); do
    grep -q 'file watcher started' "$RUN/proxy.log" && break
    sleep 0.25
done
sleep 1
grep -m1 'Version' "$RUN/proxy.log"

# --- (a) reading of 0.97 ---------------------------------------------------------
claude a0 "$AUTO_KEY"   # exempt request records the upstream 0.97 reading
[[ $STATUS == 200 ]] && pass "a0 exempt first request 200 (records 0.97 reading)" || fail "a0 exempt first request: $STATUS $(cat "$RUN/a0.body")"
claude a1 "$USER_KEY"
ra=$(header a1 retry-after)
if [[ $STATUS == 429 ]] && grep -q 'quota reserve hold' "$RUN/a1.body" && grep -q '"rate_limit_error"' "$RUN/a1.body" && [[ -n $ra ]]; then
    pass "a1 non-exempt 429, reserve message, Retry-After=$ra"
else
    fail "a1 non-exempt: status=$STATUS retry-after='$ra' body=$(cat "$RUN/a1.body")"
fi
claude a2 "$AUTO_KEY"
[[ $STATUS == 200 ]] && pass "a2 exempt 200" || fail "a2 exempt: $STATUS $(cat "$RUN/a2.body")"

# --- (b) hot-reloaded manual hold ------------------------------------------------
reload_config "    enabled: true
    hold: true"
claude b1 "$USER_KEY"
rb=$(header b1 retry-after)
if [[ $STATUS == 429 ]] && grep -q 'paused by the operator' "$RUN/b1.body" && [[ -z $rb ]]; then
    pass "b1 non-exempt 429 manual hold, no Retry-After"
else
    fail "b1 non-exempt: status=$STATUS retry-after='$rb' body=$(cat "$RUN/b1.body")"
fi
claude b2 "$AUTO_KEY"
[[ $STATUS == 200 ]] && pass "b2 exempt 200" || fail "b2 exempt: $STATUS $(cat "$RUN/b2.body")"

# --- (d) Codex route while held -----------------------------------------------------
hits_before=$(grep -c '^/codex/responses' "$RUN/upstream.hits" 2>/dev/null || true)
codex d1 "$USER_KEY"
hits_after=$(grep -c '^/codex/responses' "$RUN/upstream.hits" 2>/dev/null || true)
if [[ $STATUS == 200 && $hits_after -gt $hits_before ]]; then
    pass "d1 non-exempt Codex route 200 during hold (upstream /codex/responses hit)"
else
    fail "d1 non-exempt Codex: status=$STATUS hits $hits_before->$hits_after body=$(head -c 400 "$RUN/d1.body")"
fi

# --- (c) gate disabled ---------------------------------------------------------------
reload_config "    enabled: false"
claude c1 "$USER_KEY"
[[ $STATUS == 200 ]] && pass "c1 non-exempt 200 with enabled: false" || fail "c1 non-exempt: $STATUS $(cat "$RUN/c1.body")"
claude c2 "$AUTO_KEY"
[[ $STATUS == 200 ]] && pass "c2 exempt 200 with enabled: false" || fail "c2 exempt: $STATUS $(cat "$RUN/c2.body")"

# --- hygiene ----------------------------------------------------------------------------
if grep -q -e "$USER_KEY" -e "$AUTO_KEY" "$RUN/proxy.log"; then
    fail "proxy log contains a client key"
else
    pass "proxy log has no client keys"
fi
# Exempt a0, a2, b2 and c1, c2 reach the upstream; the refused a1 and b1 must not.
claude_hits=$(grep -c '^/v1/messages' "$RUN/upstream.hits" 2>/dev/null || true)
[[ $claude_hits == 5 ]] && pass "refused requests never reached the upstream (5 Claude upstream hits)" || fail "Claude upstream hits = $claude_hits, want 5"
grep 'reserve gate:' "$RUN/proxy.log" | sed 's/^/log: /'
printf 'upstream hits: %s\n' "$(sort "$RUN/upstream.hits" | uniq -c | tr -s ' ' | tr '\n' ';')"

cleanup; pids=()
printf 'head %s: %s\n' "$head_sha" "$([[ $failures == 0 ]] && echo 'ALL PASS' || echo "$failures FAILED")"
[[ $failures == 0 ]]
