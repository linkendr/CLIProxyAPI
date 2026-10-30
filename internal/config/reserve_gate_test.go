package config

import (
	"strings"
	"testing"
)

func TestParseConfigReserveGate(t *testing.T) {
	raw := `routing:
  strategy: earliest-reset
  reserve-gate:
    enabled: true
    hold: true
    threshold: 0.97
    resume-below: 0.92
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
	if threshold, resume := gate.Levels(); threshold != 0.97 || resume != 0.92 {
		t.Fatalf("levels = %v, %v; want 0.97, 0.92", threshold, resume)
	}
}

func TestReserveGateLevelsDefaults(t *testing.T) {
	if threshold, resume := (ReserveGateConfig{}).Levels(); threshold != 0.95 || resume != 0.90 {
		t.Fatalf("default levels = %v, %v; want 0.95, 0.90", threshold, resume)
	}
	if threshold, resume := (ReserveGateConfig{Threshold: 0.85}).Levels(); threshold != 0.85 || resume != 0.85 {
		t.Fatalf("levels with a low threshold = %v, %v; want resume capped at 0.85", threshold, resume)
	}
}

func TestParseConfigRejectsInvalidReserveGate(t *testing.T) {
	for name, tt := range map[string]struct {
		gate string
		want string
	}{
		"threshold above one":      {"enabled: true\n    threshold: 95", "threshold"},
		"negative threshold":       {"enabled: true\n    threshold: -0.5", "threshold"},
		"resume above threshold":   {"enabled: true\n    threshold: 0.9\n    resume-below: 0.95", "resume-below"},
		"hold without enabled":     {"hold: true", "hold"},
		"empty exempt key":         {"enabled: true\n    exempt-api-keys: ['']", "exempt-api-keys[0]"},
		"negative resume-below":    {"enabled: true\n    resume-below: -0.1", "resume-below"},
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
