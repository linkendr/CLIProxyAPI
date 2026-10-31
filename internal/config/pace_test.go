package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseConfigPace(t *testing.T) {
	raw := `routing:
  strategy: pace
  pace:
    override-hours: 12
    switch-margin: 0.25
`
	cfg, err := ParseConfigBytes([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Routing.Strategy != "pace" {
		t.Fatalf("strategy = %q", cfg.Routing.Strategy)
	}
	if override, margin := cfg.Routing.Pace.Settings(); override != 12*time.Hour || margin != 0.25 {
		t.Fatalf("settings = %v, %v; want 12h, 0.25", override, margin)
	}
}

func TestPaceSettingsDefaultsAndZero(t *testing.T) {
	if override, margin := (PaceConfig{}).Settings(); override != 24*time.Hour || margin != 0.10 {
		t.Fatalf("default settings = %v, %v; want 24h, 0.10", override, margin)
	}
	cfg, err := ParseConfigBytes([]byte("routing:\n  pace:\n    override-hours: 0\n    switch-margin: 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if override, margin := cfg.Routing.Pace.Settings(); override != 0 || margin != 0 {
		t.Fatalf("explicit zero settings = %v, %v; want 0, 0 (zero is a setting, not the default)", override, margin)
	}
	cfg, err = ParseConfigBytes([]byte("routing:\n  strategy: pace\n"))
	if err != nil {
		t.Fatal(err)
	}
	if override, margin := cfg.Routing.Pace.Settings(); override != 24*time.Hour || margin != 0.10 {
		t.Fatalf("settings without a pace block = %v, %v; want defaults", override, margin)
	}
}

func TestParseConfigRejectsInvalidPace(t *testing.T) {
	for name, tt := range map[string]struct {
		pace string
		want string
	}{
		"negative override-hours":  {"override-hours: -1", "routing.pace.override-hours"},
		"override beyond a week":   {"override-hours: 200", "routing.pace.override-hours"},
		"margin as a percent":      {"switch-margin: 10", "routing.pace.switch-margin"},
		"negative margin":          {"switch-margin: -0.1", "routing.pace.switch-margin"},
		"misspelled key":           {"switch-marign: 0.2", "routing.pace.switch-marign"},
		"not a number":             {"override-hours: soon", "routing.pace"},
		"invalid even if not pace": {"switch-margin: 2", "routing.pace.switch-margin"},
	} {
		t.Run(name, func(t *testing.T) {
			raw := "routing:\n  strategy: earliest-reset\n  pace:\n    " + tt.pace + "\n"
			_, err := ParseConfigBytes([]byte(raw))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ParseConfigBytes error = %v, want %s", err, tt.want)
			}
		})
	}
}

// The live config uses the v8 layout and is read by LoadConfig; routing.pace must
// survive both, and an invalid value must stop the load.
func TestLoadConfigPaceV8Layout(t *testing.T) {
	raw := `config-version: 8
server:
  port: 8317
routing:
  strategy: pace
  pace:
    override-hours: 36
`
	cfg, err := ParseConfigBytes([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if override, margin := cfg.Routing.Pace.Settings(); cfg.Routing.Strategy != "pace" || override != 36*time.Hour || margin != 0.10 {
		t.Fatalf("v8 pace = %q, %v, %v", cfg.Routing.Strategy, override, margin)
	}
	path := writeUploadTestConfig(t, raw)
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if override, _ := loaded.Routing.Pace.Settings(); override != 36*time.Hour {
		t.Fatalf("LoadConfig override = %v, want 36h", override)
	}
	path = writeUploadTestConfig(t, strings.Replace(raw, "36", "-3", 1))
	if _, err = LoadConfig(path); err == nil || !strings.Contains(err.Error(), "routing.pace.override-hours") {
		t.Fatalf("LoadConfig error = %v, want routing.pace.override-hours", err)
	}
}
