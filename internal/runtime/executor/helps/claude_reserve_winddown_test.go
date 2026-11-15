package helps

import (
	"testing"

	"github.com/MoeForge/moego/ojson"
)

func TestAppendClaudeReserveWindDownNoteDoc(t *testing.T) {
	const note = "wrap up"
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			"after the tool results, cache marker kept",
			`{"messages":[{"role":"user","content":"go"},{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"f","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok","cache_control":{"type":"ephemeral"}}]}]}`,
			`{"messages":[{"role":"user","content":"go"},{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"f","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok","cache_control":{"type":"ephemeral"}},{"text":"wrap up","type":"text"}]}]}`,
		},
		{
			"string content becomes blocks",
			`{"messages":[{"role":"user","content":"go"}]}`,
			`{"messages":[{"role":"user","content":[{"text":"go","type":"text"},{"text":"wrap up","type":"text"}]}]}`,
		},
		{
			"last message not from the user",
			`{"messages":[{"role":"user","content":"go"},{"role":"assistant","content":"{"}]}`,
			`{"messages":[{"role":"user","content":"go"},{"role":"assistant","content":"{"}]}`,
		},
		{"no messages", `{"model":"m"}`, `{"model":"m"}`},
		{"empty messages", `{"messages":[]}`, `{"messages":[]}`},
	}
	for _, tc := range cases {
		doc := ojson.NewDocString(tc.body)
		if err := AppendClaudeReserveWindDownNoteDoc(doc, note); err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
		if got := doc.String(); got != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.name, got, tc.want)
		}
	}
}

// The prefix before the note is byte-identical, so the prompt cache prefix is unbroken.
func TestAppendClaudeReserveWindDownNoteDocKeepsPrefix(t *testing.T) {
	body := `{"system":[{"type":"text","text":"sys"}],"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}],"max_tokens":8}`
	doc := ojson.NewDocString(body)
	if err := AppendClaudeReserveWindDownNoteDoc(doc, "n"); err != nil {
		t.Fatal(err)
	}
	got := doc.String()
	prefix := `{"system":[{"type":"text","text":"sys"}],"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}`
	if len(got) < len(prefix) || got[:len(prefix)] != prefix {
		t.Fatalf("prefix changed: %s", got)
	}
	if ojson.Get(got, "system.#").Int() != 1 {
		t.Fatalf("system prompt changed: %s", got)
	}
}
