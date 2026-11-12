package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/going/goingtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The status tests (c15): a status question is answered from what Dispatch stores, through the
// read tools, with no agent messaged, and the run says how fresh the roster it read was in a
// roster frame - live from Dispatch's copy of the listener's list, stored where the listener is
// unavailable, and live again once a refilling listener's Retry-After has been waited out.

// statusListener is a fake Envoy listener that records every request it is sent and answers GET
// /v1/sessions with what answer returns for that call, numbered from one.
type statusListener struct {
	url    string
	mu     sync.Mutex
	calls  []string
	at     []time.Time
	answer func(call int, w http.ResponseWriter)
}

func newStatusListener(t *testing.T, answer func(call int, w http.ResponseWriter)) *statusListener {
	t.Helper()
	listener := &statusListener{answer: answer}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		listener.mu.Lock()
		listener.calls = append(listener.calls, r.Method+" "+r.URL.Path)
		listener.at = append(listener.at, time.Now())
		call := 0
		for _, each := range listener.calls {
			if each == "GET /v1/sessions" {
				call++
			}
		}
		listener.mu.Unlock()
		if r.Method != http.MethodGet || r.URL.Path != "/v1/sessions" {
			http.NotFound(w, r)
			return
		}
		listener.answer(call, w)
	}))
	t.Cleanup(server.Close)
	listener.url = server.URL
	return listener
}

// requests is every request the listener was sent, and when.
func (l *statusListener) requests() ([]string, []time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.calls), slices.Clone(l.at)
}

// statusSessions is the listener's list of the fixture's two sessions.
const statusSessions = `[
	{"session_id":"ses-a","title":"Planner","last_seen":200,"state":"live"},
	{"session_id":"ses-b","title":"Worker","last_seen":100,"state":"live"}
]`

// seedStatusFixture seeds project ACME (its components and issues, seedDeliveryIssueFacts), an
// open ask ses-a asked on ACME-1, a claim ses-claim holds on ACME-3, and events of both sessions.
func seedStatusFixture(t *testing.T, rig blockRig) {
	t.Helper()
	seedDeliveryIssueFacts(t, rig.database)
	rig.seed(t,
		`update issues set claimed_by = '{"kind":"session","id":"ses-claim"}', claimed_at = now() where key = 'ACME-3'`,
		`insert into asks (issue_key, author, question, state) values ('ACME-1', '{"kind":"session","id":"ses-a"}', 'Which component?', 'open')`,
		`insert into events (issue_key, seq, type, actor, payload, notify, created_at) values
			('ACME-1', 1, 'issue.created', '{"kind":"user","id":"alice"}', '{}', false, now() - interval '7 days'),
			('ACME-1', 2, 'comment.created', '{"kind":"session","id":"ses-a"}', '{}', false, now() - interval '1 hour'),
			('ACME-3', 1, 'issue.claimed', '{"kind":"session","id":"ses-claim"}', '{}', false, now() - interval '2 hours')`,
	)
}

// toolResults is each read tool's answer as the model was handed it, by tool name, read off the
// call that followed the tool calls.
func toolResults(t *testing.T, model *goingtest.Model) map[string]going.Read {
	t.Helper()
	requests := model.Requests()
	if len(requests) < 2 {
		t.Fatalf("the model served %d calls, want the tool calls answered in a second", len(requests))
	}
	names := map[string]string{}
	results := map[string]going.Read{}
	for _, message := range requests[1].Messages {
		for _, call := range message.ToolCalls {
			names[call.ID] = call.Name
		}
		if message.Role != "tool" {
			continue
		}
		var read going.Read
		if err := json.Unmarshal([]byte(message.Content), &read); err != nil {
			t.Fatalf("the %s result %q is no read: %v", names[message.ToolCallID], message.Content, err)
		}
		results[names[message.ToolCallID]] = read
	}
	return results
}

// rosterFrameOf is the run's roster frame, failing the test where it stored none.
func rosterFrameOf(t *testing.T, frames []goingEvent) goingRosterFrame {
	t.Helper()
	var roster goingRosterFrame
	if !frameData(t, frames, contracts.GoingEventRoster, &roster) {
		t.Fatalf("the run stored %v, want a roster frame: it read the roster", frameNames(frames))
	}
	return roster
}

// "Summarise this project" is answered from stored data: the roster, each session's open asks and
// last activity, the issues' progress, the per-component counts and the project's events, each as
// the route that serves it answers. No agent is messaged - the listener is sent nothing but the
// session list, which here is not even asked for, and no message is recorded - the answer is
// prose with no block, and the roster frame says how old Dispatch's copy of the listener's list
// was: the stored copy was read three seconds before the run.
func TestGoingStatusSummaryIsAnsweredFromStoredDataWithNoAgentMessaged(t *testing.T) {
	listener := newStatusListener(t, func(_ int, w http.ResponseWriter) { _, _ = w.Write([]byte(statusSessions)) })
	rig := newBlockRig(t, testServerOptions{envoyURL: listener.url},
		goingtest.Reply{ToolCalls: []goingtest.ToolCall{
			{ID: "call_roster", Name: going.ReadRoster, Arguments: `{}`},
			{ID: "call_progress", Name: going.ReadProgress, Arguments: `{}`},
			{ID: "call_architecture", Name: going.ReadArchitecture, Arguments: `{}`},
			{ID: "call_activity", Name: going.ReadProjectActivity, Arguments: `{"weeks":4}`},
		}},
		goingtest.Reply{Final: &goingtest.Final{Answer: "ACME has three issues; [Ship widgets](/issues/ACME-1) is open.", Arguments: "{}"}},
	)
	seedStatusFixture(t, rig)
	// Dispatch's copy of the listener's list, as a fetch three seconds ago stored it, fresh for
	// a minute: the run reads this copy and asks the listener nothing.
	rig.seed(t, `insert into route_roster (answered, sessions, sent_at, expires_at)
		values (true, '`+statusSessions+`', now() - interval '3 seconds', now() + interval '1 minute')`)

	frames := storedGoingFrames(t, rig.goingRoutesRig, askRun(t, rig.goingRoutesRig, "alice", map[string]any{
		"words": "summarise this project", "project": "ACME", "context": map[string]any{"path": "/projects/ACME", "project": "ACME"},
	}))
	names := frameNames(frames)
	if names[len(names)-1] != contracts.GoingEventEnd || slices.Contains(names, contracts.GoingEventBlock) {
		t.Fatalf("the run stored %v, want prose and links ending with an answer and no block", names)
	}
	roster := rosterFrameOf(t, frames)
	if roster.Source != goingRosterLive || roster.AgeMS < 3000 || roster.AgeMS > 3000+20000 {
		t.Errorf("roster frame = %+v, want the live copy about three seconds old", roster)
	}
	if !slices.Contains(names, contracts.GoingEventItem) {
		t.Errorf("the run stored %v, want the issue the progress read named linked", names)
	}

	reads := toolResults(t, rig.model)
	agents := decodeBody[[]agentResponse](t, dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/agents", nil, "alice"))
	sessions := reads[going.ReadRoster].Tables["sessions"]
	if len(sessions) != len(agents) || len(agents) != 2 {
		t.Fatalf("the roster read %v, want the %d sessions GET /api/v1/agents lists", sessions, len(agents))
	}
	for index, agent := range agents {
		row := sessions[index]
		activity, _ := row["last_activity"].(string)
		wantActivity := ""
		if agent.LastActivity != nil {
			wantActivity = *agent.LastActivity
		}
		if row["session_id"] != agent.SessionID || number(t, row["open_asks"]) != agent.OpenAsks || activity != wantActivity {
			t.Errorf("roster row %d = %v, want %s with %d open asks, last active %q, as GET /api/v1/agents answers", index, row, agent.SessionID, agent.OpenAsks, wantActivity)
		}
	}

	listed := decodeBody[[]model.IssueSummary](t, dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/issues?project=ACME", nil, "alice"))
	byStatus := map[string]int{}
	for _, issue := range listed {
		byStatus[issue.Status]++
	}
	statuses := reads[going.ReadProgress].Tables["statuses"]
	if len(statuses) != len(byStatus) {
		t.Errorf("the progress read counts %v, want the statuses GET /api/v1/issues lists: %v", statuses, byStatus)
	}
	for _, row := range statuses {
		if status, _ := row["status"].(string); number(t, row["count"]) != byStatus[status] {
			t.Errorf("the progress read counts %v, want %d %s issues as GET /api/v1/issues lists", row, byStatus[status], status)
		}
	}

	tree := decodeBody[model.ArchitectureTree](t, dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/projects/ACME/architecture", nil, "alice"))
	components := reads[going.ReadArchitecture].Tables["components"]
	if len(components) != len(tree.Components) {
		t.Fatalf("the architecture read %v, want the %d components the route answers", components, len(tree.Components))
	}
	for index, component := range tree.Components {
		row := components[index]
		if row["id"] != component.ID || number(t, row["done"]) != component.Done || number(t, row["total"]) != component.Total ||
			number(t, row["own_total"]) != component.OwnTotal {
			t.Errorf("component row %d = %v, want %+v's counts as the architecture route answers", index, row, component)
		}
	}

	activity := decodeBody[projectActivity](t, dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/projects/ACME/activity?weeks=4", nil, "alice"))
	weeks := reads[going.ReadProjectActivity].Tables["weeks"]
	if len(weeks) != len(activity.Weeks) {
		t.Fatalf("the activity read %v, want the %d weeks the route answers", weeks, len(activity.Weeks))
	}
	for index, week := range activity.Weeks {
		if weeks[index]["week"] != week.Week || number(t, weeks[index]["events"]) != week.Events {
			t.Errorf("activity week %d = %v, want %s: %d as the activity route answers", index, weeks[index], week.Week, week.Events)
		}
	}

	calls, _ := listener.requests()
	for _, call := range calls {
		if call != "GET /v1/sessions" {
			t.Errorf("the listener was sent %q: a status answer sends no agent anything", call)
		}
	}
	var messages int
	if err := rig.database.Pool.QueryRow(context.Background(), `select count(*) from messages`).Scan(&messages); err != nil || messages != 0 {
		t.Errorf("Dispatch holds %d messages (%v) after the status answer, want none", messages, err)
	}
}

// When GET /api/v1/agents answers 503 ENVOY_UNAVAILABLE the answer is served from the stored rows
// alone - the sessions Dispatch's own rows name, each with its open asks and last activity - and
// the roster frame says stored. An agent-status block asked for draws those stored rows.
func TestGoingStatusUnavailableRosterIsServedFromStoredRows(t *testing.T) {
	listener := newStatusListener(t, func(_ int, w http.ResponseWriter) {
		http.Error(w, "listener restarting", http.StatusServiceUnavailable)
	})
	rig := newBlockRig(t, testServerOptions{envoyURL: listener.url},
		readCall(going.ReadRoster, `{}`),
		blockAnswer(going.BlockAgentStatus, `{"source":"read_roster","params":{}}`),
	)
	seedStatusFixture(t, rig)
	if response := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/agents", nil, "alice"); response.Code != http.StatusServiceUnavailable ||
		!strings.Contains(response.Body.String(), `"code":"ENVOY_UNAVAILABLE"`) {
		t.Fatalf("GET /api/v1/agents: status=%d body=%s, want 503 ENVOY_UNAVAILABLE", response.Code, response.Body.String())
	}

	frames, block := rig.askForBlock(t, "agent status of ACME", "ACME")
	if roster := rosterFrameOf(t, frames); roster.Source != goingRosterStored {
		t.Errorf("roster frame = %+v, want stored: the live roster was unavailable", roster)
	}
	if block == nil || block.Kind != going.BlockAgentStatus {
		t.Fatalf("block = %+v, want the agent status of the stored rows", block)
	}
	got := map[string]int{}
	for _, row := range block.Rows {
		session, _ := row["session_id"].(string)
		got[session] = number(t, row["open_asks"])
		if row["state"] != "unknown" || row["last_activity"] == nil {
			t.Errorf("stored row %v, want an unknown state and the session's last activity", row)
		}
	}
	if want := map[string]int{"ses-a": 1, "ses-claim": 0}; len(got) != len(want) || got["ses-a"] != 1 || got["ses-claim"] != 0 {
		t.Errorf("stored rows = %v, want %v: the asker of the open ask and the claim holder", got, want)
	}
}

// When the listener answers 503 refilling with a Retry-After, the roster read waits it out and
// reads again, and the roster frame reports the live roster.
func TestGoingStatusRefillingRosterIsWaitedOutThenLive(t *testing.T) {
	listener := newStatusListener(t, func(call int, w http.ResponseWriter) {
		if call == 1 {
			answerListRefilling(w, "1")
			return
		}
		_, _ = w.Write([]byte(statusSessions))
	})
	rig := newBlockRig(t, testServerOptions{envoyURL: listener.url},
		readCall(going.ReadRoster, `{}`),
		goingtest.Reply{Final: &goingtest.Final{Answer: "Two sessions are live.", Arguments: "{}"}},
	)
	// Every read asks the listener, as routeStatusServer's do, so the read after the wait meets
	// the listener and not the refusal stored for the silence window.
	rig.served.rosters.ttl, rig.served.rosters.silenceTTL = 0, 0
	seedStatusFixture(t, rig)

	frames := storedGoingFrames(t, rig.goingRoutesRig, askRun(t, rig.goingRoutesRig, "alice", map[string]any{"words": "who is working on ACME", "project": "ACME"}))
	if roster := rosterFrameOf(t, frames); roster.Source != goingRosterLive {
		t.Errorf("roster frame = %+v, want live once the refill was waited out", roster)
	}
	calls, at := listener.requests()
	if len(calls) < 2 {
		t.Fatalf("the listener was asked %v, want the refused read and the read after it", calls)
	}
	if waited := at[1].Sub(at[0]); waited < time.Second {
		t.Errorf("the second read came %s after the refusal, want the Retry-After of 1s waited out", waited)
	}
	sessions := toolResults(t, rig.model)[going.ReadRoster].Tables["sessions"]
	if len(sessions) != 2 {
		t.Errorf("the roster read %v, want the listener's two sessions", sessions)
	}
}

// Every read runs as the run's author and is held to what that author may read (decision 22): a
// credential scoped to one project is offered none of the reads that are no one project's - the
// roster and the delivery reads - and a run asked of one project reads no other, whatever project
// the model names, while a person asking of every project is offered every read.
func TestGoingStatusReadsStayInsideTheAuthorsProject(t *testing.T) {
	rig := newBlockRig(t, testServerOptions{})
	seedActivityFixture(t, rig.database)
	person := goingCredential{cookie: true, credential: &model.Credential{Scopes: []string{model.ScopePerson, model.ScopeApprove}}}
	if got := rig.served.goingReadsFor(person, false); !slices.Equal(got, []string{going.ReadArchitecture, going.ReadDelivery, going.ReadProgress, going.ReadProjectActivity, going.ReadRoster}) {
		t.Errorf("a person is offered %v, want every read", got)
	}
	scoped := goingCredential{credential: &model.Credential{Session: "ses-scoped"}}
	if got := rig.served.goingReadsFor(scoped, true); !slices.Equal(got, []string{going.ReadArchitecture, going.ReadProgress, going.ReadProjectActivity}) {
		t.Errorf("a project-scoped credential is offered %v, want the project's own reads alone", got)
	}

	reads := rig.served.goingRunReads(goingRequest{Project: "DEMO"}, goingRunStart{reads: []string{going.ReadProgress, going.ReadProjectActivity}})
	for tool, arguments := range map[string]string{
		going.ReadProgress:        `{"project":"OTHER"}`,
		going.ReadProjectActivity: `{"project":"OTHER"}`,
	} {
		_, err := reads.funcs[tool](context.Background(), json.RawMessage(arguments))
		var refused *going.ReadRefusal
		if !errors.As(err, &refused) {
			t.Errorf("%s %s under a run asked of DEMO = %v, want it refused", tool, arguments, err)
		}
	}
	if _, err := reads.funcs[going.ReadProgress](context.Background(), json.RawMessage(`{"issue":"OTHER-1"}`)); err == nil {
		t.Error("read_progress of OTHER-1 under a run asked of DEMO answered, want it refused")
	}
	if _, err := reads.funcs[going.ReadProgress](context.Background(), json.RawMessage(`{}`)); err != nil {
		t.Errorf("read_progress naming nothing under a run asked of DEMO = %v, want DEMO read", err)
	}
}
