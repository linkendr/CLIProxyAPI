package synthesizer

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestConfigSynthesizer_ClaudeKeys_FallbackWhen(t *testing.T) {
	auths, err := NewConfigSynthesizer().Synthesize(&SynthesisContext{
		Config: &config.Config{ClaudeKey: []config.ClaudeKey{
			{APIKey: "on", Priority: -10, FallbackWhen: " Model-Limit "},
			{APIKey: "off"},
			{APIKey: "any", Priority: -10, FallbackWhen: "LIMIT"},
		}},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	})
	if err != nil || len(auths) != 3 {
		t.Fatalf("synthesize: auths=%d err=%v", len(auths), err)
	}
	if got := auths[2].Attributes["fallback_when"]; got != "limit" {
		t.Fatalf("fallback_when = %q, want limit", got)
	}
	if got := auths[0].Attributes["fallback_when"]; got != "model-limit" {
		t.Fatalf("fallback_when = %q, want model-limit", got)
	}
	if value, exists := auths[1].Attributes["fallback_when"]; exists {
		t.Fatalf("fallback_when = %q, want absent by default", value)
	}
}
