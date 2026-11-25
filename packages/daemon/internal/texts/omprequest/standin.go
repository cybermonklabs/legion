package omprequest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testnats"
)

// Standin is the model of a real Oh My Pi a test starts: an `anthropic-messages` provider whose
// models.yml `baseUrl` is URL. It records every POST body and answers each Messages request with
// one text turn, in the server-sent events Anthropic streams, or, for a turn of the conversation,
// with the next tool call the test queued (CallTool). A turn is a request that carries a system
// prompt and tools and is not the plugin's own `<btw>` side question; Oh My Pi's skill-description
// compression requests carry no system prompt, so none of them is a turn. Any other POST, a token
// count, is answered a count, and a GET an empty model listing. It listens at a loopback address
// of its own (testnats.ListenLoopback), so a client a finished test left behind cannot reach it.
type Standin struct {
	// URL is the stand-in's base URL, with no path: Oh My Pi posts to URL/v1/messages.
	URL string

	mu     sync.Mutex
	bodies [][]byte
	turns  [][]byte
	calls  []ToolCall
}

// ToolCall is one call of the tool Name with Input that a turn is answered with.
type ToolCall struct {
	Name  string
	Input map[string]any
}

// NewStandin starts a stand-in, closed when the test ends.
func NewStandin(t testing.TB) *Standin {
	t.Helper()
	listener, err := testnats.ListenLoopback()
	if err != nil {
		t.Fatalf("listen for the model stand-in: %v", err)
	}
	s := &Standin{}
	server := httptest.NewUnstartedServer(s)
	server.Listener.Close()
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	s.URL = server.URL
	return s
}

// CallTool queues call: the next turn that finds no earlier call queued is answered with it.
func (s *Standin) CallTool(call ToolCall) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, call)
}

// Bodies are every POST body the stand-in has taken, in order.
func (s *Standin) Bodies() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.bodies...)
}

// Turns are the bodies of the conversation's turns, in order: the first is what Oh My Pi first
// offers its model (Read).
func (s *Standin) Turns() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.turns...)
}

func (s *Standin) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[]}`)
		return
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("read the request body: %v", err), http.StatusBadRequest)
		return
	}
	messages := strings.HasSuffix(req.URL.Path, "/v1/messages")
	turn := messages && isTurn(body)
	var call *ToolCall
	s.mu.Lock()
	s.bodies = append(s.bodies, body)
	if turn {
		s.turns = append(s.turns, body)
		if len(s.calls) > 0 {
			next := s.calls[0]
			call, s.calls = &next, s.calls[1:]
		}
	}
	id := fmt.Sprintf("toolu_standin_%d", len(s.bodies))
	s.mu.Unlock()
	if !messages {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"input_tokens":1000}`)
		return
	}
	var request struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &request)
	if call == nil {
		text, _ := json.Marshal(map[string]string{"type": "text_delta", "text": "Answered."})
		stream(w, request.Model, "end_turn", `{"type":"text","text":""}`, string(text))
		return
	}
	input, _ := json.Marshal(call.Input)
	block, _ := json.Marshal(map[string]any{"type": "tool_use", "id": id, "name": call.Name, "input": map[string]any{}})
	delta, _ := json.Marshal(map[string]string{"type": "input_json_delta", "partial_json": string(input)})
	stream(w, request.Model, "tool_use", string(block), string(delta))
}

// isTurn is whether body is a turn of the conversation: a request with a system prompt and tools
// whose newest message is not the plugin's `<btw>` side question.
func isTurn(body []byte) bool {
	var request struct {
		System   json.RawMessage   `json:"system"`
		Tools    []json.RawMessage `json:"tools"`
		Messages []json.RawMessage `json:"messages"`
	}
	if json.Unmarshal(body, &request) != nil || len(request.Tools) == 0 || len(request.Messages) == 0 {
		return false
	}
	if system, err := textBlocks(request.System); err != nil || len(system) == 0 {
		return false
	}
	return !strings.Contains(string(request.Messages[len(request.Messages)-1]), "<btw>")
}

// stream answers one Messages request with one content block, which opens as block and is written
// whole by the one delta after it, and a message that stops for stop: a text block ends the turn,
// and a tool_use block stops for tool_use, so Oh My Pi runs the tool and sends its result in its
// next request.
func stream(w http.ResponseWriter, model, stop, block, delta string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	event := func(name, data string) {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
		if flusher != nil {
			flusher.Flush()
		}
	}
	event("message_start", fmt.Sprintf(`{"type":"message_start","message":{"id":"msg_standin","type":"message","role":"assistant",`+
		`"model":%q,"content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1000,"output_tokens":1}}}`, model))
	event("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":0,"content_block":%s}`, block))
	event("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":%s}`, delta))
	event("content_block_stop", `{"type":"content_block_stop","index":0}`)
	event("message_delta", fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q,"stop_sequence":null},"usage":{"output_tokens":16}}`, stop))
	event("message_stop", `{"type":"message_stop"}`)
}
