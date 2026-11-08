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
)

// testSecret is the one secret these tests ask for, and testIssue the issue every signed reason
// names.
const (
	testSecret = "DEEL_API_KEY"
	testIssue  = "INFRA-1353"
	testModel  = "claude-standin"
)

// testRules give the judge 900 seconds of testSecret and nothing of any other name.
func testRules() airlock.Rules {
	return airlock.Rules{Secrets: []airlock.SecretRule{
		{Name: testSecret, Hosts: []string{"api.deel.example"}, Judge: new(uint32(900))},
		{Name: "NO_JUDGE_KEY", Hosts: []string{"other.example"}},
	}}
}

// pending is a request inside the class the judge decides: one secret whose entry has a judge,
// a signed lifetime of 15 minutes, and a reason naming an open issue.
func pending() PendingRequest {
	return PendingRequest{
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
	status, body := d.issueStatus, d.issue
	if strings.HasSuffix(r.URL.Path, "/artifacts/spec/text") {
		status, body = d.specStatus, d.spec
	}
	d.mu.Unlock()
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
	// decision, withheld and policyErr are what JudgePolicy answers, and policyAsked the
	// names it was asked about.
	decision    policy.Decision
	withheld    bool
	policyErr   error
	policyAsked []string
	// applyErr is what ApplyJudgeDecision answers, and logErr what LogJudged answers.
	applyErr, logErr error
	// applied and logged are the detail objects of each write, in order.
	applied, logged []map[string]any
}

func newRequests() *fakeRequests {
	return &fakeRequests{decision: policy.Decision{Outcome: policy.Approval, Approver: "sami@example.com"}}
}

func (f *fakeRequests) JudgePolicy(_ context.Context, _, name string) (policy.Decision, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.policyAsked = append(f.policyAsked, name)
	return f.decision, f.withheld, f.policyErr
}

func (f *fakeRequests) ApplyJudgeDecision(_ context.Context, recordID, modelID string, judged map[string]any) (requests.Decision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied = append(f.applied, judged)
	if f.applyErr != nil {
		return requests.Decision{}, f.applyErr
	}
	return requests.Decision{RequestID: recordID, State: "approved", GrantID: "grant-" + modelID}, nil
}

func (f *fakeRequests) LogJudged(_ context.Context, _, _ string, judged map[string]any) error {
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
		Dispatch: Dispatch{BaseURL: d.server.URL, TokenFile: tokenFile(t, "projected-token"), HTTP: d.server.Client()},
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
// goroutine of its own.
func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
		if done() {
			return
		}
	}
	t.Fatalf("waited ten seconds for %s", what)
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
