// packages/envoy/internal/broker/judge/standin_test.go
// The stand-ins every test of this package runs against: a Dispatch that answers the judge's two
// GETs, a model endpoint that speaks Chat Completions, and a request machine that records what
// the judge wrote to it. Nothing here reaches the network or a database.
package judge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/internal/broker/airlock"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/brokertoken"
)

// testSecret is the one secret these tests ask for, testIssue the issue every signed reason
// names, and shortJudgeSecret the second secret with a judge of its own, whose 300 seconds are
// the bound a request asks past in the one case the class's own 15 minutes would let through.
const (
	testSecret        = "DEEL_API_KEY"
	testIssue         = "INFRA-1353"
	testModel         = "claude-standin"
	shortJudgeSecret  = "SHORT_JUDGE_KEY"
	shortJudgeSeconds = 300
)

// testRules give the judge 900 seconds of testSecret, 300 of shortJudgeSecret — a bound of its
// own well inside the class's 15 minutes, which is what a request can ask past while still
// asking for less than the class allows — and nothing of any other name.
func testRules() airlock.Rules {
	return airlock.Rules{Secrets: []airlock.SecretRule{
		{Name: testSecret, Hosts: []string{"api.deel.example"}, Judge: new(uint32(900))},
		{Name: shortJudgeSecret, Hosts: []string{"api.short.example"}, Judge: new(uint32(shortJudgeSeconds))},
		{Name: "NO_JUDGE_KEY", Hosts: []string{"other.example"}},
	}}
}

// pending is a request inside the class the judge decides: one secret whose entry has a judge,
// a signed lifetime of 15 minutes, and a reason naming an open issue.
func pending() requests.PendingRequest {
	return requests.PendingRequest{
		RecordID: "record-1", RequestID: "request-1", EnrollmentID: "enrollment-1",
		SessionID: "legion-1353", Secrets: []string{testSecret},
		LifetimeSeconds: new(900), Reason: "Running the broker's own tests for " + testIssue,
		Approver: "sami@example.com",
	}
}

// dispatchCall is one request the Dispatch stand-in got.
type dispatchCall struct{ method, path, authorization string }

// dispatchStandIn answers the issue read and the spec read, and records every request it got so
// a test can see which routes the judge called and what it presented.
type dispatchStandIn struct {
	server *httptest.Server

	mu    sync.Mutex
	calls []dispatchCall
	// issueStatus and specStatus are what each route answers, and issue and spec the JSON
	// bodies it answers with when that status is 200.
	issueStatus, specStatus int
	issue, spec             map[string]any
	// delay holds each answer back, which is how a test spends the Dispatch phase's
	// bound. A read the judge gave up on ends the handler at once, so a stand-in told to
	// wait longer than the bound never keeps the server from closing.
	delay time.Duration
}

// newDispatch is a Dispatch holding one open issue with a spec document.
func newDispatch(t *testing.T, status, markdown string) *dispatchStandIn {
	t.Helper()
	d := &dispatchStandIn{
		issueStatus: http.StatusOK, specStatus: http.StatusOK,
		issue: map[string]any{"key": testIssue, "title": "The broker's model judge", "status": status,
			"project": "INFRA", "number": 1353},
		spec: map[string]any{"markdown": markdown, "version": 12, "token": "sha256:standin"},
	}
	d.server = httptest.NewServer(http.HandlerFunc(d.serve))
	t.Cleanup(d.server.Close)
	return d
}

func (d *dispatchStandIn) serve(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	d.calls = append(d.calls, dispatchCall{r.Method, r.URL.Path, r.Header.Get("Authorization")})
	status, body, delay := d.issueStatus, d.issue, d.delay
	if strings.HasSuffix(r.URL.Path, "/artifacts/spec/text") {
		status, body = d.specStatus, d.spec
	}
	d.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if status != http.StatusOK {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"error":{"code":"REFUSED","message":"the stand-in refuses"}}`)
		return
	}
	_ = json.NewEncoder(w).Encode(body)
}

func (d *dispatchStandIn) got() []dispatchCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]dispatchCall(nil), d.calls...)
}

// modelStandIn is a model endpoint speaking Chat Completions: it answers each call with the same
// reply and records every request body, so a test can read the data object the judge sent.
type modelStandIn struct {
	server *httptest.Server

	mu sync.Mutex
	// bodies are the request bodies it got, in order.
	bodies [][]byte
	// status, when it is not 0 or 200, is answered in place of a reply, and reply is the
	// assistant content the stream carries. delay holds the answer back, which is how a
	// test spends a bound.
	status int
	reply  string
	delay  time.Duration
	tokens chatTokens
}

// chatTokens are the counts the stand-in's usage chunk reports.
type chatTokens struct{ prompt, completion, cached, reasoning int }

func newModel(t *testing.T, reply string) *modelStandIn {
	t.Helper()
	m := &modelStandIn{reply: reply, tokens: chatTokens{prompt: 1200, completion: 48, cached: 0, reasoning: 7}}
	m.server = httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(m.server.Close)
	return m
}

// verdictReply is the JSON object a model answers the judge's schema with.
func verdictReply(verdict, reason string) string {
	body, err := json.Marshal(map[string]any{"verdict": verdict, "reason": reason})
	if err != nil {
		panic(err)
	}
	return string(body)
}

func (m *modelStandIn) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	m.mu.Lock()
	m.bodies = append(m.bodies, body)
	status, reply, delay, tokens := m.status, m.reply, m.delay, m.tokens
	m.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	if status != 0 && status != http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"error":{"message":"the stand-in is unwell"}}`)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, chatStream(reply, tokens))
}

// chatStream is the event stream Chat Completions sends for one reply: the role, the content in
// one delta, the finish reason, the usage chunk and the line that ends it.
func chatStream(content string, tokens chatTokens) string {
	var out strings.Builder
	chunk := func(choices []any, usage any) {
		body := map[string]any{
			"id": "chatcmpl-standin", "object": "chat.completion.chunk", "created": 1,
			"model": testModel, "choices": choices,
		}
		if usage != nil {
			body["usage"] = usage
		}
		encoded, err := json.Marshal(body)
		if err != nil {
			panic(err)
		}
		fmt.Fprintf(&out, "event: chunk\ndata: %s\n\n", encoded)
	}
	choice := func(delta map[string]any, finish any) []any {
		return []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}
	}
	chunk(choice(map[string]any{"role": "assistant", "content": ""}, nil), nil)
	chunk(choice(map[string]any{"content": content}, nil), nil)
	chunk(choice(map[string]any{}, "stop"), nil)
	chunk([]any{}, map[string]any{
		"prompt_tokens": tokens.prompt, "completion_tokens": tokens.completion,
		"total_tokens":              tokens.prompt + tokens.completion,
		"prompt_tokens_details":     map[string]any{"cached_tokens": tokens.cached},
		"completion_tokens_details": map[string]any{"reasoning_tokens": tokens.reasoning},
	})
	out.WriteString("data: [DONE]\n\n")
	return out.String()
}

func (m *modelStandIn) asked() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.bodies)
}

// data is the Data object the judge sent with its first call, read back out of the user message.
func (m *modelStandIn) data(t *testing.T) Data {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.bodies) == 0 {
		t.Fatal("the model was never asked, so it got no data")
	}
	var call struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(m.bodies[0], &call); err != nil {
		t.Fatalf("the model call is no JSON object: %v\n%s", err, m.bodies[0])
	}
	var data Data
	for _, message := range call.Messages {
		if message.Role != "user" {
			continue
		}
		if err := json.Unmarshal([]byte(contentText(t, message.Content)), &data); err != nil {
			t.Fatalf("the user message is no Data object: %v\n%s", err, message.Content)
		}
		return data
	}
	t.Fatalf("the model call carries no user message:\n%s", m.bodies[0])
	return data
}

// contentText is a chat message's text, whether the provider shape carried it as one string or
// as a list of parts.
func contentText(t *testing.T, content json.RawMessage) string {
	t.Helper()
	var text string
	if json.Unmarshal(content, &text) == nil {
		return text
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &parts); err != nil || len(parts) == 0 {
		t.Fatalf("a message's content is neither a string nor parts: %s", content)
	}
	return parts[0].Text
}

// fakeRequests is the request machine as the judge sees it: it answers the policy question a
// test sets and keeps every detail object the judge wrote.
type fakeRequests struct {
	mu sync.Mutex
	// decision, withheld, admin and policyErr are what JudgePolicy answers, and
	// policyAsked the names it was asked about.
	decision    policy.Decision
	withheld    bool
	admin       bool
	policyErr   error
	policyAsked []string
	// state and stateErr are what JudgeRequestState answers, and stateAsked how many
	// times a run asked it.
	state      string
	stateErr   error
	stateAsked int
	// applyErr is what ApplyJudgeDecision answers, and logErr what LogJudged answers.
	applyErr, logErr error
	// writeTakes is how long each of the two writes takes before it commits, which is how
	// a test gives the run's own transaction something to need its remaining budget for.
	writeTakes time.Duration
	// applied and logged are the detail objects of each write, in order.
	applied, logged []map[string]any
}

func newRequests() *fakeRequests {
	return &fakeRequests{
		decision: policy.Decision{Outcome: policy.Approval, Approver: "sami@example.com"},
		state:    "pending",
	}
}

func (f *fakeRequests) JudgePolicy(_ context.Context, _, name string) (policy.Decision, bool, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.policyAsked = append(f.policyAsked, name)
	return f.decision, f.withheld, f.admin, f.policyErr
}

func (f *fakeRequests) JudgeRequestState(_ context.Context, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stateAsked++
	return f.state, f.stateErr
}

// transact is one write of this machine: the time it takes, and then whether the caller's
// context outlived it. A transaction needs its context alive to commit, so a run that reaches a
// write with none of its budget left writes nothing and says why, as Postgres would.
func (f *fakeRequests) transact(ctx context.Context) error {
	f.mu.Lock()
	takes := f.writeTakes
	f.mu.Unlock()
	if takes > 0 {
		select {
		case <-time.After(takes):
		case <-ctx.Done():
		}
	}
	return ctx.Err()
}

func (f *fakeRequests) ApplyJudgeDecision(ctx context.Context, recordID, modelID string, judged map[string]any) (requests.Decision, error) {
	if err := f.transact(ctx); err != nil {
		return requests.Decision{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied = append(f.applied, judged)
	if f.applyErr != nil {
		return requests.Decision{}, f.applyErr
	}
	return requests.Decision{RequestID: recordID, State: "granted", GrantID: "grant-" + modelID}, nil
}

func (f *fakeRequests) LogJudged(ctx context.Context, _, _ string, judged map[string]any) error {
	if err := f.transact(ctx); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logged = append(f.logged, judged)
	return f.logErr
}

// rows is what the judge wrote: the details it approved with and the details it logged alone.
func (f *fakeRequests) rows() (applied, logged []map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.applied...), append([]map[string]any(nil), f.logged...)
}

// tokenFile writes a projected token for the Dispatch stand-in to read back.
func tokenFile(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dispatch-token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatalf("write the token file: %v", err)
	}
	return path
}

// testOptions judges with the three stand-ins, under the rules that give the judge testSecret.
func testOptions(t *testing.T, d *dispatchStandIn, m *modelStandIn, r *fakeRequests) Options {
	t.Helper()
	return Options{
		Rules:    testRules(),
		Dispatch: Dispatch{BaseURL: d.server.URL, Account: brokertoken.NewAccountToken(tokenFile(t, "projected-token")), HTTP: d.server.Client()},
		Model: Model{
			ID: testModel,
			Endpoint: agentloop.Endpoint{
				Kind: agentloop.OpenAIChat, BaseURL: m.server.URL,
				Token: func(context.Context) (string, error) { return "gateway-token", nil },
			},
		},
		Requests: r,
	}
}

// waitFor blocks until done answers true, which is how a test reads the work Consider put on a
// goroutine of its own. within is how long it waits: a run that spends its bounds takes most of
// runBudget, and a run against stand-ins that answer at once takes milliseconds.
func waitFor(t *testing.T, what string, within time.Duration, done func() bool) {
	t.Helper()
	for deadline := time.Now().Add(within); time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
		if done() {
			return
		}
	}
	t.Fatalf("waited %s for %s", within, what)
}

// theOneRow is the single detail object a run wrote through LogJudged, and fails when the run
// wrote anything else.
func theOneRow(t *testing.T, f *fakeRequests) map[string]any {
	t.Helper()
	applied, logged := f.rows()
	if len(applied) != 0 || len(logged) != 1 {
		t.Fatalf("the run applied %d decisions and logged %d rows; want no decision and one row", len(applied), len(logged))
	}
	return logged[0]
}

// failedCheck is the first check a detail object reports as failed, and "" when every check it
// names passed.
func failedCheck(t *testing.T, detail map[string]any) string {
	t.Helper()
	checks, ok := detail["checks"].([]check)
	if !ok {
		t.Fatalf("the detail's checks are %T, want the judge's own checks", detail["checks"])
	}
	for _, c := range checks {
		if !c.Passed {
			return c.Check
		}
	}
	return ""
}

// failedDetail is what the first failed check of a detail object says it read, and "" when every
// check passed. It is the sentence GET /v1/grant-log then serves a person, so a test that must
// tell two failures of one check apart reads it rather than the check's name.
func failedDetail(t *testing.T, detail map[string]any) string {
	t.Helper()
	checks, ok := detail["checks"].([]check)
	if !ok {
		t.Fatalf("the detail's checks are %T, want the judge's own checks", detail["checks"])
	}
	for _, c := range checks {
		if !c.Passed {
			return c.Detail
		}
	}
	return ""
}

// testSpec is a spec document with both of the sections the extract reads.
const testSpec = `## Summary

The broker's model judge decides what the rules allow it.

## Problem

People wait.

## Requirements

**R1.** It never denies.

## Verification

Checks live elsewhere.
`
