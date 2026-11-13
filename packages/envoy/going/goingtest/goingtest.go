// Package goingtest holds the fake text model shared by this module's own tests and its callers'
// tests, kept out of the going package proper so no test-only type ships in the production
// binary. It serves the chat-completions shape over httptest, so a test drives the real model
// loop - agentloop's openai-chat provider, its route, its sign-in, its reading of the stream and
// its reading of a structured reply - rather than a stub of it.
package goingtest

import (
	"cmp"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/going"
)

// defaultChunk is how many runes a frame carries when a Reply names no size: small enough that
// any reply of a few words arrives in several frames, as a real completion does.
const defaultChunk = 8

// Reply is what the model answers one call with.
type Reply struct {
	// Text is streamed as the turn's content whatever the call asked for, as a model that
	// ignores a response format writes: a call that asked for a structured reply and is
	// answered Text reads a reply that does not fit the schema.
	Text string
	// Final is the turn's answer in the form the call asks for it: the object going's Final
	// schema describes, written answer first, when the call carries a response_format, and
	// its Answer alone as prose when it does not.
	Final *Final
	// RefuseFormat answers a call that carries a response_format with a 400, as a service
	// that does not serve structured outputs does, and serves the reply to any call that
	// carries none.
	RefuseFormat bool
	// ToolCalls is a turn that ends in tool calls instead of an answer.
	ToolCalls []ToolCall
	// Chunk is how many runes each frame carries; zero picks a small default.
	Chunk int
	// Pause is how long the model waits between frames, so a test can assert the first byte
	// arrives early.
	Pause time.Duration
	// Silent never answers: the response is held open until the caller's context ends.
	Silent bool
	// Status, when non-zero, is answered with an error body instead of a completion.
	Status int
	// Message is the error body's message of a Status answer; empty is a generic one.
	Message string
}

// Final is a structured answer: the answer's text, the block drawn beside it and the arguments
// of the offered actions, as going's Final schema names them.
type Final struct {
	Answer    string
	Block     Block
	Arguments string
}

// Block is a structured answer's block. An empty Kind is written as none.
type Block struct {
	Kind  string
	Title string
	Spec  string
}

// ToolCall is one call the model makes, with its arguments as the JSON text it writes.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// Message is one message of a call's conversation as the model received it. A tool call's
// result is a message whose Role is tool and whose ToolCallID names the call it answers.
type Message struct {
	Role       string
	Content    string
	ToolCalls  []ToolCall
	ToolCallID string
}

// Tool is one tool a call offered the model, with its arguments' JSON Schema as Parameters.
type Tool struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// ReceivedRequest is one call as the model received it, decoded.
type ReceivedRequest struct {
	Model    string
	Messages []Message
	Tools    []Tool
	// ResponseFormat is the call's response_format as it was sent, and nil for a call that
	// carried none.
	ResponseFormat json.RawMessage
	// MaxTokens is the call's max_tokens, the output bound it was sent with.
	MaxTokens     int
	Authorization string
	Path          string
	RawQuery      string
}

// Structured reports whether the call asked for a structured reply.
func (r ReceivedRequest) Structured() bool { return len(r.ResponseFormat) > 0 }

// Model is a chat-completions service a test configures a model against: it answers each call
// with the reply the test named, streams it frame by frame as a real completion does, and counts
// what it served. It calls nothing and holds no credential.
type Model struct {
	// URL is the base URL to configure an endpoint with; the provider appends the route.
	URL string

	replies []Reply

	mu        sync.Mutex
	requests  []ReceivedRequest
	abandoned int
}

// NewModel starts a model that serves reply i to call i, the last reply serving every call after
// it, and closes with the test. A model with no replies answers every call with an empty
// completion.
func NewModel(t testing.TB, replies ...Reply) *Model {
	t.Helper()
	model := &Model{replies: replies}
	server := httptest.NewServer(http.HandlerFunc(model.serve))
	t.Cleanup(server.Close)
	model.URL = server.URL
	return model
}

// Calls reports how many calls the model has served.
func (m *Model) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.requests)
}

// Requests reports the decoded bodies the model received, in order.
func (m *Model) Requests() []ReceivedRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]ReceivedRequest(nil), m.requests...)
}

// Abandoned reports how many replies the caller went away from before the model finished
// writing them: the caller's request ended while a reply was still streaming, which is what a
// run cancelled under its answer looks like from the model's side.
func (m *Model) Abandoned() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.abandoned
}

// serve records one call and answers it with the reply that call's turn names.
func (m *Model) serve(w http.ResponseWriter, r *http.Request) {
	var body wireRequest
	_ = json.NewDecoder(r.Body).Decode(&body)
	received := ReceivedRequest{
		Model:          body.Model,
		Messages:       body.messages(),
		Tools:          body.tools(),
		ResponseFormat: body.ResponseFormat,
		MaxTokens:      body.MaxTokens,
		Authorization:  r.Header.Get("Authorization"),
		Path:           r.URL.Path,
		RawQuery:       r.URL.RawQuery,
	}
	m.mu.Lock()
	m.requests = append(m.requests, received)
	call := len(m.requests)
	m.mu.Unlock()

	reply := Reply{}
	if len(m.replies) > 0 {
		reply = m.replies[min(call, len(m.replies))-1]
	}
	switch {
	case reply.Status != 0:
		refuse(w, reply.Status, cmp.Or(reply.Message, "the model service refused this call"))
	case reply.RefuseFormat && received.Structured():
		refuse(w, http.StatusBadRequest, "response_format is not supported by this model")
	case reply.Silent:
		<-r.Context().Done()
	default:
		if !stream(r, w, reply, reply.content(received.Structured())) {
			m.mu.Lock()
			m.abandoned++
			m.mu.Unlock()
		}
	}
}

// refuse answers a call with an error status and the service's error body.
func refuse(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body, _ := json.Marshal(map[string]any{"error": map[string]any{
		"code": "simulated_failure", "type": "invalid_request_error", "message": message,
	}})
	_, _ = w.Write(body)
}

// content is the text the turn writes: the Final object or its answer, as the call asked for
// it, or Text where the reply names no Final.
func (r Reply) content(structured bool) string {
	if r.Final == nil {
		return r.Text
	}
	if !structured {
		return r.Final.Answer
	}
	kind := r.Final.Block.Kind
	if kind == "" {
		kind = going.BlockNone
	}
	// Marshalled in the fields' declared order, so the answer is the object's first property as
	// the schema orders it and streams first, as a model honouring the schema writes it.
	final := struct {
		Answer string `json:"answer"`
		Block  struct {
			Kind  string `json:"kind"`
			Title string `json:"title"`
			Spec  string `json:"spec"`
		} `json:"block"`
		Arguments string `json:"arguments"`
	}{Answer: r.Final.Answer, Arguments: r.Final.Arguments}
	final.Block.Kind, final.Block.Title, final.Block.Spec = kind, r.Final.Block.Title, r.Final.Block.Spec
	out, _ := json.Marshal(final)
	return string(out)
}

// stream writes the reply as a real completion does: the content in frames of Chunk runes, then
// each tool call, its arguments spread over frames too, then the turn's finish_reason and [DONE].
// It answers false where the caller went away before the reply was written whole, and stops
// writing there.
func stream(r *http.Request, w http.ResponseWriter, reply Reply, content string) bool {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	control := http.NewResponseController(w)
	_ = control.Flush()
	chunk := reply.Chunk
	if chunk <= 0 {
		chunk = defaultChunk
	}
	wrote := false
	gone := false
	write := func(choice wireChoice) {
		if gone {
			return
		}
		if wrote && reply.Pause > 0 {
			select {
			case <-r.Context().Done():
			case <-time.After(reply.Pause):
			}
		}
		if r.Context().Err() != nil {
			gone = true
			return
		}
		wrote = true
		frame, _ := json.Marshal(wireFrame{
			ID: "chatcmpl-goingtest", Object: "chat.completion.chunk", Created: 1, Model: "goingtest",
			Choices: []wireChoice{choice},
		})
		_, _ = w.Write([]byte("data: " + string(frame) + "\n\n"))
		_ = control.Flush()
	}
	write(wireChoice{Delta: wireDelta{Role: "assistant"}})
	for _, piece := range pieces(content, chunk) {
		write(wireChoice{Delta: wireDelta{Content: piece}})
	}
	for index, call := range reply.ToolCalls {
		arguments := pieces(call.Arguments, chunk)
		head := wireToolCallDelta{Index: index, ID: call.ID, Type: "function"}
		head.Function.Name = call.Name
		if len(arguments) > 0 {
			head.Function.Arguments = arguments[0]
		}
		write(wireChoice{Delta: wireDelta{ToolCalls: []wireToolCallDelta{head}}})
		for _, piece := range arguments[min(1, len(arguments)):] {
			tail := wireToolCallDelta{Index: index}
			tail.Function.Arguments = piece
			write(wireChoice{Delta: wireDelta{ToolCalls: []wireToolCallDelta{tail}}})
		}
	}
	finish := "stop"
	if len(reply.ToolCalls) > 0 {
		finish = "tool_calls"
	}
	write(wireChoice{FinishReason: &finish})
	if gone {
		return false
	}
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	_ = control.Flush()
	return true
}

// pieces cuts text into runs of at most size runes, so a frame never splits a rune.
func pieces(text string, size int) []string {
	runes := []rune(text)
	var out []string
	for start := 0; start < len(runes); start += size {
		out = append(out, string(runes[start:min(start+size, len(runes))]))
	}
	return out
}

// The chat-completions shapes the model speaks. They are declared here rather than taken from
// the provider, which keeps its own unexported, because a fake that speaks the protocol is what
// proves the provider speaks it too.
type wireRequest struct {
	Model          string          `json:"model"`
	Stream         bool            `json:"stream"`
	Messages       []wireMessage   `json:"messages"`
	Tools          []wireTool      `json:"tools"`
	ResponseFormat json.RawMessage `json:"response_format"`
	MaxTokens      int             `json:"max_tokens"`
}

type wireMessage struct {
	Role string `json:"role"`
	// Content is a string, or an array of parts of which the text parts are read.
	Content    json.RawMessage `json:"content"`
	ToolCalls  []wireToolCall  `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
}

// text is a message's content as one string: the string itself, or its text parts joined.
func (m wireMessage) text() string {
	var whole string
	if json.Unmarshal(m.Content, &whole) == nil {
		return whole
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(m.Content, &parts)
	var out strings.Builder
	for _, part := range parts {
		if part.Type == "text" {
			out.WriteString(part.Text)
		}
	}
	return out.String()
}

type wireToolCall struct {
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type wireTool struct {
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

// messages is the request's conversation, decoded.
func (r wireRequest) messages() []Message {
	var messages []Message
	for _, message := range r.Messages {
		converted := Message{Role: message.Role, Content: message.text(), ToolCallID: message.ToolCallID}
		for _, call := range message.ToolCalls {
			converted.ToolCalls = append(converted.ToolCalls, ToolCall{
				ID:        call.ID,
				Name:      call.Function.Name,
				Arguments: call.Function.Arguments,
			})
		}
		messages = append(messages, converted)
	}
	return messages
}

// tools is the request's tools, decoded.
func (r wireRequest) tools() []Tool {
	var tools []Tool
	for _, tool := range r.Tools {
		tools = append(tools, Tool{
			Name:        tool.Function.Name,
			Description: tool.Function.Description,
			Parameters:  tool.Function.Parameters,
		})
	}
	return tools
}

type wireFrame struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []wireChoice `json:"choices"`
}

type wireChoice struct {
	Index        int       `json:"index"`
	Delta        wireDelta `json:"delta"`
	FinishReason *string   `json:"finish_reason"`
}

type wireDelta struct {
	Role      string              `json:"role,omitempty"`
	Content   string              `json:"content,omitempty"`
	ToolCalls []wireToolCallDelta `json:"tool_calls,omitempty"`
}

type wireToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}
