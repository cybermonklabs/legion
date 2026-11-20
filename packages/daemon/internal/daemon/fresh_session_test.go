package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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

// An operator who stops a worker whose agent can do nothing — a provider that refuses its session,
// an agent wedged in a way no budget counts — retires that claim, and a stop is not a death: the
// issue is left in its phase, holding nothing. From there `legion claims retry --fresh-session` is
// the one command that must give that phase a working agent: the stopped claim forgets the session
// it recorded, the workflow starts its phase again, and the new session is given the phase's task.
func TestTheOperatorsFreshSessionRetryOfAStoppedWorkerStartsANewSessionWithThePhaseTask(t *testing.T) {
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
// the session that was abandoned and why — the provider's own words for a refused one, the
// context size among them where the provider gave one — and the two things that outlived that
// session, the issue's branch and the handoff records its phases wrote on the Dispatch issue.
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
