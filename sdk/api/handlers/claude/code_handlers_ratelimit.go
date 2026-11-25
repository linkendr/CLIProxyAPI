package claude

import (
	"strings"

	"github.com/MoeForge/moego/ojson"
)

// Fork-owned error-response code for the Claude handler: the cooldown reset fields.
// code_handlers.go keeps a one-line call into this file.

// claudeErrorResetFromText keeps the proxy cooldown's reset_time and
// reset_seconds so clients can still tell when to retry.
func claudeErrorResetFromText(errText string) (string, *int64) {
	if !ojson.Valid(errText) {
		return "", nil
	}
	node := ojson.Get(errText, "error")
	if !node.IsObject() {
		return "", nil
	}
	resetTime := strings.TrimSpace(node.Get("reset_time").String())
	seconds := node.Get("reset_seconds")
	if seconds.Type != ojson.Number {
		return resetTime, nil
	}
	value := seconds.Int()
	return resetTime, &value
}
