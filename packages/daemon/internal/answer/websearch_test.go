package answer

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/websearch"
)

// stubWebSearch is the web_search tool's stand-in: it records each call and answers each with
// output naming it, one search costing 0.02.
type stubWebSearch struct {
	mu    sync.Mutex
	calls []websearch.Call
}

func (s *stubWebSearch) Run(_ context.Context, call websearch.Call) websearch.Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, call)
	return websearch.Result{Output: "search output of " + call.Input, Searches: 1, CostUSD: 0.02}
}

func (s *stubWebSearch) made() []websearch.Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]websearch.Call(nil), s.calls...)
}

// anthropicWebSearch is an Anthropic tool_use block calling the web_search tool with query.
func anthropicWebSearch(id, query string) map[string]any {
	return map[string]any{"type": "tool_use", "id": id, "name": "web_search", "input": map[string]any{"query": query}}
}

// searchRecorder is answer_tool_calls as the real web_search tool writes it.
type searchRecorder struct {
	mu   sync.Mutex
	rows []store.AnswerToolCall
}

func (r *searchRecorder) StartAnswerToolCall(_ context.Context, call store.AnswerToolCall) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, call)
	return nil
}

func (r *searchRecorder) FinishAnswerToolCall(context.Context, string, int, string, *int, time.Time) error {
	return nil
}

// An answer whose request carries no web_search tool, a daemon whose answers configures none, is
// offered no web_search tool, and its system prompt names no web search.
func TestAnAnswerWithNoWebSearchToolIsOfferedNone(t *testing.T) {
	s := newStandIn(t, anthropicStream("end_turn", anthropicUsage{Input: 1, Output: 1}, anthropicText(`{"kind":"answered","text":"Yes.","needs":""}`)))
	req := anthropicRequest(t, s, &stubGH{}, 1)
	req.Dispatch = &stubDispatch{}
	if _, err := Answer(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	body := offerOf(t, s.requests()[0])
	for _, tool := range body.Tools {
		if tool.Name == "web_search" {
			t.Errorf("an answer with no web_search tool is offered %+v", tool)
		}
	}
	if len(body.Tools) != 2 || strings.Contains(body.System[0].Text, "web_search") || strings.Contains(body.System[0].Text, "web search") {
		t.Errorf("an answer with no web_search tool offers %+v under %q, want gh and dispatch and no web search named", body.Tools, body.System[0].Text)
	}
}

// The system prompt names the web_search tool, that its reply names each source it uses, and its
// bound beside the other tools': at most 3 web searches an answer.
func TestThePromptNamesTheWebSearchToolAndItsBound(t *testing.T) {
	s := newStandIn(t)
	req := anthropicRequest(t, s, &stubGH{}, 3)
	req.Dispatch = &stubDispatch{}
	req.WebSearch = &stubWebSearch{}
	prompt := systemPrompt(req)
	for _, want := range []string{
		"`web_search`", "name in your reply the address of each source you use",
		"This answer may make at most 20 gh calls, at most 20 dispatch calls, counted apart from its gh calls, at most 3 web searches, and 3 model calls",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the system prompt lacks %q:\n%s", want, prompt)
		}
	}
	req.Dispatch = nil
	if prompt := systemPrompt(req); !strings.Contains(prompt, "This answer may make at most 20 gh calls, at most 3 web searches, and 3 model calls") {
		t.Errorf("the system prompt with gh and web_search lacks their bounds:\n%s", prompt)
	}
}

// An answer whose model calls web_search runs the daemon's web_search tool: one Messages call to
// the same Anthropic endpoint the loop's own calls go to, with the same key and the same Messages
// API version, whose reply's text and sources reach the model as the call's result. Its cost, its
// tokens at the search model's price plus its search's fee, is added to the answer's cost and not
// to its tokens, and the `answers: answered` line carries the searches and what they cost, so
// costUSD is the line's tokens at the model's prices plus webSearchUSD.
func TestAWebSearchsCostIsTheAnswersAndItsLineCarriesIt(t *testing.T) {
	search := map[string]any{
		"id": "msg_search", "type": "message", "role": "assistant", "model": "claude-standin", "stop_reason": "end_turn",
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
	}
	encoded, err := json.Marshal(search)
	if err != nil {
		t.Fatal(err)
	}
	s := newStandIn(t,
		anthropicStream("tool_use", anthropicUsage{Input: 10, Output: 20}, anthropicWebSearch("toolu_1", "when do plums ripen")),
		reply{body: encoded},
		anthropicStream("end_turn", anthropicUsage{Input: 3, Output: 15}, anthropicText(`{"kind":"answered","text":"Plums ripen in August (https://example.org/plums).","needs":""}`)),
	)
	var logged bytes.Buffer
	req := anthropicRequest(t, s, &stubGH{}, 3)
	req.Log = slog.New(slog.NewJSONHandler(&logged, nil))
	recorder := &searchRecorder{}
	tool, err := websearch.Open(websearch.Host{
		Endpoint: req.Providers[config.AnswerAnthropic], Model: "claude-standin", Price: anthropicPrice, PricePerSearch: 0.01,
		Bounds: req.Bounds, Calls: recorder, Log: slog.New(slog.DiscardHandler), Now: time.Now,
	}, req.Question.ID)
	if err != nil {
		t.Fatal(err)
	}
	req.WebSearch = tool
	reply, err := Answer(context.Background(), req)
	if err != nil || reply.Kind != Answered {
		t.Fatalf("reply %+v, %v; want answered", reply, err)
	}
	got := s.requests()
	if len(got) != 3 {
		t.Fatalf("%d requests, want the two model calls and the search between them", len(got))
	}
	loopCall, searchCall := got[0], got[1]
	if searchCall.path != "/v1/messages" || bytes.Contains(searchCall.body, []byte(`"stream":true`)) {
		t.Errorf("the search went to %s with %s, want POST /v1/messages and no stream", searchCall.path, searchCall.body)
	}
	for _, name := range []string{"X-Api-Key", "Anthropic-Version"} {
		if loop, search := loopCall.header.Get(name), searchCall.header.Get(name); loop == "" || search != loop {
			t.Errorf("the search's %s is %q, want the loop's own call's %q", name, search, loop)
		}
	}
	if len(recorder.rows) != 1 || recorder.rows[0].Seq != 1 || recorder.rows[0].Tool != store.ToolWebSearch || !reflect.DeepEqual(recorder.rows[0].Args, []string{"when do plums ripen"}) {
		t.Errorf("the search was recorded as %+v, want seq 1, web_search [when do plums ripen]", recorder.rows)
	}
	results := blocks(messages(t, got[2])[4])
	if text, _ := json.Marshal(results[0]["content"]); len(results) != 1 || results[0]["tool_use_id"] != "toolu_1" || results[0]["is_error"] == true ||
		!strings.Contains(string(text), "Plums ripen in August.") || !strings.Contains(string(text), "https://example.org/plums") {
		t.Errorf("the search's result reached the model as %v, want its text and its source", results)
	}

	loopTokens := (13*5 + 35*25) / 1e6
	searchUSD := (1000*5+200*25)/1e6 + 0.01
	if reply.WebSearches != 1 || math.Abs(reply.WebSearchUSD-searchUSD) > 1e-12 || math.Abs(reply.CostUSD-(loopTokens+searchUSD)) > 1e-12 {
		t.Errorf("the reply carries %d searches costing $%.10f of $%.10f, want 1 costing $%.10f of $%.10f", reply.WebSearches, reply.WebSearchUSD, reply.CostUSD, searchUSD, loopTokens+searchUSD)
	}
	if reply.Usage.InputTokens != 13 || reply.Usage.OutputTokens != 35 {
		t.Errorf("the reply's usage %+v, want the loop's own calls' tokens alone", reply.Usage)
	}
	var line map[string]any
	for _, raw := range bytes.Split(bytes.TrimSpace(logged.Bytes()), []byte("\n")) {
		var record map[string]any
		if err := json.Unmarshal(raw, &record); err != nil {
			t.Fatal(err)
		}
		if record["msg"] == "answers: answered" {
			line = record
		}
	}
	cost, _ := line["costUSD"].(float64)
	fee, _ := line["webSearchUSD"].(float64)
	if line["webSearches"] != 1.0 || math.Abs(fee-searchUSD) > 1e-12 || math.Abs(cost-(loopTokens+searchUSD)) > 1e-12 {
		t.Errorf("the answers: answered line %v, want webSearches 1, webSearchUSD %v and costUSD %v", line, searchUSD, loopTokens+searchUSD)
	}
	inputTokens, _ := line["inputTokens"].(float64)
	outputTokens, _ := line["outputTokens"].(float64)
	if recomputed := (inputTokens*anthropicPrice.Input+outputTokens*anthropicPrice.Output)/1e6 + fee; math.Abs(recomputed-cost) > 1e-12 {
		t.Errorf("the line's costUSD %v does not recompute from its tokens and webSearchUSD: %v", cost, recomputed)
	}
}

// Each tool counts its own calls, an earlier run's counted: an answer whose earlier run made two
// web searches makes its next as the third, which the tool's bound reads, under the next seq of
// every tool's. The re-run list names each earlier search with its query and its result, a search
// a restart recorded as of unknown outcome in the tool's own words.
func TestEarlierSearchesCountTowardTheBoundAndTheRerunListNamesThem(t *testing.T) {
	s := newStandIn(t,
		anthropicStream("tool_use", anthropicUsage{Input: 4, Output: 5}, anthropicWebSearch("toolu_1", "plum season")),
		anthropicStream("end_turn", anthropicUsage{Input: 2, Output: 3}, anthropicText(`{"kind":"answered","text":"August.","needs":""}`)),
	)
	stub := &stubWebSearch{}
	req := anthropicRequest(t, s, &stubGH{}, 3)
	req.WebSearch = stub
	found, exit, unknown := "Plums ripen in August.\n\nSources:\nhttps://example.org/plums", 0, websearch.UnknownOutcome
	req.Earlier = []store.AnswerToolCall{
		{Seq: 1, Tool: store.ToolGH, Args: []string{"pr", "view", "7"}, Result: &found, Exit: &exit},
		{Seq: 2, Tool: store.ToolWebSearch, Args: []string{"plum"}, Result: &found, Exit: &exit},
		{Seq: 3, Tool: store.ToolWebSearch, Args: []string{"harvest"}, Result: &unknown},
	}
	reply, err := Answer(context.Background(), req)
	if err != nil || reply.Kind != Answered {
		t.Fatalf("reply %+v, %v; want answered", reply, err)
	}
	if calls := stub.made(); !reflect.DeepEqual(calls, []websearch.Call{{Seq: 4, N: 3, Input: `{"query":"plum season"}`}}) {
		t.Errorf("web_search calls %+v; want seq 4, the third search", calls)
	}
	if reply.WebSearches != 1 || math.Abs(reply.WebSearchUSD-0.02) > 1e-12 {
		t.Errorf("the reply carries %d searches costing $%v, want this run's one costing $0.02", reply.WebSearches, reply.WebSearchUSD)
	}
	text := blocks(messages(t, s.requests()[0])[2])[0]["text"].(string)
	for _, want := range []string{
		"\n2. web_search [\"plum\"]\nExit 0:\n" + found + "\n",
		"\n3. web_search [\"harvest\"]\nNo exit status:\n" + websearch.UnknownOutcome + "\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the question does not hold %q:\n%s", want, text)
		}
	}
}
