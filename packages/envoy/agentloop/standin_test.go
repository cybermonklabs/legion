package agentloop

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// standIn is a provider endpoint that records every request it gets and answers each with the next
// of its replies, or, where every is set, with that one reply however many requests come. A reply
// is used up only by an answer that went out whole: a request whose answer was lost, its
// connection dropped or its client gone before the stand-in wrote it, puts its reply back at the
// head of the script, so the transport's one retry of a call whose connection broke is answered as
// the call would have been (transport.go, RoundTrip; doc.go, "The retry"). That retry can arrive
// before the lost call's handler runs again, so a reply's fate is settled before anything a later
// request takes depends on it: a dropped call's reply goes back before its connection closes, and
// a request that comes while a reply taken before it is unsettled waits for it to settle.
type standIn struct {
	server  *httptest.Server
	mu      sync.Mutex
	replies []reply
	// every is the reply to each request, where the endpoint answers every request alike, as
	// one that redirects does, rather than a script of calls in turn: a request from a client
	// that is not the run's, reaching its port, takes no reply away from the run's call.
	every *reply
	// drops is how many of the first requests have their connection closed with no answer, as
	// when a call reaches the endpoint and its connection breaks before the answer. Each is a
	// lost answer, so the reply it would have had answers the next request.
	drops int
	// held, where a row sets it (holding), is waited on by the handler of a dropped request
	// after it closed the connection and before it returns, as a handler the scheduler set aside
	// there would be: the client's retry then comes while that handler has not run again.
	held chan struct{}
	// unsettled is open while a reply taken off the script has not yet been settled: kept by an
	// answer written whole, or put back by one that was lost. It is nil when no reply is.
	unsettled chan struct{}
	// got is every request the stand-in got, in order, a lost one and its retry each counted.
	got []recorded
}

// reply is one response a stand-in gives: an SSE stream, or an error status with a body, after a
// delay.
type reply struct {
	status  int
	body    []byte
	headers map[string]string
	delay   time.Duration
}

// recorded is a request a stand-in got.
type recorded struct {
	path string
	// query is the request URL's raw query string, exactly as it arrived, so a test can tell
	// no query string from an empty parameter.
	query  string
	header http.Header
	body   []byte
	// lost is whether the answer to it never went out: its connection was dropped, or closed
	// before the stand-in had written the answer whole.
	lost bool
}

func newStandIn(t *testing.T, replies ...reply) *standIn {
	t.Helper()
	return serveStandIn(t, &standIn{replies: replies})
}

// newStandInAnsweringEvery is a stand-in that answers every request with r.
func newStandInAnsweringEvery(t *testing.T, r reply) *standIn {
	t.Helper()
	return serveStandIn(t, &standIn{every: &r})
}

func serveStandIn(t *testing.T, s *standIn) *standIn {
	t.Helper()
	s.server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.server.Close)
	return s
}

// dropping makes the stand-in close the connection of its first n requests with no answer.
func (s *standIn) dropping(n int) *standIn {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drops = n
	return s
}

// holding makes the handler of each dropped request wait for held to close after it has closed
// the connection (standIn.held).
func (s *standIn) holding(held chan struct{}) *standIn {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held = held
	return s
}

func (s *standIn) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.got = append(s.got, recorded{path: r.URL.Path, query: r.URL.RawQuery, header: r.Header.Clone(), body: body})
	at := len(s.got) - 1
	drop := len(s.got) <= s.drops
	held := s.held
	// A reply an earlier request took and has not settled may yet come back to the head of the
	// script, so this request takes nothing until it has: an answer whose write failed is lost to
	// the client, which may already be retrying, before its handler learns of it.
	for s.unsettled != nil {
		unsettled := s.unsettled
		s.mu.Unlock()
		select {
		case <-unsettled:
		case <-r.Context().Done():
			s.mu.Lock()
			s.got[at].lost = true
			s.mu.Unlock()
			return
		}
		s.mu.Lock()
	}
	var next reply
	taken := false
	switch {
	case s.every != nil:
		next = *s.every
	case len(s.replies) > 0:
		next, taken = s.replies[0], true
		s.replies = s.replies[1:]
		s.unsettled = make(chan struct{})
	case !drop:
		s.mu.Unlock()
		http.Error(w, `{"type":"error","error":{"type":"invalid_request_error","message":"the stand-in has no more replies"}}`, http.StatusBadRequest)
		return
	}
	s.mu.Unlock()
	// settle ends this request's hold on the reply it took, once: an answer written whole keeps
	// it, and a lost one marks the request lost and puts the reply back at the head of the
	// script. Every way out of the handler but an answer written whole is a loss.
	settled := false
	settle := func(answered bool) {
		if settled {
			return
		}
		settled = true
		s.mu.Lock()
		defer s.mu.Unlock()
		if !answered {
			s.got[at].lost = true
			if taken {
				s.replies = append([]reply{next}, s.replies...)
			}
		}
		if taken {
			close(s.unsettled)
			s.unsettled = nil
		}
	}
	defer settle(false)
	if drop {
		// Settled before the close, the moment the client can see the loss.
		settle(false)
		if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
			_ = conn.Close()
		}
		if held != nil {
			<-held
		}
		return
	}
	if next.delay > 0 {
		select {
		case <-time.After(next.delay):
		case <-r.Context().Done():
			return
		}
	}
	for k, v := range next.headers {
		w.Header().Set(k, v)
	}
	if next.status != 0 && next.status != http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(next.status)
	} else {
		w.Header().Set("Content-Type", "text/event-stream")
	}
	// The flush puts the answer on the connection now, so a connection that broke under it is
	// a write error here rather than one the server meets after the handler has returned.
	_, err := w.Write(next.body)
	settle(err == nil && http.NewResponseController(w).Flush() == nil)
}

// requests are the model calls the stand-in got, in order: every request but one whose answer was
// lost and that the next request asks again, path, query and body alike, as the transport's one
// retry of a call whose connection broke does. A call and that retry are one model call, and a
// stand-in that got any request has at least one; attempts has every request.
func (s *standIn) requests() []recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	var calls []recorded
	for i, r := range s.got {
		if r.lost && i+1 < len(s.got) {
			if again := s.got[i+1]; again.path == r.path && again.query == r.query && bytes.Equal(again.body, r.body) {
				continue
			}
		}
		calls = append(calls, r)
	}
	return calls
}

// attempts are every request the stand-in got, in order: a retry and the call whose answer it
// replaces are two, as a row about the retry itself counts them.
func (s *standIn) attempts() []recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recorded(nil), s.got...)
}

// decoded is a recorded request's JSON body.
func (r recorded) decoded(t *testing.T) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(r.body, &body); err != nil {
		t.Fatalf("the request body is no JSON object: %v\n%s", err, r.body)
	}
	return body
}

type sseWriter struct{ bytes.Buffer }

func (w *sseWriter) event(name string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
}

// anthropicUsage is a Messages API usage object.
type anthropicUsage struct {
	Input, Output, CacheWrite, CacheRead int
}

func (u anthropicUsage) object() map[string]any {
	return map[string]any{
		"input_tokens": u.Input, "output_tokens": u.Output,
		"cache_creation_input_tokens": u.CacheWrite, "cache_read_input_tokens": u.CacheRead,
	}
}

// anthropicStream is the event stream the Messages API sends for a message of blocks (each a
// Messages API content block) that stops for stopReason.
func anthropicStream(stopReason string, usage anthropicUsage, blocks ...map[string]any) reply {
	var w sseWriter
	w.event("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_standin", "type": "message", "role": "assistant", "model": "claude-standin", "content": []any{},
		"stop_reason": nil, "stop_sequence": nil, "usage": usage.object(),
	}})
	for i, b := range blocks {
		switch b["type"] {
		case "text":
			w.event("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "text", "text": ""}})
			for _, delta := range textDeltas(b) {
				w.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "text_delta", "text": delta}})
			}
		case "thinking":
			w.event("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""}})
			w.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "thinking_delta", "thinking": b["thinking"]}})
			w.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "signature_delta", "signature": b["signature"]}})
		case "tool_use":
			w.event("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "tool_use", "id": b["id"], "name": b["name"], "input": map[string]any{}}})
			input, _ := json.Marshal(b["input"])
			w.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(input)}})
		default:
			panic(fmt.Sprintf("anthropicStream: a %v block", b["type"]))
		}
		w.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
	}
	w.event("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil}, "usage": usage.object()})
	w.event("message_stop", map[string]any{"type": "message_stop"})
	return reply{body: w.Bytes()}
}

func anthropicText(text string) map[string]any {
	return map[string]any{"type": "text", "text": text}
}

// anthropicTexts is a Messages API text block whose text arrives in several deltas, as a reply
// the model writes a piece at a time does.
func anthropicTexts(deltas ...string) map[string]any {
	return map[string]any{"type": "text", "text": strings.Join(deltas, ""), "deltas": deltas}
}

// textDeltas are the deltas one text block's text arrives in: the deltas anthropicTexts set, or
// the whole of the text as one.
func textDeltas(b map[string]any) []string {
	if deltas, ok := b["deltas"].([]string); ok {
		return deltas
	}
	return []string{b["text"].(string)}
}

func anthropicThinking(thinking, signature string) map[string]any {
	return map[string]any{"type": "thinking", "thinking": thinking, "signature": signature}
}

// anthropicToolUse is a Messages API tool_use block calling name with input as its arguments.
func anthropicToolUse(id, name string, input any) map[string]any {
	return map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}
}

// openaiUsage is a Responses API usage object.
type openaiUsage struct {
	Input, Cached, CacheWrite, Output, Reasoning int
}

func (u openaiUsage) object() map[string]any {
	return map[string]any{
		"input_tokens":          u.Input,
		"input_tokens_details":  map[string]any{"cached_tokens": u.Cached, "cache_write_tokens": u.CacheWrite},
		"output_tokens":         u.Output,
		"output_tokens_details": map[string]any{"reasoning_tokens": u.Reasoning},
		"total_tokens":          u.Input + u.Output,
	}
}

// openaiStream is the event stream the Responses API sends for a response of output items, each
// raw JSON as the response.output_item.done event carries it; incomplete, when set, is the reason
// the response did not complete.
func openaiStream(usage openaiUsage, incomplete string, items ...string) reply {
	seq := 0
	var w sseWriter
	event := func(name string, fields map[string]any) {
		fields["type"] = name
		fields["sequence_number"] = seq
		seq++
		w.event(name, fields)
	}
	response := func(status string, output []any, usage any) map[string]any {
		r := map[string]any{"id": "resp_standin", "object": "response", "created_at": 1, "status": status, "model": "gpt-standin", "output": output, "usage": usage}
		if incomplete != "" && status != "in_progress" {
			r["incomplete_details"] = map[string]any{"reason": incomplete}
		}
		return r
	}
	event("response.created", map[string]any{"response": response("in_progress", []any{}, nil)})
	var output []any
	for i, raw := range items {
		var item map[string]any
		if err := json.Unmarshal([]byte(raw), &item); err != nil {
			panic(err)
		}
		output = append(output, json.RawMessage(raw))
		id := item["id"]
		added := map[string]any{}
		for k, v := range item {
			added[k] = v
		}
		switch item["type"] {
		case "message":
			added["content"], added["status"] = []any{}, "in_progress"
			event("response.output_item.added", map[string]any{"output_index": i, "item": added})
			for ci, rc := range item["content"].([]any) {
				part := rc.(map[string]any)
				if part["type"] != "output_text" {
					continue
				}
				event("response.content_part.added", map[string]any{"item_id": id, "output_index": i, "content_index": ci, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})
				event("response.output_text.delta", map[string]any{"item_id": id, "output_index": i, "content_index": ci, "delta": part["text"], "logprobs": []any{}})
				event("response.output_text.done", map[string]any{"item_id": id, "output_index": i, "content_index": ci, "text": part["text"], "logprobs": []any{}})
				event("response.content_part.done", map[string]any{"item_id": id, "output_index": i, "content_index": ci, "part": part})
			}
		case "function_call":
			added["arguments"], added["status"] = "", "in_progress"
			event("response.output_item.added", map[string]any{"output_index": i, "item": added})
			event("response.function_call_arguments.delta", map[string]any{"item_id": id, "output_index": i, "delta": item["arguments"]})
			event("response.function_call_arguments.done", map[string]any{"item_id": id, "output_index": i, "arguments": item["arguments"]})
		case "reasoning":
			added["summary"] = []any{}
			delete(added, "encrypted_content")
			event("response.output_item.added", map[string]any{"output_index": i, "item": added})
			for si, rs := range item["summary"].([]any) {
				s := rs.(map[string]any)
				event("response.reasoning_summary_part.added", map[string]any{"item_id": id, "output_index": i, "summary_index": si, "part": map[string]any{"type": "summary_text", "text": ""}})
				event("response.reasoning_summary_text.delta", map[string]any{"item_id": id, "output_index": i, "summary_index": si, "delta": s["text"]})
				event("response.reasoning_summary_text.done", map[string]any{"item_id": id, "output_index": i, "summary_index": si, "text": s["text"]})
				event("response.reasoning_summary_part.done", map[string]any{"item_id": id, "output_index": i, "summary_index": si, "part": s})
			}
		default:
			panic(fmt.Sprintf("openaiStream: a %v item", item["type"]))
		}
		event("response.output_item.done", map[string]any{"output_index": i, "item": json.RawMessage(raw)})
	}
	final := "completed"
	if incomplete != "" {
		final = "incomplete"
	}
	event("response."+final, map[string]any{"response": response(final, output, usage.object())})
	return reply{body: w.Bytes()}
}

func openaiReasoning(id, encrypted, summary string) string {
	return `{"id":"` + id + `","type":"reasoning","encrypted_content":` + quote(encrypted) + `,"summary":[{"type":"summary_text","text":` + quote(summary) + `}]}`
}

// openaiToolCall is a Responses API function_call item calling name with input as its arguments.
func openaiToolCall(id, callID, name string, input any) string {
	arguments, err := json.Marshal(input)
	if err != nil {
		panic(err)
	}
	return `{"id":"` + id + `","type":"function_call","status":"completed","call_id":"` + callID + `","name":"` + name + `","arguments":` + quote(string(arguments)) + `}`
}

func openaiMessage(id, text string) string {
	return `{"id":"` + id + `","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":` + quote(text) + `,"annotations":[]}]}`
}

// openaiPhasedMessage is a Responses API message item the model produced in a phase, which the
// API names on the item itself.
func openaiPhasedMessage(id, phase, text string) string {
	return `{"id":"` + id + `","type":"message","role":"assistant","status":"completed","phase":` + quote(phase) +
		`,"content":[{"type":"output_text","text":` + quote(text) + `,"annotations":[]}]}`
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// chatUsage is a Chat Completions usage object: the prompt and completion counts, the prompt
// tokens the provider served from its cache, and the reasoning tokens.
type chatUsage struct {
	Prompt, Cached, Completion, Reasoning int
}

func (u chatUsage) object() map[string]any {
	return map[string]any{
		"prompt_tokens": u.Prompt, "completion_tokens": u.Completion,
		"total_tokens":              u.Prompt + u.Completion,
		"prompt_tokens_details":     map[string]any{"cached_tokens": u.Cached},
		"completion_tokens_details": map[string]any{"reasoning_tokens": u.Reasoning},
	}
}

// chatChunk is one chat.completion.chunk of a Chat Completions stream: its choices and, on the
// chunk the usage rides in, the usage.
func chatChunk(choices []any, usage any) map[string]any {
	c := map[string]any{
		"id": "chatcmpl-standin", "object": "chat.completion.chunk", "created": 1,
		"model": "gpt-standin", "choices": choices,
	}
	if usage != nil {
		c["usage"] = usage
	}
	return c
}

// chatDelta is one chunk's single choice: what the reply added, and the finish reason where the
// reply ended there.
func chatDelta(delta map[string]any, finish any) []any {
	return []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}
}

// chatStream is the event stream the Chat Completions API sends for a reply whose content
// arrives in deltas and that stops for finishReason, with the usage chunk the stream is asked
// for and the [DONE] line that ends it.
func chatStream(finishReason string, usage chatUsage, deltas ...string) reply {
	var w sseWriter
	w.event("chunk", chatChunk(chatDelta(map[string]any{"role": "assistant", "content": ""}, nil), nil))
	for _, delta := range deltas {
		w.event("chunk", chatChunk(chatDelta(map[string]any{"content": delta}, nil), nil))
	}
	w.event("chunk", chatChunk(chatDelta(map[string]any{}, finishReason), nil))
	w.event("chunk", chatChunk([]any{}, usage.object()))
	fmt.Fprint(&w, "data: [DONE]\n\n")
	return reply{body: w.Bytes()}
}

// chatRefusalStream is the event stream Chat Completions sends for a reply the model refused:
// the refusal arrives in the choice's delta, as OpenAI streams one, or on the choice's whole
// message, as a gateway that answers a stream request with the whole reply sends it. Either
// shape stops for "stop" and carries no content at all, so a refusal the body is not read for
// reaches the caller as an empty answer.
func chatRefusalStream(refusal string, whole bool) reply {
	var w sseWriter
	w.event("chunk", chatChunk(chatDelta(map[string]any{"role": "assistant", "content": ""}, nil), nil))
	if whole {
		w.event("chunk", chatChunk([]any{map[string]any{
			"index": 0, "message": map[string]any{"role": "assistant", "refusal": refusal}, "finish_reason": "stop",
		}}, nil))
	} else {
		w.event("chunk", chatChunk(chatDelta(map[string]any{"refusal": refusal}, nil), nil))
		w.event("chunk", chatChunk(chatDelta(map[string]any{}, "stop"), nil))
	}
	w.event("chunk", chatChunk([]any{}, chatUsage{Prompt: 2, Completion: 1}.object()))
	fmt.Fprint(&w, "data: [DONE]\n\n")
	return reply{body: w.Bytes()}
}
