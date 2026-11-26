package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/answer"
	"github.com/sjawhar/legion/daemon/internal/answerloop"
	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/conversation"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The A24 surface of dispatch://INFRA-204 (spec v26 to v28): the answer loop answers for an agent
// whose claim the supervise machine suspended by idle release, and gives it everything else
// through one agent_message row that wakes it. Each test drives a real machine to the suspension
// and the loop through the daemon's own answer gate, so each fails with answerGate's rule of
// 043f9c67 restored, which counted a released claim as running (`view.Released() ||` in
// runningLocked): the loop then forwards the question and wakes nobody.

const (
	releasedIssue   = "LEGION-208"
	releasedSession = "ses-implementer"
	releasedAsk     = "a0000000-0000-4000-8000-000000000208"
)

// releasedPerson is the person who writes to the released agent.
var releasedPerson = intake.DispatchActor{Kind: "user", ID: "person@example.com"}

// releasedRig is LEGION-208 in implementing, its implementer started, registered and ready on a
// session whose Oh My Pi conversation the daemon's history holds, then stopped by stop, beside an
// idle architect that holds its role; the daemon's answer gate over the implementer's machine; and
// the answer loop over the outbox's own decisions, pass-ons and wake rows, a Dispatch that takes
// every post and says the implementer opened the ask, and a listener that takes every send. The
// machines and the loop read one pause flag, paused, the open pause's id (0 while the project
// runs), as the daemon's read the pause service's.
type releasedRig struct {
	t         *testing.T
	ctx       context.Context
	pool      *store.Pool
	st        *store.Store
	rt        *fake.Runtime
	runner    *outbox
	publisher *holderPublisher
	machine   *supervise.Machine
	token     claim.Token
	path      string
	// before is the conversation as the stop left it, and launches the runtime's launches then.
	before   string
	launches int
	cfg      answerloop.Config
	loop     *answerloop.Loop
	paused   *atomic.Int64

	mu sync.Mutex
	// posts are Dispatch's posts and sends the listener's requests, each its path and body.
	posts, sends []string
	// answer is the model's reply to each question; during runs inside each answer.
	answer func(req answer.Request) answer.Reply
	during func()
}

// stopBy is how a rig's implementer is stopped: the idle release of its issue pod, a project's
// pause, which suspends it the same way, or the workflow's suspension of a parked member.
type stopBy string

const (
	stopByIdleRelease stopBy = "idle release"
	stopByPause       stopBy = "a pause"
	stopByWorkflow    stopBy = "the workflow"
)

func newReleasedRig(t *testing.T, stop stopBy) *releasedRig {
	t.Helper()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: releasedIssue, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: releasedIssue, Title: "Workflow", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 5}
	putOutboxIssue(t, pool, records, issue)
	holdSlot(t, pool, releasedIssue)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	// The machines read the project's pause flag (supervise.Deps.Paused), which a machine takes
	// where it is built, so it goes in before the start: a pause's stop names the open pause. The
	// loop reads the same flag (answerloop.Config.Paused), as the daemon's does.
	paused := &atomic.Int64{}
	sup.deps.Paused = func() (int64, bool) {
		id := paused.Load()
		return id, id != 0
	}
	st := outboxTreeStore(t, pool, releasedIssue)
	publisher := &holderPublisher{}
	runner := &outbox{
		dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup, trees: st, tokens: outboxTokens{}, project: "legion",
		stateDir: t.TempDir(), notices: publisher, now: time.Now, log: quietLogger(),
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/" + releasedIssue}, nil
		},
	}
	r := &releasedRig{t: t, ctx: ctx, pool: pool, st: st, rt: rt, runner: runner, publisher: publisher,
		token: mustClaimToken(t, releasedIssue, claim.RoleImplementer), paused: paused}
	r.answer = func(req answer.Request) answer.Reply {
		return releasedReply(req, answer.Answered, "Answered: "+req.Question.Text)
	}

	if _, _, err := sup.Create(ctx, supervise.Claim{Token: mustClaimToken(t, releasedIssue, claim.RoleArchitect), Project: "legion",
		Tree: releasedIssue, Issue: releasedIssue, Role: claim.RoleArchitect, Generation: 1, State: supervise.StateIdle, Session: "ses-architect"}, ""); err != nil {
		t.Fatalf("create the architect's claim: %v", err)
	}
	start := mustOutboxRow(t, releasedIssue, record.SuperviseRequest{Op: "start", Tree: releasedIssue, Role: claim.RoleImplementer,
		Task: "Implement it.", Generation: 1}, time.Now())
	start.ID = 57
	if err := runner.execute(ctx, start); err != nil {
		t.Fatalf("start the implementer: %v", err)
	}
	machine, found := sup.Machine(r.token)
	if !found {
		t.Fatal("the supervisor has no implementer claim")
	}
	r.machine = machine
	r.path = filepath.Join(t.TempDir(), releasedSession+".jsonl")
	r.before = `{"type":"session","version":3,"id":"s","timestamp":"2026-10-26T00:00:00.000Z","cwd":"/work"}` + "\n" +
		`{"id":"e0000001","parentId":null,"timestamp":"2026-10-26T00:00:00.000Z","type":"model_change","model":"anthropic/claude-opus-5-5"}` + "\n" +
		`{"id":"e0000002","parentId":"e0000001","timestamp":"2026-10-26T00:00:00.000Z","type":"message","message":{"role":"user","content":[{"type":"text","text":"Implement it."}],"attribution":"user","timestamp":1}}` + "\n" +
		`{"id":"e0000003","parentId":"e0000002","timestamp":"2026-10-26T00:00:00.000Z","type":"message","message":{"role":"assistant","content":[{"type":"text","text":"Implemented with A."}],"api":"anthropic-messages","provider":"anthropic","model":"claude-opus-5-5","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"stopReason":"stop","timestamp":2}}` + "\n"
	if err := os.WriteFile(r.path, []byte(r.before), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := machine.Handle(ctx, supervise.RequestRegister{Claim: r.token, Generation: 1, Session: releasedSession, SessionFile: r.path}); err != nil {
		t.Fatalf("register the implementer: %v", err)
	}
	if err := machine.Handle(ctx, supervise.RequestReady{Claim: r.token, Generation: 1, Session: releasedSession}); err != nil {
		t.Fatalf("ready the implementer: %v", err)
	}
	suspend := supervise.RequestSuspend{Claim: r.token, By: supervise.SuspendedByReleased, Reason: "the issue's idle pod is released"}
	switch stop {
	case stopByPause:
		pause := openLegionPause(t, pool, time.Now())
		paused.Store(pause)
		suspend.Pause, suspend.Reason = pause, "the project is paused"
	case stopByWorkflow:
		suspend.By, suspend.Reason = supervise.SuspendedByDaemon, "the member is parked"
	}
	if err := machine.Handle(ctx, suspend); err != nil {
		t.Fatalf("stop the implementer by %s: %v", stop, err)
	}
	if got := machine.Claim(); got.State != supervise.StateSuspended || got.Released() != (stop != stopByWorkflow) || got.Session != releasedSession {
		t.Fatalf("the implementer stopped by %s is %s by %q on %q, want suspended with its session, released %t", stop, got.State, got.SuspendedBy,
			got.Session, stop != stopByWorkflow)
	}
	r.launches = len(rt.CallsOf("Spawn")) + len(rt.CallsOf("Resume"))
	now := time.Now().UTC()
	if _, err := st.RegisterConversation(ctx, store.AgentConversation{Session: releasedSession, Project: "legion", Issue: releasedIssue,
		Role: claim.RoleImplementer, Token: r.token, Harness: runtime.BackendOmp, Location: r.path, StartedAt: now, RecordedAt: now}); err != nil {
		t.Fatal(err)
	}

	dispatchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/api/v1/asks/") {
			_, _ = w.Write([]byte(`{"ask":{"id":"` + releasedAsk + `","issue_key":"` + releasedIssue + `","author":{"kind":"session","id":"` +
				releasedSession + `"}},"replies":[]}`))
			return
		}
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.posts = append(r.posts, req.URL.Path+" "+string(body))
		r.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"reply"}`))
	}))
	t.Cleanup(dispatchServer.Close)
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.sends = append(r.sends, req.URL.Path+" "+string(body))
		r.mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(listener.Close)

	gate := newAnswerGate(&launchChecks{}, func(token claim.Token) (supervise.ClaimView, bool) {
		held, ok := sup.Machine(token)
		if !ok {
			return supervise.ClaimView{}, false
		}
		return held.View(), true
	}, quietLogger())
	r.cfg = answerloop.Config{
		Project: "legion",
		Answers: config.Answers{Bounds: config.AnswerBounds{Answer: time.Minute, ModelCalls: 1, OutputTokens: 100, Concurrent: 1, DailyUSD: 50}},
		Store:   st, Dispatch: dispatch.New(dispatchServer.URL, dispatch.BearerTransport("t", nil)), Envoy: notify.New(listener.URL, "t"), Gate: gate,
		Issues: noFacts{}, Tools: noTool{}, Conversations: answerloop.Harnesses{},
		PassOn:    controllerPassOn{pool: pool, records: records, now: time.Now, log: quietLogger()},
		Decisions: routedDecisions{outbox: runner, now: time.Now, log: quietLogger()},
		Wakes:     releasedWakes{outbox: runner, now: time.Now, log: quietLogger()},
		Answer: func(_ context.Context, req answer.Request) (answer.Reply, error) {
			r.mu.Lock()
			reply, during := r.answer, r.during
			r.mu.Unlock()
			if during != nil {
				during()
			}
			return reply(req), nil
		},
		Paused: func() bool { return paused.Load() != 0 },
		Now:    time.Now, Log: quietLogger(),
	}
	r.loop = answerloop.New(r.cfg)
	return r
}

// releasedReply is the model's reply of kind with text, and the exchange to append.
func releasedReply(req answer.Request, kind answer.Kind, text string) answer.Reply {
	return answer.Reply{Kind: kind, Text: text, Append: conversation.Append{
		QuestionID: req.Question.ID, Question: req.Question.Text, Provider: "anthropic", API: "anthropic-messages", Model: "claude-opus-5-5",
		Turns: []conversation.Turn{{Content: []conversation.Block{{Type: conversation.Text, Text: text}}}},
	}}
}

// handOff hands h to the loop as the hand-off consumer does.
func (r *releasedRig) handOff(h intake.Handoff) {
	r.t.Helper()
	raw, err := json.Marshal(h)
	if err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.loop.Receive(r.ctx, h, raw); err != nil {
		r.t.Fatalf("Receive: %v", err)
	}
}

// work runs the loop until it holds no open question: each is posted, passed on, woken, dropped or
// forwarded.
func (r *releasedRig) work() {
	r.t.Helper()
	running, stop := context.WithCancel(r.ctx)
	done := make(chan struct{})
	go func() { r.loop.Run(running); close(done) }()
	testwait.Eventually(r.t, "every question the loop holds done with", func() bool {
		open, err := r.st.OpenQuestions(r.ctx, "legion")
		return err == nil && len(open) == 0
	})
	stop()
	<-done
}

// states is the state of every question the loop holds.
func (r *releasedRig) states() []string {
	r.t.Helper()
	rows, err := r.pool.Query(r.ctx, `select state from answer_questions order by received_at`)
	if err != nil {
		r.t.Fatal(err)
	}
	states, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		r.t.Fatal(err)
	}
	return states
}

// agentMessages is every queued agent_message row's payload, in the order they were queued.
func (r *releasedRig) agentMessages() []record.AgentMessage {
	r.t.Helper()
	rows, err := r.pool.Query(r.ctx, `select payload from outbox where kind = $1 order by id`, string(record.OutboxKindAgentMessage))
	if err != nil {
		r.t.Fatal(err)
	}
	payloads, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		r.t.Fatal(err)
	}
	messages := make([]record.AgentMessage, len(payloads))
	for i, payload := range payloads {
		if err := json.Unmarshal(payload, &messages[i]); err != nil {
			r.t.Fatal(err)
		}
	}
	return messages
}

// rows is how many rows of kind LEGION-208 has queued.
func (r *releasedRig) rows(kind record.OutboxKind) int {
	return outboxRowsOfKind(r.t, r.pool, releasedIssue, kind)
}

// passedToController is what `legion state` lists as passed to the controller.
func (r *releasedRig) passedToController() []api.QuestionForController {
	r.t.Helper()
	var passed []api.QuestionForController
	if err := store.TransactWith(r.ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		passed, err = questionsForController(ctx, tx, r.st, "legion", time.Now())
		return err
	}); err != nil {
		r.t.Fatal(err)
	}
	return passed
}

// requests is what Dispatch was posted and what the listener was sent.
func (r *releasedRig) requests() (posts, sends []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.posts...), append([]string(nil), r.sends...)
}

// conversation is the implementer's session file now.
func (r *releasedRig) conversation() string {
	r.t.Helper()
	content, err := os.ReadFile(r.path)
	if err != nil {
		r.t.Fatal(err)
	}
	return string(content)
}

// assertStillReleased holds the machine to the stop: nothing launched since, and the claim
// released still.
func (r *releasedRig) assertStillReleased() {
	r.t.Helper()
	if launched := r.launched(); launched != 0 || !r.machine.Claim().Released() {
		r.t.Fatalf("the machine launched %d times and the implementer is %s by %q; want nothing launched and the claim released",
			launched, r.machine.Claim().State, r.machine.Claim().SuspendedBy)
	}
}

// releasedHandoff is a Dispatch event the listener hands the daemon for the implementer's session.
func releasedHandoff(event map[string]any) intake.Handoff {
	payload, _ := json.Marshal(event)
	topic := "notifications.agent." + releasedSession
	return intake.Handoff{
		OriginalTopic: topic, TargetSession: releasedSession, RoleToken: "legion-legion-legion-208-implementer", Project: "legion",
		Envelope: intake.HandoffEnvelope{EventID: "dispatch-1", Source: "dispatch", SourceEventID: "1", Topic: topic,
			DedupeKey: "dispatch-1", PayloadSummary: releasedIssue, Payload: string(payload), TraceID: "t"},
	}
}

// releasedMessage is a person's Dispatch message id to the implementer on LEGION-208.
func releasedMessage(id, body string) intake.Handoff {
	return releasedHandoff(map[string]any{
		"event": map[string]any{"type": "message.created", "actor": releasedPerson, "issue_key": releasedIssue, "project": "LEGION",
			"payload": map[string]any{"id": id, "issue_key": releasedIssue, "body": body, "target": "session:" + releasedSession}},
		"delivery": map[string]any{"attempt": 1, "mode": "btw"},
	})
}

// releasedComment is actor's root comment id on LEGION-208, which mentions nobody.
func releasedComment(actor intake.DispatchActor, id, body string) intake.Handoff {
	return releasedHandoff(map[string]any{"type": "comment.created", "actor": actor, "issue_key": releasedIssue, "project": "LEGION",
		"payload": map[string]any{"id": id, "issue_key": releasedIssue, "body": body}})
}

// releasedAnswer is the person's answer to the implementer's ask, selecting B, with text.
func releasedAnswer(text string) intake.Handoff {
	return releasedHandoff(map[string]any{"id": 34, "type": "ask.answered", "actor": releasedPerson, "issue_key": releasedIssue, "project": "LEGION",
		"payload": map[string]any{"id": releasedAsk, "issue_key": releasedIssue, "kind": "question", "state": "answered",
			"answer": map[string]any{"user": releasedPerson.ID, "selected": []string{"B"}, "text": text, "at": "2026-10-26T12:00:00Z"}}})
}

// A question for an agent the supervise machine suspended by idle release, and for one a pause
// suspended the same way, is answered from its conversation and posted where it was asked; the
// machine launches nothing, and no wake row is written. The control is the gate's rule of
// 043f9c67, `view.Released() ||` in runningLocked: the question is forwarded to the role topic and
// nothing is answered.
func TestIdleReleasedAgentsQuestionIsAnsweredFromItsConversationAndWakesNothing(t *testing.T) {
	t.Parallel()
	for _, stop := range []stopBy{stopByIdleRelease, stopByPause} {
		t.Run(string(stop), func(t *testing.T) {
			r := newReleasedRig(t, stop)
			r.handOff(releasedMessage("m-1", "Why A?"))
			r.work()
			posts, sends := r.requests()
			if states := r.states(); len(states) != 1 || states[0] != string(store.QuestionPosted) {
				t.Fatalf("questions = %v; want the one posted", states)
			}
			if len(posts) != 1 || !strings.HasPrefix(posts[0], "/api/v1/messages/m-1/reply ") || !strings.Contains(posts[0], "Answered: Why A?") {
				t.Fatalf("Dispatch posts = %q; want the answer to the message", posts)
			}
			if len(sends) != 0 {
				t.Fatalf("listener requests = %q; want none: nothing forwarded to the role topic", sends)
			}
			if !strings.Contains(r.conversation(), "Answered: Why A?") {
				t.Fatal("the implementer's conversation holds no exchange of the answer")
			}
			if rows := r.rows(record.OutboxKindAgentMessage); rows != 0 {
				t.Fatalf("agent_message rows = %d; want none: a question the loop answered wakes nobody", rows)
			}
			r.assertStillReleased()
		})
	}
}

// A person's answer to the released agent's ask that also asks is held until its question is
// done: the question is answered, appended to the conversation and posted on the ask's thread
// first, and then one wake row is written that carries the person's answer and says the question
// was answered and where; the architect and the controller are told nothing. The control is the
// same message for an agent the workflow suspended, which is not released: the decision goes to
// the architect as before, and no wake row is written.
func TestIdleReleasedAgentsDecisionThatAlsoAsksWakesItOnceItsAnswerIsIn(t *testing.T) {
	t.Parallel()
	for _, stop := range []stopBy{stopByIdleRelease, stopByWorkflow} {
		t.Run(string(stop), func(t *testing.T) {
			r := newReleasedRig(t, stop)
			rowsWhileAnswering := -1
			r.during = func() { rowsWhileAnswering = r.rows(record.OutboxKindAgentMessage) }
			decided := releasedAnswer("Use B. Why did you implement A?")
			r.handOff(decided)
			r.handOff(decided)
			r.work()
			posts, _ := r.requests()
			if states := r.states(); len(states) != 1 || states[0] != string(store.QuestionPosted) {
				t.Fatalf("questions = %v; want the one posted", states)
			}
			if len(posts) != 1 || !strings.HasPrefix(posts[0], "/api/v1/issues/"+releasedIssue+"/comments ") || !strings.Contains(posts[0], releasedAsk) {
				t.Fatalf("Dispatch posts = %q; want the answer on the ask's thread", posts)
			}
			if !strings.Contains(r.conversation(), "Answered: Selected: B") {
				t.Fatal("the implementer's conversation holds no exchange of the answer")
			}
			messages := r.agentMessages()
			if stop == stopByWorkflow {
				if len(messages) != 0 || r.rows(record.OutboxKindNotice) != 1 {
					t.Fatalf("agent_message rows %+v and %d notice rows for an agent that is not released; want the decision to its architect alone",
						messages, r.rows(record.OutboxKindNotice))
				}
				return
			}
			if rowsWhileAnswering != 0 {
				t.Fatalf("agent_message rows while the answer was generated = %d; want none before the answer is in", rowsWhileAnswering)
			}
			if len(messages) != 1 || messages[0].Role != claim.RoleImplementer || messages[0].Session != releasedSession {
				t.Fatalf("agent_message rows = %+v; want one for the implementer", messages)
			}
			for _, want := range []string{"Selected: B\n\nUse B. Why did you implement A?", "already answered the question in this message",
				"dispatch://" + releasedIssue + "/ask/" + releasedAsk, "do not answer it again"} {
				if !strings.Contains(messages[0].Text, want) {
					t.Errorf("the wake row tells the implementer %q; want it to hold %q", messages[0].Text, want)
				}
			}
			if notices, toController := r.rows(record.OutboxKindNotice), r.rows(record.OutboxKindControllerNotice); notices != 0 || toController != 0 {
				t.Fatalf("%d notice and %d controller notice rows; want the architect and the controller told nothing", notices, toController)
			}
			r.assertStillReleased()
		})
	}
}

// A request for work and a question the agent could not answer, for a released agent: nothing is
// posted, nothing appended, one wake row each carries the message, and the controller receives
// nothing, no agent-question notice and nothing listed in questionsForController. The control is
// the same replies for an agent the workflow suspended: the reply is posted and the controller
// receives the question.
func TestIdleReleasedAgentsRequestForWorkAndUnansweredQuestionWakeItAndTellTheControllerNothing(t *testing.T) {
	t.Parallel()
	for _, kind := range []answer.Kind{answer.NeedsWork, answer.NotAnswered} {
		for _, stop := range []stopBy{stopByIdleRelease, stopByWorkflow} {
			t.Run(string(kind)+" after "+string(stop), func(t *testing.T) {
				r := newReleasedRig(t, stop)
				r.answer = func(req answer.Request) answer.Reply { return releasedReply(req, kind, "That needs my working copy.") }
				r.handOff(releasedMessage("m-1", "Run the suite on B?"))
				r.work()
				posts, sends := r.requests()
				messages := r.agentMessages()
				if stop == stopByWorkflow {
					if len(posts) != 1 || r.rows(record.OutboxKindControllerNotice) != 1 || len(r.passedToController()) != 1 || len(messages) != 0 {
						t.Fatalf("for an agent that is not released: posts %q, %d controller notices, passed %+v, wake rows %+v; want the reply posted and the controller told",
							posts, r.rows(record.OutboxKindControllerNotice), r.passedToController(), messages)
					}
					return
				}
				if states := r.states(); len(states) != 1 || states[0] != string(store.QuestionWoken) {
					t.Fatalf("questions = %v; want the one woken", states)
				}
				if len(posts) != 0 || len(sends) != 0 || r.conversation() != r.before {
					t.Fatalf("posts %q, sends %q, conversation changed %t; want nothing posted or appended", posts, sends, r.conversation() != r.before)
				}
				if len(messages) != 1 || !strings.Contains(messages[0].Text, "Run the suite on B?") {
					t.Fatalf("agent_message rows = %+v; want one carrying the message", messages)
				}
				if rows, passed := r.rows(record.OutboxKindControllerNotice), r.passedToController(); rows != 0 || len(passed) != 0 {
					t.Fatalf("%d controller notices, %+v passed to the controller; want nothing", rows, passed)
				}
				r.assertStillReleased()
			})
		}
	}
}

// A reply of kind none appends nothing and gives the message once through one wake row, and so
// does a comment that asks nothing; a comment carrying the loop's own mark and the daemon's own
// post write no row.
func TestIdleReleasedAgentsMessagesThatAskNothingWakeItAndItsOwnMarksDoNot(t *testing.T) {
	t.Parallel()
	r := newReleasedRig(t, stopByIdleRelease)
	r.answer = func(req answer.Request) answer.Reply { return releasedReply(req, answer.None, "Noted.") }
	r.handOff(releasedMessage("m-1", "B is green, by the way."))
	r.handOff(releasedComment(releasedPerson, "c-root", "Thanks, that settles it."))
	r.handOff(releasedComment(releasedPerson, "c-marked", "Plum.\n\n"+answerloop.AnswerMarker+"0123 -->"))
	r.handOff(releasedComment(intake.DispatchActor{Kind: "session", ID: dispatch.DaemonSessionPrefix + "legion"}, "c-daemon", "LEGION-208 moved to testing."))
	r.work()
	posts, sends := r.requests()
	if states := r.states(); len(states) != 1 || states[0] != string(store.QuestionWoken) {
		t.Fatalf("questions = %v; want the message woken", states)
	}
	if len(posts) != 0 || len(sends) != 0 || r.conversation() != r.before {
		t.Fatalf("posts %q, sends %q, conversation changed %t; want nothing posted or appended", posts, sends, r.conversation() != r.before)
	}
	messages := r.agentMessages()
	if len(messages) != 2 || !strings.Contains(messages[0].Text, "Thanks, that settles it.") || !strings.Contains(messages[1].Text, "B is green, by the way.") {
		t.Fatalf("agent_message rows = %+v; want one for the comment, written as it was handed off, then one for the message, and none for the loop's mark or the daemon's post",
			messages)
	}
	r.assertStillReleased()
}

// A message for a released agent is one wake row however often it is handed off, a restart's
// hand-off included. The project's pause holds the row: while the pause stands the outbox runs
// nothing of it and the claim stays released; once the pause ends the row wakes the claim and,
// once the woken session holds its role, publishes the message to its topic and ends.
func TestIdleReleasedAgentsWakeRowIsWrittenOnceAndThePauseHoldsIt(t *testing.T) {
	t.Parallel()
	r := newReleasedRig(t, stopByIdleRelease)
	comment := releasedComment(releasedPerson, "c-root", "Thanks, that settles it.")
	r.handOff(comment)
	r.handOff(comment)
	r.loop = answerloop.New(r.cfg)
	r.handOff(comment)
	messages := r.agentMessages()
	if len(messages) != 1 {
		t.Fatalf("agent_message rows = %+v; want one for three hand-offs", messages)
	}

	pause := openLegionPause(t, r.pool, time.Now())
	if err := r.runner.RunOnce(r.ctx); err != nil {
		t.Fatalf("drain the outbox while paused: %v", err)
	}
	if attempts, _ := r.publisher.snapshot(); attempts != 0 || r.rows(record.OutboxKindAgentMessage) != 1 || podResumes(t, r.pool, releasedIssue) != 0 {
		t.Fatalf("while paused: %d publishes, %d rows, %d wakes; want the row held, nothing published or woken",
			attempts, r.rows(record.OutboxKindAgentMessage), podResumes(t, r.pool, releasedIssue))
	}
	r.assertStillReleased()

	endLegionPause(t, r.pool, pause, time.Now())
	if err := r.runner.RunOnce(r.ctx); err != nil {
		t.Fatalf("drain the outbox after the resume: %v", err)
	}
	if held := r.machine.Claim(); held.Released() || podResumes(t, r.pool, releasedIssue) != 1 || r.rows(record.OutboxKindAgentMessage) != 1 {
		t.Fatalf("after the resume the implementer is %s by %q with %d wakes and %d rows; want it woken once and the row waiting for its session",
			held.State, held.SuspendedBy, podResumes(t, r.pool, releasedIssue), r.rows(record.OutboxKindAgentMessage))
	}
	r.runner.now = func() time.Time { return time.Now().Add(time.Minute) }
	if err := r.runner.RunOnce(r.ctx); err != nil {
		t.Fatalf("the row's next attempt: %v", err)
	}
	topic := notify.RoleTopicPrefix + string(r.token)
	if _, delivered := r.publisher.snapshot(); len(delivered) != 1 || delivered[0].topic != topic || delivered[0].message != messages[0].Text ||
		r.rows(record.OutboxKindAgentMessage) != 0 {
		t.Fatalf("delivered %+v with %d rows left; want the message on %s once and the row ended", delivered, r.rows(record.OutboxKindAgentMessage), topic)
	}
}

// failForCyberPolicy has the released implementer woken, as a wake row resumes it
// (supervise.RequestRuleResume), registered and ready on its session, and then failed by a turn
// OpenAI refused for cyber policy (claim.TurnCodeCyberPolicy), which holds the claim
// (supervise.Claim.HeldForCyberPolicy).
func (r *releasedRig) failForCyberPolicy() {
	r.t.Helper()
	if err := r.machine.Handle(r.ctx, supervise.RequestRuleResume{Claim: r.token, Reason: "a wake for a delivery the daemon holds for the implementer"}); err != nil {
		r.t.Fatalf("wake the implementer: %v", err)
	}
	generation := r.machine.Claim().Generation
	for _, request := range []supervise.Event{
		supervise.RequestRegister{Claim: r.token, Generation: generation, Session: releasedSession, SessionFile: r.path},
		supervise.RequestReady{Claim: r.token, Generation: generation, Session: releasedSession},
		supervise.RequestTurnOutcome{Claim: r.token, Generation: generation, Sequence: 1, Refused: true,
			Message: "cyber_policy: synthetic refusal text", Unprompted: true, Code: claim.TurnCodeCyberPolicy},
	} {
		if err := r.machine.Handle(r.ctx, request); err != nil {
			r.t.Fatalf("%T of the woken implementer: %v", request, err)
		}
	}
	if held := r.machine.Claim(); !held.HeldForCyberPolicy() || held.Session != releasedSession {
		r.t.Fatalf("the implementer is %s (%q) on %q, want it held for cyber policy on its session", held.State, held.FailureCause, held.Session)
	}
}

// queuedNotice is an outbox row of LEGION-208 that carries a notice: the row's kind, which says
// whom the notice goes to (an architect's notice, or the controller's), and its payload.
type queuedNotice struct {
	row     record.OutboxKind
	payload map[string]any
}

// noticesOfKind is every outbox row of LEGION-208 whose notice is of kind, in the order they were
// queued, whichever row kind carries it.
func (r *releasedRig) noticesOfKind(kind record.NoticeKind) []queuedNotice {
	r.t.Helper()
	rows, err := r.pool.Query(r.ctx, `select kind, payload from outbox where issue = $1 and payload->>'kind' = $2 order by id`, releasedIssue, string(kind))
	if err != nil {
		r.t.Fatal(err)
	}
	defer rows.Close()
	var notices []queuedNotice
	for rows.Next() {
		var row string
		var payload []byte
		if err := rows.Scan(&row, &payload); err != nil {
			r.t.Fatal(err)
		}
		notice := queuedNotice{row: record.OutboxKind(row)}
		if err := json.Unmarshal(payload, &notice.payload); err != nil {
			r.t.Fatal(err)
		}
		notices = append(notices, notice)
	}
	if err := rows.Err(); err != nil {
		r.t.Fatal(err)
	}
	return notices
}

// A person's answer to the released agent's ask that also asks is held on its question for the
// agent's wake. When the claim is woken, runs, and is held for OpenAI's cyber-policy refusal
// before the loop takes the question, no model is called and nothing wakes the agent: no start of
// the daemon's own launches a held claim's agent. The decision is queued as the agent-decision
// notice it would have been for an agent that was not released, routed as such a notice is (to
// the architect that holds its role), and the question goes to the controller as an
// agent-question notice with the reason held_for_cyber_policy. The control is
// TestIdleReleasedAgentsDecisionThatAlsoAsksWakesItOnceItsAnswerIsIn: the same answer for a
// released claim that is not held is answered and given through one wake row.
func TestIdleReleasedAgentsDecisionReachesTheArchitectOnceItsClaimIsHeldForCyberPolicy(t *testing.T) {
	t.Parallel()
	r := newReleasedRig(t, stopByIdleRelease)
	var calls atomic.Int64
	r.answer = func(req answer.Request) answer.Reply {
		calls.Add(1)
		return releasedReply(req, answer.Answered, "Answered: "+req.Question.Text)
	}
	r.handOff(releasedAnswer("Use B. Why did you implement A?"))
	r.failForCyberPolicy()
	r.work()

	if states := r.states(); calls.Load() != 0 || len(states) != 1 || states[0] != string(store.QuestionPassedOn) {
		t.Fatalf("questions = %v after %d model calls; want the one passed on with no model call", states, calls.Load())
	}
	decisions := r.noticesOfKind(record.AgentDecisionNotice)
	if len(decisions) != 1 || decisions[0].row != record.OutboxKindNotice {
		t.Fatalf("agent-decision notices = %+v; want one, an architect's notice: the architect holds its role", decisions)
	}
	decided, _ := decisions[0].payload["agent_decision"].(map[string]any)
	if text, _ := decided["text"].(string); decided["id"] != "dispatch:34" || decided["session"] != releasedSession || !strings.Contains(text, "Selected: B") {
		t.Fatalf("the agent-decision notice = %+v; want the person's answer to the implementer's ask", decisions[0].payload)
	}
	questions := r.noticesOfKind(record.AgentQuestionNotice)
	if len(questions) != 1 || questions[0].row != record.OutboxKindControllerNotice ||
		questions[0].payload["reason"] != string(answerloop.ReasonHeldForCyberPolicy) {
		t.Fatalf("agent-question notices = %+v; want one controller notice with the reason %s", questions, answerloop.ReasonHeldForCyberPolicy)
	}
	if messages := r.agentMessages(); len(messages) != 0 {
		t.Fatalf("agent_message rows = %+v; want none for a held claim", messages)
	}
	if r.conversation() != r.before {
		t.Fatal("the held implementer's conversation changed")
	}
}
