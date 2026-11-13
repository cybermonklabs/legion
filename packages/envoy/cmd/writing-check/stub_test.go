package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/reader"
)

// stubModelID is the model id the stub's replies name.
const stubModelID = "stub-model-2026"

// stubModel is a Chat Completions endpoint standing in for the reader's model. It calls a
// candidate vacuous when vacuous says so of its text, and refuses with a 400 (which agentloop
// does not retry) a call that carries a candidate whose text fail picks.
type stubModel struct {
	server *httptest.Server
	calls  atomic.Int64
}

func newStubModel(t *testing.T, vacuous, fail func(text string) bool) *stubModel {
	t.Helper()
	s := &stubModel{}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(raw, &body)
		var sent struct {
			Candidates []struct {
				ID   string `json:"id"`
				Text string `json:"text"`
			} `json:"candidates"`
		}
		for _, m := range body.Messages {
			if m.Role == "user" {
				_ = json.Unmarshal([]byte(stubContent(m.Content)), &sent)
			}
		}
		reply := reader.Reply{Vacuous: []reader.Named{}}
		for _, c := range sent.Candidates {
			if fail != nil && fail(c.Text) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"message":"the stub refuses","type":"invalid_request_error"}}`)
				return
			}
			if vacuous(c.Text) {
				reply.Vacuous = append(reply.Vacuous, reader.Named{ID: c.ID, Rewrite: "Say the fact."})
			}
		}
		text, _ := json.Marshal(reply)
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := func(choices []any, usage any) {
			c := map[string]any{"id": "chatcmpl-stub", "object": "chat.completion.chunk", "created": 1, "model": stubModelID, "choices": choices}
			if usage != nil {
				c["usage"] = usage
			}
			b, _ := json.Marshal(c)
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
		delta := func(d map[string]any, finish any) []any {
			return []any{map[string]any{"index": 0, "delta": d, "finish_reason": finish}}
		}
		chunk(delta(map[string]any{"role": "assistant", "content": ""}, nil), nil)
		chunk(delta(map[string]any{"content": string(text)}, nil), nil)
		chunk(delta(map[string]any{}, "stop"), nil)
		chunk([]any{}, map[string]any{"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120})
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(s.server.Close)
	return s
}

// args are the reader flags that reach the stub, with a key file in dir.
func (s *stubModel) args(t *testing.T, dir string) []string {
	t.Helper()
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, []byte("stub-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{"--kind", "openai-chat", "--endpoint", s.server.URL + "/", "--deployment", "stub-deployment",
		"--key-file", key, "--deadline", "10s", "--model-id", stubModelID, "--concurrency", "2"}
}

// stubContent is a chat message's content as text, sent as a string or as parts.
func stubContent(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, part := range c {
			if p, _ := part.(map[string]any); p["type"] == "text" {
				b.WriteString(fmt.Sprint(p["text"]))
			}
		}
		return b.String()
	}
	return ""
}

// containsAny is a matcher of texts holding any of the words.
func containsAny(words ...string) func(string) bool {
	return func(text string) bool {
		for _, w := range words {
			if strings.Contains(text, w) {
				return true
			}
		}
		return false
	}
}

// runCmd runs one subcommand and answers its status, stdout and stderr.
func runCmd(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr strings.Builder
	code := run(t.Context(), args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}
