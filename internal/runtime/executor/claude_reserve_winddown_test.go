package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestAppendClaudeReserveWindDownNote(t *testing.T) {
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
		got := string(appendClaudeReserveWindDownNote([]byte(tc.body), note))
		if got != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.name, got, tc.want)
		}
	}
}

// The prefix before the note is byte-identical, so the prompt cache prefix is unbroken.
func TestAppendClaudeReserveWindDownNoteKeepsPrefix(t *testing.T) {
	body := `{"system":[{"type":"text","text":"sys"}],"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}],"max_tokens":8}`
	got := string(appendClaudeReserveWindDownNote([]byte(body), "n"))
	prefix := `{"system":[{"type":"text","text":"sys"}],"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}`
	if len(got) < len(prefix) || got[:len(prefix)] != prefix {
		t.Fatalf("prefix changed: %s", got)
	}
	if ojson.Get(got, "system.#").Int() != 1 {
		t.Fatalf("system prompt changed: %s", got)
	}
}

// claudeWindDownUpstream runs one Execute against a fake upstream and returns the body it got.
func claudeWindDownUpstream(t *testing.T, auth *cliproxyauth.Auth, format sdktranslator.Format, payload string, meta map[string]any, stream bool) []byte {
	t.Helper()
	var seen []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = io.ReadAll(r.Body)
		if ojson.GetBytes(seen, "stream").Bool() {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-opus-4-6\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n"+
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n"+
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-opus-4-6","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()
	auth.Attributes["base_url"] = server.URL
	executor := NewClaudeExecutor(&config.Config{})
	req := cliproxyexecutor.Request{Model: "claude-opus-4-6", Payload: []byte(payload)}
	opts := cliproxyexecutor.Options{SourceFormat: format, OriginalRequest: []byte(payload), Metadata: meta, Stream: stream}
	if stream {
		result, err := executor.ExecuteStream(context.Background(), auth, req, opts)
		if err != nil {
			t.Fatalf("ExecuteStream: %v", err)
		}
		for range result.Chunks {
		}
	} else if _, err := executor.Execute(context.Background(), auth, req, opts); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return seen
}

func lastUserBlocks(t *testing.T, body []byte) []ojson.Result {
	t.Helper()
	messages := ojson.GetBytes(body, "messages").Array()
	if len(messages) == 0 {
		t.Fatalf("upstream body has no messages: %s", body)
	}
	last := messages[len(messages)-1]
	if last.Get("role").String() != "user" {
		t.Fatalf("last upstream message is %s, want user: %s", last.Get("role"), body)
	}
	return last.Get("content").Array()
}

func TestClaudeExecutorAppendsReserveWindDownNote(t *testing.T) {
	claudeRunning := `{"model":"claude-opus-4-6","max_tokens":8,"messages":[{"role":"user","content":"go"},{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"f","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}]}`
	openAIRunning := `{"model":"claude-opus-4-6","max_tokens":8,"messages":[{"role":"user","content":"go"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":"ok"}],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]}`
	marked := func() map[string]any { return map[string]any{cliproxyauth.ReserveWindDownMetadataKey: true} }
	apiKey := func(extra map[string]string) *cliproxyauth.Auth {
		attrs := map[string]string{"api_key": "fake-key"}
		for k, v := range extra {
			attrs[k] = v
		}
		return &cliproxyauth.Auth{ID: "k", Provider: "claude", Attributes: attrs}
	}
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			name    string
			format  sdktranslator.Format
			payload string
		}{{"claude", sdktranslator.FormatClaude, claudeRunning}, {"openai", sdktranslator.FormatOpenAI, openAIRunning}} {
			body := claudeWindDownUpstream(t, apiKey(nil), tc.format, tc.payload, marked(), stream)
			blocks := lastUserBlocks(t, body)
			if len(blocks) < 2 || blocks[0].Get("type").String() != "tool_result" {
				t.Fatalf("%s stream=%v: last user message %v, want the tool result first", tc.name, stream, blocks)
			}
			end := blocks[len(blocks)-1]
			if end.Get("type").String() != "text" || end.Get("text").String() != cliproxyauth.ReserveWindDownNote {
				t.Fatalf("%s stream=%v: last block %s, want the wind-down note", tc.name, stream, end.Raw)
			}
			for _, system := range ojson.GetBytes(body, "system").Array() {
				if system.Get("text").String() == cliproxyauth.ReserveWindDownNote {
					t.Fatalf("%s stream=%v: note found in the system prompt", tc.name, stream)
				}
			}
		}
	}
	// No mark, or a credit key with fallback-when: no note.
	for name, run := range map[string]func() []byte{
		"unmarked": func() []byte {
			return claudeWindDownUpstream(t, apiKey(nil), sdktranslator.FormatClaude, claudeRunning, map[string]any{}, false)
		},
		"credit key": func() []byte {
			return claudeWindDownUpstream(t, apiKey(map[string]string{"fallback_when": "limit"}), sdktranslator.FormatClaude, claudeRunning, marked(), false)
		},
	} {
		blocks := lastUserBlocks(t, run())
		if end := blocks[len(blocks)-1]; end.Get("text").String() == cliproxyauth.ReserveWindDownNote {
			t.Fatalf("%s: note added", name)
		}
	}
}
