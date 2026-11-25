package helps

import (
	"strconv"

	"github.com/MoeForge/moego/ojson"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// ApplyClaudeReserveWindDownNoteDoc appends the reserve gate's wrap-up note to the last
// user message of doc when the gate served the request only to let a running task finish.
// Appending a text block after the existing blocks (the tool results) keeps every byte
// before it, so the cached prompt prefix and the caller's cache_control markers stay as
// they were; the system prompt is not touched. It is a built-in injection: the Claude
// executors call it last among the built-in steps, so no later rewrite moves it off the
// end of the last user message, and before the user payload rules, which stay the last
// word on the body.
func ApplyClaudeReserveWindDownNoteDoc(doc *ojson.Doc, auth *cliproxyauth.Auth, metadata map[string]any) error {
	if !cliproxyauth.ReserveWindDownNoteApplies(auth, metadata) {
		return nil
	}
	return AppendClaudeReserveWindDownNoteDoc(doc, cliproxyauth.ReserveWindDownNote)
}

// AppendClaudeReserveWindDownNoteDoc appends note as a text block to the last message of a
// Claude Messages body when that message is a user message. Any other body is left
// unchanged.
func AppendClaudeReserveWindDownNoteDoc(doc *ojson.Doc, note string) error {
	messages := doc.Get("messages")
	if !messages.IsArray() {
		return nil
	}
	count := len(messages.Array())
	if count == 0 {
		return nil
	}
	path := "messages." + strconv.Itoa(count-1)
	last := doc.Get(path)
	if last.Get("role").String() != "user" {
		return nil
	}
	block := map[string]string{"type": "text", "text": note}
	content := last.Get("content")
	switch {
	case content.IsArray():
		return doc.Set(path+".content.-1", block)
	case content.Type == ojson.String:
		blocks := []map[string]string{block}
		if content.String() != "" {
			blocks = []map[string]string{{"type": "text", "text": content.String()}, block}
		}
		return doc.Set(path+".content", blocks)
	}
	return nil
}
