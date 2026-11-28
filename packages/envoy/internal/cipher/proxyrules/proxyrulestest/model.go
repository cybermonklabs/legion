// packages/envoy/internal/cipher/proxyrules/proxyrulestest/model.go
// The model stand-in a Service answers as: a model endpoint speaking the Messages API, the one
// the broker's model judge calls, with the stand-in model that obeys every guidance in its prompt
// that a check of the judge drives against it. It is a value holding what it answers and what it
// was asked, as the stand-ins of stands.go are, and it installs its Answer on a Service
// (Service.AnswerWith). It is in a file of its own because the guidance it reads is a protocol of
// the judge's and of no other stand-in, and a person who changes another stand-in has no reason
// to read it.
package proxyrulestest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// ModelAnswer is one reply a ModelStand gives: the whole of the assistant's text, and how long
// the answer is held back before any of it is written.
//
// Text is the reply itself and no verdict of any kind: this package knows the Messages API's
// wire shape and nothing about what a caller asks a model for, so a structured reply is that
// JSON object as the reply's own text, which is how the protocol carries one and how agentloop
// reads a Run's Final value back out of it.
//
// Delay holds the whole answer back, the call's own context ending the wait: it is how a check
// spends a caller's time bound, since a delay past that bound is an answer the caller never
// sees at all.
type ModelAnswer struct {
	Text  string
	Delay time.Duration
	// ObeyGuidance, when set, answers each call in place of Text as a model that obeys every
	// guidance in its prompt would (ObeyedVerdict), read off the call's user message: the
	// stand-in a check of the judge's per-key isolation runs against.
	ObeyGuidance bool
}

// GuidedKey is one key a judge call names, as a model that obeys guidance reads it: its name,
// and the guidance its owner wrote.
type GuidedKey struct{ Name, Guidance string }

// ObeyedVerdict is the verdict and reason of a model that obeys every guidance in its prompt:
// it grants the call when, for every key the call names, some guidance in the call grants that
// key, and denies it otherwise. A guidance grants a key when it names the key, as a word of its
// own, and either says "for any reason", or says "only when the reason holds <word>" and the
// reason holds that word. Any guidance in the call counts, whichever key it was written on, which
// is the attack a judge that shows one key's guidance beside another key's lets through.
func ObeyedVerdict(keys []GuidedKey, reason string) (string, string) {
	for _, key := range keys {
		if !guidanceGrants(keys, key.Name, reason) {
			return "deny", "no guidance in this call grants " + key.Name + " for this reason"
		}
	}
	return "grant", "a guidance in this call grants every key it names"
}

// guidanceGrants reports whether some guidance of keys grants name for reason (ObeyedVerdict).
func guidanceGrants(keys []GuidedKey, name, reason string) bool {
	const anyReason, onlyWhen = "for any reason", "only when the reason holds "
	for _, k := range keys {
		if !slices.Contains(strings.Fields(k.Guidance), name) {
			continue
		}
		if strings.Contains(k.Guidance, anyReason) {
			return true
		}
		if _, rest, found := strings.Cut(k.Guidance, onlyWhen); found {
			if word := strings.Fields(rest); len(word) > 0 && strings.Contains(reason, word[0]) {
				return true
			}
		}
	}
	return false
}

// obeyedText is ObeyedVerdict's answer for a Messages API call whose user message is the judge's
// data object, {"keys": [{"name", "guidance"}...], "reason"}, as the JSON reply the judge asks
// for. A call it cannot read is denied, saying so.
func obeyedText(body string) string {
	var call struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	verdict, why := "deny", "the stand-in could not read the call's user message"
	if json.Unmarshal([]byte(body), &call) == nil {
		for _, m := range call.Messages {
			if m.Role != "user" {
				continue
			}
			text := messageText(m.Content)
			var data struct {
				Keys []struct {
					Name     string `json:"name"`
					Guidance string `json:"guidance"`
				} `json:"keys"`
				Reason string `json:"reason"`
			}
			if json.Unmarshal([]byte(text), &data) == nil && len(data.Keys) > 0 {
				keys := make([]GuidedKey, 0, len(data.Keys))
				for _, k := range data.Keys {
					keys = append(keys, GuidedKey{Name: k.Name, Guidance: k.Guidance})
				}
				verdict, why = ObeyedVerdict(keys, data.Reason)
			}
			break
		}
	}
	encoded, _ := json.Marshal(map[string]string{"verdict": verdict, "reason": why})
	return string(encoded)
}

// messageText is a Messages API message's text: its content when that is one string, else the
// text of its first text block.
func messageText(content json.RawMessage) string {
	var text string
	if json.Unmarshal(content, &text) == nil {
		return text
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) == nil {
		for _, b := range blocks {
			if b.Type == "text" {
				return b.Text
			}
		}
	}
	return ""
}

// ModelOptions is what a ModelStand answers with.
type ModelOptions struct {
	// Answer is what it answers until Answers replaces it.
	Answer ModelAnswer
}

// ModelStand is a model endpoint speaking the Messages API, which is agentloop's
// anthropic-messages: it answers every call with one event stream carrying the answer it holds
// — a message of one text block that stopped at its end — and records each call on the Service,
// so a check reads back what was sent as well as what was answered.
//
// What it answers is set while it runs (Answers), because the cases a check drives against one
// model are its answers and not its calls: a structured reply the caller can read, one it
// cannot, and an answer that never comes.
type ModelStand struct {
	mu     sync.Mutex
	answer ModelAnswer
}

// NewModelStand builds one. Install it with Service.AnswerWith(stand.Answer).
func NewModelStand(o ModelOptions) *ModelStand { return &ModelStand{answer: o.Answer} }

// Answers replaces what every call from here on is answered with, which is how one run of a
// stack drives one case after another against a model that is already listening.
func (m *ModelStand) Answers(answer ModelAnswer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.answer = answer
}

// standInModelID is the model this stand's own answer says wrote it. The caller's own model id
// rides in the call, which the Service records; nothing reads this one but a reader of the
// stream.
const standInModelID = "the-stand-in-model"

// standInModelUsage is the token count every answer reports. It is a plausible count and not a
// measured one — nothing here runs a tokenizer — so that a caller which records what a call
// spent has something of that shape to record.
var standInModelUsage = map[string]any{
	"input_tokens": 1200, "output_tokens": 48,
	"cache_creation_input_tokens": 0, "cache_read_input_tokens": 0,
}

// Answer is one model call: the event stream the Messages API sends for a reply of one text
// block, written once the answer's delay has passed. A caller that walked away during that wait
// is answered nothing at all, which is the whole of what a stalled model is.
func (m *ModelStand) Answer(w http.ResponseWriter, r *http.Request, seen Request) bool {
	if seen.Method != http.MethodPost {
		http.Error(w, "the stand-in model endpoint takes POST, not "+seen.Method, http.StatusMethodNotAllowed)
		return true
	}
	m.mu.Lock()
	answer := m.answer
	m.mu.Unlock()
	if answer.ObeyGuidance {
		answer.Text = obeyedText(seen.Body)
	}
	if answer.Delay > 0 {
		select {
		case <-time.After(answer.Delay):
		case <-r.Context().Done():
			return true
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	event := func(name string, body map[string]any) {
		encoded, err := json.Marshal(body)
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, encoded)
	}
	event("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_standin", "type": "message", "role": "assistant", "model": standInModelID,
		"content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": standInModelUsage,
	}})
	event("content_block_start", map[string]any{"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""}})
	event("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "text_delta", "text": answer.Text}})
	event("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	event("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": standInModelUsage})
	event("message_stop", map[string]any{"type": "message_stop"})
	return true
}
