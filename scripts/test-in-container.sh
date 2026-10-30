#!/bin/bash
# Runs the full Go test suite in a Linux container on the go.mod Go version.
# On a developer Mac, local port scanners (T3 Code's preview scanner, Hermes
# tailscale-autoserve) probe the tests' short-lived loopback servers and make
# network tests fail; the container isolates them. Go caches live in Docker
# volumes so source-scanning tests never see third-party code.
set -euo pipefail
root=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)
version=$(awk '$1 == "go" { print $2; exit }' "$root/go.mod")
exec docker run --rm \
    -v "$root":/src \
    -v cliproxyapi-test-gocache:/root/.cache/go-build \
    -v cliproxyapi-test-gomod:/go/pkg/mod \
    -w /src -e GOFLAGS=-buildvcs=false -e GOTOOLCHAIN=local \
    "golang:${version}" go test ./... "$@"
