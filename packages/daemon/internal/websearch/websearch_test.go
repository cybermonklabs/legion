package websearch

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
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// question is the answer every test's searches run in, and key what the provider's key file holds,
// which no line the tool logs and no result it answers may carry.
const (
	question = "dispatch:m-1"
	key      = "sk-ant-fixture-key-never-logged"
)

// price is the search model's answers.prices entry, USD per million tokens, and fee its
// answers.web_search.price_per_search.
var (
	price = config.AnswerPrice{Input: 5, Output: 25, CacheWrite: 6.25, CacheRead: 0.5}
	fee   = 0.01
)

// fakeRecorder is answer_tool_calls as the tool writes it.
type fakeRecorder struct {
	mu   sync.Mutex
	rows map[int]store.AnswerToolCall
}

func (f *fakeRecorder) StartAnswerToolCall(_ context.Context, call store.AnswerToolCall) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.rows[call.Seq]; ok {
		return fmt.Errorf("web_search call %s.%d %w", call.Question, call.Seq, store.ErrAnswerToolCallRecorded)
	}
	f.rows[call.Seq] = call
	return nil
}

func (f *fakeRecorder) FinishAnswerToolCall(_ context.Context, _ string, seq int, result string, exit *int, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[seq]
	if !ok || row.Result != nil {
		return fmt.Errorf("finish web_search call %d: no started, unfinished call has that number", seq)
	}
	row.Result, row.Exit, row.FinishedAt = &result, exit, &at
	f.rows[seq] = row
	return nil
}

func (f *fakeRecorder) row(seq int) (store.AnswerToolCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[seq]
	return row, ok
}

// recorded is one request the stand-in Anthropic endpoint took: its method, path, headers and
// body, and whether the call's record was started before it came.
type recorded struct {
	method, path string
	header       http.Header
	body         []byte
	started      bool
}

// reply is one answer the stand-in gives: a status (200 when 0) and a body.
type reply struct {
	status int
	body   string
}

// fixture is one answer's web_search tool over a stand-in Anthropic Messages endpoint that
// records each request and answers each with the next of its replies.
type fixture struct {
	t       *testing.T
	server  *httptest.Server
	keyFile string
	calls   *fakeRecorder
	logs    *bytes.Buffer
	answer  *Answer

	mu      sync.Mutex
	replies []reply
	got     []recorded
}

func newFixture(t *testing.T, bounds config.AnswerBounds, replies ...reply) *fixture {
	t.Helper()
	f := &fixture{t: t, calls: &fakeRecorder{rows: map[int]store.AnswerToolCall{}}, logs: &bytes.Buffer{}, replies: replies}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	f.keyFile = filepath.Join(t.TempDir(), "anthropic-key")
	f.writeKey(key)
	host := Host{
		Endpoint: config.AnswerEndpoint{BaseURL: f.server.URL, Credential: config.AnswerCredential{File: f.keyFile}},
		Model:    "claude-standin", Price: price, PricePerSearch: fee, Bounds: bounds,
		Calls: f.calls, Log: slog.New(slog.NewJSONHandler(f.logs, nil)), Now: time.Now,
	}
	answer, err := Open(host, question)
	if err != nil {
		t.Fatal(err)
	}
	f.answer = answer
	return f
}

func (f *fixture) writeKey(k string) {
	f.t.Helper()
	if err := os.WriteFile(f.keyFile, []byte(k+"\n"), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.calls.mu.Lock()
	started := len(f.calls.rows) > 0
	f.calls.mu.Unlock()
	f.mu.Lock()
	f.got = append(f.got, recorded{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: body, started: started})
	if len(f.replies) == 0 {
		f.mu.Unlock()
		http.Error(w, "the stand-in has no more replies", http.StatusTeapot)
		return
	}
	next := f.replies[0]
	f.replies = f.replies[1:]
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if next.status != 0 {
		w.WriteHeader(next.status)
	}
	_, _ = io.WriteString(w, next.body)
}

func (f *fixture) requests() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.got...)
}

var bounds = config.AnswerBounds{OutputTokens: 4000, GHOutputChars: 60000}

// usage is a Messages API usage object.
type usage struct{ Input, Output, CacheWrite, CacheRead int }

// message is a Messages API response of content, each a content block, with u its usage.
func message(u usage, content ...any) string {
	encoded, err := json.Marshal(map[string]any{
		"id": "msg_standin", "type": "message", "role": "assistant", "model": "claude-standin",
		"content": content, "stop_reason": "end_turn", "stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens": u.Input, "output_tokens": u.Output,
			"cache_creation_input_tokens": u.CacheWrite, "cache_read_input_tokens": u.CacheRead,
			"server_tool_use": map[string]any{"web_search_requests": 1},
		},
	})
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func text(t string, cited ...string) map[string]any {
	block := map[string]any{"type": "text", "text": t}
	if len(cited) > 0 {
		var citations []any
		for _, url := range cited {
			citations = append(citations, map[string]any{"type": "web_search_result_location", "url": url, "title": "a page", "encrypted_index": "Eo8B", "cited_text": "plum"})
		}
		block["citations"] = citations
	}
	return block
}

func serverToolUse(id, query string) map[string]any {
	return map[string]any{"type": "server_tool_use", "id": id, "name": "web_search", "input": map[string]any{"query": query}}
}

func searchResults(id string, urls ...string) map[string]any {
	results := []any{}
	for _, url := range urls {
		results = append(results, map[string]any{"type": "web_search_result", "url": url, "title": "a page", "encrypted_content": "EqgfCio", "page_age": "April 30, 2025"})
	}
	return map[string]any{"type": "web_search_tool_result", "tool_use_id": id, "content": results}
}

func searchError(id, code string) map[string]any {
	return map[string]any{"type": "web_search_tool_result", "tool_use_id": id, "content": map[string]any{"type": "web_search_tool_result_error", "error_code": code}}
}

// plumReply is a reply that searched once, found two sources and cites the first.
func plumReply(u usage) string {
	return message(u,
		text("I will search for that."),
		serverToolUse("srvtoolu_1", "plum harvest"),
		searchResults("srvtoolu_1", "https://example.org/plums", "https://example.com/harvest"),
		text("Plums ripen", "https://example.org/plums"),
		text(" in August."),
	)
}

// call is a call of the tool with query, its seq and its number among the answer's searches n.
func call(seq, n int, query string) Call {
	input, _ := json.Marshal(map[string]string{"query": query})
	return Call{Seq: seq, N: n, Input: string(input)}
}

// A search is one Messages call to answers.providers.anthropic's base URL, POST /v1/messages, as
// the loop's own Anthropic calls go: the key file's key in X-Api-Key, read again for each call, and
// the Messages API version the SDK the loop calls through sends. Its body names the configured
// model, max_tokens the answers' output_tokens bound, one user message asking for a search of the
// query and a brief answer with its sources, and Anthropic's web search server tool allowed one
// search, and asks for no stream.
func TestASearchCallCarriesTheModelTheServerToolTheBoundAndTheLoopsHeaders(t *testing.T) {
	f := newFixture(t, bounds, reply{body: plumReply(usage{Input: 10, Output: 5})}, reply{body: plumReply(usage{Input: 10, Output: 5})})
	if result := f.answer.Run(context.Background(), call(1, 1, "when do plums ripen")); result.IsError {
		t.Fatalf("the search answered an error: %+v", result)
	}
	f.writeKey("sk-ant-rotated")
	if result := f.answer.Run(context.Background(), call(2, 2, "plum varieties")); result.IsError {
		t.Fatalf("the second search answered an error: %+v", result)
	}
	got := f.requests()
	if len(got) != 2 {
		t.Fatalf("%d requests, want 2", len(got))
	}
	r := got[0]
	if r.method != http.MethodPost || r.path != "/v1/messages" {
		t.Errorf("the search went as %s %s, want POST /v1/messages", r.method, r.path)
	}
	for name, want := range map[string]string{"X-Api-Key": key, "Anthropic-Version": "2023-06-01", "Content-Type": "application/json"} {
		if value := r.header.Get(name); value != want {
			t.Errorf("the search's %s is %q, want %q", name, value, want)
		}
	}
	if rotated := got[1].header.Get("X-Api-Key"); rotated != "sk-ant-rotated" {
		t.Errorf("the second search's X-Api-Key is %q, want the key file's key at that call", rotated)
	}
	var body struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		Stream    *bool  `json:"stream"`
		Messages  []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(r.body, &body); err != nil {
		t.Fatalf("the search's body %s: %v", r.body, err)
	}
	if body.Model != "claude-standin" || body.MaxTokens != 4000 || body.Stream != nil && *body.Stream {
		t.Errorf("the search's body names model %q, max_tokens %d, stream %v; want claude-standin, 4000 and no stream", body.Model, body.MaxTokens, body.Stream)
	}
	if want := []map[string]any{{"type": "web_search_20250305", "name": "web_search", "max_uses": 1.0}}; !reflect.DeepEqual(body.Tools, want) {
		t.Errorf("the search's tools are %v, want %v", body.Tools, want)
	}
	if len(body.Messages) != 1 || body.Messages[0].Role != "user" || !strings.Contains(body.Messages[0].Content, "when do plums ripen") ||
		!strings.Contains(body.Messages[0].Content, "Search the web") || !strings.Contains(body.Messages[0].Content, "source") {
		t.Errorf("the search's messages are %+v, want one user message asking to search the web for the query and name its sources", body.Messages)
	}
	if strings.Contains(f.logs.String(), key) {
		t.Errorf("the tool's log carries the key: %s", f.logs.String())
	}
}

// What the model reads of a search is the reply's text, its text blocks joined (a block after a
// server tool's block starts a paragraph of its own), then the address of each source the reply's
// web_search_tool_result blocks and its text's citations name, each once, in the order the reply
// names them.
func TestAReplyYieldsItsTextAndEachSourceOnce(t *testing.T) {
	f := newFixture(t, bounds, reply{body: plumReply(usage{Input: 10, Output: 5})})
	result := f.answer.Run(context.Background(), call(1, 1, "when do plums ripen"))
	want := "I will search for that.\n\nPlums ripen in August.\n\nSources:\nhttps://example.org/plums\nhttps://example.com/harvest"
	if result.IsError || result.Output != want || result.Searches != 1 {
		t.Fatalf("the search answered %+v, want %q from one search", result, want)
	}
}

// A search the provider answered with an error block, an error status (529, overloaded, and 401,
// whose message echoes the key), or a body that is no Messages response is an error result naming
// the cause, and never the key: a key the reply echoes is put out of the result and the log.
func TestEachFailedSearchIsAnErrorResultNamingItsCause(t *testing.T) {
	for _, tc := range []struct {
		name     string
		reply    reply
		cause    string
		redacted bool
	}{
		{"an error block", reply{body: message(usage{Input: 10, Output: 5}, serverToolUse("srvtoolu_1", "plum"), searchError("srvtoolu_1", "too_many_requests"), text("I could not search."))},
			"the search answered the error too_many_requests", false},
		{"a 529", reply{status: 529, body: `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`},
			"Anthropic answered HTTP 529: overloaded_error: Overloaded", false},
		{"a 401 echoing the key", reply{status: 401, body: `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key ` + key + `"}}`},
			"Anthropic answered HTTP 401: authentication_error: invalid x-api-key [the provider's key]", true},
		{"a body that does not decode", reply{body: "<html>the gateway</html>"},
			"Anthropic's reply is no Messages response", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, bounds, tc.reply)
			result := f.answer.Run(context.Background(), call(1, 1, "plum"))
			if strings.Contains(result.Output, key) || strings.Contains(f.logs.String(), key) {
				t.Errorf("the result %q or the log carries the key", result.Output)
			}
			if tc.redacted && !strings.Contains(f.logs.String(), "[the provider's key]") {
				t.Errorf("the log %s does not put the key the reply echoes out of it", f.logs)
			}
			if !result.IsError || !strings.HasPrefix(result.Output, "the web search failed: ") || !strings.Contains(result.Output, tc.cause) {
				t.Fatalf("the search answered %+v, want an error result naming %q", result, tc.cause)
			}
			if result.Searches != 0 {
				t.Errorf("the failed search counts %d searches, want none", result.Searches)
			}
			if row, _ := f.calls.row(1); row.Result == nil || *row.Result != result.Output || row.Exit == nil || *row.Exit != 1 {
				t.Errorf("the record %+v, want the error result with exit 1", row)
			}
		})
	}
}

// An answers.providers.anthropic.base_url whose made-up password holds an unencoded "@" leaves the
// password's tail where the host goes, and the resolver's error names that host: a search that got
// no answer is an error result, and a line, that hold no part of the tail, in its ASCII form or in
// the `xn--` form a tail that is not ASCII is dialed by. A password holding a "/" ends the
// authority there, so url.Parse reads its head as the port and no request is built: that result,
// whose error quotes the URL whole, holds no part of the password either.
func TestAFailedSearchHoldsNoPartOfTheBaseURLsPassword(t *testing.T) {
	t.Parallel()
	passwordAt := "legion:p" + "@"
	for name, address := range map[string]string{
		"an ASCII tail":            "http://" + passwordAt + "madeuptail0123.invalid",
		"a tail that is not ASCII": "http://" + passwordAt + "madeuptail0123\u00f6rd.invalid",
		"a head read as the port":  "http://legion:madeuptail0123/x" + "@anthropic.invalid",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, bounds)
			f.answer.host.Endpoint.BaseURL = address
			result := f.answer.Run(context.Background(), call(1, 1, "plum"))
			if !result.IsError || !strings.HasPrefix(result.Output, "the web search failed: ") {
				t.Fatalf("the search answered %+v, want an error result", result)
			}
			for what, text := range map[string]string{"the result": result.Output, "the log": f.logs.String()} {
				if lower := strings.ToLower(text); strings.Contains(lower, "madeuptail") || strings.Contains(lower, "xn--") {
					t.Errorf("%s %q holds part of the password", what, text)
				}
			}
		})
	}
}

// A reply whose one search ran and whose second search, attempted past max_uses 1, answered
// max_uses_exceeded, the Messages API's error for that attempt, yields the first search's text and
// sources as a search with no error does: charged that one search, recorded with exit 0. That
// limit error is the only one kept beside a search that ran: the same reply with another error
// code, and a max_uses_exceeded with no search that ran, are each an error result naming the code.
func TestASearchThatRanIsKeptBesideTheLimitsErrorForASecond(t *testing.T) {
	u := usage{Input: 10, Output: 5}
	tokens := (10*5 + 5*25) / 1e6
	mixed := func(code string) string {
		return message(u,
			serverToolUse("srvtoolu_1", "plum harvest"),
			searchResults("srvtoolu_1", "https://example.org/plums", "https://example.com/harvest"),
			serverToolUse("srvtoolu_2", "plum varieties"),
			searchError("srvtoolu_2", code),
			text("Plums ripen", "https://example.org/plums"),
			text(" in August."))
	}
	f := newFixture(t, bounds, reply{body: mixed("max_uses_exceeded")})
	result := f.answer.Run(context.Background(), call(1, 1, "when do plums ripen"))
	want := "Plums ripen in August.\n\nSources:\nhttps://example.org/plums\nhttps://example.com/harvest"
	if result.IsError || result.Output != want || result.Searches != 1 || math.Abs(result.CostUSD-(tokens+fee)) > 1e-12 {
		t.Fatalf("the search answered %+v, want %q from one search costing $%.10f", result, want, tokens+fee)
	}
	if row, _ := f.calls.row(1); row.Result == nil || *row.Result != want || row.Exit == nil || *row.Exit != 0 {
		t.Errorf("the record %+v, want the search's text and sources with exit 0", row)
	}

	for _, tc := range []struct {
		name     string
		body     string
		cause    string
		searches int
	}{
		{"another error beside a search that ran", mixed("too_many_requests"), "the search answered the error too_many_requests", 1},
		{"the limit's error with no search that ran", message(u, serverToolUse("srvtoolu_1", "plum"), searchError("srvtoolu_1", "max_uses_exceeded"), text("I could not search.")),
			"the search answered the error max_uses_exceeded", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, bounds, reply{body: tc.body})
			result := f.answer.Run(context.Background(), call(1, 1, "plum"))
			if !result.IsError || result.Output != "the web search failed: "+tc.cause || result.Searches != tc.searches {
				t.Fatalf("the search answered %+v, want an error result naming %q over %d searches", result, tc.cause, tc.searches)
			}
			if row, _ := f.calls.row(1); row.Exit == nil || *row.Exit != 1 {
				t.Errorf("the record %+v, want exit 1", row)
			}
		})
	}
}

// An answer makes at most MaxSearches searches, an earlier run's counted: the fourth is refused
// as the tool's result, naming the bound, before any request is made, and recorded with its
// refusal and no exit. So is an input that names no query.
func TestTheFourthSearchIsRefusedWithNoRequestMade(t *testing.T) {
	if MaxSearches != 3 {
		t.Fatalf("MaxSearches = %d, want 3", MaxSearches)
	}
	f := newFixture(t, bounds)
	result := f.answer.Run(context.Background(), call(7, 4, "plum"))
	if !result.IsError || !strings.HasPrefix(result.Output, "refused: ") || !strings.Contains(result.Output, "3 web searches") {
		t.Fatalf("the fourth search answered %+v, want a refusal naming the bound of 3", result)
	}
	if n := len(f.requests()); n != 0 {
		t.Fatalf("the refused search made %d requests, want none", n)
	}
	row, ok := f.calls.row(7)
	if !ok || row.Tool != store.ToolWebSearch || !reflect.DeepEqual(row.Args, []string{"plum"}) || row.Result == nil || *row.Result != result.Output || row.Exit != nil {
		t.Errorf("the refused search's record %+v, want web_search [plum] with its refusal and no exit", row)
	}
	for _, input := range []string{`{"query":""}`, `{"q":"plum"}`, `["plum"]`} {
		if result := f.answer.Run(context.Background(), Call{Seq: 8, N: 1, Input: input}); !result.IsError || !strings.HasPrefix(result.Output, "refused: ") {
			t.Errorf("the input %s answered %+v, want a refusal", input, result)
		}
		f.calls.mu.Lock()
		delete(f.calls.rows, 8)
		f.calls.mu.Unlock()
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("the refused inputs made %d requests, want none", n)
	}
}

// Each search is recorded before its request is made and finished after: tool web_search, its
// query its one argument, no App and no owner, and what the model reads of it as its result,
// with exit 0 for a search the provider answered. One `answers: web_search` line logs it.
func TestEachSearchIsRecordedAsAWebSearchBeforeItRuns(t *testing.T) {
	f := newFixture(t, bounds, reply{body: plumReply(usage{Input: 10, Output: 5})})
	result := f.answer.Run(context.Background(), call(3, 1, "when do plums ripen"))
	got := f.requests()
	if len(got) != 1 || !got[0].started {
		t.Fatalf("requests %+v, want one, made once its record was started", got)
	}
	row, ok := f.calls.row(3)
	if !ok || row.Question != question || row.Tool != store.ToolWebSearch || !reflect.DeepEqual(row.Args, []string{"when do plums ripen"}) ||
		row.App != "" || row.Owner != "" || row.Result == nil || *row.Result != result.Output || row.Exit == nil || *row.Exit != 0 || row.FinishedAt == nil {
		t.Fatalf("the search's record %+v, want web_search [when do plums ripen], no App or owner, its result and exit 0", row)
	}
	var line map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(f.logs.Bytes()), &line); err != nil || line["msg"] != "answers: web_search" ||
		line["question"] != question || line["seq"] != 3.0 || line["query"] != "when do plums ripen" || line["searches"] != 1.0 {
		t.Errorf("the search logged %s (%v), want one answers: web_search line naming the question, the seq, the query and its search", f.logs.String(), err)
	}
}

// A search's cost is its tokens at the model's answers.prices entry, cache writes and reads
// included, plus the searches it made times price_per_search, counted from the reply's
// web_search_tool_result blocks that hold results: a block holding an error is a failed search and
// is not charged, and a reply with two result blocks is charged two (with max_uses 1 it should not
// happen; the count comes from the blocks all the same). A reply's usage counter of searches
// counts for nothing.
func TestTheCostIsTheTokensAtThePricePlusEachSearchMade(t *testing.T) {
	u := usage{Input: 1000, Output: 200, CacheWrite: 100, CacheRead: 50}
	tokens := (1000*5 + 200*25 + 100*6.25 + 50*0.5) / 1e6
	for _, tc := range []struct {
		name     string
		body     string
		searches int
	}{
		{"one search", plumReply(u), 1},
		{"a failed search", message(u, serverToolUse("srvtoolu_1", "plum"), searchError("srvtoolu_1", "unavailable"), text("No search.")), 0},
		{"two result blocks", message(u,
			serverToolUse("srvtoolu_1", "plum"), searchResults("srvtoolu_1", "https://example.org/plums"),
			serverToolUse("srvtoolu_2", "harvest"), searchResults("srvtoolu_2", "https://example.com/harvest"),
			text("Plums ripen in August.")), 2},
		{"a search that found nothing", message(u, serverToolUse("srvtoolu_1", "plum"), searchResults("srvtoolu_1"), text("Nothing found.")), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, bounds, reply{body: tc.body})
			result := f.answer.Run(context.Background(), call(1, 1, "plum"))
			want := tokens + float64(tc.searches)*fee
			if result.Searches != tc.searches || math.Abs(result.CostUSD-want) > 1e-12 {
				t.Fatalf("the search answered %d searches costing $%.10f, want %d costing $%.10f", result.Searches, result.CostUSD, tc.searches, want)
			}
		})
	}
	f := newFixture(t, bounds, reply{status: 529, body: `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`})
	if result := f.answer.Run(context.Background(), call(1, 1, "plum")); result.CostUSD != 0 || result.Searches != 0 {
		t.Errorf("a search the provider refused costs $%v over %d searches, want nothing", result.CostUSD, result.Searches)
	}
}

// What the model reads of a search is cut at gh_output_chars, as a gh call's output is, with a note
// saying how much was cut.
func TestAResultIsCutAtTheOutputBound(t *testing.T) {
	f := newFixture(t, config.AnswerBounds{OutputTokens: 4000, GHOutputChars: 20}, reply{body: plumReply(usage{Input: 10, Output: 5})})
	result := f.answer.Run(context.Background(), call(1, 1, "plum"))
	if !strings.HasPrefix(result.Output, "I will search for th\n[cut: ") || !strings.HasSuffix(result.Output, " more characters]") {
		t.Fatalf("the search answered %q, want its first 20 characters and the cut's note", result.Output)
	}
}
