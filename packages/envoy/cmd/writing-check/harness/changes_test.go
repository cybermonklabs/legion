package harness

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/cmd/writing-check/harness/claude"
	"github.com/sjawhar/envoy/cmd/writing-check/harness/llm"
	"github.com/sjawhar/envoy/internal/dispatch/modeltest"
	"github.com/sjawhar/envoy/internal/dispatch/reviewer"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck"
)

// Every text below is written for these tests; none comes from Dispatch.
const (
	changeWriter   = "01a10000-0000-7000-8000-00000000000a"
	changeAskText  = "Ship the zork train?\n- Ship it: Release it today.\n- Wait: Hold it until the review."
	changeComment  = "We spin up the node. The zork train is ready."
	changeQuestion = "What is the zork train?"
	writerAnswer   = "The zork train is the queue of pull requests that merge at noon."
	candidateID    = "claude-haiku-5-5"
)

// The markers of the writer's prompts and the component's, by which the stand-in tells them apart.
const (
	markWriterRewrite  = `Do what "dispatch_message" tells you.`
	markWriterFindings = `A reviewer read your text`
	markWriterAnswer   = `is a question about your text`
	markRewrite        = "You write a text again for its reader."
	markDefine         = "You define words for a reader."
	markDetector       = "For each candidate sentence"
)

// The writing-rules.ts refusal is the command's own: the Go copy change 2's writer reads holds the
// lines of refusalText in the TypeScript source, the command's name where it stands.
func TestTheRefusalIsTheCommands(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "envoy-client", "src", "writing-rules.ts"))
	if err != nil {
		t.Fatal(err)
	}
	_, body, found := strings.Cut(string(source), "function refusalText(")
	body, _, ended := strings.Cut(body, `].join("\n")`)
	if !found || !ended {
		t.Fatal("writing-rules.ts holds no refusalText joined by newlines")
	}
	var lines []string
	for _, m := range regexp.MustCompile("(?m)^\\s*(\"(?:[^\"\\\\]|\\\\.)*\"|`[^`]*`|rules\\.trimEnd\\(\\)),?$").FindAllStringSubmatch(body, -1) {
		lines = append(lines, m[1])
	}
	if len(lines) != len(writingRulesRefusalLines)+1 || lines[len(lines)-1] != "rules.trimEnd()" {
		t.Fatalf("refusalText's lines = %q, want %d lines and then the rules", lines, len(writingRulesRefusalLines))
	}
	for i, want := range writingRulesRefusalLines {
		got := lines[i]
		if strings.HasPrefix(got, "`") {
			got = strconv.Quote(strings.ReplaceAll(strings.Trim(got, "`"), "${commandName(tool)}", "%s"))
		}
		if unquoted, err := strconv.Unquote(got); err != nil || unquoted != want {
			t.Errorf("line %d: writing-rules.ts has %s, the harness %q", i, lines[i], want)
		}
	}
	if got := writingRulesRefusal("dispatch_comment", "RULES\n\n"); !strings.HasPrefix(got, "dispatch comment was not called:") || !strings.HasSuffix(got, "\n\nRULES") {
		t.Errorf("the refusal = %q", got)
	}
}

// A combination's changes run in the order a text meets them, and a combination that cannot run
// is refused: 3 with 4, 7b with no reviewer, a change twice or none.
func TestCombinationsAndPlans(t *testing.T) {
	combos, err := Combinations()
	if err != nil || len(combos) == 0 {
		t.Fatalf("combinations.yaml: %v, %v", combos, err)
	}
	plan, err := planOf("x", []string{"7b", "3", "2", "6"})
	if err != nil || !slices.Equal(plan.steps, []string{"2", "3", "6"}) || !plan.inputs || !plan.writer() || !plan.local() {
		t.Errorf("plan = %+v, %v", plan, err)
	}
	for _, bad := range [][]string{{"3", "4"}, {"7b"}, {"7b", "5"}, {"2", "2"}, {"8"}} {
		if _, err := planOf("x", bad); err == nil {
			t.Errorf("planOf(%v) is no error", bad)
		}
	}
	if p, _ := planOf("7a", []string{"7a"}); p.local() || !p.candidate() || !p.writer() {
		t.Errorf("7a reads no local Dispatch and calls the candidate and the writer: %+v", p)
	}
	if p, _ := planOf("2", []string{"2"}); p.candidate() {
		t.Error("change 2 calls no candidate")
	}
}

// An ask's text splits into its question and options and joins back as it was.
func TestAnAskSplitsAndJoinsBack(t *testing.T) {
	for _, text := range []string{changeAskText, "A question with no option?", "Pick one:\n\n- a\n- b: the second"} {
		question, options := splitAsk(text)
		if got := joinAsk(question, options); got != text {
			t.Errorf("joinAsk(splitAsk(%q)) = %q", text, got)
		}
	}
	if question, options := splitAsk(changeAskText); question != "Ship the zork train?" || len(options) != 2 || options[1].Description != "Hold it until the review." {
		t.Errorf("splitAsk = %q, %+v", question, options)
	}
}

// fakeDispatch is the local Dispatch and its fake Envoy as the changes call them, scripted: the
// draft route answers its scripted answers in turn, a Rewrite and a Define send the writer one
// side question when the writer is live, and every request is kept. The n-th ask opened is
// "ask-n", its Rewrite's side question "question-n", and the n-th definition "definition-n".
type fakeDispatch struct {
	t           *testing.T
	server      *httptest.Server
	mu          sync.Mutex
	drafts      []draftAnswer
	draftIn     []map[string]any
	headers     []string
	live        []string
	sends       []map[string]any
	replies     map[string]string
	rewrites    map[string]string
	defined     map[string]string
	rules       []map[string]any
	asks        int
	definitions int
}

func newFakeDispatch(t *testing.T, drafts ...draftAnswer) *fakeDispatch {
	f := &fakeDispatch{t: t, drafts: drafts, replies: map[string]string{}, rewrites: map[string]string{}, defined: map[string]string{}}
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	read := func(r *http.Request) map[string]any {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		return body
	}
	// send is a side question to the writer, as the server sends it through the listener.
	send := func(id, body string) {
		payload, _ := json.Marshal(map[string]any{"event": map[string]any{"payload": map[string]any{"id": id, "body": body}}, "delivery": map[string]any{"attempt": 1}})
		f.sends = append(f.sends, map[string]any{"target_session": changeWriter, "payload": string(payload)})
	}
	mux.HandleFunc("GET /auth/_dev/signin", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "dsession", Value: r.URL.Query().Get("login")})
		w.WriteHeader(http.StatusFound)
	})
	mux.HandleFunc("POST /api/v1/writing-rules", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.rules = append(f.rules, read(r))
		f.mu.Unlock()
		write(w, http.StatusOK, map[string]string{"reader": "reader@example.com", "text": "RULES: say the fact."})
	})
	mux.HandleFunc("POST /api/v1/reviewer-drafts", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.draftIn = append(f.draftIn, read(r))
		f.headers = append(f.headers, r.Header.Get("Dispatch-Test-Model"))
		if len(f.drafts) == 0 {
			write(w, http.StatusOK, draftAnswer{})
			return
		}
		answer := f.drafts[0]
		f.drafts = f.drafts[1:]
		write(w, http.StatusOK, answer)
	})
	mux.HandleFunc("POST /api/v1/issues/{key}/asks", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.asks++
		write(w, http.StatusCreated, map[string]string{"id": "ask-" + strconv.Itoa(f.asks)})
	})
	mux.HandleFunc("POST /api/v1/asks/{id}/rewrites", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.headers = append(f.headers, r.Header.Get("Dispatch-Test-Model"))
		f.rewrites[r.PathValue("id")] = "rewriting"
		if slices.Contains(f.live, changeWriter) {
			send("question-"+strings.TrimPrefix(r.PathValue("id"), "ask-"), "A person asked Dispatch to write your ask dispatch://TEST-1/ask/"+r.PathValue("id")+" again.")
		} else {
			f.rewrites[r.PathValue("id")] = "rewritten"
		}
		write(w, http.StatusAccepted, map[string]any{"ask_id": r.PathValue("id"), "started": true})
	})
	mux.HandleFunc("GET /api/v1/asks/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		question := "Ship the zork train, the queue of pull requests that merge at noon?"
		write(w, http.StatusOK, map[string]any{"ask": map[string]any{"question": question, "options": []map[string]string{
			{"label": "Ship it", "description": "Release it today."}, {"label": "Wait", "description": "Hold it until the review."},
		}, "rewrite": map[string]string{"state": f.rewrites[r.PathValue("id")]}}})
	})
	mux.HandleFunc("POST /api/v1/definitions", func(w http.ResponseWriter, r *http.Request) {
		body := read(r)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.headers = append(f.headers, r.Header.Get("Dispatch-Test-Model"))
		f.definitions++
		id := "definition-" + strconv.Itoa(f.definitions)
		state := "none"
		if slices.Contains(f.live, changeWriter) {
			state = "waiting"
			ask, _ := body["ask_id"].(string)
			send(id, "A person reads your ask dispatch://TEST-1/ask/"+ask+". They selected these words in it and asked what they mean: zork train")
		}
		f.defined[id] = state
		write(w, http.StatusCreated, f.definition(id))
	})
	mux.HandleFunc("GET /api/v1/definitions/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		write(w, http.StatusOK, f.definition(r.PathValue("id")))
	})
	mux.HandleFunc("POST /api/v1/messages/{id}/reply", func(w http.ResponseWriter, r *http.Request) {
		body := read(r)
		f.mu.Lock()
		defer f.mu.Unlock()
		answer, _ := body["body"].(string)
		f.replies[r.PathValue("id")] = answer
		switch id := r.PathValue("id"); {
		case strings.HasPrefix(id, "question-"):
			f.rewrites["ask-"+strings.TrimPrefix(id, "question-")] = "rewritten"
		case strings.HasPrefix(id, "definition-"):
			f.defined[id] = "answered"
		}
		write(w, http.StatusOK, map[string]string{"id": r.PathValue("id")})
	})
	mux.HandleFunc("GET /api/v1/readers/{login}/words", func(w http.ResponseWriter, r *http.Request) {
		// The harness reads one page of as many words as the reviewer's own record holds.
		if limit := r.URL.Query().Get("limit"); limit != strconv.Itoa(readerWordsShown) {
			write(w, http.StatusBadRequest, map[string]string{"code": "INVALID_QUERY", "error": "the harness asked for limit=" + limit})
			return
		}
		write(w, http.StatusOK, map[string]any{"items": []map[string]string{{"word": "zork"}}, "total": 1, "limit": readerWordsShown, "offset": 0})
	})
	mux.HandleFunc("GET /v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var sessions []map[string]any
		for _, s := range f.live {
			sessions = append(sessions, map[string]any{"session_id": s, "capabilities": []string{"btw"}})
		}
		write(w, http.StatusOK, sessions)
	})
	mux.HandleFunc("PUT /__fixture/sessions", func(w http.ResponseWriter, r *http.Request) {
		var sessions []struct {
			SessionID string `json:"session_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&sessions)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.live, f.sends = nil, nil
		for _, s := range sessions {
			f.live = append(f.live, s.SessionID)
		}
		write(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /__fixture/sends", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		write(w, http.StatusOK, f.sends)
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// definition is the definition id as Define answers it, the writer's answer with it once it came.
func (f *fakeDispatch) definition(id string) map[string]any {
	agent := map[string]any{"state": f.defined[id]}
	if f.defined[id] == "answered" {
		agent["answer"] = f.replies[id]
	}
	return map[string]any{"id": id, "definitions": []map[string]any{{"words": "zork train", "meaning": "What the record calls the train.", "links": []string{}}},
		"agent": agent, "calls": []serverCall{{Purpose: "define", Deployment: candidateID, Served: candidateID, InputTokens: 1000, OutputTokens: 100}}}
}

// changeCases are the cases of these tests: an ask and a comment whose writer has a session, and a
// comment whose session the export lacks.
func changeCases() []*Case {
	record := CaseRecord{
		Project: "TEST", Issue: &CaseIssue{Key: "TEST-1", Title: "Build the zork", Root: "TEST-1"},
		Thread: []CaseText{{Key: "comment:t1", Kind: "comment", ActorKind: "user", Actor: fixtureOwner, Text: "Start the zork work.", Time: "2026-11-01T00:00:00Z"}},
	}
	owner := &CaseText{Key: "comment:o", ActorKind: "user", Actor: fixtureOwner, Text: changeQuestion, Selected: []string{"zork train"},
		Verdict: &CaseVerdict{Verdict: "asks_word_meaning"}}
	agent := func(key, text string) CaseText {
		return CaseText{Key: key, Kind: strings.Split(key, ":")[0], ActorKind: "session", Actor: changeWriter, Session: changeWriter, Text: text}
	}
	return []*Case{
		{ID: "question/ask:k1@11", Group: GroupQuestion, Kind: "ask", SplitGroup: "T1", WriterSession: changeWriter, Agent: agent("ask:k1", changeAskText), OwnerReply: owner, Record: record},
		{ID: "question/comment:c2@20", Group: GroupQuestion, Kind: "comment", SplitGroup: "T2", WriterSession: changeWriter, Agent: agent("comment:c2", changeComment), OwnerReply: owner, Record: record},
		{ID: "plain/comment:c3@30", Group: GroupPlain, Kind: "comment", SplitGroup: "T3", WriterSession: "01a10000-0000-7000-8000-00000000000b", Agent: agent("comment:c3", changeComment), Record: record},
	}
}

// changeSessions is the session part of each case: the writer's session up to the ask and the
// first comment, and none for the comment whose session the export lacks.
func changeSessions(t *testing.T) map[string]claude.SessionText {
	t.Helper()
	dir := t.TempDir()
	row := func(index int, text string) map[string]any {
		return map[string]any{"session_id": changeWriter, "index": index, "row": map[string]any{
			"session_id": changeWriter, "host": "a", "seq": index, "ts": "2026-11-01 00:00:0" + strconv.Itoa(index), "role": "assistant", "kind": "assistant", "text": text}}
	}
	writeGzipLines(t, filepath.Join(dir, "session-rows.jsonl.gz"), row(0, "SESSIONFACT: the zork train is the queue of pull requests."), row(1, "Later."))
	places := `{"case_id":"question/ask:k1@11","session_id":"` + changeWriter + `","status":"found","rows":1}
{"case_id":"question/comment:c2@20","session_id":"` + changeWriter + `","status":"found","rows":1}
{"case_id":"plain/comment:c3@30","session_id":"01a10000-0000-7000-8000-00000000000b","status":"session_missing"}
`
	if err := os.WriteFile(filepath.Join(dir, "places.jsonl"), []byte(places), 0o600); err != nil {
		t.Fatal(err)
	}
	ids := []string{"question/ask:k1@11", "question/comment:c2@20", "plain/comment:c3@30"}
	sessions, err := claude.Excerpts(dir, ids, 30000)
	if err != nil {
		t.Fatal(err)
	}
	if sessions["question/ask:k1@11"].Empty() || !sessions["plain/comment:c3@30"].Empty() {
		t.Fatal("the fixture's session parts are not the ones the tests need")
	}
	return sessions
}

// writerRules are the stand-in's answers to the writer and the candidate.
func writerRules(t *testing.T) []modeltest.Rule {
	return []modeltest.Rule{
		{Match: func(r modeltest.Request) bool {
			return strings.Contains(r.Text, markWriterRewrite) && strings.Contains(r.Text, "Ship the zork")
		}, Reply: modeltest.Reply{Text: asJSON(t, map[string]string{"text": "Ship the zork train, the queue that merges at noon?\n- Ship it: Release it today.\n- Wait: Hold it until the review."})}},
		{Contains: markWriterRewrite, Reply: modeltest.Reply{Text: asJSON(t, map[string]string{"text": "We start the test node. The zork train, the queue of pull requests, is ready."})}},
		{Contains: markWriterFindings, Reply: modeltest.Reply{Text: asJSON(t, map[string]string{"1_action": "edit", "2_text": "EDITED: the zork train merges at noon.", "3_reason": ""})}},
		{Contains: markWriterAnswer, Reply: modeltest.Reply{Text: writerAnswer}},
		{Contains: markRewrite, Reply: modeltest.Reply{Text: `{"body":"The zork train, the queue of pull requests, is ready."}`}},
		{Contains: markDefine, Reply: modeltest.Reply{Text: `{"definitions":[{"words":"zork train","meaning":"The queue of pull requests.","links":[]}]}`}},
		{Contains: markDetector, Reply: modeltest.Reply{Text: `{"vacuous":[]}`}},
	}
}

// changeRig is a changeRun of a plan against the fake Dispatch and the stand-in model.
func changeRig(t *testing.T, steps []string, fake *fakeDispatch) (*changeRun, *modeltest.Model) {
	t.Helper()
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	stand := modeltest.New(t, writerRules(t)...)
	plan, err := planOf(strings.Join(steps, "+"), steps)
	if err != nil {
		t.Fatal(err)
	}
	runner := &llm.Runner{Limit: llm.NewLimit(4)}
	key := keyFile(t)
	writer, err := claude.New(stand.URL, key, cfg.Models.Roles.Writer.ID, "", cfg.Prices.Prices[cfg.Models.Roles.Writer.ID].agentloop(), runner)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := claude.New(stand.URL, key, candidateID, "", cfg.Prices.Prices[candidateID].agentloop(), runner)
	if err != nil {
		t.Fatal(err)
	}
	ds := &Dataset{Cases: changeCases(), docs: map[docKey]string{}}
	slices.SortFunc(ds.Cases, func(a, b *Case) int { return strings.Compare(a.ID, b.ID) })
	loaded := map[string]Loaded{}
	for _, c := range ds.Cases {
		loaded[c.ID] = Loaded{CaseID: c.ID, Reader: "reader@example.com", Project: "TEST", Issue: "TEST-1", ReplyTo: "comment-0"}
	}
	run := &changeRun{
		plan: plan, ds: ds, cfg: cfg, candidate: candidateID, candModel: candidate.Model(), runner: runner,
		writer: &Writer{Model: writer}, rules: []reviewer.RuleSet{reviewer.RuleNoslop}, rounds: 2, unedited: uneditedRewrite,
		sessions: changeSessions(t), tries: 2, backoff: time.Millisecond, poll: 10 * time.Millisecond, loaded: loaded,
	}
	if fake != nil {
		run.local = newLocalClient(LocalInstance{BaseURL: fake.server.URL, AgentToken: "e2e-token"})
		run.envoy = newFakeEnvoy(fake.server.URL)
		run.rewriteRead = func(_ context.Context, id string) (rewriteRead, error) {
			return rewriteRead{Calls: []serverCall{{Purpose: "rewrite", Deployment: candidateID, Served: candidateID, InputTokens: 2000, OutputTokens: 200}}, AgentAnswer: true}, nil
		}
		if err := run.envoy.seed(t.Context(), []string{changeWriter}); err != nil {
			t.Fatal(err)
		}
	}
	return run, stand
}

// apply is the change of the case id, which must end with no error.
func apply(t *testing.T, run *changeRun, id string) Applied {
	t.Helper()
	applied, err := run.Apply(t.Context(), run.ds.Case(id))
	if err != nil {
		t.Fatalf("%s: %v", id, err)
	}
	return applied
}

// outcomes are the outcomes a change's trace holds, in order.
func outcomes(a Applied) []string {
	var out []string
	for _, s := range a.Trace {
		out = append(out, s.Outcome)
	}
	return out
}

// Session text reaches the writer alone: no request to the candidate holds it.
func noSessionToTheCandidate(t *testing.T, stand *modeltest.Model) {
	t.Helper()
	for _, r := range stand.Requests() {
		if strings.Contains(r.Text, "SESSIONFACT") && r.Model != "claude-opus-5-5" {
			t.Errorf("model %s read session text", r.Model)
		}
	}
}

// Change 2: the writer gets the command's refusal with the rules the local Dispatch answers for
// the write, and its session, and writes its text again; a document is no write the command
// refuses.
func TestChangeTwoWritesTheTextAgainAfterTheRules(t *testing.T) {
	fake := newFakeDispatch(t)
	run, stand := changeRig(t, []string{changeRules}, fake)
	applied := apply(t, run, "question/comment:c2@20")
	if applied.Same || !strings.Contains(applied.Text, "test node") || len(applied.Calls) != 1 || applied.Calls[0].Model != "claude-opus-5-5" || applied.WaitSeconds <= 0 {
		t.Errorf("change 2 = %+v", applied)
	}
	if len(fake.rules) != 1 || fake.rules[0]["route"] != "POST /api/v1/issues/TEST-1/comments" {
		t.Errorf("the rules asked for %+v", fake.rules)
	}
	writer := stand.Requests()[0].Text
	if !strings.Contains(writer, "dispatch comment was not called") || !strings.Contains(writer, "RULES: say the fact.") || !strings.Contains(writer, "<writer_session>") {
		t.Errorf("the writer's turn holds no refusal, rules or session: %s", writer)
	}
	ask := apply(t, run, "question/ask:k1@11")
	if q, options := splitAsk(ask.Text); len(options) != 2 || !strings.Contains(q, "noon") {
		t.Errorf("an ask written again keeps its options: %q", ask.Text)
	}
}

// Change 3: the reviewer's findings go to the writer, which edits; the edit is reviewed again, and
// after the bound of reviews the last edit posts. With no session the writer is not running, and
// the reviewer's rewrite posts (DISPATCH_REVIEWER_UNEDITED=rewrite).
func TestChangeThreeBoundsTheWritersEdits(t *testing.T) {
	finding := []reviewer.Finding{{RuleSet: reviewer.RuleNoslop, Quote: "spin up", Why: "A phrasal verb.", Fix: "start"}}
	found := draftAnswer{Findings: finding, Rewrite: &wording{Body: "We start the node."},
		Calls: []serverCall{{Purpose: "review", Deployment: candidateID, Served: candidateID, InputTokens: 1000, OutputTokens: 100}}}
	fake := newFakeDispatch(t, found, found, found)
	run, stand := changeRig(t, []string{changeFindings}, fake)
	applied := apply(t, run, "question/comment:c2@20")
	if got := outcomes(applied); !slices.Equal(got, []string{"review", "edited", "review", "edited", "bound"}) {
		t.Errorf("the trace = %v, want two reviews and two edits", got)
	}
	if applied.Text != "EDITED: the zork train merges at noon." || len(fake.draftIn) != 2 || fake.headers[0] != candidateID {
		t.Errorf("change 3 = %q after %d reviews naming %v", applied.Text, len(fake.draftIn), fake.headers)
	}
	if fake.draftIn[1]["text"] != "EDITED: the zork train merges at noon." {
		t.Errorf("the second review read %v, want the edit", fake.draftIn[1]["text"])
	}
	frame := stand.Requests()[0].Text
	if !strings.Contains(frame, "A reviewer read it and found 1 problem") || !strings.Contains(frame, "dispatch held-edit") {
		t.Errorf("the writer did not get the findings' frame: %s", frame)
	}
	var cost float64
	for _, c := range applied.Calls {
		cost += c.CostUSD
	}
	if len(applied.Calls) != 4 || cost <= 0 {
		t.Errorf("the calls = %+v, want two reviews and two edits, priced", applied.Calls)
	}
	noSession := apply(t, run, "plain/comment:c3@30")
	if noSession.Text != "We start the node." || !slices.Contains(outcomes(noSession), "no_writer_rewrite_posted") || len(stand.Requests()) != 2 {
		t.Errorf("with no session: %q, %v, after %d writer calls", noSession.Text, outcomes(noSession), len(stand.Requests()))
	}
	run.unedited = uneditedAsWritten
	fake.drafts = []draftAnswer{found}
	if asWritten := apply(t, run, "plain/comment:c3@30"); !asWritten.Same {
		t.Errorf("with no session and as_written, the text posts as written: %q", asWritten.Text)
	}
}

// Change 4: the reviewer's rewrite posts where it found something and the rewrite is applied, and
// the text posts as written where the rewrite is not applied or the review found nothing.
func TestChangeFourPostsTheReviewersRewrite(t *testing.T) {
	finding := []reviewer.Finding{{RuleSet: reviewer.RuleNoslop, Quote: "spin up", Why: "A phrasal verb.", Fix: "start"}}
	fake := newFakeDispatch(t,
		draftAnswer{Findings: finding, Rewrite: &wording{Body: "We start the node."}},
		draftAnswer{Findings: finding, Rewrite: &wording{Body: "Lost the first line."}, NotApplied: "it drops the first line"},
		draftAnswer{},
	)
	run, stand := changeRig(t, []string{changeReviewRewrite}, fake)
	if a := apply(t, run, "question/comment:c2@20"); a.Text != "We start the node." {
		t.Errorf("the rewrite = %q", a.Text)
	}
	if a := apply(t, run, "question/comment:c2@20"); !a.Same || !slices.Contains(outcomes(a), "posted_as_written") {
		t.Errorf("a rewrite not applied = %q, %v", a.Text, outcomes(a))
	}
	if a := apply(t, run, "question/comment:c2@20"); !a.Same || !slices.Contains(outcomes(a), "passed") {
		t.Errorf("a clean review = %q, %v", a.Text, outcomes(a))
	}
	if len(stand.Requests()) != 0 {
		t.Error("change 4 plays no writer")
	}
}

// A scored case's line holds the text a person sees, the string its text_sha256 hashes, an ask's
// options with it; and a review's step holds the findings the draft route answered, each finding
// it dropped with its rule set and quote, and the rewrite that was not applied beside the reason.
func TestACaseLineHoldsItsTextAndWhatTheReviewReturned(t *testing.T) {
	finding := []reviewer.Finding{{RuleSet: reviewer.RuleNoslop, Quote: "spin up", Why: "A phrasal verb.", Fix: "start"}}
	dropped := []reviewer.DroppedFinding{{RuleSet: reviewer.RulePhrases, Quote: "The store keeps … rows."}}
	fake := newFakeDispatch(t,
		draftAnswer{Findings: finding, Dropped: dropped, Rewrite: &wording{Body: "Lost the first line."}, NotApplied: "it drops the first line"},
		draftAnswer{},
	)
	run, _ := changeRig(t, []string{changeReviewRewrite}, fake)
	// Both texts post as written, so lost and added are by construction and no grader is called.
	s := &scorer{ds: run.ds, sessions: run.sessions, only: []Judgement{JLost, JAdded}}
	cases := []*Case{run.ds.Case("question/comment:c2@20"), run.ds.Case("question/ask:k1@11")}
	l := &lane{change: run, out: t.TempDir(), todo: map[string]bool{}}
	for _, c := range cases {
		l.todo[c.ID] = true
	}
	if err := scoreAll(t.Context(), s, []*lane{l}, cases, 1, io.Discard, time.Now()); err != nil || l.failed != 0 {
		t.Fatalf("score: %d failed, %v", l.failed, err)
	}
	data, err := os.ReadFile(filepath.Join(l.out, "cases.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	type step struct {
		Outcome    string                    `json:"outcome"`
		Findings   []reviewer.Finding        `json:"findings"`
		Dropped    []reviewer.DroppedFinding `json:"dropped"`
		Rewrite    string                    `json:"rewrite"`
		NotApplied string                    `json:"not_applied"`
	}
	type caseLine struct {
		CaseID      string `json:"case_id"`
		Text        string `json:"text"`
		TextSHA256  string `json:"text_sha256"`
		ChangeTrace []step `json:"change_trace"`
	}
	lines := map[string]caseLine{}
	for raw := range strings.Lines(string(data)) {
		var l caseLine
		if err := json.Unmarshal([]byte(raw), &l); err != nil {
			t.Fatal(err)
		}
		lines[l.CaseID] = l
	}
	comment, ask := lines["question/comment:c2@20"], lines["question/ask:k1@11"]
	for _, l := range []caseLine{comment, ask} {
		if l.Text == "" || sha256Hex([]byte(l.Text)) != l.TextSHA256 {
			t.Errorf("%s: text %q is not what text_sha256 %s hashes", l.CaseID, l.Text, l.TextSHA256)
		}
	}
	if comment.Text != changeComment || ask.Text != changeAskText {
		t.Errorf("the texts = %q and %q, want the comment and the ask with its options", comment.Text, ask.Text)
	}
	if len(comment.ChangeTrace) == 0 {
		t.Fatal("the comment's line holds no trace")
	}
	review := comment.ChangeTrace[0]
	if review.Outcome != "review" || !slices.Equal(review.Findings, finding) || !slices.Equal(review.Dropped, dropped) {
		t.Errorf("the review step = %+v, want the review's findings and the finding it dropped", review)
	}
	if review.Rewrite != "Lost the first line." || review.NotApplied != "it drops the first line" {
		t.Errorf("the review step keeps rewrite %q beside reason %q, want the rewrite not applied and its reason", review.Rewrite, review.NotApplied)
	}
	if len(ask.ChangeTrace) == 0 || ask.ChangeTrace[0].Findings != nil || ask.ChangeTrace[0].Dropped != nil || ask.ChangeTrace[0].Rewrite != "" {
		t.Errorf("a clean review's step = %+v, want no findings, none dropped and no rewrite", ask.ChangeTrace)
	}
}

// Change 5: an ask is rewritten by the local Dispatch's Rewrite, whose side question the writer
// answers through the fake Envoy; a comment by the component, after the same side question; and a
// comment whose writer has no session gets no side answer (plan Decision 18).
func TestChangeFiveAnswersOneSideQuestion(t *testing.T) {
	fake := newFakeDispatch(t)
	run, stand := changeRig(t, []string{changeRewrite}, fake)
	ask := apply(t, run, "question/ask:k1@11")
	if !strings.Contains(ask.Text, "merge at noon") || ask.WriterAnswer != writerAnswer || fake.replies["question-1"] != writerAnswer {
		t.Errorf("the ask's Rewrite = %q, answer %q, reply %q", ask.Text, ask.WriterAnswer, fake.replies["question-1"])
	}
	if !slices.Contains(fake.headers, candidateID) || len(ask.Calls) != 2 || ask.Calls[1].Model != candidateID || ask.Calls[1].CostUSD <= 0 {
		t.Errorf("the Rewrite's calls = %+v, headers %v", ask.Calls, fake.headers)
	}
	comment := apply(t, run, "question/comment:c2@20")
	if comment.Text != "The zork train, the queue of pull requests, is ready." || comment.WriterAnswer != writerAnswer {
		t.Errorf("the comment's Rewrite = %q, answer %q", comment.Text, comment.WriterAnswer)
	}
	before := len(stand.Requests())
	noSession := apply(t, run, "plain/comment:c3@30")
	if noSession.WriterAnswer != "" || len(stand.Requests()) != before+1 {
		t.Errorf("with no session: answer %q, %d calls, want the rewrite's alone", noSession.WriterAnswer, len(stand.Requests())-before)
	}
	for _, r := range stand.Requests() {
		if strings.Contains(r.Text, markRewrite) && strings.Contains(r.Text, "SESSIONFACT") {
			t.Error("the component's rewrite read session text")
		}
	}
	noSessionToTheCandidate(t, stand)
}

// Change 6: Define of the words of the person's question, the definition from the record with the
// writer's answer to its side question beside it, for an ask through the local Dispatch and for a
// comment through the component; a case whose question names no words is defined nothing.
func TestChangeSixDefinesTheAskedWords(t *testing.T) {
	fake := newFakeDispatch(t)
	run, stand := changeRig(t, []string{changeDefine}, fake)
	ask := apply(t, run, "question/ask:k1@11")
	if !ask.Same || !strings.Contains(ask.Definition, "zork train: What the record calls the train.") || !strings.Contains(ask.Definition, writerAnswer) {
		t.Errorf("the ask's definition = %q", ask.Definition)
	}
	comment := apply(t, run, "question/comment:c2@20")
	if !strings.Contains(comment.Definition, "The queue of pull requests.") || comment.WriterAnswer != writerAnswer {
		t.Errorf("the comment's definition = %q", comment.Definition)
	}
	if none := apply(t, run, "plain/comment:c3@30"); none.Definition != "" || !slices.Contains(outcomes(none), "no_words") {
		t.Errorf("no words: %+v", none)
	}
	noSessionToTheCandidate(t, stand)
}

// Change 7a: the structure rules and the detector, whose reader is the candidate, find what the
// text breaks; the writer gets #3981's refusal with the findings and writes its text again.
func TestChangeSevenAWritesAgainAfterTheCheck(t *testing.T) {
	run, stand := changeRig(t, []string{changeCheck}, nil)
	applied := apply(t, run, "question/comment:c2@20")
	if !strings.Contains(applied.Text, "test node") || !slices.Contains(outcomes(applied), "written_again") {
		t.Errorf("change 7a = %q, %v", applied.Text, outcomes(applied))
	}
	var turn string
	for _, r := range stand.Requests() {
		if strings.Contains(r.Text, markWriterRewrite) {
			turn = r.Text
		}
	}
	if !strings.Contains(turn, "The writing check refused this write.") || !strings.Contains(turn, "phrasal-verb") {
		t.Errorf("the writer's turn holds no check refusal: %s", turn)
	}
	var written TraceStep
	for _, s := range applied.Trace {
		if s.Outcome == "written_again" {
			written = s
		}
	}
	if !slices.ContainsFunc(written.Findings, func(f TraceFinding) bool {
		return f.RuleSet == string(reviewer.RuleStructure) && f.Rule == "phrasal-verb" && strings.Contains(f.Quote, "spin up") && f.Fix != ""
	}) {
		t.Errorf("the step written again keeps no finding of the check's: %+v", written.Findings)
	}
	if got := checkRefusal([]writingcheck.Finding{{Field: "body", Rule: "r", Text: "x", Rewrite: `Write "start".`}}); !strings.Contains(got, "1. Write `start`. Rule `r`, field `body`: `x`.") {
		t.Errorf("the check's refusal = %q", got)
	}
}
