// packages/envoy/internal/cipher/judge/standin_test.go
// The stand-ins every test of this package runs against: a Dispatch that answers the judge's two
// GETs, a model endpoint that speaks Chat Completions, and a request machine that records what
// the judge wrote to it. Nothing here reaches the network or a database.
package judge

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules/proxyrulestest"
	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/ciphertoken"
)

// testSecret is the key most tests ask for, with the guidance testGuidance; testIssue the issue
// a reason names when it names one; and shortJudgeSecret the second judge key, whose 300 seconds
// are the bound a request asks past in the one case the class's own 15 minutes would let
// through. testApprover is the person the policy answers as the approver of testSecret, and
// testAsk the ask on testIssue they answered, which a reason cites when it cites one.
const (
	testSecret        = "DEEL_API_KEY"
	testGuidance      = "grant only to post text the key's owner approved in Dispatch; the reason must cite that answer"
	testIssue         = "INFRA-1353"
	testModel         = "claude-standin"
	shortJudgeSecret  = "SHORT_JUDGE_KEY"
	shortGuidance     = "grant for the broker's own release job"
	shortJudgeSeconds = 300
	testApprover      = "approver@example.com"
	testAsk           = "4f3c2a10-5b6d-4e7f-8a9b-0c1d2e3f4a5b"
	testQuestion      = "Spend the Deel key on the broker's own tests?"
	testAnswerText    = "Yes, on this issue's own runs."
)

// testPolicy is a live policy whose tags give the judge 900 seconds of testSecret and 300 of
// shortJudgeSecret, each with its guidance, and nothing of NO_JUDGE_KEY or any other name.
func testPolicy(t *testing.T) *policy.Current {
	t.Helper()
	return judgedPolicy(t, testGuidance)
}

// judgedPolicy is testPolicy with guidance as testSecret's guidance tag.
func judgedPolicy(t *testing.T, guidance string) *policy.Current {
	t.Helper()
	return guidedPolicy(t, guidance, shortGuidance)
}

// guidedPolicy is testPolicy with first as testSecret's guidance tag and second as
// shortJudgeSecret's. Both keys are testApprover's own.
func guidedPolicy(t *testing.T, first, second string) *policy.Current {
	t.Helper()
	return policytest.Current(t, secrets.NewLocal(
		policytest.Judged(policytest.Secret(testSecret, testApprover, policy.TierHuman, "v"), 900, first),
		policytest.Judged(policytest.Secret(shortJudgeSecret, testApprover, policy.TierHuman, "v"), shortJudgeSeconds, second),
		policytest.Secret("NO_JUDGE_KEY", testApprover, policy.TierHuman, "v"),
	))
}

// agentKey is a key the requesting session gets at once, which a request names beside a judge
// key: it needs no decision, the judge decides nothing of it, and it carries no judge tag.
const agentKey = "AGENT_KEY"

// pending is a request the judge holds: one judge key, which is the key it decides, a signed
// lifetime of 15 minutes, and a reason naming an issue. citedPending is the same request whose
// reason also cites the ask its approver answered on that issue.
func pending() requests.PendingRequest {
	return requests.PendingRequest{
		RecordID: "record-1", RequestID: "request-1", EnrollmentID: "enrollment-1",
		SessionID: "legion-1353", Secrets: []string{testSecret}, Decided: []string{testSecret},
		LifetimeSeconds: new(900),
		Reason:          "Running the broker's own tests for " + testIssue,
		Approver:        testApprover,
	}
}

func citedPending() requests.PendingRequest {
	p := pending()
	p.Reason = "Posting the approved notes for " + testIssue + ", approved at dispatch://" + testIssue + "/ask/" + testAsk
	return p
}

// answeredAsk is the body GET /api/v1/asks/{id} answers for an ask testApprover answered on
// testIssue: the ask beside its thread, of which the judge reads the ask alone.
func answeredAsk() map[string]any {
	return map[string]any{
		"ask": map[string]any{
			"id": testAsk, "issue_key": testIssue, "state": "answered",
			"question": testQuestion,
			"options": []any{
				map[string]any{"label": "Spend it", "description": "on this issue's runs"},
				map[string]any{"label": "Leave it"},
			},
			"answer": map[string]any{
				"user": testApprover, "selected": []any{"Spend it"}, "text": testAnswerText,
				"at": "2026-11-16T00:00:00Z",
			},
		},
		"replies": []any{}, "edits": []any{}, "followers": []any{},
	}
}

// dispatchCall is one request the Dispatch stand-in got.
type dispatchCall struct{ method, path, authorization string }

// dispatchStandIn answers the issue read, the spec read and the ask read, and records every
// request it got so a test can see which routes the judge called and what it presented.
type dispatchStandIn struct {
	server *httptest.Server

	mu    sync.Mutex
	calls []dispatchCall
	// issueStatus, specStatus and askStatus are what each route answers, and issue, spec and
	// ask the JSON bodies it answers with when that status is 200.
	issueStatus, specStatus, askStatus int
	issue, spec, ask                   map[string]any
	// askRaw, when it is not empty, is written as the ask read's whole 200 body in place of
	// ask, which is how a test drives an answer that is no JSON object.
	askRaw string
	// delay holds each answer back, which is how a test spends the Dispatch phase's
	// bound. A read the judge gave up on ends the handler at once, so a stand-in told to
	// wait longer than the bound never keeps the server from closing.
	delay time.Duration
	// answerAt, when set, is when the issue and spec reads answer, in place of delay: it is
	// given how many of them came before (0 for the first) and answers a time on the wall
	// clock. The ask read keeps delay. A test that places each read at a fixed point of the
	// phase keeps a read that woke late on a loaded runner from making every read after it
	// late as well, which a delay per read would.
	answerAt func(earlier int) time.Time
	// phaseReads is how many issue and spec reads came so far, which answerAt is given.
	phaseReads int
}

// newDispatch is a Dispatch holding one open issue with a spec document, and the ask on it that
// testApprover answered.
func newDispatch(t *testing.T, status, markdown string) *dispatchStandIn {
	t.Helper()
	d := &dispatchStandIn{
		issueStatus: http.StatusOK, specStatus: http.StatusOK, askStatus: http.StatusOK,
		issue: map[string]any{"key": testIssue, "title": "The broker's model judge", "status": status,
			"project": "INFRA", "number": 1353},
		spec: map[string]any{"markdown": markdown, "version": 12, "token": "sha256:standin"},
		ask:  answeredAsk(),
	}
	d.server = httptest.NewServer(http.HandlerFunc(d.serve))
	t.Cleanup(d.server.Close)
	return d
}

// askField sets one field of the ask this stand-in answers with, and askAnswerField one field of
// that ask's answer, so a test says only how its own case differs from an answered ask.
func (d *dispatchStandIn) askField(name string, value any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ask["ask"].(map[string]any)[name] = value
}

func (d *dispatchStandIn) askAnswerField(name string, value any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ask["ask"].(map[string]any)["answer"].(map[string]any)[name] = value
}

func (d *dispatchStandIn) serve(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	d.calls = append(d.calls, dispatchCall{r.Method, r.URL.Path, r.Header.Get("Authorization")})
	status, body, delay, raw := d.issueStatus, d.issue, d.delay, ""
	ask := false
	switch {
	case strings.HasSuffix(r.URL.Path, "/artifacts/spec/text"):
		status, body = d.specStatus, d.spec
	case strings.HasPrefix(r.URL.Path, "/api/v1/asks/"):
		status, body, raw = d.askStatus, d.ask, d.askRaw
		ask = true
	}
	answerAt := d.answerAt
	earlier := d.phaseReads
	if !ask {
		d.phaseReads++
	}
	d.mu.Unlock()
	if answerAt != nil && !ask {
		delay = time.Until(answerAt(earlier))
	}
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
	if raw != "" {
		_, _ = io.WriteString(w, raw)
		return
	}
	_ = json.NewEncoder(w).Encode(body)
}

func (d *dispatchStandIn) got() []dispatchCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]dispatchCall(nil), d.calls...)
}

// askReads is how many ask reads this stand-in got, which is what a test of a shared secret —
// whose approver is nobody — reads to see that the judge asked Dispatch about no ask at all.
func (d *dispatchStandIn) askReads() int {
	reads := 0
	for _, call := range d.got() {
		if strings.HasPrefix(call.path, "/api/v1/asks/") {
			reads++
		}
	}
	return reads
}

// modelStandIn is a model endpoint speaking Chat Completions: it answers each call with the same
// reply and records every request body, so a test can read the data object the judge sent, and
// every Authorization, so a test can read which bearer the judge presented.
type modelStandIn struct {
	server *httptest.Server

	mu sync.Mutex
	// bodies are the request bodies it got, in order, and authorizations each one's
	// Authorization header.
	bodies         [][]byte
	authorizations []string
	// status, when it is not 0 or 200, is answered in place of a reply, and reply is the
	// assistant content the stream carries. delay holds the answer back, which is how a
	// test spends a bound.
	status int
	reply  string
	delay  time.Duration
	tokens chatTokens
	// replies, when it names the one key a call decides, is that call's reply in place of reply,
	// so a test can have one key's call grant and another's deny; delays, the same for delay, so
	// one key's call can run past the model's bound while another's answers.
	replies map[string]string
	delays  map[string]time.Duration
	// obey, when set, answers each call as a model that obeys every guidance in its prompt
	// would (proxyrulestest.ObeyedVerdict), in place of reply.
	obey bool
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
	m.authorizations = append(m.authorizations, r.Header.Get("Authorization"))
	status, reply, delay, tokens := m.status, m.reply, m.delay, m.tokens
	if data, err := callData(body); err == nil {
		if len(data.Keys) == 1 {
			if own, ok := m.replies[data.Keys[0].Name]; ok {
				reply = own
			}
			if own, ok := m.delays[data.Keys[0].Name]; ok {
				delay = own
			}
		}
		if m.obey {
			keys := make([]proxyrulestest.GuidedKey, 0, len(data.Keys))
			for _, k := range data.Keys {
				keys = append(keys, proxyrulestest.GuidedKey{Name: k.Name, Guidance: k.Guidance})
			}
			reply = verdictReply(proxyrulestest.ObeyedVerdict(keys, data.Reason))
		}
	}
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

// presented is the Authorization of every call the model got, in order.
func (m *modelStandIn) presented() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.authorizations...)
}

// data is the Data object the judge sent with its first call, read back out of the user message.
func (m *modelStandIn) data(t *testing.T) Data {
	t.Helper()
	every := m.everyData(t)
	if len(every) == 0 {
		t.Fatal("the model was never asked, so it got no data")
	}
	return every[0]
}

// everyData is the Data object of every call the model got, in the order the calls arrived.
func (m *modelStandIn) everyData(t *testing.T) []Data {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	var every []Data
	for _, body := range m.bodies {
		data, err := callData(body)
		if err != nil {
			t.Fatalf("%v\n%s", err, body)
		}
		every = append(every, data)
	}
	return every
}

// callBodies is the raw body of every call the model got, in order.
func (m *modelStandIn) callBodies() [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([][]byte(nil), m.bodies...)
}

// callData reads the Data object back out of one Chat Completions request body's user message.
func callData(body []byte) (Data, error) {
	var call struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &call); err != nil {
		return Data{}, fmt.Errorf("the model call is no JSON object: %w", err)
	}
	for _, message := range call.Messages {
		if message.Role != "user" {
			continue
		}
		text, err := messageText(message.Content)
		if err != nil {
			return Data{}, err
		}
		var data Data
		if err := json.Unmarshal([]byte(text), &data); err != nil {
			return Data{}, fmt.Errorf("the user message is no Data object: %w", err)
		}
		return data, nil
	}
	return Data{}, errors.New("the model call carries no user message")
}

// contentText is a chat message's text, whether the provider shape carried it as one string or
// as a list of parts.
func contentText(t *testing.T, content json.RawMessage) string {
	t.Helper()
	text, err := messageText(content)
	if err != nil {
		t.Fatal(err)
	}
	return text
}

// messageText is contentText for a caller with no t: the text, or why there is none.
func messageText(content json.RawMessage) (string, error) {
	var text string
	if json.Unmarshal(content, &text) == nil {
		return text, nil
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &parts); err != nil || len(parts) == 0 {
		return "", fmt.Errorf("a message's content is neither a string nor parts: %s", content)
	}
	return parts[0].Text, nil
}

// fakeRequests is the request machine as the judge sees it: it answers the policy question a
// test sets and keeps every detail object the judge wrote.
type fakeRequests struct {
	mu sync.Mutex
	// decision, withheld, admin and policyErr are what JudgePolicy answers, and
	// policyAsked the names it was asked about. withheldName, when set, is the one name
	// withheld answers true for, which is how a test withholds one key of several;
	// automaticName is the one name the policy answers automatic for, and deniedName the one
	// it answers deny for.
	decision                                policy.Decision
	withheld                                bool
	withheldName, automaticName, deniedName string
	admin                                   bool
	policyErr                               error
	policyAsked                             []string
	// private and projectErr are what JudgeProject answers, projectAsked the names it was
	// asked about and projectReasons the signed reasons, and projectTakes how long it takes to
	// answer: like the real read of Dispatch, it gives up when its context ends first.
	// projectBeforeEnd, when not zero, has it answer that long before the deadline holdPhase
	// recorded instead, and only under holdPhase: without it there is no such deadline, and the
	// call fails, naming the knob.
	private          bool
	projectErr       error
	projectAsked     []string
	projectReasons   []string
	projectTakes     time.Duration
	projectBeforeEnd time.Duration
	// phase is what holdPhase and notePhase recorded of the run's Dispatch phase (phaseTimes).
	phase dispatchTimes
	// state and stateErr are what JudgeRequestState answers, and stateAsked how many
	// times a run asked it. LogJudged reads state too, for the verdict of the row it writes.
	state      string
	stateErr   error
	stateAsked int
	// applyErr is what ApplyJudgeDecision answers, logErr what LogJudged answers and denyErr
	// what DenyJudged answers.
	applyErr, logErr, denyErr error
	// writeTakes is how long each write takes before it commits, which is how a test gives
	// the run's own transaction something to need its remaining budget for, and applyTakes
	// how long ApplyJudgeDecision's reads before its transaction take: like the real reads of
	// Dispatch, they give up when the context ends first.
	writeTakes, applyTakes time.Duration
	// commitTakes is how long ApplyJudgeDecision's transaction takes before it commits, ended
	// with the context's own error when the context ends first, as a stopping broker's does;
	// wrote is what LogJudged answers it did, JudgedWritten when empty.
	commitTakes time.Duration
	wrote       requests.JudgedWrite
	// onApply, when set, runs as ApplyJudgeDecision ends, before it answers, so a test can
	// stop the broker while an approval is in flight.
	onApply func()
	// bounded is, for each call to a method that reads Dispatch in the real machine
	// (JudgePolicy and JudgeProject), the method's name and whether its context was one
	// requests.JudgeDispatch made.
	bounded []boundedCall
	// applied, logged and denied are the rows of each write, in order (written), and refusals
	// the reason each denial gave its agent; given is every verdict the run itself gave a
	// write, which the machine never takes.
	applied, logged, denied []map[string]any
	refusals                []string
	given                   []string
}

// written is the row a write of this machine keeps: what the run gave it, with the verdict the
// row's own transaction sets, as the machine's do (decider.judgedRow, LogJudged), and none where
// that transaction wrote nothing. A verdict the run gave is recorded in given and not kept.
func (f *fakeRequests) written(judged map[string]any, verdict string) map[string]any {
	row := maps.Clone(judged)
	if row == nil {
		row = map[string]any{}
	}
	if given, ok := row["verdict"]; ok {
		f.given = append(f.given, fmt.Sprint(given))
		delete(row, "verdict")
	}
	if verdict != "" {
		row["verdict"] = verdict
	}
	return row
}

// givenVerdicts is every verdict the run gave a write of this machine.
func (f *fakeRequests) givenVerdicts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.given...)
}

func newRequests() *fakeRequests {
	return &fakeRequests{
		decision: policy.Decision{Outcome: policy.Approval, Approver: testApprover},
		state:    "pending",
	}
}

// JudgePolicy answers as the machine's own does (requests.Machine.JudgePolicy): the policy folds a
// withheld name into its outcome, so a withheld name the policy would give at once comes back as
// its approval, with the withheld flag beside it. It records whether its context was the run's
// Dispatch phase, since the real one reads Dispatch for a tagged name.
func (f *fakeRequests) JudgePolicy(ctx context.Context, _, name string) (policy.Decision, bool, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.policyAsked = append(f.policyAsked, name)
	f.bounded = append(f.bounded, boundedCall{"JudgePolicy", requests.UnderJudgeDispatch(ctx)})
	f.notePhase(ctx)
	withheld := f.withheld || (f.withheldName != "" && f.withheldName == name)
	decision := f.decision
	switch name {
	case f.automaticName:
		decision = policy.Decision{Outcome: policy.Automatic}
	case f.deniedName:
		decision = policy.Decision{Outcome: policy.Deny}
	}
	if withheld && decision.Outcome == policy.Automatic {
		decision = policy.Decision{Outcome: policy.Approval, Approver: testApprover}
	}
	return decision, withheld, f.admin, f.policyErr
}

func (f *fakeRequests) JudgeProject(ctx context.Context, _, name, reason string) (bool, error) {
	f.mu.Lock()
	f.projectAsked = append(f.projectAsked, name)
	f.projectReasons = append(f.projectReasons, reason)
	f.bounded = append(f.bounded, boundedCall{"JudgeProject", requests.UnderJudgeDispatch(ctx)})
	f.notePhase(ctx)
	takes := f.projectTakes
	if f.projectBeforeEnd > 0 {
		if f.phase.end.IsZero() {
			f.mu.Unlock()
			return false, errors.New("stand-in: projectBeforeEnd is set, but no holdPhase made the run's Dispatch phase, so there is no deadline to answer before")
		}
		takes = time.Until(f.phase.end.Add(-f.projectBeforeEnd))
	}
	f.mu.Unlock()
	if takes > 0 {
		select {
		case <-time.After(takes):
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.private, f.projectErr
}

// notePhase records the deadline on the context of the first call made under the run's Dispatch
// phase. f.mu is held.
func (f *fakeRequests) notePhase(ctx context.Context) {
	if !f.phase.seen.IsZero() || !requests.UnderJudgeDispatch(ctx) {
		return
	}
	f.phase.seen, _ = ctx.Deadline()
}

// holdPhase replaces the Runner's dispatchPhase: it makes the run's Dispatch phase as
// requests.JudgeDispatch does, records the deadline JudgeDispatch gave it with the wall clock just
// before and after, and answers a phase that carries that deadline and JudgeDispatch's marker but
// that only the judge's own cancel or the run's end ends, never the wall clock passing the
// deadline. A read can then answer at the deadline itself, and however late the judge takes its
// answer on a loaded runner, the read is not cut short: a test holds the judge to the phase's
// whole bound without racing it. A judge that cut the phase short itself still cuts its reads
// short, since a context it derives keeps its own, earlier deadline. Production never takes this:
// New sets requests.JudgeDispatch, and only this package's tests reach the field.
func (f *fakeRequests) holdPhase(ctx context.Context) (context.Context, context.CancelFunc) {
	before := time.Now()
	made, stop := requests.JudgeDispatch(ctx)
	after := time.Now()
	deadline, _ := made.Deadline()
	held, cancel := context.WithCancel(context.WithoutCancel(made))
	stop()
	unhook := context.AfterFunc(ctx, cancel)
	f.mu.Lock()
	f.phase.before, f.phase.after, f.phase.end = before, after, deadline
	f.mu.Unlock()
	return heldPhase{Context: held, deadline: deadline}, func() { unhook(); cancel() }
}

// heldPhase is the phase holdPhase answers: its Context ends only by its cancel, and Deadline
// answers the deadline requests.JudgeDispatch gave the phase.
type heldPhase struct {
	context.Context
	deadline time.Time
}

func (p heldPhase) Deadline() (time.Time, bool) { return p.deadline, true }

// dispatchTimes are the times recorded for a run's Dispatch phase: the wall clock just before and
// just after holdPhase made it, the deadline requests.JudgeDispatch gave it (end), and the
// deadline on the context of the phase's first read, its first JudgePolicy call (seen). Each is
// zero until it is recorded.
type dispatchTimes struct {
	before, after, end, seen time.Time
}

// phaseTimes is what was recorded of the run's Dispatch phase so far.
func (f *fakeRequests) phaseTimes() dispatchTimes {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.phase
}

// projectsAsked is every name JudgeProject was asked about, in order, and reasonsAsked every
// reason it was given.
func (f *fakeRequests) projectsAsked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.projectAsked...)
}

func (f *fakeRequests) reasonsAsked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.projectReasons...)
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
	f.mu.Lock()
	reads, commit := f.applyTakes, f.commitTakes
	f.mu.Unlock()
	if reads > 0 {
		select {
		case <-time.After(reads):
		case <-ctx.Done():
			return requests.Decision{}, fmt.Errorf("%w: %v", requests.ErrProjectAccessUnavailable, ctx.Err())
		}
	}
	if commit > 0 {
		// The transaction itself, which a cancelled context ends with the context's own error,
		// as pgx's does.
		select {
		case <-time.After(commit):
		case <-ctx.Done():
			return requests.Decision{}, ctx.Err()
		}
	}
	if err := f.transact(ctx); err != nil {
		return requests.Decision{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	verdict := requests.JudgedGranted
	if f.applyErr != nil {
		verdict = "" // the transaction rolled back and wrote nothing
	}
	f.applied = append(f.applied, f.written(judged, verdict))
	if f.onApply != nil {
		f.onApply()
	}
	if f.applyErr != nil {
		return requests.Decision{}, f.applyErr
	}
	return requests.Decision{RequestID: recordID, State: "granted", GrantID: "grant-" + modelID}, nil
}

// LogJudged writes the row, or answers what wrote answers in its place: JudgedAlreadyApplied
// writes nothing, as the machine does when the approval's own row is there. The row's verdict is
// what the machine's is: applied unknown where wrote says so, else handed to the person for a
// request still pending (state), else not pending.
func (f *fakeRequests) LogJudged(ctx context.Context, _, _ string, judged map[string]any) (requests.JudgedLog, error) {
	if err := f.transact(ctx); err != nil {
		return requests.JudgedLog{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	wrote := cmp.Or(f.wrote, requests.JudgedWritten)
	if wrote == requests.JudgedAlreadyApplied {
		return requests.JudgedLog{Write: wrote}, f.logErr
	}
	verdict := requests.JudgedNotPending
	switch {
	case wrote == requests.JudgedAppliedUnknown:
		verdict = requests.AppliedUnknown
	case f.state == "pending":
		verdict = requests.JudgedHandedOver
	}
	f.logged = append(f.logged, f.written(judged, verdict))
	return requests.JudgedLog{Write: wrote, Verdict: verdict}, f.logErr
}

func (f *fakeRequests) DenyJudged(ctx context.Context, recordID, modelID string, judged map[string]any, why string) (requests.Decision, error) {
	if err := f.transact(ctx); err != nil {
		return requests.Decision{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	verdict := requests.JudgedRefused
	if f.denyErr != nil {
		verdict = "" // the transaction rolled back and wrote nothing
	}
	f.denied = append(f.denied, f.written(judged, verdict))
	f.refusals = append(f.refusals, why)
	if f.denyErr != nil {
		return requests.Decision{}, f.denyErr
	}
	return requests.Decision{RequestID: recordID, State: "denied"}, nil
}

// refused is what the judge refused: the verdict each denial carried and the reason it gave.
func (f *fakeRequests) refused() (denied []map[string]any, refusals []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.denied...), append([]string(nil), f.refusals...)
}

// rows is what the judge wrote: the details it approved with and the details it logged alone.
func (f *fakeRequests) rows() (applied, logged []map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.applied...), append([]map[string]any(nil), f.logged...)
}

// boundedCall is one call of a method that reads Dispatch in the real machine, and whether it was
// made under requests.JudgeDispatch.
type boundedCall struct {
	method  string
	bounded bool
}

// boundedCalls is every such call, in order.
func (f *fakeRequests) boundedCalls() []boundedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]boundedCall(nil), f.bounded...)
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

// testOptions judges with the three stand-ins, under the policy whose tags give the judge
// testSecret.
func testOptions(t *testing.T, d *dispatchStandIn, m *modelStandIn, r *fakeRequests) Options {
	t.Helper()
	return Options{
		Policy:   testPolicy(t),
		Dispatch: Dispatch{BaseURL: d.server.URL, Account: ciphertoken.NewAccountToken(tokenFile(t, "projected-token")), HTTP: d.server.Client()},
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

// theOneRow is the single detail object a run wrote through LogJudged — a run that decided
// nothing, which hands the request to its person — and fails when the run wrote anything else, a
// refusal included.
func theOneRow(t *testing.T, f *fakeRequests) map[string]any {
	t.Helper()
	applied, logged := f.rows()
	denied, _ := f.refused()
	if len(applied) != 0 || len(logged) != 1 || len(denied) != 0 {
		t.Fatalf("the run applied %d decisions, logged %d rows and refused %d requests; want one row and nothing else", len(applied), len(logged), len(denied))
	}
	return logged[0]
}

// theOneRefusal is the single refusal a run wrote through DenyJudged — the judge's final no — as
// the verdict it carried and the reason it gave the agent, and fails when the run wrote anything
// else.
func theOneRefusal(t *testing.T, f *fakeRequests) (map[string]any, string) {
	t.Helper()
	applied, logged := f.rows()
	denied, refusals := f.refused()
	if len(applied) != 0 || len(logged) != 0 || len(denied) != 1 {
		t.Fatalf("the run applied %d decisions, logged %d rows and refused %d requests; want one refusal and nothing else", len(applied), len(logged), len(denied))
	}
	return denied[0], refusals[0]
}

// failedCheck is the check a detail object reports as the one the run ended at (decidingFailure):
// the first that failed for good, else the first that failed, and "" when every check it names
// passed.
func failedCheck(t *testing.T, detail map[string]any) string {
	t.Helper()
	checks, ok := detail["checks"].([]check)
	if !ok {
		t.Fatalf("the detail's checks are %T, want the judge's own checks", detail["checks"])
	}
	if c := decidingFailure(checks); c != nil {
		return c.Check
	}
	return ""
}

// failedDetail is what that same check of a detail object says it read, and "" when every check
// passed. It is the sentence GET /v1/grant-log then serves a person, so a test that must tell two
// failures of one check apart reads it rather than the check's name.
func failedDetail(t *testing.T, detail map[string]any) string {
	t.Helper()
	checks, ok := detail["checks"].([]check)
	if !ok {
		t.Fatalf("the detail's checks are %T, want the judge's own checks", detail["checks"])
	}
	if c := decidingFailure(checks); c != nil {
		return c.Detail
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
