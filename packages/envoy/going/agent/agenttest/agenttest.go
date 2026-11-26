// Package agenttest is a fake chat model for tests of the command bar's agent: an httptest server
// that speaks the Chat Completions streaming wire the agent's loop calls (agentloop's openai-chat
// protocol, POST /chat/completions), answers each request with what the test's script names, and
// records every request, when it came and when its client left it. The api package's tests run
// the agent's loop in their own process against it, and cmd/dispatch's tests point their agent
// process at it over https, trusting its certificate through SSL_CERT_FILE.
package agenttest

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/agentloop"
)

// ModelID is the model the fake answers as, which a test names in the agent's settings.
const ModelID = "agenttest-chat"

// Token is the bearer token the fake expects, which a test writes to the agent's token file.
const Token = "agenttest-chat-token"

// Call is one tool call a reply asks for: the tool's name and its arguments.
type Call struct {
	Name      string
	Arguments any
}

// Reply is what the fake answers one request with. A Status other than 0 and 200 answers that
// status with an error body, Error where it is set and the fake's own otherwise, and RetryAfter
// its Retry-After header. Otherwise the reply streams Padding bytes of SSE comments, which a
// reader skips, then Text, in one piece, then Calls, and finishes with Finish ("stop", or
// "tool_calls" where it calls a tool, by default), having written Output completion tokens, of
// which it counts Reasoning as reasoning tokens, for a prompt it counts at Prompt tokens, 10
// where Prompt is 0.
// Hold, where set, is waited on before anything is sent, until it closes or the client leaves;
// Endless streams text with no end until the client leaves.
type Reply struct {
	Status     int
	Error      string
	RetryAfter string
	Padding    int
	Text       string
	Calls      []Call
	Finish     string
	Prompt     int
	Output     int
	Reasoning  int
	Hold       <-chan struct{}
	Endless    bool
}

// FilterRefusal is the 400 an Azure deployment answers a prompt its content filter blocked with,
// a Reply's Error under http.StatusBadRequest: one stream chunk that stops for content_filter
// (chat-calls.log of the eval's handoff-02).
const FilterRefusal = `data: {"created":1,"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"delta":{},"finish_reason":"content_filter","index":0,` +
	`"content_filter_results":{"error":{"code":"content_filter","message":"Response content blocked by label 'Jailbreak'."}}}]}` + "\n\n"

// Asked is how the agent's loop gives the model the person's words of the run it works (going/
// agent's conversation): this line, then the words and the end of their line, which Asks matches
// whole, so words that begin another run's words do not match that run.
const Asked = "The person asks now:\n"

// Request is one request the fake got: when it came, its body, and, once its stream ended, when
// and whether its client left it before the reply was whole.
type Request struct {
	At   time.Time
	Body json.RawMessage

	mu   sync.Mutex
	left time.Time
}

// Text is every message's text content of the request, joined, which a script keys its replies
// on: the person's words of the run among it.
func (r *Request) Text() string {
	var body struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(r.Body, &body)
	var texts []string
	for _, message := range body.Messages {
		var text string
		if json.Unmarshal(message.Content, &text) == nil {
			texts = append(texts, text)
			continue
		}
		var parts []struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(message.Content, &parts) == nil {
			for _, part := range parts {
				texts = append(texts, part.Text)
			}
		}
	}
	return strings.Join(texts, "\n")
}

// Field is one top-level member of the request's body, nil where it has none.
func (r *Request) Field(name string) any {
	var body map[string]any
	_ = json.Unmarshal(r.Body, &body)
	return body[name]
}

// Messages is the request's messages, each as its JSON object.
func (r *Request) Messages() []map[string]any {
	var body struct {
		Messages []map[string]any `json:"messages"`
	}
	_ = json.Unmarshal(r.Body, &body)
	return body.Messages
}

// Left is when the request's client left it before its reply was whole, and false while it has
// not.
func (r *Request) Left() (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.left, !r.left.IsZero()
}

// Model is the fake chat model.
type Model struct {
	server *httptest.Server
	script func(*Request) Reply
	// closing is closed as the test ends, before the server closes, which ends every reply held
	// or endless, since the server's close waits for each request to end.
	closing chan struct{}

	mu       sync.Mutex
	requests []*Request
}

// New is a fake chat model over http, which answers each request with script's reply to it.
func New(t testing.TB, script func(*Request) Reply) *Model {
	t.Helper()
	return start(t, script, httptest.NewServer)
}

// NewTLS is a fake chat model over https, whose certificate CertFile writes.
func NewTLS(t testing.TB, script func(*Request) Reply) *Model {
	t.Helper()
	return start(t, script, httptest.NewTLSServer)
}

// start is the fake on a server serve makes.
func start(t testing.TB, script func(*Request) Reply, serve func(http.Handler) *httptest.Server) *Model {
	m := &Model{script: script, closing: make(chan struct{})}
	m.server = serve(http.HandlerFunc(m.serve))
	t.Cleanup(m.server.Close)
	t.Cleanup(func() { close(m.closing) })
	return m
}

// URL is the fake's base URL, which /chat/completions is appended to.
func (m *Model) URL() string { return m.server.URL }

// CertFile writes the certificate of a NewTLS fake to a file of the test's own, as SSL_CERT_FILE
// names one, and answers its path.
func (m *Model) CertFile(t testing.TB) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agenttest-chat.pem")
	block := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: m.server.Certificate().Raw})
	if err := os.WriteFile(path, block, 0o600); err != nil {
		t.Fatalf("write the fake chat model's certificate: %v", err)
	}
	return path
}

// TokenFile writes Token to a file of the test's own, as DISPATCH_CHAT_TOKEN_FILE names one, and
// answers its path.
func TokenFile(t testing.TB) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agenttest-chat-token")
	if err := os.WriteFile(path, []byte(Token+"\n"), 0o600); err != nil {
		t.Fatalf("write the fake chat model's token: %v", err)
	}
	return path
}

// Model is the fake as the agent's loop calls it in the test's own process.
func (m *Model) Model() agentloop.Model {
	return agentloop.Model{
		Endpoint: agentloop.Endpoint{Kind: agentloop.OpenAIChat, BaseURL: m.server.URL,
			Token: func(context.Context) (string, error) { return Token, nil }},
		ID: ModelID,
	}
}

// Requests is every request the fake got, in order.
func (m *Model) Requests() []*Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*Request(nil), m.requests...)
}

// Count is how many requests the fake got.
func (m *Model) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.requests)
}

// Matching is every request whose Text holds words, in order.
func (m *Model) Matching(words string) []*Request {
	var matching []*Request
	for _, request := range m.Requests() {
		if strings.Contains(request.Text(), words) {
			matching = append(matching, request)
		}
	}
	return matching
}

// Asking is every request of the run whose person's words are words, in order: the requests that
// give words as what the person asks now, and not as an earlier turn of the conversation.
func (m *Model) Asking(words string) []*Request {
	return m.Matching(Asked + words + "\n")
}

// Asks reports whether the request is of the run whose person's words are words.
func (r *Request) Asks(words string) bool {
	return strings.Contains(r.Text(), Asked+words+"\n")
}

// ToolResults is the text of each tool result the request gives the model, in order.
func (r *Request) ToolResults() []string {
	var results []string
	for _, message := range r.Messages() {
		if message["role"] != "tool" {
			continue
		}
		switch content := message["content"].(type) {
		case string:
			results = append(results, content)
		case []any:
			var parts []string
			for _, part := range content {
				if part, ok := part.(map[string]any); ok {
					if text, ok := part["text"].(string); ok {
						parts = append(parts, text)
					}
				}
			}
			results = append(results, strings.Join(parts, "\n"))
		}
	}
	return results
}

func (m *Model) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	request := &Request{At: time.Now(), Body: body}
	m.mu.Lock()
	m.requests = append(m.requests, request)
	m.mu.Unlock()
	if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer "+Token {
		http.Error(w, `{"error":{"message":"the fake chat model serves POST /chat/completions under its token"}}`, http.StatusUnauthorized)
		return
	}
	reply := m.script(request)
	left := func() {
		request.mu.Lock()
		request.left = time.Now()
		request.mu.Unlock()
	}
	if reply.Hold != nil {
		select {
		case <-reply.Hold:
		case <-r.Context().Done():
			left()
			return
		case <-m.closing:
			return
		}
	}
	if reply.Status != 0 && reply.Status != http.StatusOK {
		if reply.RetryAfter != "" {
			w.Header().Set("Retry-After", reply.RetryAfter)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(reply.Status)
		if reply.Error != "" {
			_, _ = io.WriteString(w, reply.Error)
			return
		}
		_, _ = fmt.Fprintf(w, `{"error":{"message":"the fake chat model answers %d"}}`, reply.Status)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	send := func(chunk any) bool {
		encoded, err := json.Marshal(chunk)
		if err != nil {
			panic(err)
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", encoded); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	if reply.Padding > 0 {
		line := ": " + strings.Repeat("x", 4094) + "\n\n"
		for sent := 0; sent < reply.Padding; sent += len(line) {
			if _, err := fmt.Fprint(w, line); err != nil {
				left()
				return
			}
		}
	}
	if !send(chunk(map[string]any{"role": "assistant", "content": ""}, nil)) {
		left()
		return
	}
	if reply.Endless {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-m.closing:
				return
			case <-r.Context().Done():
				left()
				return
			case <-ticker.C:
			}
			if !send(chunk(map[string]any{"content": "and more "}, nil)) {
				left()
				return
			}
		}
	}
	if reply.Text != "" && !send(chunk(map[string]any{"content": reply.Text}, nil)) {
		left()
		return
	}
	for index, call := range reply.Calls {
		arguments, err := json.Marshal(call.Arguments)
		if err != nil {
			panic(err)
		}
		delta := map[string]any{"tool_calls": []any{map[string]any{
			"index": index, "id": "call_" + strconv.Itoa(len(m.Requests())) + "_" + strconv.Itoa(index), "type": "function",
			"function": map[string]any{"name": call.Name, "arguments": string(arguments)},
		}}}
		if !send(chunk(delta, nil)) {
			left()
			return
		}
	}
	finish := reply.Finish
	if finish == "" {
		finish = "stop"
		if len(reply.Calls) > 0 {
			finish = "tool_calls"
		}
	}
	output, prompt := max(reply.Output, 1), reply.Prompt
	if prompt == 0 {
		prompt = 10
	}
	usage := map[string]any{"prompt_tokens": prompt, "completion_tokens": output, "total_tokens": prompt + output}
	if reply.Reasoning > 0 {
		usage["completion_tokens_details"] = map[string]any{"reasoning_tokens": reply.Reasoning}
	}
	if !send(chunk(map[string]any{}, finish)) || !send(map[string]any{
		"id": "chatcmpl-agenttest", "object": "chat.completion.chunk", "created": 1, "model": ModelID, "choices": []any{},
		"usage": usage,
	}) {
		left()
		return
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
}

// chunk is one chat.completion.chunk whose one choice adds delta, finishing for finish where it
// is not nil.
func chunk(delta map[string]any, finish any) map[string]any {
	return map[string]any{
		"id": "chatcmpl-agenttest", "object": "chat.completion.chunk", "created": 1, "model": ModelID,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	}
}
