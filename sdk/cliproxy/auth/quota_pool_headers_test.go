package auth

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const unifiedPrefix = "Anthropic-Ratelimit-Unified-"

func (r *reserveRig) auth(id string) *Auth {
	r.manager.mu.RLock()
	defer r.manager.mu.RUnlock()
	return r.manager.auths[id].Clone()
}

// A response a credit key served carries the unified limit headers of the Max pool,
// always with the status "allowed". A response of a Max login is passed as it is.
func TestCreditKeyResponseCarriesMaxPoolHeaders(t *testing.T) {
	for name, selector := range reserveSelectors() {
		t.Run(name, func(t *testing.T) {
			rig := newReserveRig(t, selector(), true, reserveGateOn(nil))
			ctx := context.Background()
			req := cliproxyexecutor.Request{Model: reserveTestClaudeModel}
			early := time.Now().Add(48 * time.Hour).Truncate(time.Second)
			rig.observe(t, rig.a, "7d", 0.50, early)

			resp, err := rig.manager.Execute(ctx, []string{"claude"}, req, reserveOpts(reserveTestUserKey, ""))
			if err != nil || (string(resp.Payload) != rig.a && string(resp.Payload) != rig.b) {
				t.Fatalf("Execute = %q, %v; want a Max login", resp.Payload, err)
			}
			if got := resp.Headers.Get(unifiedPrefix + "Status"); got != "" {
				t.Fatalf("a Max login's response got the unified status %q from the proxy", got)
			}

			rig.limitAt(t, rig.a, early)
			rig.limitAt(t, rig.b, time.Now().Add(72*time.Hour))
			want := map[string]string{
				"Status":               "allowed",
				"Representative-Claim": "seven_day",
				"Reset":                strconv.FormatInt(early.Unix(), 10),
				"7d-Utilization":       "1",
				"7d-Reset":             strconv.FormatInt(early.Unix(), 10),
				// No window status: Claude Code reads "rejected" there as a limit event.
				"7d-Status": "",
				"5h-Status": "",
			}
			requirePool := func(what string, headers http.Header) {
				t.Helper()
				for name, value := range want {
					if got := headers.Get(unifiedPrefix + name); got != value {
						t.Fatalf("%s: %s = %q, want %q (headers %v)", what, unifiedPrefix+name, got, value, headers)
					}
				}
			}
			resp, err = rig.manager.Execute(ctx, []string{"claude"}, req, reserveOpts(reserveTestUserKey, ""))
			if err != nil || string(resp.Payload) != rig.credit {
				t.Fatalf("Execute with both logins limited = %q, %v; want the credit key", resp.Payload, err)
			}
			requirePool("Execute", resp.Headers)

			stream, err := rig.manager.ExecuteStream(ctx, []string{"claude"}, req, reserveOpts(reserveTestExemptKey, ""))
			if err != nil {
				t.Fatalf("ExecuteStream: %v", err)
			}
			served := ""
			for chunk := range stream.Chunks {
				served += string(chunk.Payload)
			}
			if served != rig.credit {
				t.Fatalf("ExecuteStream served %q, want the credit key", served)
			}
			requirePool("ExecuteStream", stream.Headers)
		})
	}
}

func TestWithMaxPoolHeaders(t *testing.T) {
	rig := newReserveRig(t, &RoundRobinSelector{}, true, internalconfig.ReserveGateConfig{})
	now := time.Now()
	credit, login, codex := rig.auth(rig.credit), rig.auth(rig.a), rig.auth(rig.codex)
	upstream := http.Header{"Request-Id": []string{"req_1"}}

	// No reading of any Max login: nothing to describe.
	if got := rig.manager.withMaxPoolHeaders(credit, upstream, now); got.Get(unifiedPrefix+"Status") != "" || len(got) != 1 {
		t.Fatalf("headers with no reading = %v, want them unchanged", got)
	}

	// The login with the most room describes the pool.
	roomReset := now.Add(100 * time.Hour).Truncate(time.Second)
	rig.observe(t, rig.a, "7d", 0.70, now.Add(20*time.Hour))
	rig.observe(t, rig.b, "7d", 0.404, roomReset)
	got := rig.manager.withMaxPoolHeaders(credit, upstream, now)
	for name, value := range map[string]string{
		"Status": "allowed", "Representative-Claim": "seven_day", "7d-Utilization": "0.4",
		"Reset": strconv.FormatInt(roomReset.Unix(), 10), "7d-Reset": strconv.FormatInt(roomReset.Unix(), 10),
	} {
		if got.Get(unifiedPrefix+name) != value {
			t.Fatalf("%s = %q, want %q", unifiedPrefix+name, got.Get(unifiedPrefix+name), value)
		}
	}
	if got.Get("Request-Id") != "req_1" || len(upstream) != 1 {
		t.Fatalf("upstream headers: result %v, input %v; want the input kept and not written to", got, upstream)
	}
	if fromNil := rig.manager.withMaxPoolHeaders(credit, nil, now); fromNil.Get(unifiedPrefix+"Status") != "allowed" {
		t.Fatalf("headers from none = %v", fromNil)
	}

	// A login at a real limit counts as full whatever its last reading says.
	rig.coolDown(t, rig.b)
	if got = rig.manager.withMaxPoolHeaders(credit, upstream, time.Now()); got.Get(unifiedPrefix+"7d-Utilization") != "0.7" {
		t.Fatalf("utilization with the roomier login limited = %q, want 0.7", got.Get(unifiedPrefix+"7d-Utilization"))
	}

	// Left alone: a Max login, another provider, and headers that already carry a status.
	if got = rig.manager.withMaxPoolHeaders(login, upstream, now); got.Get(unifiedPrefix+"Status") != "" {
		t.Fatalf("a Max login's headers = %v", got)
	}
	if got = rig.manager.withMaxPoolHeaders(codex, upstream, now); got.Get(unifiedPrefix+"Status") != "" {
		t.Fatalf("a Codex credential's headers = %v", got)
	}
	own := http.Header{}
	own.Set(unifiedPrefix+"Status", "allowed_warning")
	if got = rig.manager.withMaxPoolHeaders(credit, own, now); got.Get(unifiedPrefix+"Status") != "allowed_warning" || got.Get(unifiedPrefix+"Reset") != "" {
		t.Fatalf("headers with an upstream status = %v, want them unchanged", got)
	}
}
