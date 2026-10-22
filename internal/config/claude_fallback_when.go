package config

import (
	"fmt"
	"strings"
)

// ClaudeFallbackWhenModelLimit marks a Claude key that serves a model only while
// every higher-priority credential supporting it is blocked for that model by a
// model-scoped limit (for example the Fable weekly window).
const ClaudeFallbackWhenModelLimit = "model-limit"

// NormalizeClaudeFallbackWhen maps a raw fallback-when value to its canonical form.
// The boolean is false for an unsupported value.
func NormalizeClaudeFallbackWhen(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return "", true
	case ClaudeFallbackWhenModelLimit:
		return ClaudeFallbackWhenModelLimit, true
	default:
		return "", false
	}
}

// ValidateClaudeFallbackWhen rejects an unsupported fallback-when value. A typo must
// fail loudly: ignoring it would turn a conditional fallback into an ordinary one.
func (cfg *Config) ValidateClaudeFallbackWhen() error {
	if cfg == nil {
		return nil
	}
	for index := range cfg.ClaudeKey {
		if _, ok := NormalizeClaudeFallbackWhen(cfg.ClaudeKey[index].FallbackWhen); !ok {
			return fmt.Errorf("claude-api-key[%d].fallback-when: unsupported value %q (supported: %q or empty)", index, strings.TrimSpace(cfg.ClaudeKey[index].FallbackWhen), ClaudeFallbackWhenModelLimit)
		}
	}
	return nil
}
