package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// freshSessionRig is one issue implementing, its implementer running on the session oldSession,
// and the workflow behind it: the engine the facts are applied through, the outbox that runs what
// the workflow queues, and the runtime the launches go to.
type freshSessionRig struct {
	t           *testing.T
	pool        *store.Pool
	records     record.Store
	issue       record.Issue
	sup         *supervisor
	rt          *fake.Runtime
	engine      *workflow.Engine
	runner      *outbox
	operatorAPI http.Handler
	implementer claim.Token
	machine     *supervise.Machine
	conn        *fake.Conn
}

const (
	oldSession     = "ses-refused"
	oldSessionFile = "/tmp/ses-refused.jsonl"
	newSession     = "ses-fresh"
	newSessionFile = "/tmp/ses-fresh.jsonl"
	phaseTask      = "Continue Workflow. Issue: LEGION-208. Phase: implementing."
	// freshSessionOperatorToken is the bearer `legion claims` sends, which the operator routes
	// compare against.
	freshSessionOperatorToken = "operator-token"
)

func newFreshSessionRig(t *testing.T) *freshSessionRig {
	t.Helper()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	ctx := context.Background()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: "LEGION-208", Title: "Workflow", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"}
	putOutboxIssue(t, pool, records, issue)
	// The issue is at work, so it holds the admission slot its work holds: the task half of a
	// start is claimable only while its issue holds one (workflow.StartOrOwe, record.NeedsSlot).
	if _, err := pool.Exec(ctx, `insert into slots (issue, index, admitted_at) values ($1, 0, now())`, issue.Key); err != nil {
		t.Fatalf("seed the issue's admission slot: %v", err)
	}
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	// The operator's claim route answers the claim's step record, which the supervisor reads from
	// the store itself (supervisor.Step), so this rig's supervisor holds the test database's own.
	sup.store = openOutboxStore(t, pool)
	sup.deps.PhaseHolds = (&workflowRuntime{pool: pool, records: records}).phaseHolds
	engine := workflow.New(records, workflow.Config{Project: "legion", Repositories: testRepositories}, quietLogger())
	sup.OnDurableState((&workflowRuntime{pool: pool, records: records, handlers: []intake.Handler{engine}, log: quietLogger()}).terminal)
	rig := &freshSessionRig{t: t, pool: pool, records: records, issue: issue, sup: sup, rt: rt, engine: engine}
	trees := outboxTreeStore(t, pool, issue.Key)
	workspaceDir := t.TempDir()
	rig.runner = &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, project: "legion",
		stateDir: t.TempDir(), tokens: outboxTokens{}, notices: &outboxPublisher{}, handlers: []intake.Handler{engine}, now: time.Now,
		trees: trees,
		// A retired claim's start provisions its workspace again, as it does for a re-admitted tree.
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: workspaceDir, Bookmark: "legion/" + issue.Key}, nil
		}}
	// The operator's own routes, wired as the daemon wires them: what `legion claims` talks to.
	rig.operatorAPI = api.NewServer("127.0.0.1", 8437, api.Options{
		Supervisor: sup, BootTokens: api.NewBootTokens(trees), Project: "legion", DispatchProject: "LEGION",
		OperatorToken: freshSessionOperatorToken, Controller: trees, Trees: trees, Releaser: rt, Pool: pool, Record: records,
		Handlers: []intake.Handler{engine}, RoleFor: workflow.RoleFor, Log: quietLogger(),
	}).Handler
	rig.implementer = mustClaimToken(t, issue.Key, claim.RoleImplementer)
	machine, _, err := sup.Create(ctx, supervise.Claim{Token: rig.implementer, Project: "legion", Tree: issue.Tree, Issue: issue.Key,
		Role: claim.RoleImplementer, State: supervise.StateQueued}, "")
	if err != nil {
		t.Fatalf("create the implementer: %v", err)
	}
	rig.machine = machine
	rig.conn = fake.NewConn()
	sup.deps.Conns.(*fake.Conns).Register(rig.implementer, rig.conn)
	if err := machine.Handle(ctx, supervise.RequestSpawn{Claim: rig.implementer}); err != nil {
		t.Fatalf("spawn the implementer: %v", err)
	}
	rig.register(oldSession, oldSessionFile)
	rig.must(supervise.RequestDeliver{Claim: rig.implementer, Task: phaseTask, ID: "outbox:7", Phase: phase.Implementing, Generation: issue.Generation})
	return rig
}

// register drives the launched claim through its shim's hello, its agent's registration on session
// and its ready, at the claim's current generation, and waits out the send its ready starts.
func (r *freshSessionRig) register(session, file string) {
	r.t.Helper()
	generation := r.machine.Claim().Generation
	for _, ev := range []supervise.Event{
		supervise.StreamHello{Claim: r.implementer, Generation: generation},
		supervise.RequestRegister{Claim: r.implementer, Generation: generation, Session: session, SessionFile: file},
		supervise.RequestReady{Claim: r.implementer, Generation: generation, Session: session},
	} {
		if err := r.machine.Handle(context.Background(), ev); err != nil {
			r.t.Fatalf("boot the implementer: handle %T: %v", ev, err)
		}
	}
	r.machine.Wait()
}

func (r *freshSessionRig) must(ev supervise.Event) {
	r.t.Helper()
	if err := r.machine.Handle(context.Background(), ev); err != nil {
		r.t.Fatalf("handle %T: %v", ev, err)
	}
}

// operator is one `legion claims` request against the operator routes: the command's own HTTP
// call, with its bearer. A nil body sends none, as a subcommand with no flags of its own does.
func (r *freshSessionRig) operator(method, target string, body any) *httptest.ResponseRecorder {
	r.t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			r.t.Fatalf("encode the request body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, target, reader)
	request.Header.Set("Authorization", "Bearer "+freshSessionOperatorToken)
	recorder := httptest.NewRecorder()
	r.operatorAPI.ServeHTTP(recorder, request)
	return recorder
}

// refusal400 is what the claim's producer reports of each refused turn below: the provider's words
// for a malformed request, which the turn-outcome report carries as the provider gave them.
const refusal400 = "400 invalid_request_error: messages: unexpected role"

// refuseTurns works the claim's pending task through turns its provider refuses, each reported
// refused by the claim's producer, and then answers the task's prompt with a provider error that
// cannot clear: an HTTP 400, a client error that is none of 401, 403, 408 and 429, which fails the
// claim (supervise's classifyProviderError). The reports fail nothing, however many there are
// (dispatch://INFRA-4508): they are the claim's run of refused turns, which the daemon answers each
// report with and which decides nothing.
func (r *freshSessionRig) refuseTurns(turns int) {
	r.t.Helper()
	// The phase task's prompt is acknowledged once its send is over, so its result is awaited
	// across the turns below.
	r.machine.Wait()
	pending := r.machine.Claim().Pending
	if pending == nil {
		r.t.Fatal("the implementer holds no task for its provider to refuse")
	}
	for turn := 1; turn <= turns; turn++ {
		generation := r.machine.Claim().Generation
		r.must(supervise.StreamTurnStart{Claim: r.implementer})
		r.must(supervise.RequestTurnOutcome{Claim: r.implementer, Generation: generation, Sequence: uint64(turn), Refused: true, Message: refusal400})
		r.must(supervise.StreamTurnEnd{Claim: r.implementer})
		if got := r.machine.Claim(); got.State == supervise.StateFailed {
			r.t.Fatalf("the report of refused turn %d failed the claim (%q); a run of refused turns fails none", turn, got.Failure)
		}
	}
	r.must(supervise.StreamPromptResult{Claim: r.implementer, DeliveryID: pending.ID, AgentInvoked: true,
		Error: "invalid_request_error", HTTPStatus: 400})
	r.machine.Wait()
}

// told is one notice the workflow queued, whether for the issue's architect or for the
// controller, as this rig reads it: its kind and the `detail` of the failure record beside it,
// which is where the sentence the failure wrote stands (record.NoticeFailure; a notice with no
// record reads "").
type told struct{ kind, detail string }

// failureDetail is the sentence a notice's failure record carries, and "" for a notice with no
// record: the tree architect's own, a production-check look's, and a fact that carried no cause.
func failureDetail(failure *record.NoticeFailure) string {
	if failure == nil {
		return ""
	}
	return failure.Detail
}

// notices are the notices the workflow has queued, in the order it queued them.
func (r *freshSessionRig) notices() []told {
	r.t.Helper()
	rows, err := r.pool.Query(context.Background(), "select id, kind, issue, payload from outbox where kind in ('notice', 'controller_notice') order by id")
	if err != nil {
		r.t.Fatalf("read the notice rows: %v", err)
	}
	defer rows.Close()
	var notices []told
	for rows.Next() {
		var row record.OutboxRow
		if err := rows.Scan(&row.ID, &row.Kind, &row.Issue, &row.Payload); err != nil {
			r.t.Fatalf("scan a notice row: %v", err)
		}
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			r.t.Fatalf("decode notice row %d: %v", row.ID, err)
		}
		switch notice := payload.(type) {
		case record.Notice:
			notices = append(notices, told{kind: string(notice.Kind), detail: failureDetail(notice.Failure)})
		case record.ControllerNotice:
			notices = append(notices, told{kind: string(notice.Kind) + " (controller)", detail: failureDetail(notice.Failure)})
		default:
			r.t.Fatalf("outbox row %d is no notice: %#v", row.ID, payload)
		}
	}
	return notices
}

// recordedPhase is the issue's phase and the phase it is held from, as the record holds them.
func (r *freshSessionRig) recordedPhase() (phase.Phase, phase.Phase) {
	r.t.Helper()
	var current phase.Phase
	var from *phase.Phase
	if err := r.pool.QueryRow(context.Background(), "select phase, held_from from issues where key = $1", r.issue.Key).Scan(&current, &from); err != nil {
		r.t.Fatalf("read the issue's phase: %v", err)
	}
	if from == nil {
		return current, ""
	}
	return current, *from
}

// wantsHeldForTheArchitect holds what a failure that holds the issue leaves: the issue held from
// implementing, and the architect's held notice and the worker-died notice, each carrying the
// failure's sentence in its failure record's `detail`, where every reader of the notice finds it
// beside the cause word, so each names the status the provider answered.
func (r *freshSessionRig) wantsHeldForTheArchitect() {
	r.t.Helper()
	current, from := r.recordedPhase()
	if current != phase.Held || from != phase.Implementing {
		r.t.Fatalf("after the refusal the issue is in %s held from %q, want held from implementing", current, from)
	}
	var kinds []string
	for _, notice := range r.notices() {
		kinds = append(kinds, notice.kind)
		if !strings.Contains(notice.detail, "HTTP 400") {
			r.t.Errorf("the %s notice's failure detail says %q, want the status the provider answered in it", notice.kind, notice.detail)
		}
	}
	if want := []string{"held", "worker-died"}; !slices.Equal(kinds, want) {
		r.t.Fatalf("the failure queued %v, want %v", kinds, want)
	}
}

// A refused turn whose provider error cannot clear fails the claim at once, and the workflow holds
// the issue for its architect. The turn makes both reports an Oh My Pi worker's refused turn
// makes: its producer's report of the turn, refused, and the result of the prompt the turn ran,
// which names the status and is what decides (supervise's providerErrored). The claim keeps its
// session and the task it owes, which are the architect's to decide, and the task is not sent
// again. The supervise tests of the same decision have no workflow, and the retry test below reads
// the same hold after a run of refused turns that reaches launch_failure_limit, so this is the one
// test in which a single refused turn holds the issue, with the claim's task kept and sent no
// second time.
func TestARefusal400HoldsTheIssueForItsArchitect(t *testing.T) {
	t.Parallel()
	rig := newFreshSessionRig(t)

	rig.refuseTurns(1)

	got := rig.machine.Claim()
	if got.State != supervise.StateFailed || got.FailureCause != supervise.CauseProviderError ||
		!strings.Contains(got.Failure, "cannot clear") || !strings.Contains(got.Failure, "HTTP 400") {
		t.Fatalf("the implementer is %s (%q, %q), want it failed by a provider error that cannot clear, naming HTTP 400",
			got.State, got.FailureCause, got.Failure)
	}
	if got.Session != oldSession || got.Pending == nil {
		t.Fatalf("the failed implementer records session %q holding %+v, want its session and the task it owes kept", got.Session, got.Pending)
	}
	rig.wantsHeldForTheArchitect()
	if prompts := rig.conn.Prompts(); len(prompts) != 1 {
		t.Fatalf("the implementer was prompted %d times, want once: a provider error that cannot clear sends nothing again", len(prompts))
	}
}

// A turn that ran no prompt of the daemon's — the phase reminder's, which the plugin starts itself
// with pi.sendMessage — has no prompt result, so the daemon judges it from its producer's report,
// marked unprompted and carrying the status the provider refused it with (supervise's
// turnOutcome, dispatch://INFRA-4508's R4). Refused with a 400, it fails the claim at once with
// cause provider_error, and the workflow holds the issue for its architect, as a refused prompt's
// 400 does in the test above. The claim first finishes its task's turn with no error, so it is
// idle and owes nothing, which is where a worker is when its reminder fires.
func TestAPluginTurnRefused400HoldsTheIssueForItsArchitect(t *testing.T) {
	t.Parallel()
	rig := newFreshSessionRig(t)
	rig.machine.Wait()
	pending := rig.machine.Claim().Pending
	if pending == nil {
		t.Fatal("the implementer holds no task to finish before its reminder's turn")
	}
	rig.must(supervise.StreamTurnStart{Claim: rig.implementer})
	rig.must(supervise.StreamTurnEnd{Claim: rig.implementer})
	rig.must(supervise.StreamPromptResult{Claim: rig.implementer, DeliveryID: pending.ID, AgentInvoked: true})
	rig.machine.Wait()
	if got := rig.machine.Claim(); got.State != supervise.StateIdle || got.Pending != nil {
		t.Fatalf("after its task's turn the implementer is %s holding %+v, want it idle owing nothing", got.State, got.Pending)
	}

	rig.must(supervise.RequestTurnOutcome{Claim: rig.implementer, Generation: rig.machine.Claim().Generation, Sequence: 2,
		Refused: true, Message: refusal400, HTTPStatus: 400, Unprompted: true})
	rig.machine.Wait()

	got := rig.machine.Claim()
	if got.State != supervise.StateFailed || got.FailureCause != supervise.CauseProviderError ||
		!strings.Contains(got.Failure, "cannot clear") || !strings.Contains(got.Failure, "HTTP 400") {
		t.Fatalf("the implementer is %s (%q, %q), want it failed by a provider error that cannot clear, naming HTTP 400",
			got.State, got.FailureCause, got.Failure)
	}
	if got.Session != oldSession {
		t.Fatalf("the failed implementer records session %q, want %q kept for its architect's retry", got.Session, oldSession)
	}
	rig.wantsHeldForTheArchitect()
}

// A claim whose provider refused the recorded session's last launch_failure_limit turns in a row
// keeps that session at its retry, whoever retries it, unless the retry asks for a new one
// (supervise's freshSession, dispatch://INFRA-4508's R5). The run of refusals fails no claim: this
// one fails because the prompt those turns ran ended in a provider error that cannot clear, and
// its issue is held, so the tree's architect can retry it. That one retry resumes the recorded
// session with fresh budgets and gives it the task the claim owes; if the provider refuses it
// again, the claim fails again at once, and a new session stays the operator's or the
// controller's choice.
func TestAnArchitectsRetryOfARefusedClaimResumesItsSessionWithThePhaseTask(t *testing.T) {
	t.Parallel()
	rig := newFreshSessionRig(t)
	ctx := context.Background()
	limit := rig.sup.deps.Limits.LaunchFailures

	rig.refuseTurns(limit)

	if got := rig.machine.Claim(); got.State != supervise.StateFailed || got.Session != oldSession || got.RefusedRun() != limit {
		t.Fatalf("after its refused turns the implementer is %s on session %q with a refused run of %d, want failed with the refused session still recorded and a run of %d",
			got.State, got.Session, got.RefusedRun(), limit)
	}
	rig.wantsHeldForTheArchitect()
	spawned := len(rig.rt.CallsOf("Spawn"))

	// The one command: the architect's retry of the held phase.
	if _, err := intake.ApplyFact(ctx, rig.pool, "api", "retry:"+rig.issue.Key,
		intake.RetryOrEscalate{Issue: rig.issue.Key, Decision: intake.RetryDecision}, rig.engine); err != nil {
		t.Fatalf("the architect's retry: %v", err)
	}
	// A start is two rows now (workflow.StartOrOwe): the relaunch leaves a failed claim to the row
	// that carries the task, so the drain runs until both have run.
	for range 3 {
		if err := rig.runner.RunOnce(ctx); err != nil {
			t.Fatalf("run what the retry queued: %v", err)
		}
	}
	rig.machine.Wait()

	if got := rig.machine.Claim(); got.State != supervise.StateLaunching || got.Session != oldSession || got.Abandoned.Session != "" {
		t.Fatalf("after the retry the implementer is %s on session %q, abandoned %+v, want it launching to resume %q",
			got.State, got.Session, got.Abandoned, oldSession)
	}
	if resumes := resumesOf(rig.rt, rig.implementer); len(resumes) != 1 {
		t.Fatalf("the retry resumed %+v, want the recorded session resumed once", resumes)
	}
	if spawns := rig.rt.CallsOf("Spawn"); len(spawns) != spawned {
		t.Fatalf("the retry spawned %+v, want no new agent", spawns[spawned:])
	}
	rig.register(oldSession, oldSessionFile)
	// The task the claim owes, which the provider error took back: the retried phase's own task
	// waits behind it, as it does behind any task the claim still owes. No sentence of an
	// abandoned session goes with it: the session is the one the agent was working in.
	prompts := rig.conn.Prompts()
	if len(prompts) != 2 || !strings.HasSuffix(prompts[1].Message, phaseTask) || strings.Contains(prompts[1].Message, "abandoned") {
		t.Fatalf("the resumed session was prompted %+v, want it to end with the phase task the claim owes and name no abandoned session", prompts)
	}
}

// An operator who stops a worker whose agent can do nothing — a provider that refuses its session,
// an agent wedged in a way no budget counts — retires that claim, and a stop is not a death: the
// issue is left in its phase, holding nothing. From there `legion claims retry --fresh-session` is
// the one command that must give that phase a working agent: the stopped claim forgets the session
// it recorded, the workflow starts its phase again, and the new session is given the phase's task.
func TestTheOperatorsFreshSessionRetryOfAStoppedWorkerStartsANewSessionWithThePhaseTask(t *testing.T) {
	t.Parallel()
	rig := newFreshSessionRig(t)
	ctx := context.Background()
	route := "/legion/v1/operator/claims/" + string(rig.implementer)

	if recorder := rig.operator(http.MethodPost, route+"/stop", nil); recorder.Code != http.StatusOK {
		t.Fatalf("the operator's stop = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	if got := rig.machine.Claim(); got.State != supervise.StateRetired || got.Session != oldSession {
		t.Fatalf("after the operator's stop the implementer is %s on session %q, want it retired with its session still recorded", got.State, got.Session)
	}
	if current, from := rig.recordedPhase(); current != phase.Implementing || from != "" {
		t.Fatalf("after the operator's stop the issue is in %s held from %q, want implementing and not held: a stop is not a death", current, from)
	}

	// The one command: the operator's retry in a new session.
	recorder := rig.operator(http.MethodPost, route+"/retry", api.RetryRequest{FreshSession: true})
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("retry --fresh-session of the stopped implementer = %d, want 202; body %s", recorder.Code, recorder.Body)
	}
	// A start is two rows now (workflow.StartOrOwe): the relaunch leaves a failed claim to the row
	// that carries the task, so the drain runs until both have run.
	for range 3 {
		if err := rig.runner.RunOnce(ctx); err != nil {
			t.Fatalf("run what the retry queued: %v", err)
		}
	}
	rig.machine.Wait()

	if got := rig.machine.Claim(); got.State != supervise.StateLaunching || got.Session != "" || got.SessionFile != "" {
		t.Fatalf("after the retry the implementer is %s on session %q (%s), want it launching with no session to resume", got.State, got.Session, got.SessionFile)
	}
	if resumes := resumesOf(rig.rt, rig.implementer); len(resumes) != 0 {
		t.Fatalf("the retry resumed %+v, want the stopped session left alone", resumes)
	}
	spawns := rig.rt.CallsOf("Spawn")
	if len(spawns) != 2 || spawns[1].Spec.ResumeSessionFile != "" {
		t.Fatalf("the retry's launch = %+v, want a spawn resuming no session file", spawns)
	}
	rig.register(newSession, newSessionFile)
	if got := rig.machine.Claim(); got.Session != newSession || got.Session == oldSession {
		t.Fatalf("the retried implementer registered session %q, want the new %q", got.Session, newSession)
	}
	prompts := rig.conn.Prompts()
	if len(prompts) != 2 || !strings.HasSuffix(prompts[1].Message, phaseTask) {
		t.Fatalf("the new session was prompted %+v, want it to end with its phase's task", prompts)
	}
	wantsAbandonment(t, prompts[1].Message, "`legion claims forget-session`")
}

// wantsAbandonment is the check both retries make of the fresh session's first prompt: it names
// the session that was abandoned and why, in the words the claim recorded, and the two things that
// outlived that session, the issue's branch and the handoff records its phases wrote on the
// Dispatch issue.
func wantsAbandonment(t *testing.T, prompt, why string) {
	t.Helper()
	for _, want := range []string{
		oldSession, "abandoned", why,
		"legion/" + "LEGION-208", "dispatch handoff-read --issue LEGION-208 --phase", "(plan, implement, test, review)",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the fresh session's first prompt does not name %q:\n%s", want, prompt)
		}
	}
}
