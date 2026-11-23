// Package modeltest is the stand-in model of Dispatch's Go tests (INFRA-3231): one httptest
// server that answers both protocols a model group speaks, Anthropic Messages (POST /v1/messages)
// and OpenAI Chat Completions (POST <any path>/chat/completions), each whole or streamed as the
// request asks, from the replies a test scripts, and that keeps every request it got. A test
// points a model group at its URL, as DISPATCH_TEST_MODEL_URL points a test server's groups, so
// the code under test calls it over HTTP as it calls a deployment. It is kept out of the packages
// it serves so no test-only type ships in the production binary;
// packages/dispatch/e2e/fake-model.ts is its browser-harness counterpart.
package modeltest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// The protocols a Request names.
const (
	// Anthropic is Anthropic Messages, POST /v1/messages.
	Anthropic = "anthropic"
	// Chat is OpenAI Chat Completions, POST <any path>/chat/completions, which an Azure AI Foundry
	// chat deployment speaks.
	Chat = "chat"
)

// unscripted is the message of the error a request no rule answers gets.
const unscripted = "the stand-in model has no reply scripted for this request"

// Reply is what the stand-in answers one request with.
type Reply struct {
	// Text is the model's text.
	Text string
	// Status, when it is set and not 200, is answered with the protocol's error object, whose
	// message is Text, in place of a reply.
	Status int
	// Delay is how long the stand-in waits before it answers, or until the request is
	// abandoned.
	Delay time.Duration
}

// Rule answers the requests it matches with its Reply. A request is matched by the first rule,
// in the order they were scripted, whose fields that are set all hold.
type Rule struct {
	// Model, when set, matches a request that names it as its model: a group's deployment.
	Model string
	// Contains, when set, matches a request whose text (Request.Text) holds it.
	Contains string
	// Match, when set, matches a request it reports true for: a test that matches on more than
	// one part of the text at once.
	Match func(Request) bool
	// Once uses the rule up on the first request it answers, so rules scripted with it answer
	// the next requests in turn.
	Once  bool
	Reply Reply
}

// Request is one request the stand-in got.
type Request struct {
	// Protocol is Anthropic or Chat.
	Protocol string
	Path     string
	// Query is the URL's raw query, as it arrived.
	Query  string
	Header http.Header
	// Model is the model the request names.
	Model string
	// Text is the text of every message the request carries, its system prompt first, joined
	// by newlines.
	Text string
	// Stream is whether the request asked for a streamed reply.
	Stream bool
	Body   []byte
}

// Model is one stand-in model, serving from its rules until the test ends.
type Model struct {
	// URL is the base URL a model group's endpoint is set to: the client appends its route.
	URL string

	mu       sync.Mutex
	rules    []Rule
	requests []Request
}

// New starts a stand-in that answers by rules, and stops it when t ends.
func New(t testing.TB, rules ...Rule) *Model {
	t.Helper()
	m := &Model{rules: rules}
	server := httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(server.Close)
	m.URL = server.URL
	return m
}

// Script adds rules after those the stand-in holds.
func (m *Model) Script(rules ...Rule) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rules = append(m.rules, rules...)
}

// Requests are the requests the stand-in got, in the order they arrived.
func (m *Model) Requests() []Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Request(nil), m.requests...)
}

// wireRequest is the part of either protocol's request the stand-in reads.
type wireRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	System   any    `json:"system"`
	Messages []struct {
		Content any `json:"content"`
	} `json:"messages"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

func (m *Model) serve(w http.ResponseWriter, r *http.Request) {
	protocol := ""
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/messages":
		protocol = Anthropic
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/chat/completions"):
		protocol = Chat
	default:
		http.Error(w, "the stand-in model serves POST /v1/messages and POST .../chat/completions", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var wire wireRequest
	if err := json.Unmarshal(body, &wire); err != nil {
		writeError(w, protocol, http.StatusBadRequest, "the request is no JSON object: "+err.Error())
		return
	}
	texts := textsOf(wire.System)
	for _, message := range wire.Messages {
		texts = append(texts, textsOf(message.Content)...)
	}
	request := Request{
		Protocol: protocol, Path: r.URL.Path, Query: r.URL.RawQuery, Header: r.Header.Clone(),
		Model: wire.Model, Text: strings.Join(texts, "\n"), Stream: wire.Stream, Body: body,
	}
	reply, scripted := m.answer(request)
	if reply.Delay > 0 {
		select {
		case <-time.After(reply.Delay):
		case <-r.Context().Done():
			return
		}
	}
	switch {
	case !scripted:
		writeError(w, protocol, http.StatusInternalServerError, unscripted)
	case reply.Status != 0 && reply.Status != http.StatusOK:
		writeError(w, protocol, reply.Status, reply.Text)
	case protocol == Anthropic:
		writeAnthropic(w, request, reply.Text)
	default:
		writeChat(w, request, reply.Text, wire.StreamOptions != nil && wire.StreamOptions.IncludeUsage)
	}
}

// answer records request and finds the rule that answers it, using up a Once rule.
func (m *Model) answer(request Request) (Reply, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, request)
	for i, rule := range m.rules {
		if (rule.Model != "" && rule.Model != request.Model) || !strings.Contains(request.Text, rule.Contains) ||
			(rule.Match != nil && !rule.Match(request)) {
			continue
		}
		if rule.Once {
			m.rules = slices.Delete(m.rules, i, i+1)
		}
		return rule.Reply, true
	}
	return Reply{}, false
}

// textsOf is the text a system prompt or a message's content carries: the string itself, or the
// text of each of its blocks or parts, a tool result's content included.
func textsOf(content any) []string {
	switch value := content.(type) {
	case string:
		return []string{value}
	case []any:
		var texts []string
		for _, part := range value {
			texts = append(texts, textsOf(part)...)
		}
		return texts
	case map[string]any:
		if text, ok := value["text"].(string); ok {
			return []string{text}
		}
		return textsOf(value["content"])
	}
	return nil
}

// tokens is a rough count of text's tokens, four characters a token, for a reply's usage.
func tokens(text string) int {
	return max(utf8.RuneCountInString(text)/4, 1)
}

func writeError(w http.ResponseWriter, protocol string, status int, message string) {
	body := map[string]any{"error": map[string]any{"message": message, "type": "server_error"}}
	if protocol == Anthropic {
		body = map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": message}}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeAnthropic(w http.ResponseWriter, request Request, text string) {
	usage := map[string]any{"input_tokens": tokens(request.Text), "output_tokens": tokens(text)}
	if !request.Stream {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "msg_standin", "type": "message", "role": "assistant", "model": request.Model,
			"content":     []any{map[string]any{"type": "text", "text": text}},
			"stop_reason": "end_turn", "stop_sequence": nil, "usage": usage,
		})
		return
	}
	events := newEvents(w)
	events.send("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_standin", "type": "message", "role": "assistant", "model": request.Model, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": usage["input_tokens"], "output_tokens": 0},
	}})
	events.send("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
	events.send("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text}})
	events.send("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	events.send("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": usage})
	events.send("message_stop", map[string]any{"type": "message_stop"})
}

func writeChat(w http.ResponseWriter, request Request, text string, includeUsage bool) {
	usage := map[string]any{"prompt_tokens": tokens(request.Text), "completion_tokens": tokens(text), "total_tokens": tokens(request.Text) + tokens(text)}
	if !request.Stream {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-standin", "object": "chat.completion", "created": 1, "model": request.Model,
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": text}, "finish_reason": "stop"}},
			"usage":   usage,
		})
		return
	}
	chunk := func(choices []any, usage any) map[string]any {
		return map[string]any{"id": "chatcmpl-standin", "object": "chat.completion.chunk", "created": 1, "model": request.Model, "choices": choices, "usage": usage}
	}
	events := newEvents(w)
	events.send("", chunk([]any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": text}, "finish_reason": nil}}, nil))
	events.send("", chunk([]any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}, nil))
	if includeUsage {
		events.send("", chunk([]any{}, usage))
	}
	events.done()
}

// events writes a server-sent event stream.
type events struct{ w http.ResponseWriter }

func newEvents(w http.ResponseWriter) events {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	return events{w}
}

// send writes one event, named where name is set, whose data is v as JSON.
func (e events) send(name string, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	if name != "" {
		fmt.Fprintf(e.w, "event: %s\n", name)
	}
	fmt.Fprintf(e.w, "data: %s\n\n", data)
}

// done ends a Chat Completions stream.
func (e events) done() {
	fmt.Fprint(e.w, "data: [DONE]\n\n")
}
