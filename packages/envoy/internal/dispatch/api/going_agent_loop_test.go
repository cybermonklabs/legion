package api

// going_agent_loop_test.go holds a run as the person meets it: the agent's real loop
// (going/agent) runs in the test process, connected to the server over its real socket, and calls
// a fake chat model (going/agent/agenttest) that answers what each row scripts and counts its
// calls.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/going/agent"
	"github.com/sjawhar/envoy/going/agent/agenttest"
	"github.com/sjawhar/envoy/going/agentwire"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// goingScript is what a test's fake chat model answers, by the person's words of the run that
// asks: each row's reply to the n-th request of its run, from 1.
type goingScript struct {
	mu   sync.Mutex
	rows map[string]func(n int) agenttest.Reply
}

// set scripts the replies to the run whose person's words are words.
func (s *goingScript) set(words string, reply func(n int) agenttest.Reply) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[words] = reply
}

// goingLoop is a fake chat model that answers each request as its script says, and the agent's
// loop working the rig's runs against it.
type goingLoop struct {
	fake   *agenttest.Model
	script *goingScript
	stop   func()
}

// newGoingLoop is the fake and the agent's loop, which stops when the test ends.
func newGoingLoop(t *testing.T, rig goingAgentRig) *goingLoop {
	t.Helper()
	loop := &goingLoop{script: &goingScript{rows: map[string]func(int) agenttest.Reply{}}}
	loop.fake = agenttest.New(t, func(request *agenttest.Request) agenttest.Reply {
		loop.script.mu.Lock()
		defer loop.script.mu.Unlock()
		for words, reply := range loop.script.rows {
			if request.Asks(words) {
				return reply(len(loop.fake.Asking(words)))
			}
		}
		return agenttest.Reply{Text: "No row of this test asks that."}
	})
	loop.start(rig)
	t.Cleanup(func() { loop.stop() })
	return loop
}

// start starts an agent's loop, a new instance, at the rig's socket. Its stop waits for the loop
// to return; the loop fails only where the server refuses its hello, which every row's run then
// shows by never ending.
func (l *goingLoop) start(rig goingAgentRig) {
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan error, 1)
	go func() { ran <- agent.Run(ctx, rig.client, agent.Config{Model: l.fake.Model(), Slots: 8}) }()
	l.stop = sync.OnceFunc(func() {
		cancel()
		<-ran
	})
}

// toolCall is the reply that calls tool with arguments.
func toolCall(tool string, arguments map[string]any) agenttest.Reply {
	return agenttest.Reply{Calls: []agenttest.Call{{Name: tool, Arguments: arguments}}}
}

// answered is the reply that answers text and calls nothing.
func answered(text string) agenttest.Reply { return agenttest.Reply{Text: text} }

// goingLoopRun is a run a row starts, as the route that starts a run records one: login's words,
// at level, in mode (ask where it is empty), following continues where it is set, asked on the
// page of the issue page names where it is set, with what it already spent, granted to the header
// identity or, where token is set, to that agent token.
type goingLoopRun struct {
	login, words, level, mode, continues, token, page string
	workingMS                                         int64
}

// begin records run queued and announces it, and answers its id.
func (rig goingAgentRig) begin(t *testing.T, run goingLoopRun) string {
	t.Helper()
	id := uuid.NewString()
	if run.level == "" {
		run.level = "standard"
	}
	var continues *string
	if run.continues != "" {
		continues = &run.continues
	}
	if run.mode == "" {
		run.mode = goingModeAsk
	}
	var chip *string
	if run.page != "" {
		encoded, err := json.Marshal(goingContext{Path: "/issues/" + run.page, Issue: run.page})
		if err != nil {
			t.Fatal(err)
		}
		text := string(encoded)
		chip = &text
	}
	rig.exec(t, `
		insert into going_runs (id, author_login, words, conversation_id, continues, level, mode, state, shape, created_at, budget_used, context)
		values ($1::uuid, $2, $3, coalesce((select coalesce(conversation_id, id) from going_runs where id = $4::uuid), $1::uuid), $4::uuid,
		  $5, $8, 'queued', $6, clock_timestamp(), jsonb_build_object('calls', 0, 'working_ms', $7::bigint, 'tokens', 0), $9::jsonb)
	`, id, run.login, run.words, continues, run.level, goingRunShape, run.workingMS, run.mode, chip)
	if run.token != "" {
		rig.exec(t, `insert into going_run_grants (run_id, kind, login, token_id) values ($1::uuid, 'token', $2, $3::uuid)`, id, run.login, run.token)
	} else {
		rig.exec(t, `insert into going_run_grants (run_id, kind, login) values ($1::uuid, 'header', $2)`, id, run.login)
	}
	rig.exec(t, `select pg_notify($1, 'ready')`, GoingRunsChannel)
	return id
}

// doneRun records an ended run of login's, the first of a conversation of its own, started and
// ended age ago, as its agent leaves one, and answers its id.
func (rig goingAgentRig) doneRun(t *testing.T, login, words string, age time.Duration) string {
	t.Helper()
	id := uuid.NewString()
	rig.exec(t, `
		insert into going_runs (id, author_login, words, conversation_id, level, mode, state, shape, created_at, finished_at)
		values ($1::uuid, $2, $3, $1::uuid, 'standard', 'ask', 'done', $4, now() - $5::interval, now() - $5::interval)
	`, id, login, words, goingRunShape, age.String())
	rig.exec(t, `insert into going_run_grants (run_id, kind, login) values ($1::uuid, 'header', $2)`, id, login)
	return id
}

// goingRunRecord is what a test reads of a run's row.
type goingRunRecord struct {
	state, reason string
	used          agentwire.Budget
}

// record reads run's row.
func (rig goingAgentRig) record(t *testing.T, run string) goingRunRecord {
	t.Helper()
	var record goingRunRecord
	var used []byte
	if err := rig.database.Pool.QueryRow(context.Background(), `
		select state, coalesce(stop_reason, ''), budget_used from going_runs where id = $1::uuid
	`, run).Scan(&record.state, &record.reason, &used); err != nil {
		t.Fatalf("read run %s: %v", run, err)
	}
	if err := json.Unmarshal(used, &record.used); err != nil {
		t.Fatalf("read what run %s spent: %v", run, err)
	}
	return record
}

// ended waits up to within for run to leave queued and working, and answers its row.
func (rig goingAgentRig) ended(t *testing.T, run string, within time.Duration) goingRunRecord {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		record := rig.record(t, run)
		if record.state != "queued" && record.state != "working" {
			return record
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s is still %s after %s", run, record.state, within)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// toolResults is what the model was told of each tool call of run, in the order of its steps.
func (rig goingAgentRig) toolResults(t *testing.T, run string) []string {
	t.Helper()
	rows, err := rig.database.Pool.Query(context.Background(), `
		select body->>'result' from going_run_steps where run_id = $1::uuid and kind = 'tool' order by step
	`, run)
	if err != nil {
		t.Fatal(err)
	}
	results, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return results
}

// endFrame is the end frame of run's events.
func (rig goingAgentRig) endFrame(t *testing.T, run string) agentwire.EndFrame {
	t.Helper()
	var data []byte
	if err := rig.database.Pool.QueryRow(context.Background(), `
		select data from going_run_events where run_id = $1::uuid and name = $2 order by sequence desc limit 1
	`, run, agentwire.FrameEnd).Scan(&data); err != nil {
		t.Fatalf("read run %s's end frame: %v", run, err)
	}
	var frame agentwire.EndFrame
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatal(err)
	}
	return frame
}

// TestTheAgentActsAsThePerson: the agent acts as the person who started the run, with a tool for
// each thing the person can read or change (check c12): every route of the table a person's
// credential can call is a row of a tool, or a route R12 leaves out; a run of A reads the Inbox
// through its tool, and the answer holds A's rows and no row of B's; a read of
// http://169.254.169.254/, and of an address in no word and no tool result, are refused; and a
// GitHub read of a repository no project maps is refused.
func TestTheAgentActsAsThePerson(t *testing.T) {
	t.Run("every route a person can call has a tool, or is left out with its reason", goingRouteInventory)

	rig := newGoingAgentRig(t, nil)
	loop := newGoingLoop(t, rig)
	// read runs a run of login's whose model makes call, then answers, and answers what the
	// model was told of the call.
	read := func(t *testing.T, login, words string, call map[string]any) string {
		t.Helper()
		loop.script.set(words, func(n int) agenttest.Reply {
			if n == 1 {
				return toolCall("call_tool", call)
			}
			return answered("Read.")
		})
		run := rig.begin(t, goingLoopRun{login: login, words: words})
		if record := rig.ended(t, run, time.Minute); record.state != "done" {
			t.Fatalf("the run ended %s with %q, want done", record.state, record.reason)
		}
		results := rig.toolResults(t, run)
		if len(results) != 1 {
			t.Fatalf("the run saved %d tool results, want 1: %q", len(results), results)
		}
		if told := loop.fake.Asking(words); len(told) != 2 || len(told[1].ToolResults()) != 1 {
			t.Fatalf("the model was not told the result of the call: %d model calls", len(told))
		}
		return results[0]
	}

	t.Run("a run of A reads A's Inbox through its tool, with no row of B's", func(t *testing.T) {
		rig.ask(t, rig.issue(t, "Alice's roof"), "Which tiles does the roof take?")
		bobs := decodeBody[model.Issue](t, rig.fixture(t, "bob", http.MethodPost, "/api/v1/issues", map[string]any{
			"project": "DEMO", "title": "Bob's cellar", "force": true,
		})).Key
		rig.ask(t, bobs, "How deep is the cellar?")
		result := read(t, "alice", "show me my inbox", map[string]any{"tool": "dispatch_inbox", "arguments": map[string]any{"assignee": "me"}})
		if !strings.Contains(result, "status 200") || !strings.Contains(result, "Which tiles does the roof take?") {
			t.Errorf("alice's run read her Inbox as %q, want 200 with her question", result)
		}
		if strings.Contains(result, "How deep is the cellar?") {
			t.Errorf("alice's run read bob's question in her Inbox: %q", result)
		}
	})

	t.Run("a read of http://169.254.169.254/ is refused", func(t *testing.T) {
		const address = "http://169.254.169.254/latest/meta-data/"
		result := read(t, "alice", "what is at "+address, map[string]any{"tool": "web_read", "arguments": map[string]any{"url": address}})
		if !strings.Contains(result, agentwire.CodeAddressRefused) {
			t.Errorf("the read of %s was told %q, want %s", address, result, agentwire.CodeAddressRefused)
		}
	})

	t.Run("a read of an address in no word and no tool result is refused", func(t *testing.T) {
		result := read(t, "alice", "read the page I mean", map[string]any{"tool": "web_read", "arguments": map[string]any{"url": "https://example.com/never-named"}})
		if !strings.Contains(result, agentwire.CodeAddressUnseen) {
			t.Errorf("the read of an address nobody named was told %q, want %s", result, agentwire.CodeAddressUnseen)
		}
	})

	t.Run("a GitHub read of a repository no project maps is refused", func(t *testing.T) {
		result := read(t, "alice", "read the pull request of acme/unmapped", map[string]any{
			"tool": "github_read", "arguments": map[string]any{"repository": "acme/unmapped", "path": "pulls/1"},
		})
		if !strings.Contains(result, agentwire.CodeRepositoryUnmapped) {
			t.Errorf("the read of an unmapped repository was told %q, want %s", result, agentwire.CodeRepositoryUnmapped)
		}
	})
}

// TestARunKeepsItsBoundsAndItsRecord: a run keeps its bounds, its record and its credential, and
// survives a restart (check c11). A Fast run whose model never ends stops after 6 calls, says the
// call budget stopped it, and offers Continue; a plan that waits 10 minutes for its person uses
// none of a Fast run's 90 s; a Standard run lasts past 30 s; a run cut by a restart in its model
// loop resumes from its last stored turn; a run whose bar closed goes on, and History shows its
// result; History shows a run 2 hours old and not one 8 days old; a run cut by a restart between
// two writes of its plan resumes, and each change runs once; a second person reads no
// conversation of the first; a plan that waits shows in its person's Inbox; a short answer adds no
// run to History; a follow-up's request holds the turns before it; Continue starts a follow-up
// with a budget of its own; and a run whose token is revoked stops at its next call, naming the
// revoked token.
func TestARunKeepsItsBoundsAndItsRecord(t *testing.T) {
	rig := newGoingAgentRig(t, nil)
	rig.served.deps.Identity = headerIdentity(rig.database)
	loop := newGoingLoop(t, rig)
	neverEnds := func(int) agenttest.Reply {
		return toolCall("find_tools", map[string]any{"need": "an issue's status"})
	}

	t.Run("a Fast run whose model never ends stops after 6 calls, names the call budget and offers Continue", func(t *testing.T) {
		const words = "keep looking for ever"
		loop.script.set(words, neverEnds)
		run := rig.begin(t, goingLoopRun{login: "alice", words: words, level: "fast"})
		record := rig.ended(t, run, time.Minute)
		if record.state != "stopped" || !strings.HasPrefix(record.reason, agent.ReasonCallBudget) {
			t.Fatalf("the run ended %s with %q, want stopped with %s", record.state, record.reason, agent.ReasonCallBudget)
		}
		if calls := len(loop.fake.Asking(words)); calls != 6 || record.used.Calls != 6 {
			t.Errorf("the run made %d model calls and counts %d, want 6", calls, record.used.Calls)
		}
		if frame := rig.endFrame(t, run); frame.State != "stopped" || len(frame.Offers) != 1 || frame.Offers[0] != agentwire.OfferContinue {
			t.Errorf("the run's end frame is %+v, want stopped offering continue", frame)
		}
	})

	approve := func(t *testing.T, run string) {
		t.Helper()
		if response := dispatchRequest(t, rig.mux, http.MethodPost, "/api/v1/going/runs/"+run+"/plan/approve", map[string]any{"plan": 1}, "alice"); response.Code != http.StatusOK {
			t.Fatalf("approve the plan of %s answered %d %s", run, response.Code, response.Body.String())
		}
	}
	comments := func(t *testing.T, issue string) int {
		t.Helper()
		return rig.count(t, `select count(*) from comments where issue_key = $1`, issue)
	}
	commentOn := func(issue string) map[string]any {
		return map[string]any{"tool": "dispatch_comment", "arguments": map[string]any{"issue": issue, "body": "A comment on " + issue + "."}}
	}

	t.Run("a plan that waits 10 minutes uses none of Fast's 90 s", func(t *testing.T) {
		const words = "comment, after a long wait"
		issue := rig.issue(t, "Waited on")
		loop.script.set(words, func(n int) agenttest.Reply {
			if n == 1 {
				return toolCall("propose_plan", map[string]any{"summary": "A comment.", "changes": []map[string]any{commentOn(issue)}})
			}
			return answered("Commented.")
		})
		run := rig.begin(t, goingLoopRun{login: "alice", words: words, level: "fast"})
		if record := rig.ended(t, run, time.Minute); record.state != "waiting" {
			t.Fatalf("the run ended %s with %q, want its plan waiting", record.state, record.reason)
		}
		// Ten minutes of waiting for its person, as the run's times show them.
		rig.exec(t, `update going_runs set created_at = created_at - interval '10 minutes', finished_at = finished_at - interval '10 minutes' where id = $1::uuid`, run)
		approve(t, run)
		record := rig.ended(t, run, time.Minute)
		if record.state != "done" || comments(t, issue) != 1 || record.used.WorkingMS >= 90_000 {
			t.Fatalf("after the wait the run ended %s with %q, %d comments and %d ms worked, want done with the comment and the wait unworked",
				record.state, record.reason, comments(t, issue), record.used.WorkingMS)
		}
	})

	t.Run("a Standard run lasts past 30 s", func(t *testing.T) {
		const words = "think about it for a while"
		wait := make(chan struct{})
		time.AfterFunc(31*time.Second, func() { close(wait) })
		loop.script.set(words, func(int) agenttest.Reply { return agenttest.Reply{Hold: wait, Text: "Thought it over."} })
		started := time.Now()
		run := rig.begin(t, goingLoopRun{login: "alice", words: words})
		record := rig.ended(t, run, 2*time.Minute)
		if record.state != "done" || time.Since(started) < 30*time.Second {
			t.Errorf("the run ended %s with %q after %s, want done past 30 s", record.state, record.reason, time.Since(started))
		}
	})

	t.Run("a run cut by a restart in its model loop resumes from its last stored turn", func(t *testing.T) {
		const words = "find the tools, then wait"
		never := make(chan struct{})
		loop.script.set(words, func(n int) agenttest.Reply {
			switch n {
			case 1:
				return toolCall("find_tools", map[string]any{"need": "an issue"})
			case 2:
				return agenttest.Reply{Hold: never}
			}
			return answered("Resumed.")
		})
		run := rig.begin(t, goingLoopRun{login: "alice", words: words})
		goingEventually(t, "the run's second model call", func() bool { return len(loop.fake.Asking(words)) == 2 })
		loop.stop()
		loop.start(rig)
		if record := rig.ended(t, run, time.Minute); record.state != "done" {
			t.Fatalf("the run ended %s with %q after the restart, want done", record.state, record.reason)
		}
		requests := loop.fake.Asking(words)
		if len(requests) != 3 {
			t.Fatalf("the run made %d model calls, want 3: the tool call, the call the restart cut, and the one that resumed", len(requests))
		}
		if _, left := requests[1].Left(); !left {
			t.Error("the restart did not cut the model call in flight")
		}
		if results := requests[2].ToolResults(); len(results) != 1 || !strings.Contains(results[0], "dispatch_") {
			t.Errorf("the call after the restart gave the model the tool results %q, want the find_tools result it stored", results)
		}
		if results := rig.toolResults(t, run); len(results) != 1 {
			t.Errorf("the run saved %d tool steps, want the one it made before the restart and none again", len(results))
		}
	})

	history := func(t *testing.T, login string) goingConversationsPage {
		t.Helper()
		return goingHistory(t, rig.mux, login, "")
	}
	listed := func(page goingConversationsPage, run string) (goingRun, bool) {
		for _, conversation := range page.Rows {
			for _, listed := range conversation.Runs {
				if listed.ID == run {
					return listed, true
				}
			}
		}
		return goingRun{}, false
	}
	start := func(t *testing.T, login string, body map[string]any) goingRun {
		t.Helper()
		response := dispatchRequest(t, rig.mux, http.MethodPost, "/api/v1/going/runs", body, login)
		if response.Code != http.StatusCreated {
			t.Fatalf("POST /api/v1/going/runs as %s answered %d %s", login, response.Code, response.Body.String())
		}
		return decodeBody[goingRun](t, response)
	}

	t.Run("a run whose bar closed goes on, and its result shows in History", func(t *testing.T) {
		const words = "look it up while the bar is closed"
		loop.script.set(words, func(n int) agenttest.Reply {
			if n == 1 {
				return toolCall("find_tools", map[string]any{"need": "an issue"})
			}
			return answered("Looked it up.")
		})
		// Started by the route the bar sends, and read by nobody after it answers: the bar closed.
		run := start(t, "alice", map[string]any{"words": words, "level": "fast"})
		if run.State != "queued" || run.Conversation != run.ID || run.Level != "fast" || run.Mode != goingModeAsk {
			t.Fatalf("the start answered %+v, want a queued fast run in ask, its own conversation", run)
		}
		if record := rig.ended(t, run.ID, time.Minute); record.state != "done" {
			t.Fatalf("the run ended %s with %q, want done", record.state, record.reason)
		}
		got, ok := listed(history(t, "alice"), run.ID)
		if !ok || got.State != "done" || got.Words != words || got.Used.Calls != 2 || got.FinishedAt == nil {
			t.Errorf("History holds the run as %+v (listed %v), want it done after its 2 model calls", got, ok)
		}
	})
	t.Run("a run 2 hours old shows in History, and a run 8 days old does not", func(t *testing.T) {
		recent := rig.doneRun(t, "alice", "asked two hours ago", 2*time.Hour)
		old := rig.doneRun(t, "alice", "asked eight days ago", 8*24*time.Hour)
		if err := sweepGoingRuns(context.Background(), rig.database.Pool); err != nil {
			t.Fatalf("sweep: %v", err)
		}
		page := history(t, "alice")
		if _, ok := listed(page, recent); !ok {
			t.Errorf("History misses the run 2 hours old")
		}
		if _, ok := listed(page, old); ok {
			t.Errorf("History lists the run 8 days old")
		}
		if response := dispatchRequest(t, rig.mux, http.MethodGet, "/api/v1/going/runs/"+recent, nil, "alice"); response.Code != http.StatusOK {
			t.Errorf("GET the run 2 hours old answered %d %s, want 200", response.Code, response.Body.String())
		}
		if response := dispatchRequest(t, rig.mux, http.MethodGet, "/api/v1/going/runs/"+old, nil, "alice"); response.Code != http.StatusNotFound {
			t.Errorf("GET the run 8 days old answered %d %s, want 404", response.Code, response.Body.String())
		}
	})
	t.Run("a run stopped by a restart between two writes of its plan resumes, and each change runs once", func(t *testing.T) {
		const words = "comment twice, across a restart"
		first, second := rig.issue(t, "Before the restart"), rig.issue(t, "After the restart")
		loop.script.set(words, func(n int) agenttest.Reply {
			if n == 1 {
				return toolCall("propose_plan", map[string]any{"summary": "Two comments.", "changes": []map[string]any{commentOn(first), commentOn(second)}})
			}
			return answered("Commented twice.")
		})
		run := rig.begin(t, goingLoopRun{login: "alice", words: words})
		if record := rig.ended(t, run, time.Minute); record.state != "waiting" {
			t.Fatalf("the run ended %s with %q, want its plan waiting", record.state, record.reason)
		}
		held, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		rig.hooks.set(t, http.MethodPost, "/api/v1/issues/"+second+"/comments", func(w http.ResponseWriter, r *http.Request, inner http.Handler) {
			once.Do(func() { close(held) })
			<-release
			inner.ServeHTTP(w, r)
		})
		approve(t, run)
		<-held
		loop.stop()
		close(release)
		loop.start(rig)
		if record := rig.ended(t, run, 2*time.Minute); record.state != "done" || comments(t, first) != 1 || comments(t, second) != 1 {
			t.Fatalf("after the restart the run ended %s with %q and the issues hold %d and %d comments, want done with one each",
				record.state, record.reason, comments(t, first), comments(t, second))
		}
	})
	t.Run("a second person reads no conversation of the first (404)", func(t *testing.T) {
		run := rig.doneRun(t, "alice", "alice's own question", time.Minute)
		for _, read := range []struct{ method, path string }{
			{http.MethodGet, "/api/v1/going/runs/" + run},
			{http.MethodGet, "/api/v1/going/runs/" + run + "/events"},
			{http.MethodPost, "/api/v1/going/runs/" + run + "/stop"},
		} {
			response := dispatchRequest(t, rig.mux, read.method, read.path, nil, "bob")
			if response.Code != http.StatusNotFound || decodeBody[refusalAnswer](t, response).Code != codeGoingRunNotFound {
				t.Errorf("%s %s as bob answered %d %s, want 404 %s", read.method, read.path, response.Code, response.Body.String(), codeGoingRunNotFound)
			}
		}
		if response := dispatchRequest(t, rig.mux, http.MethodPost, "/api/v1/going/runs", map[string]any{"words": "and then?", "continues": run}, "bob"); response.Code != http.StatusNotFound {
			t.Errorf("bob's follow-up of alice's run answered %d %s, want 404", response.Code, response.Body.String())
		}
		if _, ok := listed(history(t, "bob"), run); ok {
			t.Error("bob's History lists alice's run")
		}
		if record := rig.record(t, run); record.state != "done" {
			t.Errorf("after bob's stop alice's run is %s, want it done as it was", record.state)
		}
		if response := dispatchRequest(t, rig.mux, http.MethodGet, "/api/v1/going/runs/"+run, nil, "alice"); response.Code != http.StatusOK {
			t.Errorf("GET the run as alice answered %d %s, want 200", response.Code, response.Body.String())
		}
	})
	t.Run("a plan that waits shows in the person's Inbox", func(t *testing.T) {
		const words = "comment, and wait in my Inbox"
		issue := rig.issue(t, "Planned from the Inbox")
		loop.script.set(words, func(n int) agenttest.Reply {
			if n == 1 {
				return toolCall("propose_plan", map[string]any{"summary": "A comment from the Inbox.", "changes": []map[string]any{commentOn(issue)}})
			}
			return answered("Commented.")
		})
		inbox := func(t *testing.T, login string) []goingWaitingPlan {
			t.Helper()
			response := dispatchRequest(t, rig.mux, http.MethodGet, "/api/v1/me/going-plans", nil, login)
			if response.Code != http.StatusOK {
				t.Fatalf("GET /api/v1/me/going-plans as %s answered %d %s", login, response.Code, response.Body.String())
			}
			return decodeBody[goingWaitingPlansPage](t, response).Rows
		}
		events := func() int {
			return rig.count(t, `select count(*) from events where type = 'going_plan.updated' and payload->>'login' = 'alice'`)
		}
		before := events()
		run := start(t, "alice", map[string]any{"words": words})
		if record := rig.ended(t, run.ID, time.Minute); record.state != "waiting" {
			t.Fatalf("the run ended %s with %q, want its plan waiting", record.state, record.reason)
		}
		waiting := inbox(t, "alice")
		if len(waiting) != 1 || waiting[0].Run != run.ID || waiting[0].Plan != 1 || waiting[0].Waiting != 1 ||
			waiting[0].Summary != "A comment from the Inbox." || waiting[0].Words != words {
			t.Fatalf("alice's Inbox holds the plans %+v, want the run's one waiting plan", waiting)
		}
		if others := inbox(t, "bob"); len(others) != 0 {
			t.Errorf("bob's Inbox holds the plans %+v, want none of alice's", others)
		}
		if after := events(); after <= before {
			t.Errorf("the plan that began to wait appended %d going_plan.updated naming alice, want one", after-before)
		}
		approved := events()
		approve(t, run.ID)
		if record := rig.ended(t, run.ID, time.Minute); record.state != "done" || comments(t, issue) != 1 {
			t.Fatalf("after the approval the run ended %s with %q and %d comments, want done with the comment", record.state, record.reason, comments(t, issue))
		}
		if left := inbox(t, "alice"); len(left) != 0 {
			t.Errorf("after the approval alice's Inbox holds the plans %+v, want none", left)
		}
		if after := events(); after <= approved {
			t.Error("the approval appended no going_plan.updated naming alice")
		}
	})
	t.Run("a short answer adds no run to History", func(t *testing.T) {
		const words = "what is open right now?"
		asked := dispatchRequest(t, rig.mux, http.MethodPost, "/api/v1/going/requests", map[string]any{"words": words}, "alice")
		if asked.Code != http.StatusCreated {
			t.Fatalf("POST /api/v1/going/requests answered %d %s", asked.Code, asked.Body.String())
		}
		id := decodeBody[goingRequest](t, asked).ID
		goingEventually(t, "the short answer's ending", func() bool {
			return rig.count(t, `select count(*) from going_events where request_id = $1::uuid and name = any($2::text[])`, id, goingTerminalNames) == 1
		})
		if runs := rig.count(t, `select count(*) from going_runs where id = $1::uuid`, id); runs != 0 {
			t.Errorf("the short answer wrote %d runs, want none", runs)
		}
		for _, conversation := range history(t, "alice").Rows {
			for _, run := range conversation.Runs {
				if run.Words == words {
					t.Errorf("History lists the short answer as the run %+v", run)
				}
			}
		}
	})

	t.Run("a follow-up's request holds the turns before it", func(t *testing.T) {
		const first, second = "which colour are plums?", "and pears?"
		loop.script.set(first, func(int) agenttest.Reply { return answered("Plums are purple.") })
		loop.script.set(second, func(int) agenttest.Reply { return answered("Pears are green.") })
		earlier := rig.begin(t, goingLoopRun{login: "alice", words: first})
		if record := rig.ended(t, earlier, time.Minute); record.state != "done" {
			t.Fatalf("the first run ended %s with %q", record.state, record.reason)
		}
		follow := rig.begin(t, goingLoopRun{login: "alice", words: second, continues: earlier})
		if record := rig.ended(t, follow, time.Minute); record.state != "done" {
			t.Fatalf("the follow-up ended %s with %q", record.state, record.reason)
		}
		requests := loop.fake.Asking(second)
		if len(requests) != 1 {
			t.Fatalf("the follow-up made %d model calls, want 1", len(requests))
		}
		if text := requests[0].Text(); !strings.Contains(text, first) || !strings.Contains(text, "Plums are purple.") {
			t.Errorf("the follow-up's request does not hold the turn before it: %s", text)
		}
	})

	t.Run("Continue starts a follow-up with a new budget", func(t *testing.T) {
		const first, next = "look until the budget ends", "go on from there"
		loop.script.set(first, neverEnds)
		loop.script.set(next, func(n int) agenttest.Reply {
			if n == 1 {
				return toolCall("find_tools", map[string]any{"need": "an issue"})
			}
			return answered("Found it.")
		})
		earlier := rig.begin(t, goingLoopRun{login: "alice", words: first, level: "fast"})
		if record := rig.ended(t, earlier, time.Minute); record.state != "stopped" || rig.endFrame(t, earlier).Offers == nil {
			t.Fatalf("the first run ended %s with %q, want stopped offering Continue", record.state, record.reason)
		}
		follow := rig.begin(t, goingLoopRun{login: "alice", words: next, level: "fast", continues: earlier})
		record := rig.ended(t, follow, time.Minute)
		if record.state != "done" || record.used.Calls != 2 || len(loop.fake.Asking(next)) != 2 {
			t.Errorf("the follow-up ended %s with %q after %d calls, want done after 2 calls of a budget of its own",
				record.state, record.reason, record.used.Calls)
		}
	})

	t.Run("when the token that started a run is revoked, its next call fails, and the run stops naming the revoked token", func(t *testing.T) {
		const words = "who am I acting as?"
		hash := make([]byte, 32)
		_, _ = rand.Read(hash)
		var token string
		if err := rig.database.Pool.QueryRow(context.Background(), `
			insert into agent_tokens (owner, name, token_hash, prefix) values ('alice', 'a script of alice''s', $1, 'dsp_test') returning id::text
		`, hash).Scan(&token); err != nil {
			t.Fatal(err)
		}
		revoked := make(chan struct{})
		loop.script.set(words, func(n int) agenttest.Reply {
			if n == 1 {
				return agenttest.Reply{Hold: revoked, Calls: []agenttest.Call{{Name: "call_tool", Arguments: map[string]any{"tool": "dispatch_whoami", "arguments": map[string]any{}}}}}
			}
			return answered("You are alice.")
		})
		run := rig.begin(t, goingLoopRun{login: "alice", words: words, token: token})
		goingEventually(t, "the run's first model call", func() bool { return len(loop.fake.Asking(words)) == 1 })
		rig.exec(t, `update agent_tokens set revoked_at = now() where id = $1::uuid`, token)
		close(revoked)
		record := rig.ended(t, run, time.Minute)
		if record.state != "stopped" || !strings.HasPrefix(record.reason, goingGrantTokenRevoked) {
			t.Errorf("the run ended %s with %q, want stopped with %s", record.state, record.reason, goingGrantTokenRevoked)
		}
		if calls := len(loop.fake.Asking(words)); calls != 1 {
			t.Errorf("the run made %d model calls, want none after the refused call", calls)
		}
	})
}

// TestARunsBlockIsDrawnFromItsSavedResults: a run's model asks for a block through call_tool's
// draw_block, naming the step and call of the list read it made and a chain over its rows, and
// the server draws the block from that saved result alone (settlement 9, R3): the run's events
// hold the block frame with the server's count, and none of the number the model wrote. A spec
// that holds a number of the model's is refused back to the model, BLOCK_REFUSED, and draws
// nothing.
func TestARunsBlockIsDrawnFromItsSavedResults(t *testing.T) {
	rig := newGoingAgentRig(t, nil)
	loop := newGoingLoop(t, rig)
	const words = "chart how many issues DEMO holds"
	spec := func(title string) map[string]any {
		return map[string]any{"tool": "draw_block", "arguments": map[string]any{
			"kind": "metric", "title": title, "steps": []map[string]any{{"step": 2, "call": 1}},
			"chain": []map[string]any{{"op": "count"}}, "field": "count",
		}}
	}
	loop.script.set(words, func(n int) agenttest.Reply {
		switch n {
		case 1:
			return toolCall("call_tool", map[string]any{"tool": "dispatch_issues", "arguments": map[string]any{"project": "DEMO"}})
		case 2:
			return toolCall("call_tool", spec("The 7 issues of DEMO"))
		case 3:
			return toolCall("call_tool", spec("The issues of DEMO"))
		}
		return answered("DEMO holds 7 issues.")
	})
	run := rig.begin(t, goingLoopRun{login: "alice", words: words})
	if record := rig.ended(t, run, time.Minute); record.state != "done" {
		t.Fatalf("the run ended %s with %q, want done", record.state, record.reason)
	}
	results := rig.toolResults(t, run)
	if len(results) != 3 || !strings.Contains(results[0], "step 2, call 1: status 200") {
		t.Fatalf("the run saved the tool results %q, want the list read as step 2, call 1, then two draws", results)
	}
	if !strings.Contains(results[1], agentwire.CodeBlockRefused) || !strings.Contains(results[1], "holds no number") {
		t.Errorf("the draw whose title held a number was told %q, want %s for the number", results[1], agentwire.CodeBlockRefused)
	}
	var body []byte
	if err := rig.database.Pool.QueryRow(context.Background(), `select body from going_run_steps where run_id = $1::uuid and step = 2`, run).Scan(&body); err != nil {
		t.Fatalf("read the saved list read: %v", err)
	}
	saved, err := agent.ReadSavedResult(body)
	var listed []map[string]any
	if err != nil || json.Unmarshal(saved.Body, &listed) != nil || len(listed) == 0 || len(listed) == 7 {
		t.Fatalf("the saved list read reads as %d issues (%v), want the rig's issues, which are not 7", len(listed), err)
	}
	rows, err := rig.database.Pool.Query(context.Background(), `select data from going_run_events where run_id = $1::uuid and name = 'block' order by sequence`, run)
	if err != nil {
		t.Fatal(err)
	}
	blocks, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (going.BlockData, error) {
		var block going.BlockData
		var data []byte
		if err := row.Scan(&data); err != nil {
			return block, err
		}
		return block, json.Unmarshal(data, &block)
	})
	if err != nil {
		t.Fatalf("read the block frames: %v", err)
	}
	want := fmt.Sprintf(`[{"count":%d}]`, len(listed))
	if len(blocks) != 1 {
		t.Fatalf("the run's events hold %d block frames, want the one the server drew", len(blocks))
	}
	if got, _ := json.Marshal(blocks[0].Rows); string(got) != want || blocks[0].Kind != going.BlockMetric || blocks[0].Title != "The issues of DEMO" {
		t.Errorf("the block frame is %s %q with rows %s, want the metric with the server's count %s", blocks[0].Kind, blocks[0].Title, got, want)
	}
}

// TestARunWhoseSavedStepNamesAToolThisBuildLacksFails: a run whose saved step names a tool this
// build's agent does not know, as a later build that has the tool saves one, fails with
// UNKNOWN_TOOL naming the tool, and its model is never called (R15, check c25's last row); a run
// whose own model makes a tool name up is told so and goes on to its answer.
func TestARunWhoseSavedStepNamesAToolThisBuildLacksFails(t *testing.T) {
	rig := newGoingAgentRig(t, nil)
	loop := newGoingLoop(t, rig)

	t.Run("a step a later build saved with its own tool fails the run, naming the tool", func(t *testing.T) {
		const words = "carry on with the dragon"
		loop.script.set(words, func(int) agenttest.Reply { return answered("Carried on.") })
		// Queued at a shape no build reads, so no agent claims it before its step is saved.
		run := rig.queued(t, "alice", goingRunShape+1, words)
		rig.exec(t, `insert into going_run_steps (run_id, step, kind, body, note) values ($1::uuid, 1, $2, $3::jsonb, 'a later build''s step')`,
			run, agentwire.StepModel, `{"text":"","calls":[{"id":"call_1","tool":"summon_dragon","arguments":{}}],"output_tokens":1}`)
		rig.exec(t, `update going_runs set shape = $2 where id = $1::uuid`, run, goingRunShape)
		rig.exec(t, `select pg_notify($1, 'ready')`, GoingRunsChannel)
		record := rig.ended(t, run, time.Minute)
		if record.state != "failed" || !strings.HasPrefix(record.reason, agentwire.CodeUnknownTool) || !strings.Contains(record.reason, `"summon_dragon"`) {
			t.Fatalf("the run ended %s with %q, want failed with %s naming summon_dragon", record.state, record.reason, agentwire.CodeUnknownTool)
		}
		if calls := len(loop.fake.Asking(words)); calls != 0 {
			t.Errorf("the run made %d model calls, want none: the unknown tool would have been replayed to the model", calls)
		}
	})

	t.Run("a tool the run's own model makes up is answered, and the run goes on", func(t *testing.T) {
		const words = "summon whatever you need"
		loop.script.set(words, func(n int) agenttest.Reply {
			if n == 1 {
				return toolCall("summon_dragon", map[string]any{})
			}
			return answered("Done without a dragon.")
		})
		run := rig.begin(t, goingLoopRun{login: "alice", words: words})
		if record := rig.ended(t, run, time.Minute); record.state != "done" {
			t.Fatalf("the run ended %s with %q, want done", record.state, record.reason)
		}
		if calls := len(loop.fake.Asking(words)); calls != 2 {
			t.Errorf("the run made %d model calls, want the one that made the name up and the one that answered", calls)
		}
	})
}
