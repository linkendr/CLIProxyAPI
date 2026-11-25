package diff

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestBuildConfigChangeDetails_ClaudeFallbackWhen(t *testing.T) {
	oldCfg := &config.Config{ClaudeKey: []config.ClaudeKey{{APIKey: "key"}}}
	newCfg := &config.Config{ClaudeKey: []config.ClaudeKey{{APIKey: "key", FallbackWhen: "model-limit"}}}
	expectContains(t, BuildConfigChangeDetails(oldCfg, newCfg), "claude[0].fallback-when:  -> model-limit")
}
