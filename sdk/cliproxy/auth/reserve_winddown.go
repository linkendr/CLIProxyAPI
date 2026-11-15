package auth

import (
	"strings"
	"sync"
	"time"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// ReserveWindDownMetadataKey marks, in Options.Metadata, a request of a caller that is
// not exempt that the reserve gate serves only to let a running task finish. The Claude
// executor then appends ReserveWindDownNote to the last user message.
const ReserveWindDownMetadataKey = "reserve_wind_down"

// ReserveWindDownNote is the text the Claude executor appends to the last user message of
// a wind-down request.
const ReserveWindDownNote = "Quota reserve reached. Finish the current step, write a short status of what is done and what is left, and end your turn. Do not start new tool calls or subagents."

// ReserveWindDownNoteApplies reports whether the request in meta is a wind-down request
// and auth is a credential the gate would have refused it: a Claude credential that is
// not a paid credit key with fallback-when. A credit key serving under its own rule (every
// Max login at a real limit) would serve a new prompt too, so it gets no note.
func ReserveWindDownNoteApplies(auth *Auth, meta map[string]any) bool {
	if auth == nil || meta == nil {
		return false
	}
	flag, _ := meta[ReserveWindDownMetadataKey].(bool)
	return flag && isClaudeAuth(auth) && authFallbackWhen(auth) == ""
}

// reserveRequestIsRunningTask reports whether a request continues a running task: its
// last message hands tool results back to the model. Anything else, including a format
// this does not know, counts as a new prompt.
//
//   - Claude Messages: the last message is a user message with a tool_result block.
//   - OpenAI Chat Completions: the last message has role "tool" (or legacy "function").
//   - OpenAI Responses: the last input item is a *_call_output item.
//   - Gemini: the last content has a functionResponse part.
func reserveRequestIsRunningTask(format sdktranslator.Format, payload []byte) bool {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return false
	}
	switch format {
	case sdktranslator.FormatClaude:
		last := lastArrayItem(gjson.GetBytes(payload, "messages"))
		if !last.Exists() || !strings.EqualFold(last.Get("role").String(), "user") {
			return false
		}
		content := last.Get("content")
		if !content.IsArray() {
			return false
		}
		found := false
		content.ForEach(func(_, block gjson.Result) bool {
			found = block.Get("type").String() == "tool_result"
			return !found
		})
		return found
	case sdktranslator.FormatOpenAI:
		role := strings.ToLower(lastArrayItem(gjson.GetBytes(payload, "messages")).Get("role").String())
		return role == "tool" || role == "function"
	case sdktranslator.FormatOpenAIResponse:
		return strings.HasSuffix(lastArrayItem(gjson.GetBytes(payload, "input")).Get("type").String(), "_call_output")
	case sdktranslator.FormatGemini:
		found := false
		lastArrayItem(gjson.GetBytes(payload, "contents")).Get("parts").ForEach(func(_, part gjson.Result) bool {
			found = part.Get("functionResponse").Exists()
			return !found
		})
		return found
	default:
		return false
	}
}

func lastArrayItem(array gjson.Result) gjson.Result {
	if !array.IsArray() {
		return gjson.Result{}
	}
	items := array.Array()
	if len(items) == 0 {
		return gjson.Result{}
	}
	return items[len(items)-1]
}

// reserveTriggerClock keeps when the proxy first saw every Max login held (the reserve
// trigger). It lives in memory only: a restart starts the wind-down again.
type reserveTriggerClock struct {
	mu    sync.Mutex
	since time.Time
}

// observe records the trigger's state at now and returns when it turned on (zero while
// it is off). Turning off clears the start, so the next trigger starts a new wind-down.
func (c *reserveTriggerClock) observe(on bool, now time.Time, windDown time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !on {
		if !c.since.IsZero() {
			log.Infof("reserve gate: a Max login has room again; the wind-down that started %s is over", formatReserveReset(c.since))
			c.since = time.Time{}
		}
		return time.Time{}
	}
	if c.since.IsZero() {
		c.since = now
		log.Infof("reserve gate: every Max login is held; non-exempt new prompts are refused and running tasks are served until %s", formatReserveReset(now.Add(windDown)))
	}
	return c.since
}

// start returns when the trigger turned on, zero while it is off.
func (c *reserveTriggerClock) start() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.since
}

func (c *reserveTriggerClock) clear() {
	c.mu.Lock()
	c.since = time.Time{}
	c.mu.Unlock()
}

// observeReserveTrigger evaluates the reserve trigger from the current Max logins and
// records it on the clock. It takes m.mu (read), so the caller must not hold it.
func (m *Manager) observeReserveTrigger(gate *reserveGate, now time.Time) {
	check := &reserveCheck{gate: gate, book: &m.reserveHolds, now: now, logins: m.reserveMaxLogins(now)}
	check.evaluate()
	m.reserveTrigger.observe(check.everyHeld, now, gate.windDown)
}
