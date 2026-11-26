package answerloop

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/answer"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/websearch"
)

// webSearchToolsFunc opens each answer's web_search tool with a function of the test's own.
type webSearchToolsFunc func(question string) (answer.WebSearch, error)

func (f webSearchToolsFunc) Open(question string) (answer.WebSearch, error) { return f(question) }

// anthropicStandIn is the Anthropic Messages endpoint the loop's model calls and its web searches
// both go to: it answers each request with the next of its replies, the loop's calls an event
// stream and a search a Messages response, and records each request's body.
type anthropicStandIn struct {
	*httptest.Server
	mu      sync.Mutex
	replies []string
	bodies  []string
}

func newAnthropicStandIn(t *testing.T, replies ...string) *anthropicStandIn {
	s := &anthropicStandIn{replies: replies}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.bodies = append(s.bodies, string(body))
		if len(s.replies) == 0 {
			s.mu.Unlock()
			http.Error(w, `{"type":"error","error":{"type":"invalid_request_error","message":"no more replies"}}`, http.StatusBadRequest)
			return
		}
		next := s.replies[0]
		s.replies = s.replies[1:]
		s.mu.Unlock()
		if strings.HasPrefix(next, "event: ") {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		_, _ = io.WriteString(w, next)
	}))
	t.Cleanup(s.Close)
	return s
}

// anthropicEvents is the event stream of one Messages call whose one content block is block (a
// text block or a tool_use block), stopping for stopReason, with input and output its tokens.
func anthropicEvents(stopReason string, input, output int, block map[string]any) string {
	var b bytes.Buffer
	event := func(name string, v map[string]any) {
		encoded, _ := json.Marshal(v)
		fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", name, encoded)
	}
	usage := map[string]any{"input_tokens": input, "output_tokens": output, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0}
	event("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_standin", "type": "message", "role": "assistant", "model": "claude-opus-5-5", "content": []any{},
		"stop_reason": nil, "stop_sequence": nil, "usage": usage,
	}})
	switch block["type"] {
	case "text":
		event("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
		event("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": block["text"]}})
	case "tool_use":
		event("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": block["id"], "name": block["name"], "input": map[string]any{}}})
		input, _ := json.Marshal(block["input"])
		event("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(input)}})
	}
	event("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	event("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil}, "usage": usage})
	event("message_stop", map[string]any{"type": "message_stop"})
	return b.String()
}

// An answer whose model calls web_search posts its reply as any answer does, the real model loop
// and the real web_search tool against a stand-in Anthropic endpoint: the loop opens the answer's
// web_search tool for the question and hands it to the model loop beside gh, the search is
// recorded as the question's web_search call, and the day's spend holds the answer's whole cost,
// its model calls' tokens and its search's, tokens and fee. A loop with no web_search tool, a
// daemon whose answers configures none, hands the model loop none.
func TestAnAnswerThatCallsWebSearchPostsItsReplyAndTheDaysSpendHoldsTheFee(t *testing.T) {
	search, err := json.Marshal(map[string]any{
		"id": "msg_search", "type": "message", "role": "assistant", "model": "claude-opus-5-5", "stop_reason": "end_turn",
		"content": []any{
			map[string]any{"type": "server_tool_use", "id": "srvtoolu_1", "name": "web_search", "input": map[string]any{"query": "when do plums ripen"}},
			map[string]any{"type": "web_search_tool_result", "tool_use_id": "srvtoolu_1", "content": []any{
				map[string]any{"type": "web_search_result", "url": "https://example.org/plums", "title": "Plums", "encrypted_content": "Eq"},
			}},
			map[string]any{"type": "text", "text": "Plums ripen in August.", "citations": []any{
				map[string]any{"type": "web_search_result_location", "url": "https://example.org/plums", "cited_text": "August"},
			}},
		},
		"usage": map[string]any{"input_tokens": 1000, "output_tokens": 200, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	const reply = "Plums ripen in August, says https://example.org/plums."
	endpoint := newAnthropicStandIn(t,
		anthropicEvents("tool_use", 10, 20, map[string]any{"type": "tool_use", "id": "toolu_1", "name": "web_search", "input": map[string]any{"query": "when do plums ripen"}}),
		string(search),
		anthropicEvents("end_turn", 3, 15, map[string]any{"type": "text", "text": `{"kind":"answered","text":"` + reply + `","needs":""}`}),
	)
	keyFile := filepath.Join(t.TempDir(), "anthropic-key")
	if err := os.WriteFile(keyFile, []byte("sk-ant-standin\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	price := config.AnswerPrice{Input: 5, Output: 25, CacheWrite: 6.25, CacheRead: 0.5}
	anthropic := config.AnswerEndpoint{BaseURL: endpoint.URL, Credential: config.AnswerCredential{File: keyFile}}

	r := newRig(t)
	r.conversation("ses_tester", testerToken, 0)
	r.cfg.Answers.Providers = map[config.AnswerProvider]config.AnswerEndpoint{config.AnswerAnthropic: anthropic}
	r.cfg.Answers.Prices = map[string]config.AnswerPrice{"anthropic/claude-opus-5-5": price}
	var opened []string
	r.cfg.WebSearchTools = webSearchToolsFunc(func(question string) (answer.WebSearch, error) {
		opened = append(opened, question)
		return websearch.Open(websearch.Host{
			Endpoint: anthropic, Model: "claude-opus-5-5", Price: price, PricePerSearch: 0.01, Bounds: r.cfg.Answers.Bounds,
			Calls: r.st, Log: slog.New(slog.DiscardHandler), Now: time.Now,
		}, question)
	})
	// The real model loop needs the issue's repository, which its gh tool's description names.
	r.cfg.Issues = issuesFunc(func(string) IssueFacts {
		return IssueFacts{Recorded: true, DispatchStatus: "done", Phase: "done", Repository: ghrepo.MustParse("acme/widgets")}
	})
	r.restart()
	r.answer = func(req answer.Request) (answer.Reply, error) {
		if req.WebSearch == nil || req.GH == nil {
			t.Errorf("the request carries gh %v and web_search %v; want both", req.GH, req.WebSearch)
		}
		reply, err := answer.Answer(context.Background(), req)
		if err != nil || reply.Fault != "" {
			t.Logf("the answer ended with fault %q (%s), %v", reply.Fault, reply.Detail, err)
		}
		return reply, err
	}
	r.receive(messageHandoff("ses_tester", "m-1", 1, "When do plums ripen?"))
	r.drain()
	q := r.only()
	posts := r.dispatch.posts()
	if q.State != store.QuestionPosted || len(posts) != 1 || !strings.HasPrefix(posts[0].Body["body"].(string), reply+"\n\n"+AnswerMarker+q.ID+" -->") {
		t.Fatalf("question %s, posts %s; want the reply from the web search posted", q.State, describe(posts))
	}
	if !slices.Equal(opened, []string{q.ID}) {
		t.Errorf("the web_search tool was opened for %v, want once for %s", opened, q.ID)
	}
	call, ok, err := r.st.AnswerToolCall(r.ctx, q.ID, 1)
	if err != nil || !ok || call.Tool != store.ToolWebSearch || !slices.Equal(call.Args, []string{"when do plums ripen"}) || call.Result == nil ||
		!strings.Contains(*call.Result, "https://example.org/plums") || call.Exit == nil || *call.Exit != 0 {
		t.Fatalf("call 1 = %+v, %t, %v; want the web search recorded with its result", call, ok, err)
	}
	want := (13*5+35*25)/1e6 + (1000*5+200*25)/1e6 + 0.01
	if spent, err := r.st.AnswerSpend(r.ctx, project, testNow); err != nil || math.Abs(spent-want) > 1e-9 {
		t.Fatalf("the day's spend = %v, %v; want the answer's %v, its search's fee included", spent, err, want)
	}

	plain := newRig(t)
	plain.conversation("ses_tester", testerToken, 0)
	plain.answer = func(req answer.Request) (answer.Reply, error) {
		if req.WebSearch != nil {
			t.Errorf("a loop with no web_search tool handed the model loop %v", req.WebSearch)
		}
		return answered(req, "Plum."), nil
	}
	plain.receive(messageHandoff("ses_tester", "m-1", 1, "Why plum?"))
	plain.drain()
	if q := plain.only(); q.State != store.QuestionPosted {
		t.Fatalf("question = %s; want posted with no web_search tool", q.State)
	}
}

// A question the daemon stopped while a web search ran is answered again on restart with that
// call recorded as of unknown outcome, in the web_search tool's own words, neither made again by
// the loop nor looked up on GitHub, which holds nothing of a search: the gh call of unknown
// outcome beside it is looked up as before.
func TestARestartRecordsAnUnknownSearchAndLooksNothingUp(t *testing.T) {
	r := newRig(t)
	r.conversation("ses_tester", testerToken, 0)
	r.receive(messageHandoff("ses_tester", "m-1", 1, "When do plums ripen?"))
	q := r.only()
	q.State, q.UpdatedAt = store.QuestionGenerating, testNow
	if err := r.st.UpdateQuestion(r.ctx, q, store.QuestionReceived); err != nil {
		t.Fatal(err)
	}
	for _, call := range []store.AnswerToolCall{
		{Seq: 1, Tool: store.ToolWebSearch, Args: []string{"plum harvest"}},
		{Seq: 2, Tool: store.ToolGH, Args: []string{"pr", "comment", "7", "--body", "Done."}, App: "review", Owner: "o"},
	} {
		call.Question, call.StartedAt = q.ID, testNow
		if err := r.st.StartAnswerToolCall(r.ctx, call); err != nil {
			t.Fatal(err)
		}
	}
	r.restart()
	r.drain()
	if r.answerCount() != 1 {
		t.Fatalf("answers = %d; want the question answered again once", r.answerCount())
	}
	earlier := r.requests[0].Earlier
	if len(earlier) != 2 || earlier[0].Tool != store.ToolWebSearch || earlier[0].Result == nil || *earlier[0].Result != websearch.UnknownOutcome {
		t.Fatalf("earlier calls = %+v; want the web search of unknown outcome", earlier)
	}
	if call, _, _ := r.st.AnswerToolCall(r.ctx, q.ID, 1); call.Result == nil || *call.Result != websearch.UnknownOutcome || call.Exit != nil {
		t.Fatalf("call 1 = %+v; want its unknown outcome recorded as its result, no exit", call)
	}
	if looked := r.tool.lookedUp(); !slices.Equal(looked, []int{2}) {
		t.Fatalf("the gh tool looked up calls %v; want the gh call alone", looked)
	}
}
