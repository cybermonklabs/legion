package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/answer"
	"github.com/sjawhar/legion/daemon/internal/answerloop"
	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/conversation"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/ghtool"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testpg"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

type noTool struct{}

func (noTool) Run(context.Context, answer.GHCall) ghtool.Result { return ghtool.Result{} }
func (noTool) Lookup(context.Context, int) (ghtool.Found, error) {
	return ghtool.Found{}, ghtool.ErrNotAWrite
}
func (noTool) Close() error { return nil }
func (noTool) Open(context.Context, ghtool.Agent, string) (answerloop.Tool, error) {
	return noTool{}, nil
}

type noFacts struct{}

func (noFacts) Facts(context.Context, string) (answerloop.IssueFacts, error) {
	return answerloop.IssueFacts{}, nil
}

// The race the gate exists for, through the real gate and the loop: the stopped tester's claim is
// started while its answer is being generated. The start passes its check, the answer is dropped
// with nothing added to the conversation, and the question goes to the running agent on its role
// topic (A7).
func TestAClaimStartedDuringAnAnswerGetsTheQuestionAndItsConversationNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := testpg.CloneStore(t, "legion_daemon_answers")
	path := filepath.Join(t.TempDir(), "ses_tester.jsonl")
	session := `{"type":"session","version":3,"id":"s","timestamp":"2026-10-26T00:00:00.000Z","cwd":"/work"}` + "\n" +
		`{"id":"e0000001","parentId":null,"timestamp":"2026-10-26T00:00:00.000Z","type":"model_change","model":"anthropic/claude-opus-5-5"}` + "\n" +
		`{"id":"e0000002","parentId":"e0000001","timestamp":"2026-10-26T00:00:00.000Z","type":"message","message":{"role":"user","content":[{"type":"text","text":"Test it."}],"attribution":"user","timestamp":1}}` + "\n" +
		`{"id":"e0000003","parentId":"e0000002","timestamp":"2026-10-26T00:00:00.000Z","type":"message","message":{"role":"assistant","content":[{"type":"text","text":"Tested."}],"api":"anthropic-messages","provider":"anthropic","model":"claude-opus-5-5","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"stopReason":"stop","timestamp":2}}` + "\n"
	if err := os.WriteFile(path, []byte(session), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	if _, err := st.RegisterConversation(ctx, store.AgentConversation{
		Session: "ses_tester", Project: "legion", Issue: "LEGION-9", Role: claim.RoleTester, Token: gateToken,
		Harness: runtime.BackendOmp, Location: path, StartedAt: now, RecordedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var published []map[string]any
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		published = append(published, body)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer listener.Close()
	posted := false
	dispatchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posted = true
		w.WriteHeader(http.StatusCreated)
	}))
	defer dispatchServer.Close()

	gate, _, views := newTestGate(t)
	loop := answerloop.New(answerloop.Config{
		Project: "legion",
		Answers: config.Answers{Bounds: config.AnswerBounds{Answer: time.Minute, ModelCalls: 1, OutputTokens: 100, Concurrent: 1, DailyUSD: 50}},
		Store:   st, Dispatch: dispatch.New(dispatchServer.URL, dispatch.BearerTransport("t", nil)), Envoy: notify.New(listener.URL, "t"), Gate: gate,
		Issues: noFacts{}, Tools: noTool{}, Conversations: answerloop.Harnesses{},
		PassOn:    controllerPassOn{pool: st.Pool(), records: record.NewStore(), now: func() time.Time { return now }, log: slog.New(slog.NewTextHandler(io.Discard, nil))},
		Decisions: routedDecisions{},
		Answer: func(ctx context.Context, req answer.Request) (answer.Reply, error) {
			// The operator resumes the tester while its answer is generated.
			if err := gate.CheckLaunch(ctx, views.view.Claim); err != nil {
				t.Errorf("the start's check: %v", err)
			}
			views.set(supervise.StateLaunching, 4)
			return answer.Reply{Kind: answer.Answered, Text: "Tested.", Append: conversation.Append{
				QuestionID: req.Question.ID, Question: req.Question.Text, Provider: "anthropic", API: "anthropic-messages", Model: "claude-opus-5-5",
				Turns: []conversation.Turn{{Content: []conversation.Block{{Type: conversation.Text, Text: "Tested."}}}},
			}}, nil
		},
		Now: func() time.Time { return now },
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	handoff := intake.Handoff{
		OriginalTopic: "notifications.agent.ses_tester", TargetSession: "ses_tester", RoleToken: string(gateToken), Project: "legion",
		Envelope: intake.HandoffEnvelope{EventID: "evt-1", Source: "agent", SourceEventID: "s", SourceSession: "ses_asker",
			Topic: "notifications.agent.ses_tester", DedupeKey: "agent.ses_tester.0123456789abcdef0123456789abcdef", PayloadSummary: "Did it pass?", TraceID: "t"},
	}
	raw, _ := json.Marshal(handoff)
	if err := loop.Receive(ctx, handoff, raw); err != nil {
		t.Fatal(err)
	}
	running, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { loop.Run(running); close(done) }()
	testwait.Eventually(t, "the question forwarded to the running tester", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(published) == 1
	})
	stop()
	<-done
	if content, _ := os.ReadFile(path); string(content) != session {
		t.Fatalf("the conversation changed:\n%s", content[len(session):])
	}
	if published[0]["topic"] != "notifications.role."+string(gateToken) || published[0]["handoff"] != false || posted {
		t.Fatalf("listener got %v, Dispatch posted %t; want the question on the tester's role topic alone", published, posted)
	}
}

// A question passed to the controller is one agent-question notice, however often PassOn is
// called for it (a restart that finds it not yet marked passed on calls it again, also after the
// notice went): one controller notice row, published once to the controller topic, whose body
// names the question's id, issue, role, session, asker, ref, text and reason.
func TestOutboxAgentQuestionIsQueuedAndDeliveredOncePerQuestion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	passOn := controllerPassOn{pool: pool, records: records, now: func() time.Time { return now }, log: quietLogger()}
	q := store.AnswerQuestion{ID: "5f0c2a9e7b14d3866a1e0c4b9d2f7a35", Issue: "LEGION-9", Role: claim.RoleTester, Session: "ses_tester"}
	origin := answerloop.Origin{Text: "Is PR 7 green?", Asker: "user:person@example.com", Ref: "dispatch://LEGION-9/message/m-1"}

	for range 2 {
		if err := passOn.PassOn(ctx, q, answerloop.ReasonNotAnswered, origin); err != nil {
			t.Fatal(err)
		}
	}
	if rows := outboxRows(t, pool); rows != 1 {
		t.Fatalf("outbox rows after passing the question twice = %d, want one", rows)
	}
	publisher := &outboxPublisher{}
	runner := &outbox{pool: pool, project: "legion", dispatchProject: "LEGION", records: records, notices: publisher, now: func() time.Time { return now }, log: quietLogger()}
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := passOn.PassOn(ctx, q, answerloop.ReasonNotAnswered, origin); err != nil {
		t.Fatal(err)
	}
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	published := publisher.publishes()
	if len(published) != 1 || outboxRows(t, pool) != 0 {
		t.Fatalf("publishes = %+v with %d rows left; want one and none", published, outboxRows(t, pool))
	}
	if published[0].topic != "notifications.legion.legion.controller" || published[0].message != "agent-question on LEGION-9" {
		t.Fatalf("published to %q as %q; want the controller topic, as an agent-question on the agent's issue", published[0].topic, published[0].message)
	}
	body, err := json.Marshal(published[0].payload)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"kind":"agent-question","role":"tester","reason":"not_answered","question":{"id":"5f0c2a9e7b14d3866a1e0c4b9d2f7a35",` +
		`"issue":"LEGION-9","session":"ses_tester","asker":"user:person@example.com","ref":"dispatch://LEGION-9/message/m-1","text":"Is PR 7 green?"}}`
	if string(body) != want {
		t.Fatalf("notice body = %s, want %s", body, want)
	}
}

// A person's decision on an ask a stopped tester of a child opened goes on as one agent-decision
// notice, to the architect that owns the child's issue as the outbox routes any notice
// (noticeArchitect): the child's own architect while it holds its role, else the tree's while it
// holds its own, and otherwise the controller — the tree's claim suspended, none the daemon
// supervises, or its root lingering on a status someone moved back into the workflow, which is
// nobody's parked tree, so #2163's relay sends nothing for it (record.RelayNotParked). Handed off
// again, again after a restart, and again after the notice went, it queues nothing more, and the
// notice names the decision's id, the agent's issue, role and session, the person, where and what
// they decided.
func TestADecisionGoesOnceToTheRunningArchitectElseToTheController(t *testing.T) {
	t.Parallel()
	const controller = "notifications.legion.legion.controller"
	for _, tc := range []struct {
		name      string
		architect supervise.ClaimState
		// child is the state of the child's own architect claim, "" for a parked child that has
		// none; a child with one is in the workflow, at testing.
		child   supervise.ClaimState
		lingers bool
		to      string
	}{
		{name: "the tree's architect holds its role", architect: supervise.StateWorking, to: "LEGION-1"},
		{name: "the child's own architect holds its role", architect: supervise.StateWorking, child: supervise.StateWorking, to: "LEGION-4"},
		{name: "the child's own architect holds its role and the tree's is suspended", architect: supervise.StateSuspended, child: supervise.StateWorking, to: "LEGION-4"},
		{name: "the child's own architect is suspended and the tree's holds its role", architect: supervise.StateWorking, child: supervise.StateSuspended, to: "LEGION-1"},
		{name: "the tree's architect is suspended", architect: supervise.StateSuspended},
		{name: "no architect of the tree is supervised"},
		{name: "the root lingers on a status someone moved back into the workflow", architect: supervise.StateWorking, lingers: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			noticeTree(t, pool, records, tc.lingers)
			root := "LEGION-1"
			child := record.Issue{Key: "LEGION-4", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
				Tree: root, Parent: &root, Title: "Parked child", Phase: phase.Done, Generation: 1, Status: "backlog"}
			if tc.child != "" {
				child.Title, child.Phase, child.Status = "Child at testing", phase.Testing, "testing"
			}
			putOutboxIssue(t, pool, records, child)
			if tc.lingers {
				movedBackIntoTheWorkflow(t, pool, records, root)
			}
			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			if tc.architect != "" {
				architectClaim(t, sup, root, tc.architect)
			}
			if tc.child != "" {
				architectClaim(t, sup, child.Key, tc.child)
			}
			publisher := &holderPublisher{}
			now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
			// A restart builds the outbox and the decisions anew over the same database.
			boot := func() routedDecisions {
				runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup,
					project: "legion", now: func() time.Time { return now }}
				return routedDecisions{outbox: runner, now: func() time.Time { return now }, log: quietLogger()}
			}
			decision := answerloop.Decision{ID: "dispatch:34", Asker: "user:person@example.com", Ref: "dispatch://LEGION-4/ask/a-1", Text: "Selected: B\n\nUse B.",
				AskIssue:     "LEGION-4",
				Conversation: store.AgentConversation{Session: "ses_tester", Project: "legion", Issue: "LEGION-4", Role: claim.RoleTester}}
			decide := func(d routedDecisions) {
				t.Helper()
				if err := d.Decide(ctx, decision); err != nil {
					t.Fatal(err)
				}
			}
			first := boot()
			decide(first)
			decide(first)
			restarted := boot()
			decide(restarted)
			if rows := outboxRows(t, pool); rows != 1 {
				t.Fatalf("outbox rows after the decision was handed off three times = %d, want one", rows)
			}
			if err := restarted.outbox.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			decide(restarted)
			if err := restarted.outbox.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			_, delivered := publisher.snapshot()
			if len(delivered) != 1 || outboxRows(t, pool) != 0 {
				t.Fatalf("publishes = %+v with %d rows left; want one and none", delivered, outboxRows(t, pool))
			}
			want := controller
			if tc.to != "" {
				want = architectTopic(t, tc.to)
			}
			if delivered[0].topic != want || delivered[0].message != "agent-decision on LEGION-4" {
				t.Fatalf("published to %q as %q; want %q, as an agent-decision on the agent's issue", delivered[0].topic, delivered[0].message, want)
			}
			body, err := json.Marshal(delivered[0].payload)
			if err != nil {
				t.Fatal(err)
			}
			wantBody := `{"kind":"agent-decision","role":"tester","agent_decision":{"id":"dispatch:34","issue":"LEGION-4","session":"ses_tester",` +
				`"asker":"user:person@example.com","ref":"dispatch://LEGION-4/ask/a-1","text":"Selected: B\n\nUse B."}}`
			if string(body) != wantBody {
				t.Fatalf("notice body = %s, want %s", body, wantBody)
			}
		})
	}
}

// movedBackIntoTheWorkflow puts the lingering root back at in_progress, as a person moving it out
// of the status its close left it in does (admission's releaseUnslotted leaves one at in_progress
// too): it still lingers, but it is nobody's parked tree, so the parked-decision relay sends
// nothing for a decision on it (record.RelayNotParked, record.ParkedTree).
func movedBackIntoTheWorkflow(t *testing.T, pool *store.Pool, records record.Store, key string) {
	t.Helper()
	ctx := context.Background()
	var root *record.Issue
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		root, err = records.Issue(ctx, tx, key)
		return err
	}); err != nil || root == nil {
		t.Fatalf("read the lingering root %s = %#v, %v", key, root, err)
	}
	if !root.Lingers() {
		t.Fatalf("the root %s = %#v, want it lingering", key, root)
	}
	root.Phase, root.Status = phase.Implementing, "in_progress"
	putOutboxIssue(t, pool, records, *root)
}

// A person's decision on an ask of a child of a tree whose root has left the workflow is the
// parked-decision relay's alone (sjawhar/legion#2163): the relay tells the controller in its own
// parked-decision notice and keeps the decision for the root's next start, and the answer loop
// queues no agent-decision beside it, however often the hand-off arrives and across a restart. The
// loop still records the decision passed, so a redelivery after the relay's notice went queues
// nothing either.
func TestADecisionOnAParkedTreeIsLeftToTheRelayThatOwnsIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, true)
	root, child := "LEGION-1", "LEGION-4"
	putOutboxIssue(t, pool, records, record.Issue{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: root, Parent: &root, Title: "Parked child", Phase: phase.Done, Generation: 1, Status: "backlog"})
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, root, supervise.StateWorking)
	publisher := &holderPublisher{}
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	// A restart builds the outbox and the decisions anew over the same database.
	boot := func() routedDecisions {
		runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup,
			project: "legion", now: func() time.Time { return now }}
		return routedDecisions{outbox: runner, now: func() time.Time { return now }, log: quietLogger()}
	}
	// #2163's relay: the person's reply hands the stopped tester's ask back to it, and nobody in
	// the parked tree hears it. The same Dispatch event again writes nothing more (ApplyFact).
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories,
		Clock: func() time.Time { return now }}, quietLogger())
	for range 2 {
		result, err := intake.ApplyFact(ctx, pool, "dispatch", "dispatch-41", intake.DispatchAskReply{Key: child, Ask: "a-1", WaitingOn: "agent",
			Actor: dispatch.Actor{Kind: "user", ID: "person@example.com"}, Mark: record.EventMark{Seq: 7, At: now}, Comment: "c-1"}, engine)
		if err != nil || result.Refusal != nil {
			t.Fatalf("apply the person's reply = %#v, %v", result, err)
		}
	}
	decision := answerloop.Decision{ID: "dispatch:c-1", Asker: "user:person@example.com", Ref: "dispatch://LEGION-4/comment/c-1", Text: "Use B.",
		AskIssue:     child,
		Conversation: store.AgentConversation{Session: "ses_tester", Project: "legion", Issue: child, Role: claim.RoleTester}}
	decide := func(d routedDecisions) {
		t.Helper()
		if err := d.Decide(ctx, decision); err != nil {
			t.Fatal(err)
		}
	}
	first := boot()
	decide(first)
	decide(first)
	restarted := boot()
	decide(restarted)
	if rows := outboxRows(t, pool); rows != 1 {
		t.Fatalf("outbox rows after the decision was handed off three times = %d, want the relay's one", rows)
	}
	if err := restarted.outbox.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	decide(restarted)
	if err := restarted.outbox.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	_, delivered := publisher.snapshot()
	if len(delivered) != 1 || outboxRows(t, pool) != 0 {
		t.Fatalf("publishes = %+v with %d rows left; want the relay's one and none", delivered, outboxRows(t, pool))
	}
	if delivered[0].topic != "notifications.legion.legion.controller" || delivered[0].message != "parked-decision on LEGION-4" {
		t.Fatalf("published to %q as %q; want the relay's parked-decision notice on the controller topic",
			delivered[0].topic, delivered[0].message)
	}
	body, err := json.Marshal(delivered[0].payload)
	if err != nil {
		t.Fatal(err)
	}
	wantBody := `{"kind":"parked-decision","actor":"user:person@example.com","ref":"dispatch://LEGION-4/comment/c-1",` +
		`"decision":"ask-reply","root":"LEGION-1"}`
	if string(body) != wantBody {
		t.Fatalf("notice body = %s, want the relay's %s", body, wantBody)
	}
	if kept := keptDecisions(t, pool, records, root); len(kept) != 1 || kept[0].Ref != decision.Ref {
		t.Fatalf("kept %#v, want the relay's one decision kept for the root's next start", kept)
	}
}

// A person's answer of an ask the relay does not answer for — a legacy `action` ask, whose kind
// is neither a question nor an approval request (record.RelaysAskAnswer) — reaches nobody through
// the relay, parked tree or not. So the loop passes it on itself, as it does on a tree that runs:
// one agent-decision notice to the controller, no parked-decision notice, and nothing more
// however often the hand-off arrives or the daemon restarts.
func TestADecisionTheRelayLeavesAloneOnAParkedTreeGoesOnceToTheController(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, true)
	root, child := "LEGION-1", "LEGION-4"
	putOutboxIssue(t, pool, records, record.Issue{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: root, Parent: &root, Title: "Parked child", Phase: phase.Done, Generation: 1, Status: "backlog"})
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, root, supervise.StateWorking)
	publisher := &holderPublisher{}
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	boot := func() routedDecisions {
		runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup,
			project: "legion", now: func() time.Time { return now }}
		return routedDecisions{outbox: runner, now: func() time.Time { return now }, log: quietLogger()}
	}
	// The relay reads the person's answer and sends nothing: the mirror keeps no kind for an ask
	// whose own is one this daemon does not know (intake's askKind), and the relay answers for a
	// question alone.
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories,
		Clock: func() time.Time { return now }}, quietLogger())
	if result, err := intake.ApplyFact(ctx, pool, "dispatch", "dispatch-42", intake.DispatchAskClosed{Key: child, Ask: "a-2", Kind: "",
		State: "answered", Actor: dispatch.Actor{Kind: "user", ID: "person@example.com"}, Mark: record.EventMark{Seq: 7, At: now}}, engine); err != nil || result.Refusal != nil {
		t.Fatalf("apply the person's answer = %#v, %v", result, err)
	}
	if got := parkedDecisions(t, pool); len(got) != 0 {
		t.Fatalf("parked-decision notices = %#v, want none: the relay does not answer for this ask's kind", got)
	}
	// The loop carries the kind the Dispatch event itself named, which is no question either.
	decision := answerloop.Decision{ID: "dispatch:42", Asker: "user:person@example.com", Ref: "dispatch://LEGION-4/ask/a-2", Text: "Use B.",
		Answered: true, AskKind: "action", AskIssue: child,
		Conversation: store.AgentConversation{Session: "ses_tester", Project: "legion", Issue: child, Role: claim.RoleTester}}
	decide := func(d routedDecisions) {
		t.Helper()
		if err := d.Decide(ctx, decision); err != nil {
			t.Fatal(err)
		}
	}
	first := boot()
	decide(first)
	decide(first)
	restarted := boot()
	decide(restarted)
	if rows := outboxRows(t, pool); rows != 1 {
		t.Fatalf("outbox rows after the decision was handed off three times = %d, want the loop's one", rows)
	}
	if err := restarted.outbox.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	decide(restarted)
	if err := restarted.outbox.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	_, delivered := publisher.snapshot()
	if len(delivered) != 1 || outboxRows(t, pool) != 0 {
		t.Fatalf("publishes = %+v with %d rows left; want the loop's one and none", delivered, outboxRows(t, pool))
	}
	if delivered[0].topic != "notifications.legion.legion.controller" || delivered[0].message != "agent-decision on LEGION-4" {
		t.Fatalf("published to %q as %q; want the loop's agent-decision notice on the controller topic",
			delivered[0].topic, delivered[0].message)
	}
}

// The loop posts its answer on an ask's thread in the stopped agent's name, which leaves the ask
// with the person as the agent's own hand-back would. That post is nobody's act on the ask: the
// decoder reads no fact from a comment carrying the loop's marker (intake.AnswerMarker), so an
// admitted root whose tree's question the loop answered declares no hand-back and stays where it
// is. The same reply without the marker is the tree's hand-back, which declares the root and moves
// it to awaiting_person at the next pass. Either way the root keeps its slot, since a root waiting
// on a person holds one (record.KeepsSlot).
func TestTheLoopsOwnAnswerOnAnAskLeavesTheAdmittedRootItsSlot(t *testing.T) {
	t.Parallel()
	const root, ask, session = "LEGION-700", "ask-q", "ses-architect"
	for _, tc := range []struct {
		name      string
		body      string
		handsBack bool
	}{
		{name: "the loop's answer in the agent's name", body: "Because B.\n\n" + intake.AnswerMarker + "dispatch:c-1 -->"},
		{name: "the agent's own hand-back", body: "Over to you: B?", handsBack: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
			putOutboxIssue(t, pool, records, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Project: "LEGION", Tree: root, Title: "Root", Phase: phase.Admitted, Generation: 1, Status: "in_progress", Rank: "A", HandedOver: true})
			if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
				if err := records.PutSlot(ctx, tx, record.Slot{Issue: root, Index: 0, AdmittedAt: now}); err != nil {
					return err
				}
				// The architect's question stands on the agent's turn: a person replied on it, and
				// that reply is what the stopped agent's loop answers. Nothing is with the person,
				// so the root waits on nobody and holds the slot its spec is written in.
				return records.PutRootAsk(ctx, tx, record.RootAsk{Issue: root, Ask: ask, Kind: record.AskQuestion, Open: true,
					Seq: 11, WaitingOn: "agent", WaitingOnSeq: 11, ByTree: true})
			}); err != nil {
				t.Fatalf("seed the root's slot and its question: %v", err)
			}
			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			trees := outboxTreeStore(t, pool, root)
			architect := launchedClaim(t, sup, root, root, claim.RoleArchitect)
			machine, ok := sup.Machine(architect)
			if !ok {
				t.Fatalf("no machine for %s", architect)
			}
			if err := trees.PutClaim(ctx, machine.Claim()); err != nil {
				t.Fatalf("persist the architect's claim: %v", err)
			}
			engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories}, quietLogger())
			admission := admit.New(records, engine, 1, "LEGION", quietLogger())
			apply := func(id string, fact intake.Fact) {
				t.Helper()
				if result, err := intake.ApplyFact(ctx, pool, "test", id, fact, engine, admission); err != nil || result.Refusal != nil {
					t.Fatalf("apply %s = %#v, %v", id, result, err)
				}
			}

			event, err := json.Marshal(map[string]any{
				"id": 12, "issue_key": root, "seq": 12, "type": "comment.created", "notify": true, "created_at": now,
				"actor":   map[string]any{"kind": "session", "id": session},
				"payload": map[string]any{"id": "c-1", "ask_id": ask, "ask_state": "open", "ask_waiting_on": "human", "body": tc.body},
			})
			if err != nil {
				t.Fatal(err)
			}
			fact, unread, err := intake.DecodeDispatchEvent("LEGION", root, event)
			if err != nil || len(unread) != 0 {
				t.Fatalf("decode the reply = %#v, unread %q, %v", fact, unread, err)
			}
			if (fact != nil) != tc.handsBack {
				t.Fatalf("the reply decoded to %#v, want a fact: %t", fact, tc.handsBack)
			}
			if fact != nil {
				apply("agent-reply", fact)
			}
			apply("tick", intake.ControllerTick{})

			var after *record.Issue
			if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				after, err = records.Issue(ctx, tx, root)
				return err
			}); err != nil || after == nil {
				t.Fatalf("read the root = %#v, %v", after, err)
			}
			var slotted bool
			if err := pool.QueryRow(ctx, "select exists(select 1 from slots where issue = $1)", root).Scan(&slotted); err != nil {
				t.Fatalf("read the root's slot: %v", err)
			}
			if tc.handsBack {
				if after.DeclaredSeq != 12 || !after.AwaitingPerson() || !slotted {
					t.Fatalf("root after the agent's hand-back = %#v, slotted %t; want it declared at 12, awaiting its person and still slotted", after, slotted)
				}
				return
			}
			if after.DeclaredSeq != 0 || after.Awaiting != nil || !slotted {
				t.Fatalf("root after the loop's own answer = %#v, slotted %t; want it undeclared, waiting on nobody and still slotted", after, slotted)
			}
		})
	}
}

// `legion state` lists a question passed to the controller for seven days after it was passed,
// and not after, newest first, with who asked and where as its hand-off says; a question never
// passed on is not listed.
func TestQuestionsForControllerListsAPassedQuestionForSevenDays(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := testpg.CloneStore(t, "legion_daemon_passed_questions")
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	passOn := controllerPassOn{pool: st.Pool(), records: record.NewStore(), now: func() time.Time { return now }, log: quietLogger()}
	question := func(id, event string) store.AnswerQuestion {
		t.Helper()
		handoff := intake.Handoff{
			OriginalTopic: "notifications.agent.ses_tester", TargetSession: "ses_tester", RoleToken: string(gateToken), Project: "legion",
			Envelope: intake.HandoffEnvelope{EventID: event, Source: "agent", SourceEventID: "s", SourceSession: "ses_asker",
				Topic: "notifications.agent.ses_tester", DedupeKey: "agent.ses_tester." + id, PayloadSummary: "Did it pass?", TraceID: "t",
				Sender: &intake.HandoffSender{SessionID: "ses_asker", Title: "LEGION-9 implementer"}},
		}
		raw, err := json.Marshal(handoff)
		if err != nil {
			t.Fatal(err)
		}
		q, _, err := st.ReceiveQuestion(ctx, store.AnswerQuestion{
			ID: id, Project: "legion", Identity: handoff.Envelope.DedupeKey, Session: "ses_tester", Token: gateToken, Issue: "LEGION-9",
			Role: claim.RoleTester, Kind: "envoy", MayAskNothing: true, Handoff: raw, ReceivedAt: now,
		})
		if err != nil {
			t.Fatal(err)
		}
		return q
	}
	pass := func(q store.AnswerQuestion, reason answerloop.Reason, at time.Time) {
		t.Helper()
		q.PassReason = string(reason)
		q.State, q.UpdatedAt = store.QuestionAppended, at
		if err := st.UpdateQuestion(ctx, q, store.QuestionReceived); err != nil {
			t.Fatal(err)
		}
		if err := passOn.PassOn(ctx, q, reason, answerloop.Origin{Asker: "session:ses_asker (LEGION-9 implementer)", Ref: "envoy:evt"}); err != nil {
			t.Fatal(err)
		}
		q.State = store.QuestionPassedOn
		if err := st.UpdateQuestion(ctx, q, store.QuestionAppended); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Pool().Exec(ctx, "update processed_events set processed_at = $3 where source = $1 and event_id = $2",
			store.QuestionPassedSource, q.ID, at); err != nil {
			t.Fatal(err)
		}
	}
	older, newer := now.Add(-7*24*time.Hour+time.Minute), now.Add(-time.Hour)
	pass(question("q-older", "evt-older"), answerloop.ReasonNeedsWork, older)
	pass(question("q-newer", "evt-newer"), answerloop.Reason("answer_seconds"), newer)
	question("q-answered", "evt-answered")

	listed := func(at time.Time) []api.QuestionForController {
		t.Helper()
		var questions []api.QuestionForController
		if err := store.Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			var err error
			questions, err = questionsForController(ctx, tx, st, "legion", at)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return questions
	}
	entry := func(id, event, reason string, at time.Time) api.QuestionForController {
		return api.QuestionForController{ID: id, Issue: "LEGION-9", Role: claim.RoleTester, Session: "ses_tester",
			Asker: "session:ses_asker (LEGION-9 implementer)", Ref: "envoy:" + event, Reason: reason, PassedAt: at}
	}
	newerEntry, olderEntry := entry("q-newer", "evt-newer", "answer_seconds", newer), entry("q-older", "evt-older", "needs_work", older)
	if got := listed(now); !reflect.DeepEqual(got, []api.QuestionForController{newerEntry, olderEntry}) {
		t.Fatalf("questions for the controller = %+v, want the newer and then the older", got)
	}
	// Seven days and a second after the older was passed it is no longer listed.
	later := older.Add(api.QuestionsForControllerWindow + time.Second)
	if got := listed(later); !reflect.DeepEqual(got, []api.QuestionForController{newerEntry}) {
		t.Fatalf("questions for the controller seven days after the older was passed = %+v, want the newer alone", got)
	}
}

// recordingTool is the gh tool of one answer as ghtool.Run records its calls (answer_tool_calls):
// each call started under the routed owner before it runs and finished with its output and exit
// after, here with gh's output for a merge or a read and exit 0.
type recordingTool struct {
	st       *store.Store
	question string
	now      time.Time
}

func (r recordingTool) Run(ctx context.Context, call answer.GHCall) ghtool.Result {
	var argv []string
	if err := json.Unmarshal([]byte(call.Args), &argv); err != nil {
		return ghtool.Result{Output: err.Error(), Exit: -1}
	}
	if err := r.st.StartAnswerToolCall(ctx, store.AnswerToolCall{Question: r.question, Seq: call.Seq, Tool: store.ToolGH, Args: argv, App: "implement", Owner: "acme", StartedAt: r.now}); err != nil {
		return ghtool.Result{Output: err.Error(), Exit: -1}
	}
	output := "title:\tFix the leak\nstate:\tMERGED"
	if len(argv) > 1 && argv[1] == "merge" {
		output = "✓ Squashed and merged pull request acme/widgets#7 (Fix the leak)"
	}
	exit := 0
	if err := r.st.FinishAnswerToolCall(ctx, r.question, call.Seq, output, &exit, r.now); err != nil {
		return ghtool.Result{Output: err.Error(), Exit: -1}
	}
	return ghtool.Result{Output: output, Exit: exit, Ran: true}
}

func (recordingTool) Lookup(context.Context, int) (ghtool.Found, error) {
	return ghtool.Found{}, ghtool.ErrNotAWrite
}

func (recordingTool) Close() error { return nil }

type recordingTools struct {
	st  *store.Store
	now time.Time
}

func (r recordingTools) Open(_ context.Context, _ ghtool.Agent, question string) (answerloop.Tool, error) {
	return recordingTool{st: r.st, question: question, now: r.now}, nil
}

type issueFacts answerloop.IssueFacts

func (f issueFacts) Facts(context.Context, string) (answerloop.IssueFacts, error) {
	return answerloop.IssueFacts(f), nil
}

// anthropicGHCall is the Messages API's event stream of a reply that calls the gh tool with args
// and stops for it to run, with the usage a metered call reports.
func anthropicGHCall(id string, args ...string) []byte {
	var b bytes.Buffer
	event := func(name string, v any) {
		data, err := json.Marshal(v)
		if err != nil {
			panic(err)
		}
		fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", name, data)
	}
	usage := map[string]any{"input_tokens": 40, "output_tokens": 12, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0}
	input, err := json.Marshal(map[string]any{"args": args})
	if err != nil {
		panic(err)
	}
	event("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_" + id, "type": "message", "role": "assistant", "model": "claude-opus-5-5", "content": []any{},
		"stop_reason": nil, "stop_sequence": nil, "usage": usage,
	}})
	event("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": id, "name": "gh", "input": map[string]any{}}})
	event("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(input)}})
	event("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	event("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use", "stop_sequence": nil}, "usage": usage})
	event("message_stop", map[string]any{"type": "message_stop"})
	return b.Bytes()
}

// A gh write the stopped agent's answer made before a fault is in every record the agent, the
// asker and the controller read, through the real loop, the real answer on a stand-in model, the
// store's record of the calls and the controller's pass: the tester's answer merges the pull
// request with gh, then still calls gh on the last of its two model calls, which is past
// model_calls. Its conversation holds the question, the merge and its result, the read after it,
// and the daemon's note naming the merge, in that order; the reply posted in its name names the
// merge by its command, repository and exit, and no read; and the controller's agent-question
// notice and the questions `legion state` lists name the merge alone.
func TestAGhWriteBeforeAFaultIsInTheConversationTheReplyAndTheControllersRecords(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := testpg.CloneStore(t, "legion_daemon_fault_writes")
	path := filepath.Join(t.TempDir(), "ses_tester.jsonl")
	session := `{"type":"session","version":3,"id":"s","timestamp":"2026-10-26T00:00:00.000Z","cwd":"/work"}` + "\n" +
		`{"id":"e0000001","parentId":null,"timestamp":"2026-10-26T00:00:00.000Z","type":"model_change","model":"anthropic/claude-opus-5-5"}` + "\n" +
		`{"id":"e0000002","parentId":"e0000001","timestamp":"2026-10-26T00:00:00.000Z","type":"message","message":{"role":"user","content":[{"type":"text","text":"Test it."}],"attribution":"user","timestamp":1}}` + "\n" +
		`{"id":"e0000003","parentId":"e0000002","timestamp":"2026-10-26T00:00:00.000Z","type":"message","message":{"role":"assistant","content":[{"type":"text","text":"Tested."}],"api":"anthropic-messages","provider":"anthropic","model":"claude-opus-5-5","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"stopReason":"stop","timestamp":2}}` + "\n"
	if err := os.WriteFile(path, []byte(session), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	if _, err := st.RegisterConversation(ctx, store.AgentConversation{
		Session: "ses_tester", Project: "legion", Issue: "LEGION-9", Role: claim.RoleTester, Token: gateToken,
		Harness: runtime.BackendOmp, Location: path, StartedAt: now, RecordedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	merge := []string{"pr", "merge", "7", "-R", "acme/widgets", "--squash"}
	var mu sync.Mutex
	replies := [][]byte{anthropicGHCall("toolu_1", merge...), anthropicGHCall("toolu_2", "pr", "view", "7", "-R", "acme/widgets")}
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if len(replies) == 0 {
			http.Error(w, `{"type":"error","error":{"type":"invalid_request_error","message":"no more replies"}}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(replies[0])
		replies = replies[1:]
	}))
	defer model.Close()
	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key, []byte("sk-ant-standin\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var sends []string
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Path == "/v1/messages/send" {
			mu.Lock()
			sends = append(sends, fmt.Sprint(body["message"]))
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer listener.Close()
	dispatchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusCreated) }))
	defer dispatchServer.Close()

	records := record.NewStore()
	gate, _, _ := newTestGate(t)
	loop := answerloop.New(answerloop.Config{
		Project: "legion",
		Answers: config.Answers{
			Providers: map[config.AnswerProvider]config.AnswerEndpoint{config.AnswerAnthropic: {BaseURL: model.URL, Credential: config.AnswerCredential{File: key}}},
			Prices:    map[string]config.AnswerPrice{"anthropic/claude-opus-5-5": {Input: 5, Output: 25, CacheWrite: 6.25, CacheRead: 0.5}},
			Bounds: config.AnswerBounds{Answer: time.Minute, ModelCalls: 2, OutputTokens: 1000, GHCall: 30 * time.Second, GHOutputChars: 60000,
				GHCalls: 20, Concurrent: 1, DailyUSD: 50},
		},
		Store: st, Dispatch: dispatch.New(dispatchServer.URL, dispatch.BearerTransport("t", nil)), Envoy: notify.New(listener.URL, "t"), Gate: gate,
		Issues: issueFacts{Recorded: true, DispatchStatus: "in_progress", Phase: "testing", Repository: ghrepo.MustParse("acme/widgets")},
		Tools:  recordingTools{st: st, now: now}, Conversations: answerloop.Harnesses{},
		PassOn:    controllerPassOn{pool: st.Pool(), records: records, now: func() time.Time { return now }, log: quietLogger()},
		Decisions: routedDecisions{},
		Answer:    answer.Answer, Now: func() time.Time { return now }, Log: quietLogger(),
	})
	handoff := intake.Handoff{
		OriginalTopic: "notifications.agent.ses_tester", TargetSession: "ses_tester", RoleToken: string(gateToken), Project: "legion",
		Envelope: intake.HandoffEnvelope{EventID: "evt-1", Source: "agent", SourceEventID: "s", SourceSession: "ses_asker",
			Topic: "notifications.agent.ses_tester", DedupeKey: "agent.ses_tester.0123456789abcdef0123456789abcdef", PayloadSummary: "Is PR 7 merged?", TraceID: "t"},
	}
	raw, _ := json.Marshal(handoff)
	if err := loop.Receive(ctx, handoff, raw); err != nil {
		t.Fatal(err)
	}
	running, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { loop.Run(running); close(done) }()
	testwait.Eventually(t, "the question passed to the controller", func() bool {
		var passed bool
		err := st.Pool().QueryRow(ctx, "select count(*) = 1 from answer_questions where state = $1", string(store.QuestionPassedOn)).Scan(&passed)
		return err == nil && passed
	})
	stop()
	<-done

	// The conversation: the merge, its result and then the daemon's note naming it.
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(merge)
	written := string(content[len(session):])
	call := strings.Index(written, `"args":`+string(args))
	result := strings.Index(written, "Squashed and merged pull request acme/widgets#7")
	note := strings.LastIndex(written, "posted this reply in your name where the question was asked:\\n\\nI could not answer within the bound of 2 model calls. "+
		"I had already run these gh writes: `gh pr merge` on acme/widgets (exit 0).")
	if call < 0 || result < call || note < result {
		t.Errorf("the conversation's append holds no merge, its result and then the daemon's note naming it, in that order (at %d, %d, %d):\n%s", call, result, note, written)
	}

	// The reply posted in the agent's name.
	const reply = "I could not answer within the bound of 2 model calls. I had already run these gh writes: `gh pr merge` on acme/widgets (exit 0). " +
		"I passed your question to the Legion controller."
	mu.Lock()
	posted := append([]string(nil), sends...)
	mu.Unlock()
	if len(posted) != 1 || !strings.HasPrefix(posted[0], reply+"\n\n"+answerloop.AnswerMarker) {
		t.Errorf("replies posted = %q; want one naming the merge and no read: %q", posted, reply)
	}

	// The controller's agent-question notice.
	publisher := &outboxPublisher{}
	runner := &outbox{pool: st.Pool(), project: "legion", dispatchProject: "LEGION", records: records, notices: publisher, now: func() time.Time { return now }, log: quietLogger()}
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	published := publisher.publishes()
	if len(published) != 1 {
		t.Fatalf("publishes = %+v; want the one agent-question notice", published)
	}
	body, err := json.Marshal(published[0].payload)
	if err != nil {
		t.Fatal(err)
	}
	if want := `"reason":"model_calls","question":{`; !strings.Contains(string(body), want) ||
		!strings.HasSuffix(string(body), `"writes":[{"seq":1,"command":"pr merge","repository":"acme/widgets","exit":0}]}}`) {
		t.Errorf("notice body = %s; want the model_calls question naming the merge alone", body)
	}

	// The questions `legion state` lists.
	var questions []api.QuestionForController
	if err := store.Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		questions, err = questionsForController(ctx, tx, st, "legion", time.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	zero := 0
	if len(questions) != 1 || questions[0].Reason != "model_calls" ||
		!reflect.DeepEqual(questions[0].Writes, []record.AgentQuestionWrite{{Seq: 1, Command: "pr merge", Repository: "acme/widgets", Exit: &zero}}) {
		t.Errorf("questions for the controller = %+v; want the model_calls question naming the merge alone", questions)
	}
}

// The facts the answer loop reads of an agent's issue carry, by role, the commit each role's
// phases row records it completed its round against (record.PhaseRow.HandoffCommit): the head the
// gh tool names in a stopped agent's review footer (ghtool.Agent.Head). A role whose row records
// no commit is absent, so its agent's tool holds no head and submits no review.
func TestRecordFactsCarryEachRolesHandoffCommit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	const key, reviewed = "LEGION-5", "8848896da05779c592221b1199ee55ddb11cc3f3"
	putOutboxIssue(t, pool, records, record.Issue{Key: key, Tree: key, Project: "LEGION", Title: "Reviewed", Repo: "acme/widgets", DefaultBranch: "main",
		GitHubRepo: "acme/widgets", Phase: phase.Reviewing, Generation: 1, Status: "in_progress"})
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := records.PutPhase(ctx, tx, record.PhaseRow{Issue: key, Role: claim.RoleReviewer, Claim: "legion-5-reviewer", HandoffCommit: reviewed}); err != nil {
			return err
		}
		return records.PutPhase(ctx, tx, record.PhaseRow{Issue: key, Role: claim.RoleTester, Claim: "legion-5-tester"})
	}); err != nil {
		t.Fatal(err)
	}
	facts, err := recordFacts{&workflowRuntime{pool: pool, records: records, log: quietLogger()}}.Facts(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[claim.Role]string{claim.RoleReviewer: reviewed}; !facts.Recorded || !reflect.DeepEqual(facts.HandoffCommits, want) {
		t.Fatalf("the facts of %s = %+v; want it recorded, with the reviewer's handoff commit alone, %v", key, facts, want)
	}
}
