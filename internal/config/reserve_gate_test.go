package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseConfigReserveGate(t *testing.T) {
	raw := `routing:
  strategy: earliest-reset
  reserve-gate:
    enabled: true
    hold: true
    threshold: 0.97
    exempt-api-keys:
      - automation-key
`
	cfg, err := ParseConfigBytes([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	gate := cfg.Routing.ReserveGate
	if !gate.Enabled || !gate.Hold || len(gate.ExemptAPIKeys) != 1 || gate.ExemptAPIKeys[0] != "automation-key" {
		t.Fatalf("reserve-gate = %+v", gate)
	}
	if threshold := gate.EffectiveThreshold(); threshold != 0.97 {
		t.Fatalf("threshold = %v; want 0.97", threshold)
	}
}

func TestReserveGateThresholdDefault(t *testing.T) {
	if threshold := (ReserveGateConfig{}).EffectiveThreshold(); threshold != 0.95 {
		t.Fatalf("default threshold = %v; want 0.95", threshold)
	}
	if threshold := (ReserveGateConfig{Threshold: 0.85}).EffectiveThreshold(); threshold != 0.85 {
		t.Fatalf("threshold = %v; want 0.85", threshold)
	}
}

func TestParseConfigRejectsInvalidReserveGate(t *testing.T) {
	for name, tt := range map[string]struct {
		gate string
		want string
	}{
		"threshold above one":      {"enabled: true\n    threshold: 95", "threshold"},
		"negative threshold":       {"enabled: true\n    threshold: -0.5", "threshold"},
		"hold without enabled":     {"hold: true", "hold"},
		"empty exempt key":         {"enabled: true\n    exempt-api-keys: ['']", "exempt-api-keys[0]"},
		"invalid even if disabled": {"threshold: 2", "threshold"},
	} {
		t.Run(name, func(t *testing.T) {
			raw := "routing:\n  reserve-gate:\n    " + tt.gate + "\n"
			_, err := ParseConfigBytes([]byte(raw))
			if err == nil || !strings.Contains(err.Error(), "routing.reserve-gate."+tt.want) {
				t.Fatalf("ParseConfigBytes error = %v, want routing.reserve-gate.%s", err, tt.want)
			}
		})
	}
}

// The removed hysteresis, release and weekly relaxation keys are unknown now: a config
// that still sets one must fail instead of silently losing its meaning.
func TestParseConfigRejectsRemovedReserveGateKeys(t *testing.T) {
	for _, line := range []string{
		"resume-below: 0.90",
		"release-before-reset: 1h",
		"weekly-relax-before-reset: 4h",
		"weekly-relax-threshold: 0.99",
		"weekly-final-release-before-reset: 30m",
	} {
		for _, prefix := range []string{"", "config-version: 8\n"} {
			key := strings.SplitN(line, ":", 2)[0]
			t.Run(key+"/"+strings.TrimSpace(prefix), func(t *testing.T) {
				raw := prefix + "routing:\n  reserve-gate:\n    enabled: true\n    " + line + "\n"
				_, err := ParseConfigBytes([]byte(raw))
				if err == nil || !strings.Contains(err.Error(), key) {
					t.Fatalf("ParseConfigBytes(%q) error = %v, want an unknown-field error naming %s", line, err, key)
				}
			})
		}
	}
}

// The startup loader rejects a removed key too, so a stale live config fails to load
// instead of running with a different reserve than it says.
func TestLoadConfigRejectsRemovedReserveGateKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := "config-version: 8\nrouting:\n  reserve-gate:\n    enabled: true\n    threshold: 0.95\n    weekly-relax-before-reset: 4h\n"
	if errWrite := os.WriteFile(path, []byte(raw), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "weekly-relax-before-reset") {
		t.Fatalf("LoadConfig error = %v, want an unknown-field error naming weekly-relax-before-reset", err)
	}
	if errWrite := os.WriteFile(path, []byte("config-version: 8\nrouting:\n  reserve-gate:\n    enabled: true\n    threshold: 0.95\n"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	cfg, err := LoadConfig(path)
	if err != nil || !cfg.Routing.ReserveGate.Enabled {
		t.Fatalf("LoadConfig without removed keys = %+v, %v", cfg, err)
	}
}

// The live config uses the v8 layout; routing.reserve-gate must survive it unchanged.
func TestParseConfigReserveGateV8Layout(t *testing.T) {
	raw := `config-version: 8
server:
  port: 8317
access:
  api-keys: [automation-key, interactive-key]
routing:
  strategy: earliest-reset
  reserve-gate:
    enabled: true
    threshold: 0.95
    exempt-api-keys: [automation-key]
`
	cfg, err := ParseConfigBytes([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	gate := cfg.Routing.ReserveGate
	if !gate.Enabled || gate.Hold || len(gate.ExemptAPIKeys) != 1 || gate.ExemptAPIKeys[0] != "automation-key" {
		t.Fatalf("reserve-gate = %+v", gate)
	}
	if cfg.Routing.Strategy != "earliest-reset" {
		t.Fatalf("strategy = %q", cfg.Routing.Strategy)
	}
}

func TestParseConfigReserveGateWindDown(t *testing.T) {
	cases := []struct {
		line string
		want time.Duration
	}{
		{"", 15 * time.Minute},
		{"    wind-down-minutes: 15\n", 15 * time.Minute},
		{"    wind-down-minutes: 1\n", time.Minute},
		{"    wind-down-minutes: 0\n", 0},
	}
	for _, tc := range cases {
		raw := "routing:\n  reserve-gate:\n    enabled: true\n" + tc.line
		cfg, err := ParseConfigBytes([]byte(raw))
		if err != nil {
			t.Fatalf("%q: %v", tc.line, err)
		}
		if got := cfg.Routing.ReserveGate.EffectiveWindDown(); got != tc.want {
			t.Fatalf("%q: wind-down = %v, want %v", tc.line, got, tc.want)
		}
	}
	for _, bad := range []string{"-1", "1441"} {
		raw := "routing:\n  reserve-gate:\n    enabled: true\n    wind-down-minutes: " + bad + "\n"
		if _, err := ParseConfigBytes([]byte(raw)); err == nil || !strings.Contains(err.Error(), "wind-down-minutes") {
			t.Fatalf("wind-down-minutes %s: err = %v, want a wind-down-minutes error", bad, err)
		}
	}
}
