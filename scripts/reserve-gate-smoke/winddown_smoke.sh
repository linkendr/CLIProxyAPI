#!/usr/bin/env bash
# Reserve wind-down smoke with fake OAuth Max logins (see winddown_smoke.py).
#
# Builds the checkout for Linux in the go.mod Go image (same flags as smoke.sh), then runs
# winddown_smoke.py with that binary in a second container started with --network none:
# loopback only, api.anthropic.com mapped to 127.0.0.1, a throwaway CA trusted through
# SSL_CERT_FILE, fake keys and a fresh folder. Never touches the live proxy, its config,
# credentials or port 8317.
#
# Usage: bash scripts/reserve-gate-smoke/winddown_smoke.sh <new output dir>
set -euo pipefail
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
WT=$(git -C "$HERE" rev-parse --show-toplevel)
OUT=${1:?usage: winddown_smoke.sh <new output dir>}
[[ ! -e $OUT ]] || { echo "output dir $OUT exists" >&2; exit 2; }
mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)
version=$(awk '$1 == "go" { print $2; exit }' "$WT/go.mod")
commit=$(git -C "$WT" rev-parse --short HEAD)
tag=$(git -C "$WT" describe --tags --abbrev=0 --match 'v[0-9]*' HEAD 2>/dev/null || true)
app_version=${tag#v}; [[ -n "$app_version" ]] || app_version=0.0.0
build_date=$(git -C "$WT" show -s --format=%cI HEAD)
echo "building linux binary with golang:${version} from $WT (head $(git -C "$WT" rev-parse HEAD), uncommitted changes included)"
docker run --rm \
    -v "$WT":/src:ro -v "$OUT":/out \
    -v cliproxyapi-test-gocache:/root/.cache/go-build -v cliproxyapi-test-gomod:/go/pkg/mod \
    -w /src -e GOFLAGS=-buildvcs=false -e GOTOOLCHAIN=local -e CGO_ENABLED=1 \
    "golang:${version}" go build \
    -ldflags="-s -w -X main.Version=${app_version} -X main.Commit=${commit} -X main.BuildDate=${build_date}" \
    -o /out/cli-proxy-api-linux ./cmd/server/
printf 'linux binary sha256 %s\n' "$(shasum -a 256 "$OUT/cli-proxy-api-linux" | cut -d' ' -f1)"
docker run --rm --network none -e WIND_DOWN_MINUTES="${WIND_DOWN_MINUTES:-1}" \
    -v "$OUT":/work -v "$HERE":/smoke:ro \
    "golang:${version}" sh -c 'echo "127.0.0.1 api.anthropic.com" >> /etc/hosts && exec python3 -I /smoke/winddown_smoke.py --binary /work/cli-proxy-api-linux --run-dir /work/run --wind-down-minutes ${WIND_DOWN_MINUTES:-1}'
