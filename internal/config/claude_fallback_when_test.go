package config

import (
	"strings"
	"testing"
)

func TestNormalizeClaudeFallbackWhen(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		raw  string
		want string
		ok   bool
	}{
		{"", "", true},
		{"  ", "", true},
		{"model-limit", ClaudeFallbackWhenModelLimit, true},
		{" Model-Limit ", ClaudeFallbackWhenModelLimit, true},
		{"limit", ClaudeFallbackWhenLimit, true},
		{" Limit ", ClaudeFallbackWhenLimit, true},
		{"model_limit", "", false},
		{"always", "", false},
	} {
		got, ok := NormalizeClaudeFallbackWhen(tt.raw)
		if got != tt.want || ok != tt.ok {
			t.Fatalf("NormalizeClaudeFallbackWhen(%q) = (%q, %t), want (%q, %t)", tt.raw, got, ok, tt.want, tt.ok)
		}
	}
}

func TestParseConfigClaudeFallbackWhenInheritsFromGroup(t *testing.T) {
	raw := `api-keys:
  claude:
    - name: credit
      priority: -10
      fallback-when: model-limit
      keys:
        - api-key: inherited
        - api-key: cleared
          fallback-when: ''
`
	cfg, err := ParseConfigBytes([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.ClaudeKey) != 2 {
		t.Fatalf("ClaudeKey count = %d, want 2", len(cfg.ClaudeKey))
	}
	if got := cfg.ClaudeKey[0].FallbackWhen; got != ClaudeFallbackWhenModelLimit {
		t.Fatalf("inherited fallback-when = %q, want %q", got, ClaudeFallbackWhenModelLimit)
	}
	if got := cfg.ClaudeKey[1].FallbackWhen; got != "" {
		t.Fatalf("explicit empty fallback-when = %q, want it to override the group", got)
	}
}

func TestParseConfigRejectsInvalidClaudeFallbackWhen(t *testing.T) {
	raw := `api-keys:
  claude:
    - name: credit
      fallback-when: model_limit
      keys:
        - api-key: k
`
	_, err := ParseConfigBytes([]byte(raw))
	if err == nil || !strings.Contains(err.Error(), "fallback-when") {
		t.Fatalf("ParseConfigBytes error = %v, want a fallback-when validation error", err)
	}
}

func TestParseConfigRejectsFallbackWhenOutsideClaude(t *testing.T) {
	for name, raw := range map[string]string{
		"group": "api-keys:\n  gemini:\n    - name: g\n      fallback-when: model-limit\n      keys:\n        - api-key: k\n",
		"key":   "api-keys:\n  gemini:\n    - name: g\n      keys:\n        - api-key: k\n          fallback-when: model-limit\n",
	} {
		if _, err := ParseConfigBytes([]byte(raw)); err == nil || !strings.Contains(err.Error(), "fallback-when") {
			t.Fatalf("%s: ParseConfigBytes error = %v, want fallback-when rejection", name, err)
		}
	}
}
