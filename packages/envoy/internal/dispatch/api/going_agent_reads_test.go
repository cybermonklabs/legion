package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/going/agent/agenttest"
	"github.com/sjawhar/envoy/going/agentwire"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// TestTheServerReadsGitHubAndTheWeb: the agent's reads outside Dispatch, which the server makes
// for it. A GitHub read of a repository no project maps is refused. A web read reads an address in
// the person's words or in a tool result of the run, and refuses one in neither; it stops at 1 MiB
// and at 10 s, and says it was cut; and a redirect to a private address is refused, judged on the
// connection itself. The web server of this test listens on loopback, which the reads admit beside
// the public addresses the server's own check admits; the check itself is held to addresses.
func TestTheServerReadsGitHubAndTheWeb(t *testing.T) {
	t.Parallel()
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/large":
			chunk := []byte(strings.Repeat("m", 64<<10))
			for range 48 {
				if _, err := w.Write(chunk); err != nil {
					return
				}
			}
		case "/slow":
			_, _ = w.Write([]byte(strings.Repeat("s", 1024)))
			w.(http.Flusher).Flush()
			select {
			case <-time.After(15 * time.Second):
			case <-r.Context().Done():
			}
		case "/away":
			http.Redirect(w, r, "http://10.0.0.1/", http.StatusFound)
		default:
			_, _ = w.Write([]byte("a page of the web"))
		}
	}))
	t.Cleanup(web.Close)
	loopback := netip.MustParseAddrPort(web.Listener.Addr().String()).Addr()
	rig := newGoingAgentRig(t, func(agent *AgentListener) {
		agent.web.admit = func(address netip.Addr) bool { return address == loopback || goingPublicAddress(address) }
	})
	words := "read " + web.URL + "/large, then " + web.URL + "/slow, then " + web.URL + "/away"
	webRead := func(run, token string, call int, address string) (agentwire.CallAnswer, error) {
		return rig.call(agentwire.CallRequest{Run: run, LeaseToken: token, Step: 1, Call: call, Kind: agentwire.KindWeb, URL: address})
	}

	t.Run("a GitHub read of a repository no project maps is refused", func(t *testing.T) {
		run, token := rig.working(t, "alice", "read the pull request of acme/unmapped")
		_, err := rig.call(agentwire.CallRequest{Run: run, LeaseToken: token, Step: 1, Call: 1, Kind: agentwire.KindGitHub,
			Repository: "acme/unmapped", Path: "pulls/1"})
		requireAgentCode(t, err, agentwire.CodeRepositoryUnmapped, "a GitHub read of a repository no project maps")
	})

	t.Run("a web read of 3 MB answers 1 MB marked cut", func(t *testing.T) {
		run, token := rig.working(t, "alice", words)
		answer, err := webRead(run, token, 1, web.URL+"/large")
		if err != nil || answer.Status != http.StatusOK || len(answer.Body) != agentwire.CallBodyLimit || !answer.Cut {
			t.Fatalf("a read of 3 MiB answered %d with %d bytes, cut %v, %v; want 1 MiB marked cut", answer.Status, len(answer.Body), answer.Cut, err)
		}
	})

	t.Run("a read that takes 15 s ends at 10 s", func(t *testing.T) {
		run, token := rig.working(t, "alice", words)
		started := time.Now()
		answer, err := webRead(run, token, 1, web.URL+"/slow")
		took := time.Since(started)
		if err != nil || answer.Status != http.StatusOK || len(answer.Body) != 1024 || !answer.Cut || took < goingWebReadTime || took > goingWebReadTime+3*time.Second {
			t.Fatalf("a read of 15 s answered %d with %d bytes, cut %v, after %s, %v; want what it read by 10 s, marked cut", answer.Status, len(answer.Body), answer.Cut, took, err)
		}
	})

	t.Run("a redirect to 10.0.0.1 is refused", func(t *testing.T) {
		run, token := rig.working(t, "alice", words)
		started := time.Now()
		_, err := webRead(run, token, 1, web.URL+"/away")
		requireAgentCode(t, err, agentwire.CodeAddressRefused, "a read redirected to 10.0.0.1")
		if took := time.Since(started); took >= goingWebReadTime {
			t.Fatalf("the redirected read took %s, want it refused before any connection to 10.0.0.1", took)
		}
	})

	t.Run("an address in no word and no tool result is refused", func(t *testing.T) {
		run, token := rig.working(t, "alice", words)
		unseen := web.URL + "/unseen"
		_, err := webRead(run, token, 1, unseen)
		requireAgentCode(t, err, agentwire.CodeAddressUnseen, "a read of an address in no word and no tool result")
		// A tool result of the run names it: it is read.
		body, err := json.Marshal(map[string]any{"tool": "dispatch_read", "result": "the issue links " + unseen})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rig.client.Step(t.Context(), agentwire.StepRequest{Run: run, LeaseToken: token, Step: 1, Kind: agentwire.StepTool,
			Body: body, Note: "read an issue"}); err != nil {
			t.Fatalf("save the tool step: %v", err)
		}
		answer, err := webRead(run, token, 2, unseen)
		if err != nil || answer.Status != http.StatusOK || string(answer.Body) != "a page of the web" {
			t.Fatalf("a read of an address a tool result named answered %d %q, %v; want the page", answer.Status, answer.Body, err)
		}
	})

	t.Run("the public-address check", func(t *testing.T) {
		for address, public := range map[string]bool{
			"127.0.0.1":            false,
			"::1":                  false,
			"10.0.0.1":             false,
			"172.16.0.1":           false,
			"192.168.1.1":          false,
			"169.254.169.254":      false,
			"fe80::1":              false,
			"fd00:ec2::254":        false,
			"100.64.0.1":           false,
			"0.0.0.0":              false,
			"::ffff:10.0.0.1":      false,
			"64:ff9b::a00:1":       false,
			"224.0.0.1":            false,
			"8.8.8.8":              true,
			"2606:4700:4700::1111": true,
		} {
			if got := goingPublicAddress(netip.MustParseAddr(address)); got != public {
				t.Errorf("goingPublicAddress(%s) = %v, want %v", address, got, public)
			}
		}
	})
}

// goingServedRequest is one request the route table served in-process: its row, as
// "<METHOD> <pattern>", the value of each placeholder of its path, its query and its body.
type goingServedRequest struct {
	route string
	path  map[string]string
	query url.Values
	body  []byte
}

// goingServedRecord keeps each request the route table it wraps serves, with the row mux routes
// it to, until take.
type goingServedRecord struct {
	mux    *http.ServeMux
	mu     sync.Mutex
	served []goingServedRequest
}

// wrap is inner with the record in front of it.
func (s *goingServedRecord) wrap(inner http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body []byte
		if r.Body != nil {
			body, _ = io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		_, route := s.mux.Handler(r)
		values := map[string]string{}
		if _, pattern, found := strings.Cut(route, " "); found {
			// A mux of the one row reads the value of each placeholder as the table reads it.
			one := http.NewServeMux()
			one.HandleFunc(route, func(_ http.ResponseWriter, matched *http.Request) {
				for _, placeholder := range goingPlaceholders(pattern) {
					placeholder = strings.TrimSuffix(placeholder, "...")
					values[placeholder] = matched.PathValue(placeholder)
				}
			})
			one.ServeHTTP(httptest.NewRecorder(), r.Clone(r.Context()))
		}
		s.mu.Lock()
		s.served = append(s.served, goingServedRequest{route: route, path: values, query: r.URL.Query(), body: body})
		s.mu.Unlock()
		inner.ServeHTTP(w, r)
	})
}

// take is the requests served since the last take.
func (s *goingServedRecord) take() []goingServedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	served := s.served
	s.served = nil
	return served
}

// goingTaggedNames is the names v's struct type gives its fields under tag, an embedded struct's
// fields included; none for nil.
func goingTaggedNames(v any, tag string) map[string]bool {
	names := map[string]bool{}
	var walk func(reflect.Type)
	walk = func(typ reflect.Type) {
		for typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct {
			return
		}
		for index := range typ.NumField() {
			field := typ.Field(index)
			name, _, _ := strings.Cut(field.Tag.Get(tag), ",")
			switch {
			case name == "-":
			case name != "":
				names[name] = true
			case field.Anonymous:
				walk(field.Type)
			}
		}
	}
	if v != nil {
		walk(reflect.TypeOf(v))
	}
	return names
}

// goingCarries reports whether request carries every item of value where its route reads it: as
// the value of a placeholder of its path, under a query key the route reads (its declaration's
// Shape.Params, and session, which every route reads), or in a field of the body its route reads
// (its declaration's Shape.Request).
func goingCarries(request goingServedRequest, value any) bool {
	declaration, _ := declaredRoute(request.route)
	shape := declaration.Shape
	keys := goingTaggedNames(shape.Params, "query")
	keys["session"] = true
	fields := goingTaggedNames(shape.Request, "json")
	var body map[string]any
	_ = json.Unmarshal(request.body, &body)
	carried := func(text string) bool {
		for _, held := range request.path {
			if held == text {
				return true
			}
		}
		for key := range keys {
			if slices.Contains(request.query[key], text) {
				return true
			}
		}
		for field := range fields {
			held, many := body[field].([]any)
			if !many {
				held = []any{body[field]}
			}
			for _, item := range held {
				if heldText, isText := goingArgumentText(item); isText && heldText == text {
					return true
				}
			}
		}
		return false
	}
	items, many := value.([]any)
	if !many {
		items = []any{value}
	}
	for _, item := range items {
		if text, isText := goingArgumentText(item); !isText || !carried(text) {
			return false
		}
	}
	return true
}

// goingReadsNoPersonCalls are the read tools whose every row reads the calling session, which a
// person's run acts as none of: the call is refused before any request, with its reason.
var goingReadsNoPersonCalls = map[string]string{
	"dispatch_answers": "it lists the answers written to the calling session",
}

// TestTheAgentReadsWhatItsArgumentsName: a read of the agent's reaches the route that reads what its
// arguments name, and each argument reaches it where that route reads it (R12: a search, a read of
// an item, and call_tool's read by a tool's name). The agent's search, and dispatch_search called
// by its name, send their words as the search route's q and find the issue whose title holds them.
// A read of a reference reads the item it names through the route a person reads it with: an
// issue, a comment with its thread, an issue's log, a document of an issue or a project, an ask, a
// message, an issue's children, an issue's comments and a block; a read naming how many events
// reads the issue's events, and a project alone its documents on no issue. dispatch_doc_read
// reads the text of the document its issue, artifact or reference names, with its blocks or at a
// version; dispatch_handoff_read reads the version it names; dispatch_issues lists the issues with no
// priority. Then every read tool find_tools offers, called by call_tool with its spec's example,
// reaches a row of its own with each argument where that row's route reads it: in its path, under
// a query key the route reads, or in a field of the body it reads (its declaration's Shape).
func TestTheAgentReadsWhatItsArgumentsName(t *testing.T) {
	t.Parallel()
	writes := newGoingWritesRig(t)
	record := &goingServedRecord{mux: writes.mux.(*http.ServeMux)}
	writes.served.inProcess = record.wrap(writes.served.inProcess)
	rig := newGoingAgentRigOver(t, writes, nil)
	loop := newGoingLoop(t, rig)
	// call runs a run of alice's whose model calls tool with arguments and then answers, and
	// answers what the model was told of the call and the requests the route table served for it.
	call := func(t *testing.T, words, tool string, arguments map[string]any) (string, []goingServedRequest) {
		t.Helper()
		loop.script.set(words, func(n int) agenttest.Reply {
			if n == 1 {
				return toolCall(tool, arguments)
			}
			return answered("Read.")
		})
		record.take()
		run := rig.begin(t, goingLoopRun{login: "alice", words: words})
		if ended := rig.ended(t, run, time.Minute); ended.state != "done" {
			t.Fatalf("the run ended %s with %q, want done", ended.state, ended.reason)
		}
		results := rig.toolResults(t, run)
		if len(results) != 1 {
			t.Fatalf("the run saved %d tool results, want 1: %q", len(results), results)
		}
		return results[0], record.take()
	}
	created := func(response *httptest.ResponseRecorder, what string) string {
		t.Helper()
		if response.Code != http.StatusCreated {
			t.Fatalf("%s: status=%d body=%s", what, response.Code, response.Body.String())
		}
		return decodeBody[struct {
			ID string `json:"id"`
		}](t, response).ID
	}

	mill := decodeBody[model.Issue](t, rig.fixture(t, "alice", http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "DEMO", "title": "The quernstone mill", "spec": "# The mill\n\nThe quernstone grinds the oats.\n", "force": true,
	})).Key
	comments := "/api/v1/issues/" + mill + "/comments"
	comment := created(dispatchRequest(t, rig.mux, http.MethodPost, comments, map[string]any{"body": "Which millstone turns?"}, "alice"), "comment")
	created(dispatchRequest(t, rig.mux, http.MethodPost, comments, map[string]any{"body": "The bedstone stays.", "reply_to": comment}, "alice"), "reply")
	ask := rig.ask(t, mill, "Who dresses the quernstone?")
	message := created(dispatchRequest(t, rig.mux, http.MethodPost, "/api/v1/issues/"+mill+"/messages",
		map[string]any{"body": "The miller is on his way."}, "alice"), "message")
	runbook := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, rig.fixture(t, "alice", http.MethodPost, "/api/v1/projects/DEMO/artifacts", map[string]any{
		"name": "Mill runbook", "content": "# Mill runbook\n\nOil the spindle weekly.\n",
	})).Artifact.Slug
	for _, note := range []string{"first hand-off", "second hand-off"} {
		body, err := json.Marshal(map[string]string{"note": note})
		if err != nil {
			t.Fatal(err)
		}
		created(sessionRequest(t, rig.mux, http.MethodPost, "/api/v1/issues/"+mill+"/handoffs/implement",
			map[string]any{"actor": sessionActor(), "body": string(body)}), "handoff")
	}
	blocks := decodeBody[[]struct {
		ID string `json:"id"`
	}](t, rig.fixture(t, "alice", http.MethodGet, "/api/v1/issues/"+mill+"/artifacts/spec/blocks", nil))
	if len(blocks) == 0 {
		t.Fatalf("%s's spec holds no block", mill)
	}
	issueRef := "dispatch://" + mill
	blockRef := issueRef + "/spec#b-" + text.EncodeBlockID(blocks[0].ID)
	docRead := func(arguments map[string]any) map[string]any {
		return map[string]any{"tool": "dispatch_doc_read", "arguments": arguments}
	}

	for _, row := range []struct {
		name, tool string
		arguments  map[string]any
		// route is the row of the table the call reads, and query what that row's request holds
		// under each key named, nil where the request holds none.
		route string
		query url.Values
		holds []string
	}{
		{name: "the agent's search finds the issue whose title holds its words", tool: "search",
			arguments: map[string]any{"query": "quernstone"},
			route:     "GET /api/v1/search", query: url.Values{"q": {"quernstone"}}, holds: []string{mill}},
		{name: "dispatch_search finds it in its project", tool: "call_tool",
			arguments: map[string]any{"tool": "dispatch_search", "arguments": map[string]any{"query": "quernstone", "project": "DEMO"}},
			route:     "GET /api/v1/search", query: url.Values{"q": {"quernstone"}, "project": {"DEMO"}}, holds: []string{mill}},
		{name: "an issue's reference reads the issue", tool: "read", arguments: map[string]any{"ref": issueRef},
			route: "GET /api/v1/issues/{key}", holds: []string{"The quernstone mill"}},
		{name: "a comment's reference reads the comment with its thread", tool: "read",
			arguments: map[string]any{"ref": issueRef + "/comment/" + comment},
			route:     "GET /api/v1/comments/{id}", holds: []string{"Which millstone turns?", "The bedstone stays."}},
		{name: "a comment's reference beside its issue reads the comment", tool: "read",
			arguments: map[string]any{"ref": issueRef + "/comment/" + comment, "issue": mill},
			route:     "GET /api/v1/comments/{id}", holds: []string{"Which millstone turns?"}},
		{name: "an issue's log reads its newest events", tool: "read", arguments: map[string]any{"ref": issueRef + "/log"},
			route: "GET /api/v1/issues/{key}/events", query: url.Values{"order": {"desc"}, "limit": {"10"}}, holds: []string{"comment.created"}},
		{name: "an issue read naming all_events reads every event", tool: "read", arguments: map[string]any{"issue": mill, "all_events": true},
			route: "GET /api/v1/issues/{key}/events", query: url.Values{"order": nil, "limit": nil}, holds: []string{"issue.created"}},
		{name: "an issue read naming events reads that many of the newest", tool: "read", arguments: map[string]any{"issue": mill, "events": 2},
			route: "GET /api/v1/issues/{key}/events", query: url.Values{"order": {"desc"}, "limit": {"2"}}},
		{name: "a spec's reference reads the document", tool: "read", arguments: map[string]any{"ref": issueRef + "/spec"},
			route: "GET /api/v1/issues/{key}/artifacts/{slug}", holds: []string{`"slug":"spec"`}},
		{name: "an issue and its artifact read the document", tool: "read", arguments: map[string]any{"issue": mill, "artifact": "spec"},
			route: "GET /api/v1/issues/{key}/artifacts/{slug}", holds: []string{`"slug":"spec"`}},
		{name: "a project document's reference reads the document", tool: "read", arguments: map[string]any{"ref": "dispatch://DEMO/artifact/" + runbook},
			route: "GET /api/v1/projects/{key}/artifacts/{slug}", holds: []string{"Mill runbook"}},
		{name: "an ask's reference reads the ask", tool: "read", arguments: map[string]any{"ref": issueRef + "/ask/" + ask},
			route: "GET /api/v1/asks/{id}", holds: []string{"Who dresses the quernstone?"}},
		{name: "a message's reference reads the message", tool: "read", arguments: map[string]any{"ref": issueRef + "/message/" + message},
			route: "GET /api/v1/issues/{key}/messages/{id}", holds: []string{"The miller is on his way."}},
		{name: "an issue's children read the issue", tool: "read", arguments: map[string]any{"ref": issueRef + "/children"},
			route: "GET /api/v1/issues/{key}", holds: []string{"The quernstone mill"}},
		{name: "an issue's comments read its comments", tool: "read", arguments: map[string]any{"ref": issueRef + "/comments"},
			route: "GET /api/v1/issues/{key}/comments", holds: []string{"Which millstone turns?", "The bedstone stays."}},
		{name: "a block's reference reads the block", tool: "read", arguments: map[string]any{"ref": blockRef},
			route: "GET /api/v1/references", query: url.Values{"to": {blockRef}}, holds: []string{blocks[0].ID}},
		{name: "a project alone lists its documents on no issue", tool: "read", arguments: map[string]any{"project": "DEMO"},
			route: "GET /api/v1/projects/{key}/artifacts", query: url.Values{"unlinked": {"true"}}, holds: []string{runbook}},
		{name: "dispatch_doc_read of an issue reads its spec's text", tool: "call_tool", arguments: docRead(map[string]any{"issue": mill}),
			route: "GET /api/v1/issues/{key}/artifacts/{slug}/text", holds: []string{"grinds the oats"}},
		{name: "dispatch_doc_read of an issue's artifact reads its text", tool: "call_tool", arguments: docRead(map[string]any{"issue": mill, "artifact": "spec"}),
			route: "GET /api/v1/issues/{key}/artifacts/{slug}/text", holds: []string{"grinds the oats"}},
		{name: "dispatch_doc_read of a reference reads its text", tool: "call_tool", arguments: docRead(map[string]any{"ref": issueRef + "/spec"}),
			route: "GET /api/v1/issues/{key}/artifacts/{slug}/text", holds: []string{"grinds the oats"}},
		{name: "dispatch_doc_read with blocks reads the text with its blocks", tool: "call_tool", arguments: docRead(map[string]any{"issue": mill, "blocks": true}),
			route: "GET /api/v1/issues/{key}/artifacts/{slug}/text", query: url.Values{"blocks": {"true"}}, holds: []string{blocks[0].ID}},
		{name: "dispatch_doc_read with a version reads that version", tool: "call_tool", arguments: docRead(map[string]any{"issue": mill, "version": 1}),
			route: "GET /api/v1/issues/{key}/artifacts/{slug}/versions/{number}", holds: []string{"grinds the oats"}},
		{name: "dispatch_doc_read of a project document's reference at a version reads that version", tool: "call_tool",
			arguments: docRead(map[string]any{"ref": "dispatch://DEMO/artifact/" + runbook + "@v1"}),
			route:     "GET /api/v1/projects/{key}/artifacts/{slug}/versions/{number}", holds: []string{"Oil the spindle weekly."}},
		{name: "dispatch_handoff_read of a version reads that version", tool: "call_tool",
			arguments: map[string]any{"tool": "dispatch_handoff_read", "arguments": map[string]any{"issue": mill, "phase": "implement", "version": 1}},
			route:     "GET /api/v1/handoffs/{id}", holds: []string{"first hand-off"}},
		{name: "dispatch_issues lists the issues with no priority", tool: "call_tool",
			arguments: map[string]any{"tool": "dispatch_issues", "arguments": map[string]any{"project": "DEMO", "priority": []any{nil}}},
			route:     "GET /api/v1/issues", query: url.Values{"priority": {"none"}}, holds: []string{mill}},
	} {
		t.Run(row.name, func(t *testing.T) {
			result, served := call(t, row.name, row.tool, row.arguments)
			if len(served) == 0 {
				t.Fatalf("the call reached no route; the model was told %q", result)
			}
			read := served[len(served)-1]
			if read.route != row.route {
				t.Fatalf("the call read %s?%s, want %s; the model was told %q", read.route, read.query.Encode(), row.route, result)
			}
			for key, want := range row.query {
				if got := read.query[key]; !slices.Equal(got, want) {
					t.Errorf("the read's query holds %s=%q, want %q", key, got, want)
				}
			}
			if !strings.Contains(result, "status 200") {
				t.Errorf("the model was told %q, want status 200", result)
			}
			for _, held := range row.holds {
				if !strings.Contains(result, held) {
					t.Errorf("the model was told %q, which does not hold %q", result, held)
				}
			}
		})
	}

	for _, spec := range contracts.DispatchTools {
		if _, reads := goingReadTools[spec.Name]; !reads {
			continue
		}
		t.Run(spec.Name+" with its example", func(t *testing.T) {
			routes, err := goingReadToolRoutes(spec.Name)
			if err != nil {
				t.Fatal(err)
			}
			var example map[string]any
			if err := json.Unmarshal(spec.Example, &example); err != nil {
				t.Fatal(err)
			}
			result, served := call(t, "call "+spec.Name+" with its example", "call_tool", map[string]any{"tool": spec.Name, "arguments": example})
			if len(served) == 0 {
				if _, refused := goingReadsNoPersonCalls[spec.Name]; refused && strings.Contains(result, agentwire.CodeCallRefused) {
					return
				}
				t.Fatalf("the call reached no route; the model was told %q", result)
			}
			read := served[len(served)-1]
			if !slices.ContainsFunc(routes, func(route GoingRoute) bool { return route.Method+" "+route.Pattern == read.route }) {
				t.Fatalf("the call read %s, which is no row of %s", read.route, spec.Name)
			}
			for name, value := range example {
				if !goingCarries(read, value) {
					t.Errorf("%s=%v never reaches %s?%s (body %s), which reads it under no name", name, value, read.route, read.query.Encode(), read.body)
				}
			}
		})
	}
}

// TestTheAgentReadsAnUploadedFileAsDocReadDoes: dispatch_doc_read of an uploaded file reads it as
// `dispatch doc-read` does, once the server read its record: through the route of one of its
// versions, the latest where the call names none and the one the call or its reference names
// otherwise, the model told a text file's text after a line that names the file, its type, the
// version and its size; and a file whose bytes are not UTF-8 text, and a picture, by those facts
// and the route that serves the bytes, never the bytes themselves. A call for a file's blocks is
// refused in the command's words, and no route of the file is read. A document still reads its
// markdown.
func TestTheAgentReadsAnUploadedFileAsDocReadDoes(t *testing.T) {
	t.Parallel()
	writes := newGoingWritesRig(t)
	record := &goingServedRecord{mux: writes.mux.(*http.ServeMux)}
	writes.served.inProcess = record.wrap(writes.served.inProcess)
	rig := newGoingAgentRigOver(t, writes, nil)
	loop := newGoingLoop(t, rig)
	// read runs a run of alice's whose model calls dispatch_doc_read with arguments and then
	// answers, and answers what the model was told of the call and the requests the route table
	// served for it.
	read := func(t *testing.T, words string, arguments map[string]any) (string, []goingServedRequest) {
		t.Helper()
		loop.script.set(words, func(n int) agenttest.Reply {
			if n == 1 {
				return toolCall("call_tool", map[string]any{"tool": "dispatch_doc_read", "arguments": arguments})
			}
			return answered("Read.")
		})
		record.take()
		run := rig.begin(t, goingLoopRun{login: "alice", words: words})
		if ended := rig.ended(t, run, time.Minute); ended.state != "done" {
			t.Fatalf("the run ended %s with %q, want done", ended.state, ended.reason)
		}
		results := rig.toolResults(t, run)
		if len(results) != 1 {
			t.Fatalf("the run saved %d tool results, want 1: %q", len(results), results)
		}
		return results[0], record.take()
	}

	issue := decodeBody[model.Issue](t, rig.fixture(t, "alice", http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "DEMO", "title": "Large uploads fail", "spec": "# Uploads\n\nThe proxy refuses a large upload.\n", "force": true,
	})).Key
	owner := "/api/v1/issues/" + issue
	lines := [][]byte{[]byte("PUT /upload answered 413 for 24.1 MB\n"), []byte("PUT /upload answered 413 Request Entity Too Large for 24.1 MB\n")}
	errorFile := uploadVersions(t, rig.mux, owner, "upload-error.txt", "text/plain", lines...)
	dump := []byte("\xff\xfe\x00binary dump")
	binary := uploadVersions(t, rig.mux, owner, "dump.bin", "application/octet-stream", dump)
	shot := pngBytes("a chart of uploads")
	chart := uploadVersions(t, rig.mux, owner, "chart.png", "image/png", shot)
	errorRef := "dispatch://" + issue + "/artifact/" + errorFile.Slug
	const recordRow = "GET /api/v1/issues/{key}/artifacts/{slug}"
	const versionRow = recordRow + "/versions/{number}"

	for _, c := range []struct {
		name      string
		arguments map[string]any
		// route is the row the call reads, after the read of the item's record, and number the
		// version that row reads, "" for none.
		route, number string
		holds, lacks  []string
	}{
		{name: "a text file read with no version reads the text of its latest version", arguments: map[string]any{"issue": issue, "artifact": errorFile.Slug},
			route: versionRow, number: "2",
			holds: []string{"File upload-error.txt: text/plain", fmt.Sprintf(", version 2, %d bytes.\n\n%s", len(lines[1]), lines[1])}},
		{name: "a text file's reference reads the text of its latest version", arguments: map[string]any{"ref": errorRef},
			route: versionRow, number: "2", holds: []string{fmt.Sprintf(", version 2, %d bytes.\n\n%s", len(lines[1]), lines[1])}},
		{name: "a text file read at a version reads that version", arguments: map[string]any{"issue": issue, "artifact": errorFile.Slug, "version": 1},
			route: versionRow, number: "1", holds: []string{fmt.Sprintf(", version 1 of 2, %d bytes.\n\n%s", len(lines[0]), lines[0])}, lacks: []string{"Request Entity Too Large"}},
		{name: "a text file's reference at a version reads that version", arguments: map[string]any{"ref": errorRef + "@v1"},
			route: versionRow, number: "1", holds: []string{fmt.Sprintf(", version 1 of 2, %d bytes.\n\n%s", len(lines[0]), lines[0])}},
		{name: "a file that is not UTF-8 text is described and not sent", arguments: map[string]any{"issue": issue, "artifact": binary.Slug},
			route: versionRow, number: "1",
			holds: []string{
				fmt.Sprintf("dump.bin is an uploaded application/octet-stream file (version 1, %d bytes) that is not UTF-8 text", len(dump)),
				"GET /api/v1/artifacts/" + binary.ID + "/versions/1 serves its bytes.",
			},
			lacks: []string{"binary dump"}},
		{name: "a picture is described and not sent", arguments: map[string]any{"issue": issue, "artifact": chart.Slug},
			route: versionRow, number: "1",
			holds: []string{
				fmt.Sprintf("chart.png is an uploaded image/png file (version 1, %d bytes)", len(shot)),
				"GET /api/v1/artifacts/" + chart.ID + "/versions/1 serves its bytes.",
			},
			lacks: []string{"a chart of uploads"}},
		{name: "a document still reads its markdown", arguments: map[string]any{"issue": issue},
			route: "GET /api/v1/issues/{key}/artifacts/{slug}/text", holds: []string{"The proxy refuses a large upload."}},
	} {
		t.Run(c.name, func(t *testing.T) {
			result, served := read(t, c.name, c.arguments)
			if len(served) != 2 || served[0].route != recordRow {
				t.Fatalf("the call read %v, want the item's record and then %s; the model was told %q", served, c.route, result)
			}
			if sent := served[1]; sent.route != c.route || sent.path["number"] != c.number {
				t.Fatalf("the call read %s at version %q, want %s at version %q; the model was told %q", sent.route, sent.path["number"], c.route, c.number, result)
			}
			if !strings.Contains(result, "status 200") {
				t.Errorf("the model was told %q, want status 200", result)
			}
			for _, held := range c.holds {
				if !strings.Contains(result, held) {
					t.Errorf("the model was told %q, which does not hold %q", result, held)
				}
			}
			for _, lacked := range c.lacks {
				if strings.Contains(result, lacked) {
					t.Errorf("the model was told %q, which holds %q", result, lacked)
				}
			}
		})
	}

	t.Run("a file's blocks are refused in the command's words", func(t *testing.T) {
		result, served := read(t, "list the blocks of the upload error", map[string]any{"issue": issue, "artifact": errorFile.Slug, "blocks": true})
		const refusal = "upload-error.txt is an uploaded file, not a document, so it has no blocks. Read it with dispatch doc-read without --blocks."
		if !strings.Contains(result, agentwire.CodeCallRefused) || !strings.Contains(result, refusal) {
			t.Errorf("the model was told %q, want the refusal %q", result, refusal)
		}
		if len(served) != 1 || served[0].route != recordRow {
			t.Errorf("the call read %v, want the item's record alone", served)
		}
	})
}
