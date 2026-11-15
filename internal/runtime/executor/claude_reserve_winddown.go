package executor

import (
	"strconv"

	"github.com/MoeForge/moego/ojson"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// applyClaudeReserveWindDownNote appends the reserve gate's wrap-up note to the last user
// message of body when the gate served the request only to let a running task finish.
// Appending a text block after the existing blocks (the tool results) keeps every byte
// before it, so the cached prompt prefix and the caller's cache_control markers stay as
// they were; the system prompt is not touched.
func applyClaudeReserveWindDownNote(auth *cliproxyauth.Auth, metadata map[string]any, body []byte) []byte {
	if !cliproxyauth.ReserveWindDownNoteApplies(auth, metadata) {
		return body
	}
	return appendClaudeReserveWindDownNote(body, cliproxyauth.ReserveWindDownNote)
}

// appendClaudeReserveWindDownNote appends note as a text block to the last message of a
// Claude Messages body when that message is a user message. Any other body is returned
// unchanged.
func appendClaudeReserveWindDownNote(body []byte, note string) []byte {
	messages := ojson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return body
	}
	count := len(messages.Array())
	if count == 0 {
		return body
	}
	path := "messages." + strconv.Itoa(count-1)
	last := ojson.GetBytes(body, path)
	if last.Get("role").String() != "user" {
		return body
	}
	block := map[string]string{"type": "text", "text": note}
	content := last.Get("content")
	var (
		updated []byte
		err     error
	)
	switch {
	case content.IsArray():
		updated, err = ojson.SetBytes(body, path+".content.-1", block)
	case content.Type == ojson.String:
		blocks := []map[string]string{block}
		if content.String() != "" {
			blocks = []map[string]string{{"type": "text", "text": content.String()}, block}
		}
		updated, err = ojson.SetBytes(body, path+".content", blocks)
	default:
		return body
	}
	if err != nil {
		return body
	}
	return updated
}
