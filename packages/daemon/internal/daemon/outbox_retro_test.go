package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// retroRunningTree is the root of a tree the workflow still runs, and retroClosedChild its child
// a close took out of the workflow on its second run: the issue a retro after a close runs on
// when its tree goes on working. The start of that retro runs where a phase start finishes
// without acting, since the child stands in no phase and out of the workflow.
func retroRunningTree() record.Issue {
	return record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: "LEGION-208", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"}
}

func retroClosedChild() record.Issue {
	parent := "LEGION-208"
	return record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: "LEGION-208", Parent: &parent, Title: "child", Phase: phase.Done, Generation: 2, Status: "done", Rank: "V"}
}

// The session the retro's agent kept from the run, which every relaunch of it must resume, and the
// one a root architect kept in a test that relaunches both (claimOfSession).
const (
	retroAgentSession     = "ses_implementer"
	retroAgentSessionFile = "/sessions/implementer.jsonl"
	retroArchitectSession = "ses_architect"
)

// sessionFileOf is the session file the claim that kept session left: retroAgentSessionFile for
// the retro's agent, and a file of its own for any other.
func sessionFileOf(session string) string {
	if session == retroAgentSession {
		return retroAgentSessionFile
	}
	return "/sessions/" + strings.TrimPrefix(session, "ses_") + ".jsonl"
}

// retroOutboxRig is the outbox of one project over a database of its own, with the daemon's claim
// store (so a restart reads back what the claims wrote) and the fake runtime under its supervisor.
// tree is the one tree its rows run in, whose lifecycle stands open under the workflow as
// admission opened it. It counts the workspaces its runner provisioned and removed.
type retroOutboxRig struct {
	t           *testing.T
	ctx         context.Context
	pool        *store.Pool
	records     record.Store
	st          *store.Store
	sup         *supervisor
	rt          *fake.Runtime
	runner      *outbox
	facts       *outboxFactHandler
	provisioned atomic.Int32
	removed     atomic.Int32
}

// newRetroOutboxRig records issues, the first of them the root of their tree, and builds the rig
// over them. handlers are what the runner applies the facts of its rows through, beside the
// rig's own record of every such fact (retroOutboxRig.facts).
func newRetroOutboxRig(t *testing.T, issues []record.Issue, handlers ...intake.Handler) *retroOutboxRig {
	t.Helper()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	for _, issue := range issues {
		putOutboxIssue(t, pool, records, issue)
	}
	st := outboxTreeStore(t, pool, issues[0].Tree)
	rt := fake.NewRuntime()
	sup := newSupervisor(ctx, st, "legion", t.TempDir(), quietLogger())
	sup.deps = supervise.Deps{
		Runtime: rt, Conns: fake.NewConns(), Store: st, Specs: outboxSpecs{}, Clock: stillClock{}, Log: quietLogger(),
		RootHears:      (&workflowRuntime{pool: pool, records: records}).rootHears,
		RetroWakeHolds: (&workflowRuntime{pool: pool, records: records}).retroWakeHolds,
		Limits:         supervise.Limits{LaunchFailures: 2, PromptFailures: 2, PromptRetires: 2},
		Timeouts:       supervise.Timeouts{Boot: time.Second, RegistrationIntervals: 2, RPC: time.Second, Probe: time.Second, Stop: time.Second, LaunchBackoff: time.Millisecond},
		PluginContract: 24,
		RefreshGate:    supervise.NewRefreshGate(0),
	}
	t.Cleanup(sup.stop)
	r := &retroOutboxRig{t: t, ctx: ctx, pool: pool, records: records, st: st, sup: sup, rt: rt, facts: &outboxFactHandler{}}
	r.runner = &outbox{log: quietLogger(), dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup, trees: st,
		tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), now: time.Now,
		handlers: append([]intake.Handler{r.facts}, handlers...),
		dispatch: &outboxDispatch{issue: dispatch.Issue{Key: issues[0].Key, Status: issues[0].Status}}, notices: &outboxPublisher{},
		githubAPI: newBranchGitHub(t, nil, branchExists).url,
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			r.provisioned.Add(1)
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/" + issues[len(issues)-1].Key}, nil
		},
		remove: func(context.Context, workspace.Workspace, sync.Locker) error { r.removed.Add(1); return nil },
	}
	return r
}

// claimOf records issue's claim of role in state, the source of a suspension in by, with the
// session and the session file the run left it: as the close, a linger's end or the agent's own
// exit left it. It answers the machine the supervisor holds for it, with its own connection
// registered, so a resumed or relaunched claim can be brought up as its process brings it.
func (r *retroOutboxRig) claimOf(issue record.Issue, role claim.Role, state supervise.ClaimState, by supervise.SuspendSource) (*supervise.Machine, claim.Token) {
	r.t.Helper()
	return r.claimOfSession(issue, role, state, by, retroAgentSession)
}

// claimOfSession is claimOf with the session the claim kept named: two claims of one daemon cannot
// record one session or one session file, so a test that relaunches two roles gives each its own
// (sessionFileOf, readyUpAs, relaunchedInSession).
func (r *retroOutboxRig) claimOfSession(issue record.Issue, role claim.Role, state supervise.ClaimState, by supervise.SuspendSource, session string) (*supervise.Machine, claim.Token) {
	r.t.Helper()
	token := mustClaimToken(r.t, issue.Key, role)
	machine, _, err := r.sup.Create(r.ctx, supervise.Claim{Token: token, Project: "legion", Tree: issue.Tree, TreeEpoch: 1, Issue: issue.Key, Role: role,
		Generation: 1, State: state, SuspendedBy: by, Session: session, SessionFile: sessionFileOf(session)}, "")
	if err != nil {
		r.t.Fatalf("record the %s %s of %s: %v", state, role, issue.Key, err)
	}
	r.sup.deps.Conns.(*fake.Conns).Register(token, fake.NewConn())
	return machine, token
}

// openRetroAfterClose records issue's current run as started, its retro step row as started a
// minute ago, and the retro Legion runs after the issue's close open on that run by agent, bound
// RetroAfterCloseBound after the step row's start: what the close leaves for a run that had no
// retro (workflow's openRetroAfterClose), and what the outbox's start of the retro is fenced on
// (INFRA-5602's P2). It answers the run.
func (r *retroOutboxRig) openRetroAfterClose(issue record.Issue, agent claim.Token) uint64 {
	r.t.Helper()
	return r.openRetroOnEntry(issue, agent, 0)
}

// openRetroOnEntry is openRetroAfterClose for a retro that was running at the close, on the phase
// entry its agent's task carries (record.RunRetro.Entry), or 0 for a retro the close started.
func (r *retroOutboxRig) openRetroOnEntry(issue record.Issue, agent claim.Token, entry int64) uint64 {
	r.t.Helper()
	run := issue.Generation
	started := time.Now().Add(-time.Minute).UTC().Truncate(time.Microsecond)
	if err := store.Transact(r.ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := r.records.MarkRunStarted(ctx, tx, issue.Key, run, issue.Tree, started.Add(-24*time.Hour)); err != nil {
			return err
		}
		if err := r.records.MarkRetroStarted(ctx, tx, issue.Key, run, issue.Tree, started); err != nil {
			return err
		}
		return r.records.OpenRetroAfterClose(ctx, tx, issue.Key, run, issue.Tree, started, started.Add(workflow.RetroAfterCloseBound), agent, entry)
	}); err != nil {
		r.t.Fatalf("open the retro after the close of %s: %v", issue.Key, err)
	}
	return run
}

// endRetroAfterClose ends issue's open retro of run with outcome, by cause.
func (r *retroOutboxRig) endRetroAfterClose(issue record.Issue, run uint64, outcome record.RetroOutcome, cause record.RetroEndCause) {
	r.t.Helper()
	r.endRetroAfterCloseAt(issue, run, time.Now(), outcome, cause)
}

// endRetroAfterCloseAt is endRetroAfterClose at the moment at.
func (r *retroOutboxRig) endRetroAfterCloseAt(issue record.Issue, run uint64, at time.Time, outcome record.RetroOutcome, cause record.RetroEndCause) {
	r.t.Helper()
	if err := store.Transact(r.ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		_, ended, err := r.records.EndRetroAfterClose(ctx, tx, issue.Key, run, at.UTC().Truncate(time.Microsecond), outcome, cause)
		if err == nil && !ended {
			return fmt.Errorf("the retro of run %d was not open", run)
		}
		return err
	}); err != nil {
		r.t.Fatalf("end the retro after the close of %s: %v", issue.Key, err)
	}
}

// wakeEngine is the workflow engine the rig's facts go through where a retro's wake is at stake,
// with the store's pod records (workflow.Config.Pods), through which the listener's report of a
// role message finds the claim it names, as the daemon wires them; the runner applies the facts of
// its rows through it too.
func (r *retroOutboxRig) wakeEngine() *workflow.Engine {
	r.t.Helper()
	pods, err := r.st.PodRecordsOf("LEGION")
	if err != nil {
		r.t.Fatalf("read the project's pod records: %v", err)
	}
	engine := workflow.New(r.records, workflow.Config{Project: "LEGION", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(),
		Linger: time.Hour, Pods: pods}, quietLogger())
	r.runner.handlers = append(r.runner.handlers, engine)
	return engine
}

// readyUp brings the launched claim of token up as its process does, as bringUp does, without
// waiting for a task to be sent: the hello, the registration with its session, and its ready.
func readyUp(ctx context.Context, t *testing.T, machine *supervise.Machine, token claim.Token) {
	t.Helper()
	readyUpAs(ctx, t, machine, token, retroAgentSession)
}

// readyUpAs is readyUp for a claim that kept session (claimOfSession).
func readyUpAs(ctx context.Context, t *testing.T, machine *supervise.Machine, token claim.Token, session string) {
	t.Helper()
	machine.Wait()
	generation := machine.Claim().Generation
	for _, ev := range []supervise.Event{
		supervise.StreamHello{Claim: token, Generation: generation},
		supervise.RequestRegister{Claim: token, Generation: generation, Session: session, SessionFile: sessionFileOf(session),
			CapabilityHash: []byte("capability-of-" + session)},
		supervise.RequestReady{Claim: token, Generation: generation, Session: session},
	} {
		if err := machine.Handle(ctx, ev); err != nil {
			t.Fatalf("handle %T for %s: %v", ev, token, err)
		}
	}
	machine.Wait()
}

// promptsOf is every prompt the fake process of the claim token was sent.
func (r *retroOutboxRig) promptsOf(token claim.Token) []fake.Prompt {
	r.t.Helper()
	conn, ok := r.sup.deps.Conns.Conn(token)
	if !ok {
		r.t.Fatalf("no fake connection registered for %s", token)
	}
	return conn.(*fake.Conn).Prompts()
}

// runRetro is issue's record of run as the store holds it now, nil for none.
func (r *retroOutboxRig) runRetro(issue record.Issue, run uint64) *record.RunRetro {
	r.t.Helper()
	var got *record.RunRetro
	if err := store.Transact(r.ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		got, err = r.records.RunRetro(ctx, tx, issue.Key, run)
		return err
	}); err != nil {
		r.t.Fatalf("read the record of run %d of %s: %v", run, issue.Key, err)
	}
	return got
}

// lifecycle is the tree's lifecycle as the table holds it now.
func (r *retroOutboxRig) lifecycle(tree string) treelifecycle.Lifecycle {
	r.t.Helper()
	var got treelifecycle.Lifecycle
	var found bool
	if err := store.Transact(r.ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		got, found, err = treelifecycle.Read(ctx, tx, "legion", tree)
		return err
	}); err != nil || !found {
		r.t.Fatalf("read the lifecycle of %s = %+v (found %t), %v", tree, got, found, err)
	}
	return got
}

// retroStart is the start row id of the retro after issue's close of run for role: what the
// close queues for the retro's agent (workflow's openRetroAfterClose), a start of no phase and no
// entry at the issue's generation, carrying the run, whose task opens as every retro task does.
func retroStart(t *testing.T, issue record.Issue, role claim.Role, run uint64, id int64) record.OutboxRow {
	t.Helper()
	row := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: role, Generation: issue.Generation,
		RetroRun: run, Task: retroTaskLine(issue)}, time.Now())
	row.ID = id
	return row
}

// retroTaskLine is the first line of the retro's task on issue (workflow's continueLine at the
// retro).
func retroTaskLine(issue record.Issue) string {
	return fmt.Sprintf("Continue %s. Issue: %s. Phase: retro.", issue.Title, issue.Key)
}

// heldRetroTask fails unless machine's claim holds the task start row id delivers: the row's own
// delivery, of no phase, at the issue's generation, carrying run, opening as every retro task
// does.
func heldRetroTask(t *testing.T, machine *supervise.Machine, id int64, issue record.Issue, run uint64) {
	t.Helper()
	p := machine.Claim().Pending
	if p == nil || p.ID != fmt.Sprintf("outbox:%d", id) || p.Phase != "" || p.Generation != issue.Generation ||
		p.RetroRun != supervise.RetroRun(run) || !strings.HasPrefix(p.Task, retroTaskLine(issue)) {
		t.Fatalf("the agent's pending delivery = %+v, want the retro's task under outbox:%d with no phase at generation %d, carrying run %d",
			p, id, issue.Generation, run)
	}
}

// relaunchedInItsSession fails unless the runtime launched the claim of token exactly once, by
// resuming the session file it kept, and never spawned it afresh.
func relaunchedInItsSession(t *testing.T, rt *fake.Runtime, token claim.Token) {
	t.Helper()
	relaunchedInSession(t, rt, token, retroAgentSession)
}

// relaunchedInSession is relaunchedInItsSession for a claim that kept session (claimOfSession).
func relaunchedInSession(t *testing.T, rt *fake.Runtime, token claim.Token, session string) {
	t.Helper()
	file := sessionFileOf(session)
	var resumes, spawns int
	for _, call := range rt.Calls() {
		switch {
		case call.Method == "Resume" && call.Spec.Claim == token:
			resumes++
			if call.Spec.ResumeSessionFile != file {
				t.Fatalf("the runtime resumed %s from %q, want the session file it kept, %q", token, call.Spec.ResumeSessionFile, file)
			}
		case call.Method == "Spawn" && call.Spec.Claim == token:
			spawns++
		}
	}
	if resumes != 1 || spawns != 0 {
		t.Fatalf("the runtime resumed %s %d times and spawned it %d times, want it resumed once in its kept session", token, resumes, spawns)
	}
}

// bringUp brings the launched claim of token up as its process does: the hello, the registration
// with its session, and its ready, and waits for the task it holds to be sent.
func bringUp(ctx context.Context, t *testing.T, machine *supervise.Machine, token claim.Token) {
	t.Helper()
	machine.Wait()
	generation := machine.Claim().Generation
	for _, ev := range []supervise.Event{
		supervise.StreamHello{Claim: token, Generation: generation},
		supervise.RequestRegister{Claim: token, Generation: generation, Session: retroAgentSession, SessionFile: retroAgentSessionFile,
			CapabilityHash: []byte("capability-of-" + retroAgentSession)},
		supervise.RequestReady{Claim: token, Generation: generation, Session: retroAgentSession},
	} {
		if err := machine.Handle(ctx, ev); err != nil {
			t.Fatalf("handle %T for %s: %v", ev, token, err)
		}
	}
	awaitSent(t, machine)
}

// awaitSent waits for the task machine's claim holds to be sent to its process.
func awaitSent(t *testing.T, machine *supervise.Machine) {
	t.Helper()
	testwait.Eventually(t, "the claim's task to be sent", func() bool {
		p := machine.Claim().Pending
		return p != nil && !p.DeliveredAt.IsZero()
	})
}

// takeHeldTask starts the turn of the task machine's claim holds, which confirms it.
func takeHeldTask(ctx context.Context, t *testing.T, machine *supervise.Machine, token claim.Token) {
	t.Helper()
	p := machine.Claim().Pending
	if p == nil {
		t.Fatalf("%s holds no task to take", token)
	}
	if err := machine.Handle(ctx, supervise.StreamTurnStart{Claim: token, DeliveryID: p.ID}); err != nil {
		t.Fatalf("start the turn of %s on %s: %v", token, p.ID, err)
	}
}

// completionRoute is the daemon's completion route over the rig's supervisor, with the rig's
// record of facts as its one handler: what a completion of a claim is taken as, and nothing a
// workflow would make of it.
func (r *retroOutboxRig) completionRoute() func(machine *supervise.Machine, req api.HandoffCompleteRequest) (int, string) {
	grants := credential.New(nil)
	handler := api.NewServer("127.0.0.1", 0, api.Options{
		Steps: config.DesignGateRootIssues.Steps(), Supervisor: r.sup, Project: "legion", DispatchProject: "LEGION",
		Pool: r.pool, Handlers: []intake.Handler{r.facts}, Record: r.records, Grants: grants,
		Dispatch: &fleetHandoffs{handoffs: map[string]dispatch.Handoff{}}, Log: quietLogger(),
	}).Handler
	return func(machine *supervise.Machine, req api.HandoffCompleteRequest) (int, string) {
		r.t.Helper()
		grant, err := grants.Mint(machine.Claim())
		if err != nil {
			r.t.Fatalf("mint the grant of %s: %v", machine.Claim().Token, err)
		}
		req.GrantID = grant.ID
		body, err := json.Marshal(req)
		if err != nil {
			r.t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/legion/v1/handoff/complete", bytes.NewReader(body)))
		return recorder.Code, strings.TrimSpace(recorder.Body.String())
	}
}

// retroReport is the report of a retro after a close the plugin posts: a summary and the rating of
// the plan with its reason, and no verdict.
func retroReport() api.HandoffCompleteRequest {
	rating, reason := 4, "The plan named the tests the change needed."
	return api.HandoffCompleteRequest{Summary: "the retro of the closed run", PlanRating: &rating, PlanReason: &reason}
}

// A retro start is a start the out-of-workflow guard and the lingering-root guard let through, as
// a production check's look is (INFRA-5602's P3): it reaches the implementer the close or the
// linger suspended, resumes it in the session it kept, and delivers the retro's task, where a
// phase start of the same issue finishes without acting. The delivery names no phase, so nothing
// drops it for the phase the issue has left, carries the run, and is the row's own; the same row
// run again is that same delivery. It holds for a closed child whose tree the workflow still
// runs and for a closed root whose tree lingers.
func TestARetroStartResumesTheSuspendedImplementerOfAClosedIssue(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		issues func() []record.Issue
	}{
		{name: "a closed child of a tree the workflow runs", issues: func() []record.Issue { return []record.Issue{retroRunningTree(), retroClosedChild()} }},
		{name: "a closed root whose tree lingers", issues: func() []record.Issue { return []record.Issue{checkRoot()} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issues := tc.issues()
			closed := issues[len(issues)-1]
			r := newRetroOutboxRig(t, issues)
			machine, token := r.claimOf(closed, claim.RoleImplementer, supervise.StateSuspended, supervise.SuspendedByDaemon)
			run := r.openRetroAfterClose(closed, token)

			if err := r.runner.execute(r.ctx, retroStart(t, closed, claim.RoleImplementer, run, 71)); err != nil {
				t.Fatalf("the retro start of the closed issue = %v, want the implementer resumed", err)
			}
			if got := machine.Claim(); got.LastStartRow != 71 || got.State == supervise.StateSuspended {
				t.Fatalf("the implementer after the retro start = %s with last start %d, want resumed by row 71", got.State, got.LastStartRow)
			}
			relaunchedInItsSession(t, r.rt, token)
			heldRetroTask(t, machine, 71, closed, run)

			if err := r.runner.execute(r.ctx, retroStart(t, closed, claim.RoleImplementer, run, 71)); err != nil {
				t.Fatalf("the retro start run again = %v, want its delivery accepted as the same", err)
			}
			relaunchedInItsSession(t, r.rt, token)
			heldRetroTask(t, machine, 71, closed, run)
		})
	}
}

// A retro start reaches an implementer that is retired: its agent's own exit retired it after the
// run, or the tree's close did. The start provisions the workspace again, since the close may
// have removed it, and relaunches the claim in the session it kept, as a check start relaunches
// the role its check records (INFRA-5602's P3), and gives it the retro's task.
func TestARetroStartRelaunchesTheRetiredImplementerInItsKeptSession(t *testing.T) {
	t.Parallel()
	closed := retroClosedChild()
	r := newRetroOutboxRig(t, []record.Issue{retroRunningTree(), closed})
	machine, token := r.claimOf(closed, claim.RoleImplementer, supervise.StateRetired, "")
	run := r.openRetroAfterClose(closed, token)

	if err := r.runner.execute(r.ctx, retroStart(t, closed, claim.RoleImplementer, run, 72)); err != nil {
		t.Fatalf("the retro start of the retired implementer = %v, want it relaunched", err)
	}
	if got := machine.Claim(); got.State == supervise.StateRetired || got.LastStartRow != 72 || got.Session != retroAgentSession {
		t.Fatalf("the implementer after the retro start = %s with last start %d on session %q, want relaunched by row 72 on %q",
			got.State, got.LastStartRow, got.Session, retroAgentSession)
	}
	if got := r.provisioned.Load(); got != 1 {
		t.Fatalf("the start provisioned %d workspaces, want the one the relaunch runs in", got)
	}
	relaunchedInItsSession(t, r.rt, token)
	heldRetroTask(t, machine, 72, closed, run)
}

// A retro start whose agent never launched in the run — the issue closed before its implementer
// had a turn, as a parent that never started a phase of its own does (INFRA-5602's c9) — creates
// the claim, provisions its workspace and spawns it, and gives it the retro's task.
func TestARetroStartCreatesTheImplementerThatNeverLaunched(t *testing.T) {
	t.Parallel()
	closed := retroClosedChild()
	r := newRetroOutboxRig(t, []record.Issue{retroRunningTree(), closed})
	token := mustClaimToken(t, closed.Key, claim.RoleImplementer)
	run := r.openRetroAfterClose(closed, token)

	if err := r.runner.execute(r.ctx, retroStart(t, closed, claim.RoleImplementer, run, 73)); err != nil {
		t.Fatalf("the retro start with no claim = %v, want the implementer created and spawned", err)
	}
	machine, found := r.sup.Machine(token)
	if !found {
		t.Fatal("the retro start created no claim of the implementer")
	}
	if spawns := r.rt.CallsOf("Spawn"); len(spawns) != 1 || spawns[0].Spec.Claim != token {
		t.Fatalf("spawns = %+v, want the implementer spawned once", spawns)
	}
	if got := r.provisioned.Load(); got != 1 {
		t.Fatalf("the start provisioned %d workspaces, want the one the spawn runs in", got)
	}
	heldRetroTask(t, machine, 73, closed, run)
}

// A retro start is fenced on the record of the run it names (INFRA-5602's P2), where a phase start
// is fenced on the generation and the phase: it finishes without acting, and starts, resumes,
// creates and records nothing, when that record says the retro is not the one to run. A start of
// a run with no record (an earlier run of the issue, whose record the later run replaced); a
// retro its report ended; an issue set back to todo, which ended its retro and stands in the
// workflow again; and a start of a role that is not the retro's agent. Every claim the issue
// holds stays as it was, and no claim is made for a role that had none.
func TestARetroStartFinishesWithoutActingWhenItsRetroIsNotOpen(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// fence seeds the record the row meets, given the implementer's token, and answers the
		// start row.
		fence func(r *retroOutboxRig, issue record.Issue, implementer claim.Token) record.OutboxRow
	}{
		{name: "no record of the run it names", fence: func(r *retroOutboxRig, issue record.Issue, implementer claim.Token) record.OutboxRow {
			run := r.openRetroAfterClose(issue, implementer)
			return retroStart(r.t, issue, claim.RoleImplementer, run-1, 74)
		}},
		{name: "the retro its report ended", fence: func(r *retroOutboxRig, issue record.Issue, implementer claim.Token) record.OutboxRow {
			run := r.openRetroAfterClose(issue, implementer)
			r.endRetroAfterClose(issue, run, record.RetroOutcomePassed, record.RetroEndReport)
			return retroStart(r.t, issue, claim.RoleImplementer, run, 74)
		}},
		{name: "the issue back in the workflow", fence: func(r *retroOutboxRig, issue record.Issue, implementer claim.Token) record.OutboxRow {
			run := r.openRetroAfterClose(issue, implementer)
			row := retroStart(r.t, issue, claim.RoleImplementer, run, 74)
			r.endRetroAfterClose(issue, run, record.RetroOutcomeStopped, record.RetroEndTodo)
			again := issue
			again.Phase, again.Status, again.Generation = phase.Admitted, "todo", issue.Generation+1
			putOutboxIssue(r.t, r.pool, r.records, again)
			return row
		}},
		{name: "a start of a role that is not the retro's agent", fence: func(r *retroOutboxRig, issue record.Issue, implementer claim.Token) record.OutboxRow {
			run := r.openRetroAfterClose(issue, implementer)
			return retroStart(r.t, issue, claim.RoleTester, run, 74)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			closed := retroClosedChild()
			r := newRetroOutboxRig(t, []record.Issue{retroRunningTree(), closed})
			implementer, implementerToken := r.claimOf(closed, claim.RoleImplementer, supervise.StateSuspended, supervise.SuspendedByDaemon)
			tester, _ := r.claimOf(closed, claim.RoleTester, supervise.StateSuspended, supervise.SuspendedByDaemon)
			row := tc.fence(r, closed, implementerToken)

			if err := r.runner.execute(r.ctx, row); err != nil {
				t.Fatalf("the retro start = %v, want it finished without acting", err)
			}
			for _, machine := range []*supervise.Machine{implementer, tester} {
				if got := machine.Claim(); got.State != supervise.StateSuspended || got.LastStartRow != 0 || got.Pending != nil {
					t.Fatalf("%s = %s with last start %d and pending %+v, want left suspended with nothing started", got.Token, got.State, got.LastStartRow, got.Pending)
				}
			}
			if launches := len(r.rt.CallsOf("Resume")) + len(r.rt.CallsOf("Spawn")); launches != 0 {
				t.Fatalf("the runtime launched %d processes, want none: resumes %+v, spawns %+v", launches, r.rt.CallsOf("Resume"), r.rt.CallsOf("Spawn"))
			}
			if got := r.provisioned.Load(); got != 0 {
				t.Fatalf("the start provisioned %d workspaces, want none", got)
			}
		})
	}
}

// A retro_bound row ends the retro after a close it bounds when it comes due (INFRA-5602's P5),
// as a check_look row ends the look it bounds: it applies the bound's fact through the
// workflow's handlers, under the event of the issue and the run, so the row run again is a
// duplicate. A row whose retro its report already ended, and a row of a run the issue's later
// run replaced, apply nothing.
func TestARetroBoundRowEndsTheRetroAfterItsClose(t *testing.T) {
	t.Parallel()
	t.Run("the retro is open", func(t *testing.T) {
		closed := retroClosedChild()
		r := newRetroOutboxRig(t, []record.Issue{retroRunningTree(), closed})
		run := r.openRetroAfterClose(closed, mustClaimToken(t, closed.Key, claim.RoleImplementer))
		bound := mustOutboxRow(t, closed.Key, record.RetroBound{Generation: run}, time.Now())
		bound.ID = 75
		for range 2 {
			if err := r.runner.execute(r.ctx, bound); err != nil {
				t.Fatalf("the retro_bound row = %v", err)
			}
		}
		want := intake.RetroBoundEnded{Issue: closed.Key, Generation: run}
		if got := r.facts.facts(); len(got) != 1 || got[0] != want {
			t.Fatalf("facts applied = %+v, want %+v once: the row run again is the same event", got, want)
		}
	})
	for _, tc := range []struct {
		name  string
		bound func(r *retroOutboxRig, issue record.Issue, run uint64) record.RetroBound
	}{
		{name: "the retro its report ended", bound: func(r *retroOutboxRig, issue record.Issue, run uint64) record.RetroBound {
			r.endRetroAfterClose(issue, run, record.RetroOutcomePassed, record.RetroEndReport)
			return record.RetroBound{Generation: run}
		}},
		{name: "a row of a run the later run replaced", bound: func(_ *retroOutboxRig, _ record.Issue, run uint64) record.RetroBound {
			return record.RetroBound{Generation: run - 1}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			closed := retroClosedChild()
			r := newRetroOutboxRig(t, []record.Issue{retroRunningTree(), closed})
			run := r.openRetroAfterClose(closed, mustClaimToken(t, closed.Key, claim.RoleImplementer))
			row := mustOutboxRow(t, closed.Key, tc.bound(r, closed, run), time.Now())
			row.ID = 76
			if err := r.runner.execute(r.ctx, row); err != nil {
				t.Fatalf("the retro_bound row = %v, want it finished without acting", err)
			}
			if got := r.facts.facts(); len(got) != 0 {
				t.Fatalf("facts applied = %+v, want none", got)
			}
		})
	}
}

// The retro after a close survives a restart of the daemon (INFRA-5602's s11). The agent was given
// the retro's task and worked a turn of it; the daemon that comes back builds the claim from the
// store alone, and the claim still serves that retro, so the report its next turn makes is the
// retro's: the completion route takes it, as the one report of that run, and the record of the
// run is still open for the workflow to end with it.
func TestARetroAfterACloseSurvivesARestart(t *testing.T) {
	t.Parallel()
	closed := retroClosedChild()
	r := newRetroOutboxRig(t, []record.Issue{retroRunningTree(), closed})
	machine, token := r.claimOf(closed, claim.RoleImplementer, supervise.StateSuspended, supervise.SuspendedByDaemon)
	run := r.openRetroAfterClose(closed, token)
	if err := r.runner.execute(r.ctx, retroStart(t, closed, claim.RoleImplementer, run, 77)); err != nil {
		t.Fatalf("the retro start = %v, want the implementer resumed", err)
	}
	bringUp(r.ctx, t, machine, token)
	takeHeldTask(r.ctx, t, machine, token)
	endTurn(r.ctx, t, machine, token)

	stored, err := r.st.Claims(r.ctx)
	if err != nil {
		t.Fatalf("read the stored claims: %v", err)
	}
	restarted := newSupervisor(r.ctx, r.st, "legion", t.TempDir(), quietLogger())
	restarted.deps = r.sup.deps
	t.Cleanup(restarted.stop)
	if _, err := restarted.restore(r.ctx, stored); err != nil {
		t.Fatalf("restore the stored claims: %v", err)
	}
	again, found := restarted.Machine(token)
	if !found {
		t.Fatal("the restarted daemon holds no claim of the implementer")
	}
	if got := again.Claim().ServingRetro(); got != supervise.RetroRun(run) {
		t.Fatalf("after the restart the implementer serves retro run %d, want run %d", got, run)
	}

	r.sup = restarted
	complete := r.completionRoute()
	if status, body := complete(again, retroReport()); status != http.StatusOK {
		t.Fatalf("the retro's report after the restart = %d %s, want 200", status, body)
	}
	want := intake.RetroReport{Issue: closed.Key, Claim: token, Generation: run,
		PlanRating: record.PlanRating{Rating: 4, Reason: "The plan named the tests the change needed."}}
	if got := r.facts.facts(); len(got) != 1 || got[0] != want {
		t.Fatalf("facts after the report = %+v, want the one report %+v", got, want)
	}
	if got := r.runRetro(closed, run); got == nil || !got.Open() || got.Agent != token {
		t.Fatalf("the record of run %d after the restart = %+v, want its retro open by %s", run, got, token)
	}
}

// One claim owes both a look of the production check and the retro after the close: a child
// whose pull request merged before its done, under `production_check: implementer`, where the
// close opens both for the implementer (INFRA-5602's s12). Each start is delivered in turn: the
// one queued behind the other's running turn waits until that turn ends, and the report a turn
// makes is routed by what it names and what its claim serves: the look's report names its check
// (checkOpenedAt, which main's #5463 made the one field that selects a check report) and goes to
// the check, the retro's names none and, from the claim serving the retro, goes to the retro —
// whichever start came first.
func TestARetroAndALookOfOneClaimAreServedInTurn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		retroFirst bool
	}{
		{name: "the retro's start first, then the look's", retroFirst: true},
		{name: "the look's start first, then the retro's"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := checkRoot()
			r := newRetroOutboxRig(t, []record.Issue{root})
			check := openCheck(root, "a1b2c3d4")
			putProductionCheck(t, r.pool, r.records, check)
			machine, token := r.claimOf(root, claim.RoleImplementer, supervise.StateSuspended, supervise.SuspendedByDaemon)
			run := r.openRetroAfterClose(root, token)
			complete := r.completionRoute()

			retroRow := retroStart(t, root, claim.RoleImplementer, run, 78)
			lookRow := mustOutboxRow(t, root.Key, record.SuperviseRequest{Op: "start", Tree: root.Tree, Role: claim.RoleImplementer,
				Generation: root.Generation, CheckLook: 1, CheckOpenedAt: check.OpenedAt, Task: "Production check of " + root.Key}, time.Now())
			lookRow.ID = 79
			first, second := lookRow, retroRow
			if tc.retroFirst {
				first, second = retroRow, lookRow
			}
			opened := check.OpenedAt
			lookReport := api.HandoffCompleteRequest{Summary: "production carries the merge", Verdict: "pass", CheckOpenedAt: &opened}
			report := func(row record.OutboxRow) api.HandoffCompleteRequest {
				if row.ID == retroRow.ID {
					return retroReport()
				}
				return lookReport
			}
			takenAs := func(row record.OutboxRow, fact intake.Fact) bool {
				if row.ID == retroRow.ID {
					_, ok := fact.(intake.RetroReport)
					return ok
				}
				_, ok := fact.(intake.CheckReport)
				return ok
			}

			if err := r.runner.execute(r.ctx, first); err != nil {
				t.Fatalf("the first start = %v, want the implementer resumed with its task", err)
			}
			bringUp(r.ctx, t, machine, token)
			takeHeldTask(r.ctx, t, machine, token)
			// The second start comes while the first's turn runs: its task waits behind that turn.
			if err := r.runner.execute(r.ctx, second); err != nil {
				t.Fatalf("the second start behind the running turn = %v, want its task queued", err)
			}
			if p := machine.Claim().Pending; p == nil || p.ID != fmt.Sprintf("outbox:%d", second.ID) || !p.ConfirmedAt.IsZero() {
				t.Fatalf("the implementer's pending delivery = %+v, want the second start's task queued unconfirmed behind the running turn", p)
			}
			if status, body := complete(machine, report(first)); status != http.StatusOK {
				t.Fatalf("the first turn's report = %d %s, want 200", status, body)
			}
			if got := r.facts.facts(); len(got) != 1 || !takenAs(first, got[0]) {
				t.Fatalf("facts after the first turn's report = %+v, want it taken as the report of its own task (row %d)", got, first.ID)
			}

			endTurn(r.ctx, t, machine, token)
			awaitSent(t, machine)
			takeHeldTask(r.ctx, t, machine, token)
			if status, body := complete(machine, report(second)); status != http.StatusOK {
				t.Fatalf("the second turn's report = %d %s, want 200", status, body)
			}
			if got := r.facts.facts(); len(got) != 2 || !takenAs(second, got[1]) {
				t.Fatalf("facts after the second turn's report = %+v, want it taken as the report of its own task (row %d)", got, second.ID)
			}
		})
	}
}

// A tree past its linger holds a retro owed (INFRA-5602's P8, s18): a root parked in the backlog
// lingers, its linger's end retires every claim and confirms the tree's cleanup, and a person
// then sets it done, whose run had no retro. The retro's start reopens the tree's lifecycle for
// it, at the next epoch, provisions the workspace the linger removed and relaunches the retired
// implementer in the session it kept. The retro is the tree's only open one: its agent publishes a
// finding to the root architect's role topic and reports at once (P6). The report ends the retro
// and retires its agent, and leaves the tree's close to the hour Envoy keeps the finding's message,
// so the wake the listener reports for it relaunches the architect the linger's end retired, in its
// kept session, in the reopened epoch, and the woken process is sent the wake's task. Once that
// hour has passed the tree closes again: the architect is retired, the reopened epoch's cleanup is
// confirmed and the workspace the retro ran in is removed.
func TestARetroAfterTheLingerReopensAndClosesTheTree(t *testing.T) {
	t.Parallel()
	until := time.Now().Add(-time.Minute)
	root := checkRoot()
	root.Status, root.LingerUntil = "backlog", &until
	r := newRetroOutboxRig(t, []record.Issue{root})
	podRecords, err := r.st.PodRecordsOf("LEGION")
	if err != nil {
		t.Fatalf("read the project's pod records: %v", err)
	}
	clock := time.Now()
	engine := workflow.New(r.records, workflow.Config{Project: "legion", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(),
		Linger: time.Hour, Pods: podRecords, Clock: func() time.Time { return clock }}, quietLogger())
	r.runner.handlers = append(r.runner.handlers, engine)
	machine, token := r.claimOf(root, claim.RoleImplementer, supervise.StateSuspended, supervise.SuspendedByDaemon)
	architect, architectToken := r.claimOfSession(root, claim.RoleArchitect, supervise.StateSuspended, supervise.SuspendedByDaemon, retroArchitectSession)
	drain := func(what string) {
		t.Helper()
		for range 3 {
			if err := r.runner.RunOnce(r.ctx); err != nil {
				t.Fatalf("run %s: %v", what, err)
			}
		}
	}

	// The linger ends: its close retires the implementer and the architect, removes the workspace
	// and confirms the tree's cleanup.
	applyFact(t, r.pool, "outbox", "linger:LEGION-208:1", intake.LingerExpired{Issue: root.Key, Generation: root.Generation}, engine)
	drain("the linger's end")
	// A close row that waited on the census (cleanup waits for every claim to retire) runs again
	// on its backoff; let each finish now, so none is left to run in the reopened epoch.
	for range 10 {
		var left int
		if err := r.pool.QueryRow(r.ctx, `select count(*) from outbox where kind = 'supervise' and payload->>'op' = 'tree_close'`).Scan(&left); err != nil {
			t.Fatalf("count the close rows left: %v", err)
		}
		if left == 0 {
			break
		}
		if _, err := r.pool.Exec(r.ctx, `update outbox set next_at = now() where kind = 'supervise' and payload->>'op' = 'tree_close'`); err != nil {
			t.Fatalf("bring the close rows due: %v", err)
		}
		drain("the close rows that waited")
	}
	if got, gotArchitect := machine.Claim().State, architect.Claim().State; got != supervise.StateRetired || gotArchitect != supervise.StateRetired {
		t.Fatalf("the implementer and the architect after the linger's end = %s and %s, want both retired", got, gotArchitect)
	}
	if lc := r.lifecycle(root.Tree); !lc.CleanupStarted || lc.CleanupConfirmedAt.IsZero() || lc.Epoch != 1 {
		t.Fatalf("the lifecycle after the linger's end = %+v, want epoch 1's cleanup confirmed", lc)
	}
	removedAtLinger := r.removed.Load()

	// A person sets the parked root done, and the close owes its run the retro.
	root.Status = "done"
	putOutboxIssue(t, r.pool, r.records, root)
	run := r.openRetroAfterClose(root, token)
	// The start's row id is below the ids of the rows the retro's end writes, as a real start's is:
	// the close of the agent's claim is a stop that a newer start would supersede (workflow.StopActs).
	const startRow = 1
	if err := r.runner.execute(r.ctx, retroStart(t, root, claim.RoleImplementer, run, startRow)); err != nil {
		t.Fatalf("the retro start in the tree past its linger = %v, want the lifecycle reopened and the implementer relaunched", err)
	}
	if lc := r.lifecycle(root.Tree); !lc.OpenUnderWorkflow() || lc.Epoch != 2 {
		t.Fatalf("the lifecycle after the retro's start = %+v, want epoch 2 open under the workflow", lc)
	}
	if got := machine.Claim(); got.State == supervise.StateRetired || got.TreeEpoch != 2 || got.LastStartRow != startRow {
		t.Fatalf("the implementer after the retro's start = %s in epoch %d with last start %d, want relaunched by row %d in epoch 2",
			got.State, got.TreeEpoch, got.LastStartRow, startRow)
	}
	if got := r.provisioned.Load(); got != 1 {
		t.Fatalf("the retro's start provisioned %d workspaces, want the one the linger removed, again", got)
	}
	relaunchedInItsSession(t, r.rt, token)
	heldRetroTask(t, machine, startRow, root, run)
	// The agent's process comes up, as it does in a deployment: its report then finds a claim to
	// close, which a launching one is not.
	bringUp(r.ctx, t, machine, token)

	// The retro's agent reports at once after it publishes its finding: the report ends the tree's
	// last open retro, and the listener's report of the finding's message wakes the architect.
	applyFact(t, r.pool, "api", fmt.Sprintf("retro-after-close:%s:%d", root.Key, run), intake.RetroReport{Issue: root.Key, Claim: token,
		Generation: run, PlanRating: record.PlanRating{Rating: 4, Reason: "The plan fit the work."}}, engine)
	applyFact(t, r.pool, "listener", "finding-of-"+root.Key, intake.ListenerWake{Kind: intake.ListenerWakeRole, Role: string(architectToken),
		At: time.Now(), Report: "finding-of-" + root.Key}, engine)
	drain("the retro's end and the finding's wake")
	if got := machine.Claim().State; got != supervise.StateRetired {
		t.Fatalf("the implementer after the retro's end = %s, want retired", got)
	}
	if got := architect.Claim(); got.State == supervise.StateRetired || got.TreeEpoch != 2 || got.Pending == nil || !got.Pending.RetroWake() {
		t.Fatalf("the architect after the finding's wake = %s in epoch %d holding %+v, want it relaunched in epoch 2 holding the wake's task",
			got.State, got.TreeEpoch, got.Pending)
	}
	relaunchedInSession(t, r.rt, architectToken, retroArchitectSession)
	if lc := r.lifecycle(root.Tree); !lc.OpenUnderWorkflow() || lc.Epoch != 2 {
		t.Fatalf("the lifecycle within the hour after the retro's end = %+v, want epoch 2 still open: the tree's close waits that hour out", lc)
	}
	readyUpAs(r.ctx, t, architect, architectToken, retroArchitectSession)
	awaitSent(t, architect)
	if prompts := r.promptsOf(architectToken); len(prompts) != 1 || !strings.Contains(prompts[0].Message, string(architectToken)) {
		t.Fatalf("the woken architect's process was sent %+v, want the one wake task naming its role topic token %s", prompts, architectToken)
	}

	// The architect's turn on the finding ends and the idle release stops it, which the root's pod
	// suspension, ahead of the tree's close in the root's lane, waits for.
	if err := architect.Handle(r.ctx, supervise.RequestSuspend{Claim: architectToken, Reason: "the issue's pod is released", By: supervise.SuspendedByReleased}); err != nil {
		t.Fatalf("release the woken architect: %v", err)
	}
	drain("the root's pod suspension")

	// The hour passes, and the tree, past its linger, closes again.
	clock = clock.Add(pods.RetroWakeWindow + time.Minute)
	// Each row that waited on another runs again on its backoff, which the test brings forward.
	for range 10 {
		if _, err := r.pool.Exec(r.ctx, `update outbox set next_at = now() where next_at > now()`); err != nil {
			t.Fatalf("bring the tree's close and the rows ahead of it due: %v", err)
		}
		drain("the tree's close the hour after its last retro ended")
		if architect.Claim().State == supervise.StateRetired {
			break
		}
	}
	if got := architect.Claim().State; got != supervise.StateRetired {
		t.Fatalf("the architect once the hour has passed = %s, want retired: the tree closed again", got)
	}
	if lc := r.lifecycle(root.Tree); !lc.CleanupStarted || lc.CleanupConfirmedAt.IsZero() || lc.Epoch != 2 {
		t.Fatalf("the lifecycle once the hour has passed = %+v, want epoch 2's cleanup confirmed", lc)
	}
	if got := r.removed.Load(); got <= removedAtLinger {
		t.Fatalf("the tree's close removed no workspace (%d removals, %d at the linger's end), want the one the retro ran in removed", got, removedAtLinger)
	}
}

// The agent the close keeps for the retro is mid-turn in another phase (INFRA-5602's P3, s20): a
// person sets a root done while its implementer's turn of implementing runs. The close keeps that
// claim working, so its turn finishes, and the retro's start, which the outbox runs behind the
// running turn, queues the retro's task on the claim rather than stopping or replacing the turn.
// Once the turn ends the retro's task is sent, and the next turn is on it. The issue is recorded
// mid-run, in planning, with its run's start on record, and the planner's completion moves it to
// implementing, so the close reads that run as one that started and had no retro.
func TestARetroTaskFollowsTheRunningTurnOfTheSparedAgent(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	ctx := context.Background()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-208",
		Title: "Workflow", Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 5}
	putOutboxIssue(t, pool, records, issue)
	// An issue recorded mid-run stands for one the migration filled: its run's start is on record.
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		return records.MarkRunStarted(ctx, tx, issue.Key, issue.Generation, issue.Tree, time.Now().Add(-time.Hour).UTC())
	}); err != nil {
		t.Fatalf("mark the run of %s started: %v", issue.Key, err)
	}
	holdSlot(t, pool, issue.Key)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.PhaseHolds = (&workflowRuntime{pool: pool, records: records}).phaseHolds
	planner := mustClaimToken(t, issue.Key, claim.RolePlanner)
	implementer := mustClaimToken(t, issue.Key, claim.RoleImplementer)
	sup.deps.Conns.(*fake.Conns).Register(planner, fake.NewConn())
	sup.deps.Conns.(*fake.Conns).Register(implementer, fake.NewConn())
	plannerMachine, _, err := sup.Create(ctx, supervise.Claim{Token: planner, Project: "legion", Tree: issue.Tree, Issue: issue.Key, Role: claim.RolePlanner, State: supervise.StateQueued}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := plannerMachine.Handle(ctx, supervise.RequestSpawn{Claim: planner}); err != nil {
		t.Fatalf("spawn the planner: %v", err)
	}
	generation := plannerMachine.Claim().Generation
	for _, ev := range []supervise.Event{
		supervise.StreamHello{Claim: planner, Generation: generation},
		supervise.RequestRegister{Claim: planner, Generation: generation, Session: "ses-plan", SessionFile: "/tmp/plan.jsonl", CapabilityHash: []byte("planner-capability")},
		supervise.RequestReady{Claim: planner, Generation: generation, Session: "ses-plan"},
		supervise.RequestDeliver{Claim: planner, Task: "Continue Workflow. Issue: LEGION-208. Phase: planning.", Phase: phase.Planning, Generation: 1},
	} {
		if err := plannerMachine.Handle(ctx, ev); err != nil {
			t.Fatalf("handle %T: %v", ev, err)
		}
	}
	plannerMachine.Wait()
	if err := plannerMachine.Handle(ctx, supervise.StreamTurnStart{Claim: planner}); err != nil {
		t.Fatalf("start the planning turn: %v", err)
	}
	engine := workflow.New(records, workflow.Config{Project: "legion", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(), Linger: 24 * time.Hour}, quietLogger())
	clock := time.Now()
	runner := &outbox{
		dispatchProject: "LEGION",
		pool:            pool, records: records, supervisor: sup, trees: outboxTreeStore(t, pool, issue.Tree), tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
		dispatch: &outboxDispatch{issue: dispatch.Issue{Key: issue.Key, Status: "in_progress"}}, notices: &outboxPublisher{},
		handlers: []intake.Handler{engine}, now: func() time.Time { return clock }, log: quietLogger(),
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
		},
	}
	run := func(what string) {
		t.Helper()
		clock = clock.Add(time.Hour)
		if err := runner.RunOnce(ctx); err != nil {
			t.Fatalf("run %s: %v", what, err)
		}
	}

	// The planner completes planning: the run's first transition, into implementing, starts the
	// implementer, which takes its implementing task and begins its turn.
	applyFact(t, pool, "api", "handoff:planner:planning", intake.HandoffComplete{Generation: 1, Issue: issue.Key, Role: claim.RolePlanner,
		Claim: planner, HandoffRecord: "plan-1", HandoffVersion: 1}, engine)
	run("the transition's effects")
	machine, started := sup.Machine(implementer)
	if !started {
		t.Fatal("the move into implementing started no implementer")
	}
	machine.Wait()
	generation = machine.Claim().Generation
	for _, ev := range []supervise.Event{
		supervise.StreamHello{Claim: implementer, Generation: generation},
		supervise.RequestRegister{Claim: implementer, Generation: generation, Session: "ses-implement", SessionFile: "/tmp/implement.jsonl",
			CapabilityHash: []byte("implementer-capability")},
		supervise.RequestReady{Claim: implementer, Generation: generation, Session: "ses-implement"},
	} {
		if err := machine.Handle(ctx, ev); err != nil {
			t.Fatalf("handle %T: %v", ev, err)
		}
	}
	awaitSent(t, machine)
	implementing := machine.Claim().Pending
	if implementing.Phase != phase.Implementing {
		t.Fatalf("the implementer's first task = %+v, want its implementing task", implementing)
	}
	takeHeldTask(ctx, t, machine, implementer)

	// A person sets the issue done while that turn runs.
	applyFact(t, pool, "dispatch", "ev-done", intake.DispatchIssue{Key: issue.Key, ResolvedRepo: "acme/widgets", Seq: 6, Type: "issue.closed",
		Status: "done", Title: issue.Title, Rank: "U"}, engine)
	run("the close's effects")
	held := machine.Claim()
	if held.State != supervise.StateWorking {
		t.Fatalf("the implementer after the close is %s, want still working the turn the close found it in", held.State)
	}
	for _, call := range rt.Calls() {
		if (call.Method == "Suspend" || call.Method == "Release") && call.Locator.Claim == implementer {
			t.Fatalf("the close stopped the implementer (%s), want it kept for the retro", call.Method)
		}
	}
	p := held.Pending
	if p == nil || p.ID == implementing.ID || p.RetroRun != 1 || p.Phase != "" || !p.ConfirmedAt.IsZero() ||
		!strings.HasPrefix(p.Task, retroTaskLine(issue)) {
		t.Fatalf("the implementer's pending delivery after the close = %+v, want the retro's task of run 1, of no phase, queued unconfirmed behind the running turn", p)
	}

	// The turn ends, and the retro's task is the next one sent and taken.
	endTurn(ctx, t, machine, implementer)
	awaitSent(t, machine)
	takeHeldTask(ctx, t, machine, implementer)
	if got := machine.Claim(); got.ServingRetro() != 1 || got.Pending == nil || got.Pending.ID != p.ID {
		t.Fatalf("the implementer's next turn serves retro run %d on %+v, want run 1 on the retro's task %s", got.ServingRetro(), got.Pending, p.ID)
	}
}

// A retro wake of a closed, lingering tree's root architect names no run and holds no decisions,
// so supervise's asksRoot must not hold it behind Deps.RootHears while the tree lingers
// (INFRA-5602's R2): once the outbox resumes the architect's suspended claim and its process comes
// up ready, the wake task is sent to that process and its delivery names generation 0.
func TestARetroWakeOfTheRootArchitectOfALingeringTreeIsSentNotHeld(t *testing.T) {
	t.Parallel()
	root := checkRoot()
	r := newRetroOutboxRig(t, []record.Issue{root})
	machine, token := r.claimOf(root, claim.RoleArchitect, supervise.StateSuspended, supervise.SuspendedByDaemon)
	r.openRetroAfterClose(root, mustClaimToken(t, root.Key, claim.RoleImplementer))
	conn, ok := r.sup.deps.Conns.Conn(token)
	if !ok {
		t.Fatalf("no fake connection registered for %s", token)
	}
	fakeConn := conn.(*fake.Conn)

	wakeTask := "Your tree closed, and the daemon stopped your session with it. A retro Legion runs after the close of an issue of your tree is open."
	row := mustOutboxRow(t, root.Key, record.SuperviseRequest{Op: "start", Tree: root.Tree, Role: claim.RoleArchitect,
		Generation: root.Generation, WakeKind: pods.WakeRoleMessage, RetroWake: true, Task: wakeTask}, time.Now())
	row.ID = 79

	if err := r.runner.execute(r.ctx, row); err != nil {
		t.Fatalf("the retro wake of %s's architect = %v, want the architect resumed", root.Key, err)
	}
	if got := machine.Claim(); got.LastStartRow != 79 || got.State == supervise.StateSuspended {
		t.Fatalf("the architect after the retro wake = %s with last start %d, want resumed by row 79", got.State, got.LastStartRow)
	}
	relaunchedInItsSession(t, r.rt, token)

	// Bring the resumed process up through hello, register and ready, then wait for any in-flight
	// send goroutine to settle. A delivery held by asksRoot starts no send at all.
	machine.Wait()
	launch := machine.Claim().Generation
	for _, ev := range []supervise.Event{
		supervise.StreamHello{Claim: token, Generation: launch},
		supervise.RequestRegister{Claim: token, Generation: launch, Session: retroAgentSession, SessionFile: retroAgentSessionFile,
			CapabilityHash: []byte("capability-of-" + retroAgentSession)},
		supervise.RequestReady{Claim: token, Generation: launch, Session: retroAgentSession},
	} {
		if err := machine.Handle(r.ctx, ev); err != nil {
			t.Fatalf("handle %T for %s: %v", ev, token, err)
		}
	}
	machine.Wait()

	p := machine.Claim().Pending
	if p == nil || p.ID != supervise.RetroWakeTaskID("outbox:79") || !p.RetroWake() || p.Phase != "" || p.Task != wakeTask {
		t.Fatalf("the architect's pending delivery = %+v, want outbox:79 with no phase carrying the wake task", p)
	}
	if p.Generation != 0 {
		t.Errorf("the retro wake delivery names generation %d, want 0 (a task of no run)", p.Generation)
	}
	if prompts := fakeConn.Prompts(); len(prompts) != 1 || prompts[0].DeliveryID != p.ID || prompts[0].Message != wakeTask {
		t.Errorf("the resumed architect's process received prompts %+v, want the one wake task outbox:79 sent rather than held", prompts)
	}
	if p.DeliveredAt.IsZero() {
		t.Errorf("the retro wake delivery %s has zero DeliveredAt, want it sent to the resumed architect", p.ID)
	}
}

// retroWakeTask is a task the wake of a root architect for a retro's finding carries in these
// tests (workflow's retroArchitectWakeTask writes the daemon's own).
const retroWakeTask = "Your tree closed, and the daemon stopped your session with it. Somebody published a message to your role topic."

// retroWakeRow is the start of the wake of root's architect for a retro's finding, row id: the
// architect's start of the role message's kind at the root's generation, marked as a retro's wake
// (record.SuperviseRequest.RetroWake), as the workflow's retroWake queues it.
func retroWakeRow(t *testing.T, root record.Issue, id int64) record.OutboxRow {
	t.Helper()
	row := mustOutboxRow(t, root.Key, record.SuperviseRequest{Op: "start", Tree: root.Tree, Role: claim.RoleArchitect,
		Generation: root.Generation, WakeKind: pods.WakeRoleMessage, RetroWake: true, Task: retroWakeTask}, time.Now())
	row.ID = id
	return row
}

// The outbox's start of a root architect's wake for a retro's finding is fenced on the retros
// after a close of its tree (INFRA-5602's P6): it resumes the architect while one is open, and for
// the hour Envoy keeps the message after the tree's last one ended (pods.RetroWakeWindow), since
// the retro's agent reports a moment after it publishes and its report can end that retro before
// this row runs; in a tree past its linger too, whose close waits that hour out. It finishes
// without acting, and starts and resumes nothing, once that hour has passed, and in a tree that
// runs again.
func TestARetroWakeStartIsFencedOnTheHourAfterTheTreesLastRetroEnded(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// fence ends the root's retro, given its run, or leaves it open, and may record the root
		// anew; it answers the root as the row meets it.
		fence func(r *retroOutboxRig, root record.Issue, run uint64) record.Issue
		acts  bool
	}{
		{name: "a retro of the tree is open", acts: true, fence: func(_ *retroOutboxRig, root record.Issue, _ uint64) record.Issue { return root }},
		{name: "the tree's last retro ended a minute ago", acts: true, fence: func(r *retroOutboxRig, root record.Issue, run uint64) record.Issue {
			r.endRetroAfterCloseAt(root, run, time.Now().Add(-time.Minute), record.RetroOutcomePassed, record.RetroEndReport)
			return root
		}},
		{name: "the tree's last retro ended over an hour ago", fence: func(r *retroOutboxRig, root record.Issue, run uint64) record.Issue {
			r.endRetroAfterCloseAt(root, run, time.Now().Add(-time.Hour-time.Minute), record.RetroOutcomePassed, record.RetroEndReport)
			return root
		}},
		{name: "the tree is past its linger, its last retro ended a minute ago", acts: true, fence: func(r *retroOutboxRig, root record.Issue, run uint64) record.Issue {
			r.endRetroAfterCloseAt(root, run, time.Now().Add(-time.Minute), record.RetroOutcomePassed, record.RetroEndReport)
			until := time.Now().Add(-30 * time.Second)
			root.LingerUntil = &until
			putOutboxIssue(r.t, r.pool, r.records, root)
			return root
		}},
		{name: "the tree runs again", fence: func(r *retroOutboxRig, root record.Issue, run uint64) record.Issue {
			r.endRetroAfterCloseAt(root, run, time.Now().Add(-time.Minute), record.RetroOutcomeStopped, record.RetroEndTodo)
			root.Phase, root.Status, root.LingerUntil = phase.Planning, "in_progress", nil
			putOutboxIssue(r.t, r.pool, r.records, root)
			return root
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := checkRoot()
			r := newRetroOutboxRig(t, []record.Issue{root})
			architect, token := r.claimOf(root, claim.RoleArchitect, supervise.StateSuspended, supervise.SuspendedByDaemon)
			run := r.openRetroAfterClose(root, mustClaimToken(t, root.Key, claim.RoleImplementer))
			root = tc.fence(r, root, run)

			if err := r.runner.execute(r.ctx, retroWakeRow(t, root, 79)); err != nil {
				t.Fatalf("the retro wake of %s's architect = %v, want it run", root.Key, err)
			}
			got := architect.Claim()
			if !tc.acts {
				if got.State != supervise.StateSuspended || got.LastStartRow != 0 || got.Pending != nil || len(r.rt.CallsOf("Resume")) != 0 {
					t.Fatalf("the architect after the retro wake = %s with last start %d and pending %+v, resumes %+v; want it left suspended with nothing started",
						got.State, got.LastStartRow, got.Pending, r.rt.CallsOf("Resume"))
				}
				return
			}
			if got.State == supervise.StateSuspended || got.LastStartRow != 79 || got.Pending == nil || got.Pending.Task != retroWakeTask {
				t.Fatalf("the architect after the retro wake = %s with last start %d and pending %+v, want it resumed by row 79 holding the wake's task",
					got.State, got.LastStartRow, got.Pending)
			}
			relaunchedInItsSession(t, r.rt, token)
		})
	}
}

// s24: the retro after the close of a root is its tree's only open retro (INFRA-5602's P6, P7). Its
// agent publishes a finding to the root architect's role topic and reports a moment later, so the
// report ends the tree's last open retro either before the outbox has run the wake the listener's
// report of that message queued, or after that wake resumed the architect. The wake stands either
// way, for the hour Envoy keeps the message: the outbox's start of it resumes the architect's
// suspended claim in its kept session, the report queues no suspension of the architect, which
// would stand above the wake's start and stop it at its ready, and once its process is up the
// architect is sent the wake's task and is not stopped.
func TestARootArchitectWithAQueuedRetroWakeIsNotSuspended(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		wakeFirst bool
	}{
		{name: "the report before the outbox runs the wake"},
		{name: "the report after the wake resumed the architect", wakeFirst: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := checkRoot()
			r := newRetroOutboxRig(t, []record.Issue{root})
			engine := r.wakeEngine()
			architect, token := r.claimOf(root, claim.RoleArchitect, supervise.StateSuspended, supervise.SuspendedByDaemon)
			agent := mustClaimToken(t, root.Key, claim.RoleImplementer)
			run := r.openRetroAfterClose(root, agent)
			drain := func(what string) {
				t.Helper()
				for range 2 {
					if err := r.runner.RunOnce(r.ctx); err != nil {
						t.Fatalf("run %s: %v", what, err)
					}
				}
			}

			applyFact(t, r.pool, "listener", "finding-of-"+root.Key, intake.ListenerWake{Kind: intake.ListenerWakeRole, Role: string(token),
				At: time.Now(), Report: "finding-of-" + root.Key}, engine)
			if tc.wakeFirst {
				drain("the wake")
			}
			applyFact(t, r.pool, "api", fmt.Sprintf("retro-after-close:%s:%d", root.Key, run), intake.RetroReport{Issue: root.Key, Claim: agent,
				Generation: run, PlanRating: record.PlanRating{Rating: 4, Reason: "The plan fit the work."}}, engine)
			drain("the rows of the finding and the report")

			if got := architect.Claim(); got.State == supervise.StateSuspended || got.Pending == nil {
				t.Fatalf("the root architect after its retro's finding and report = %s holding %+v, want it resumed by the wake, holding the wake's task",
					got.State, got.Pending)
			}
			relaunchedInItsSession(t, r.rt, token)
			readyUp(r.ctx, t, architect, token)
			if got := architect.Claim(); got.State == supervise.StateSuspended || got.Pending == nil {
				t.Fatalf("the root architect at its ready = %s holding %+v, want it up with the wake's task: no suspension stops it before its task",
					got.State, got.Pending)
			}
			awaitSent(t, architect)
			if prompts := r.promptsOf(token); len(prompts) != 1 || !strings.Contains(prompts[0].Message, string(token)) {
				t.Fatalf("the woken architect's process was sent %+v, want the one wake task naming its role topic token %s", prompts, token)
			}
		})
	}
}

// s25: a root architect a retro's wake resumed is stopped again before it takes the wake's task,
// which then waits on its suspended claim, as a task of no phase does. The task says a retro after
// a close of the tree ran within the last hour and that somebody published a message to the
// architect's role topic, which the tree's readmission makes stale: the architect comes back for
// the readmission's own start, and the wake's task is dropped rather than sent
// (workflowRuntime.retroWakeHolds), so its process gets the readmission's task alone.
func TestARetroWakeTaskIsNotSentAtTheTreesReadmission(t *testing.T) {
	t.Parallel()
	root := checkRoot()
	r := newRetroOutboxRig(t, []record.Issue{root})
	architect, token := r.claimOf(root, claim.RoleArchitect, supervise.StateSuspended, supervise.SuspendedByDaemon)
	r.openRetroAfterClose(root, mustClaimToken(t, root.Key, claim.RoleImplementer))
	if err := r.runner.execute(r.ctx, retroWakeRow(t, root, 79)); err != nil {
		t.Fatalf("the retro wake of %s's architect = %v, want the architect resumed", root.Key, err)
	}
	suspend := mustOutboxRow(t, root.Key, record.SuperviseRequest{Op: "suspend", Tree: root.Tree, Role: claim.RoleArchitect,
		Generation: root.Generation, Reason: "the tree of " + root.Key + " lingers"}, time.Now())
	suspend.ID = 80
	// The suspension of a launch is held until the agent is ready, and acts then, before any task
	// goes (supervise's holdSuspendUntilReady); the row is answered that it is held.
	_ = r.runner.execute(r.ctx, suspend)
	readyUp(r.ctx, t, architect, token)
	if got := architect.Claim(); got.State != supervise.StateSuspended || got.Pending == nil || got.Pending.Task != retroWakeTask {
		t.Fatalf("the architect after its suspension = %s holding %+v, want it suspended holding the wake's task", got.State, got.Pending)
	}
	sent := len(r.promptsOf(token))

	// The tree is readmitted: its root runs again at its next generation, and the readmission
	// starts the root's architect with a task of its own.
	readmitted := root
	readmitted.Phase, readmitted.Status, readmitted.Generation, readmitted.LingerUntil = phase.Planning, "in_progress", root.Generation+1, nil
	putOutboxIssue(t, r.pool, r.records, readmitted)
	const readmissionTask = "Continue root. Issue: LEGION-208. The tree runs again."
	start := mustOutboxRow(t, root.Key, record.SuperviseRequest{Op: "start", Tree: root.Tree, Role: claim.RoleArchitect,
		Generation: readmitted.Generation, Task: readmissionTask}, time.Now())
	start.ID = 81
	// The claim still holds the wake's task, which its start's task waits behind until the claim's
	// ready decides it; the row runs again then, as its backoff runs it.
	_ = r.runner.execute(r.ctx, start)
	readyUp(r.ctx, t, architect, token)
	if err := r.runner.execute(r.ctx, start); err != nil {
		t.Fatalf("the readmission's start of the architect = %v, want its task delivered", err)
	}
	awaitSent(t, architect)
	var tasks []string
	for _, prompt := range r.promptsOf(token)[sent:] {
		tasks = append(tasks, prompt.Message)
	}
	if len(tasks) != 1 || tasks[0] != readmissionTask {
		t.Fatalf("the architect's process was sent %q after the readmission, want the readmission's task alone and never the stale wake's", tasks)
	}
}

// retroIdleChild is retroClosedChild at work in its implementing phase on its second run, before
// its close: the issue of the idle rig newRetroIdleRig builds.
func retroIdleChild() record.Issue {
	child := retroClosedChild()
	child.Phase, child.Status, child.LastDispatchSeq = phase.Implementing, "in_progress", 1
	return child
}

// newRetroIdleRig is the idle rig over retroIdleChild under the root of a tree the workflow still
// runs (retroRunningTree), with a release of the child's pod marked and its implementer released by
// that release, whose teardown is held back so the test runs it (holdTeardownBack). A person then
// closed the child, whose run had no retro, and the close kept the released implementer as the
// agent of the retro after it (workflow's openRetroAfterClose). It answers the closed child and the
// run of that retro.
func newRetroIdleRig(t *testing.T) (*idleRig, record.Issue, uint64) {
	t.Helper()
	ctx := context.Background()
	rig := newIdleRigOf(t, retroIdleChild(), retroRunningTree())
	rig.w.releaseIdlePod(ctx, rig.issue.Key)
	holdTeardownBack(t, rig)
	rig.drain(t)
	if got := rig.machine.Claim(); !got.Released() || !rig.marked(t) {
		t.Fatalf("the implementer = %s by %s and the mark stands = %t, want it released under a release still running",
			got.State, got.SuspendedBy, rig.marked(t))
	}
	closed := rig.issue
	closed.Phase, closed.Status = phase.Done, "done"
	putOutboxIssue(t, rig.pool, rig.issues, closed)
	run := closed.Generation
	started := time.Now().Add(-time.Minute).UTC().Truncate(time.Microsecond)
	if err := store.Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := rig.issues.MarkRunStarted(ctx, tx, closed.Key, run, closed.Tree, started.Add(-24*time.Hour)); err != nil {
			return err
		}
		if err := rig.issues.MarkRetroStarted(ctx, tx, closed.Key, run, closed.Tree, started); err != nil {
			return err
		}
		return rig.issues.OpenRetroAfterClose(ctx, tx, closed.Key, run, closed.Tree, started, started.Add(workflow.RetroAfterCloseBound), rig.token, 0)
	}); err != nil {
		t.Fatalf("open the retro after the close of %s: %v", closed.Key, err)
	}
	return rig, closed, run
}

// wakeRetroAgent is a role message to the retro's agent while the release stands, as the
// workflow's wake takes it for that agent (wakeReleased): the resume, and the start that carries
// the wake's kind, its detail, its task and the retro's run (RetroWakeOf), which waits for the
// release's teardown.
func wakeRetroAgent(t *testing.T, rig *idleRig, closed record.Issue, run uint64) {
	t.Helper()
	if taken, err := rig.st.TakePodWake(context.Background(), "legion", closed.Key, rig.token, pods.WakeRoleMessage, time.Now().UTC(), 0); err != nil || !taken {
		t.Fatalf("take the role message's wake = %t, %v; want it taken", taken, err)
	}
	enqueueOutbox(t, rig.pool, rig.issues, mustOutboxRow(t, closed.Key, record.SuperviseRequest{Op: "start", Tree: closed.Tree,
		Role: claim.RoleImplementer, Generation: closed.Generation, Task: workflow.WakeTask(pods.WakeRoleMessage, string(rig.token), workflow.StoppedByRelease),
		WakeKind: pods.WakeRoleMessage, WakeDetail: string(rig.token), RetroRun: run}, time.Now().UTC()))
}

// implementerStarts is each start of the rig's implementer the outbox holds for issue, in id order.
func implementerStarts(t *testing.T, rig *idleRig, issue string) []record.SuperviseRequest {
	t.Helper()
	var starts []record.SuperviseRequest
	for _, start := range startRequestsOf(t, rig.pool, issue) {
		if start.Role == claim.RoleImplementer {
			starts = append(starts, start)
		}
	}
	return starts
}

// resumedOnceInThePodThatStayed drains the rig's issue with every row of it due, and fails unless
// that brought the rig's implementer back from `released`, by one resume of its process.
func resumedOnceInThePodThatStayed(t *testing.T, rig *idleRig) {
	t.Helper()
	resumes := len(rig.rt.CallsOf("Resume"))
	if _, err := rig.pool.Exec(context.Background(), `update outbox set next_at = now() where issue = $1`, rig.issue.Key); err != nil {
		t.Fatalf("make the issue's rows due: %v", err)
	}
	rig.drain(t)
	if got := rig.machine.Claim(); got.Released() {
		t.Fatalf("the implementer after the drain = %s by %s, want the cancel's resume to have brought it back in the pod that stayed",
			got.State, got.SuspendedBy)
	}
	if n := len(rig.rt.CallsOf("Resume")) - resumes; n != 1 {
		t.Fatalf("the implementer was resumed %d times after the cancel, want once", n)
	}
}

// The agent of a retro after a close, which an idle release released once its turn ended, works
// in a pod the retro holds although its issue has left the workflow (retroHoldsPod). A release of
// that pod can be cancelled, as any release can: here a production check's look of the tester is
// queued once the release's rows stand, which is the pod given work, so the teardown cancels the
// release as overtaken (store.IssueRelease). The cancel queues the agent's resume in the pod that
// stayed (store.CancelIssueRelease). The outbox finishes a start of an issue that has left the
// workflow without acting unless the retro's record fences the start (retroHolds), so the resume
// carries the run of the retro: the cancel reads it from the issue's open retro after its close in
// its own transaction, and not from a wake it dropped.
//
// The first row is a message that woke the agent after the release recorded it (wakeReleased): the
// cancel drops that wake and its start, and its resume takes their place. In the second row no
// wake of the agent was taken, so the cancel drops nothing of it. A resume with no run would leave
// the agent `released` for as long as its retro runs, and while that resume is queued no wake of
// the agent is taken (workflow's StartQueued).
func TestACancelledReleaseResumesTheRetrosAgentWhetherOrNotItDroppedAWakeOfIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		woken bool
	}{
		{name: "a wake of the agent dropped", woken: true},
		{name: "no wake of the agent dropped", woken: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			rig, closed, run := newRetroIdleRig(t)
			if tc.woken {
				wakeRetroAgent(t, rig, closed, run)
			}
			const lookTask = "Production check of LEGION-209: the look the merge's deploy woke."
			enqueueOutbox(t, rig.pool, rig.issues, mustOutboxRow(t, closed.Key, record.SuperviseRequest{Op: "start", Tree: closed.Tree,
				Role: claim.RoleTester, Generation: closed.Generation, Task: lookTask, CheckLook: 1,
				CheckOpenedAt: time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)}, time.Now().UTC()))
			var teardown int64
			if err := rig.pool.QueryRow(ctx, `select id from outbox where issue = $1 and kind = 'issue_release'`, closed.Key).Scan(&teardown); err != nil {
				t.Fatalf("read the teardown row: %v", err)
			}
			decision, err := rig.st.IssueRelease(ctx, "legion", store.IssueReleased{Issue: closed.Key, Tree: closed.Tree,
				IssueGeneration: closed.Generation, TreeGeneration: retroRunningTree().Generation, Row: teardown}, record.OutOfWorkflow)
			if err != nil || decision.Act != store.ReleaseCancel || decision.Cause != store.ReleaseCauseOvertaken {
				t.Fatalf("the teardown's decision = %s for %s, %v; want a cancel, the release overtaken by the look", decision.Act, decision.Cause, err)
			}

			rig.runRow(t, "issue_release", "")
			if rig.marked(t) {
				t.Fatal("the teardown left the release marked, want it cancelled")
			}
			if starts := implementerStarts(t, rig, closed.Key); len(starts) != 1 || starts[0].RetroRun != run || starts[0].Task != "" ||
				starts[0].WakeKind != "" || starts[0].Generation != closed.Generation {
				t.Fatalf("queued starts of the implementer = %+v, want the cancel's one resume at generation %d, carrying run %d of the retro after the close and no task",
					starts, closed.Generation, run)
			}
			resumedOnceInThePodThatStayed(t, rig)
		})
	}
}

// A person sets the closed child back to `todo` while the release of its pod stands, and the
// engine takes it into its next run (workflow.Engine.ReenterChild): its generation rises, and its
// retro after the close ends, stopped by the todo. The release's teardown then reads the
// generation moved on and cancels the release, and the pod is live again by the ordinary rule,
// since the child runs again under a tree the workflow runs. So the cancel's resume of the
// implementer, which a message had woken for the retro (wakeReleased), carries no mark of that
// retro: it is a resume of the role in the new run. A resume that carried the run that closed would
// be finished by the retro's fence ("the issue runs again", retroHolds) and leave the implementer
// `released`, where this one brings it back in the pod that stayed.
func TestACancelAfterTheRetrosIssueWentBackToTodoResumesTheRoleWithNoRetroMark(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rig, closed, run := newRetroIdleRig(t)
	wakeRetroAgent(t, rig, closed, run)

	engine := workflow.New(rig.issues, workflow.Config{Project: "LEGION", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(),
		Linger: time.Hour}, quietLogger())
	if _, err := intake.ApplyFact(ctx, rig.pool, "dispatch", "child-todo", intake.DispatchIssue{Key: closed.Key, ResolvedRepo: "acme/widgets",
		Seq: closed.LastDispatchSeq + 1, Type: "issue.updated", Status: "todo", Title: closed.Title, Parent: *closed.Parent, Rank: closed.Rank}, engine); err != nil {
		t.Fatalf("apply the person's todo on %s: %v", closed.Key, err)
	}
	var retro *record.RunRetro
	if err := store.Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		retro, err = rig.issues.RunRetro(ctx, tx, closed.Key, run)
		return err
	}); err != nil {
		t.Fatalf("read the record of run %d of %s: %v", run, closed.Key, err)
	}
	if again := readIssue(t, rig.pool, closed.Key); again == nil || again.Generation != run+1 || retro == nil || retro.Open() || retro.EndedBy != record.RetroEndTodo {
		t.Fatalf("after the todo the child = %+v and its retro of run %d = %+v, want the child at generation %d and the retro ended by the todo",
			again, run, retro, run+1)
	}

	rig.runRow(t, "issue_release", "")
	if rig.marked(t) {
		t.Fatal("the teardown left the release marked, want it cancelled")
	}
	if starts := implementerStarts(t, rig, closed.Key); len(starts) != 1 || starts[0].Generation != run+1 || starts[0].RetroRun != 0 ||
		starts[0].RetroWake || starts[0].WakeKind != "" || starts[0].Task != "" {
		t.Fatalf("queued starts of the implementer = %+v, want the cancel's one resume at generation %d with no mark of the retro that ended and no task",
			starts, run+1)
	}
	resumedOnceInThePodThatStayed(t, rig)
}

// The same cancel of the root architect that a retro's finding woke and an idle release took
// since: the wake the cancel dropped was a retro's wake of that architect (RetroWake), and the
// cancel's resume carries it, with the dropped wake's kind and detail, which a retro's wake
// carries (record's validation of it). The outbox fences that resume on the tree's retros
// (retroWakeHolds) rather than on the root, which has left the workflow and lingers, and resumes
// the architect in the session it kept.
//
// The resume carries the dropped start's task too, in the words of a release that did not finish
// (workflow.TakenOverWakeTask), as a kept wake's start does: that is what makes the architect's
// delivery a retro wake's (supervise.RetroWakeTaskID), which the claim holds until the turn that
// takes it ends. The end of another retro of the tree reads that delivery as the wake still owed
// (workflow's retroWakeOwed) and leaves the architect to the turn it works the finding in, where a
// resume with no task would leave it nothing owed, and that end would suspend it.
func TestACancelsResumeOfTheRootArchitectARetrosFindingWokeResumesIt(t *testing.T) {
	t.Parallel()
	root := checkRoot()
	r := newRetroOutboxRig(t, []record.Issue{root})
	architect, token := r.claimOf(root, claim.RoleArchitect, supervise.StateSuspended, supervise.SuspendedByReleased)
	r.openRetroAfterClose(root, mustClaimToken(t, root.Key, claim.RoleImplementer))
	dropped := workflow.WakeTask(pods.WakeRoleMessage, string(token), workflow.StoppedByRelease)
	resume, err := r.runner.releaseResume(root.Key, root.Tree)(claim.RoleArchitect, root.Generation,
		store.DroppedWake{Kind: pods.WakeRoleMessage, Detail: string(token), Task: dropped, RetroWake: true})
	if err != nil {
		t.Fatalf("build the cancel's resume of the root architect: %v", err)
	}
	row := record.OutboxRow{ID: 83, Kind: record.OutboxKind(resume.Kind), Issue: root.Key, Payload: resume.Payload}
	if err := r.runner.execute(r.ctx, row); err != nil {
		t.Fatalf("the cancel's resume of the root architect = %v, want the architect resumed", err)
	}
	got := architect.Claim()
	if got.State == supervise.StateSuspended || got.LastStartRow != 83 {
		t.Fatalf("the root architect after the cancel's resume = %s by %s with last start %d, want it resumed by row 83",
			got.State, got.SuspendedBy, got.LastStartRow)
	}
	want := workflow.TakenOverWakeTask(pods.WakeRoleMessage, string(token), dropped)
	if p := got.Pending; p == nil || p.ID != supervise.RetroWakeTaskID("outbox:83") || !p.RetroWake() || p.Task != want || p.Generation != 0 {
		t.Fatalf("the root architect's pending delivery = %+v, want the retro wake's task of row 83, %q, at generation 0", p, want)
	}
	if strings.Contains(want, "while your pod was gone") {
		t.Fatalf("the resume's task %q says the pod went away, which the cancel made false", want)
	}
	relaunchedInItsSession(t, r.rt, token)
}

// retroWindow is one claim of s26: the implementer of a child standing in its retro, its machine,
// its fake connection and the clock its machine's timers wait on.
type retroWindow struct {
	t       *testing.T
	r       *retroOutboxRig
	issue   record.Issue
	machine *supervise.Machine
	token   claim.Token
	conn    *fake.Conn
	clock   *heldClock
}

// retroPhaseEntry is the phase entry the retro's task carries in s26, which the record of the
// retro running at the close names (record.RunRetro.Entry).
const retroPhaseEntry = 5

// resume is the retro phase's start reaching the implementer's suspended claim: its rule resume.
func (w *retroWindow) resume() {
	w.t.Helper()
	if err := w.machine.Handle(w.r.ctx, supervise.RequestRuleResume{Claim: w.token, Reason: "start row 81 of " + w.issue.Key + ` (phase "retro")`}); err != nil {
		w.t.Fatalf("resume %s: %v", w.token, err)
	}
}

// deliver is that start's task, of the retro phase of the child's run on retroPhaseEntry.
func (w *retroWindow) deliver() {
	w.t.Helper()
	err := w.machine.Handle(w.r.ctx, supervise.RequestDeliver{Claim: w.token, Task: retroTaskLine(w.issue), Phase: phase.Retro,
		Generation: w.issue.Generation, Entry: retroPhaseEntry, ID: "outbox:81", Row: 81})
	if err != nil {
		w.t.Fatalf("deliver the retro's task to %s: %v", w.token, err)
	}
	w.machine.Wait()
}

// s26: the child's retro of run g was running at the close of run g (INFRA-5602's P4): the start
// of its retro phase had run, so the close keeps the agent's claim and sends no task of its own,
// and the task the claim holds is that start's, of the retro phase of run g, which the done issue
// has left. While the retro after the close of run g is open that task is held, not dropped
// (workflowRuntime.phaseHolds), in each window in which no turn has it at the close: the claim
// booting after the resume, the task queued behind a turn of the agent's own, its send in a
// backoff after the transport lost it, and a turn that took it before the close and ended in a
// provider error after it, which takes the task back to be sent again. Each window ends with the
// task sent and taken, and its completion is the retro's report, answered 200 with the rating.
func TestARetroTaskOfTheClosedRunIsHeldNotDropped(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// before brings the resumed claim, holding the retro's task, to the window the close
		// comes in; after ends that window, which is where the task goes on or is dropped.
		before, after func(w *retroWindow)
	}{
		{name: "booting after the resume", before: func(w *retroWindow) {
			w.resume()
			w.deliver()
		}, after: func(w *retroWindow) {
			readyUp(w.r.ctx, w.t, w.machine, w.token)
		}},
		{name: "queued behind a turn of the agent's own", before: func(w *retroWindow) {
			w.resume()
			readyUp(w.r.ctx, w.t, w.machine, w.token)
			w.conn.SetStreaming(true)
			if err := w.machine.Handle(w.r.ctx, supervise.StreamTurnStart{Claim: w.token}); err != nil {
				w.t.Fatalf("start the agent's own turn: %v", err)
			}
			w.deliver()
		}, after: func(w *retroWindow) {
			w.conn.SetStreaming(false)
			endTurn(w.r.ctx, w.t, w.machine, w.token)
		}},
		{name: "in a send backoff after the transport lost it", before: func(w *retroWindow) {
			w.resume()
			readyUp(w.r.ctx, w.t, w.machine, w.token)
			w.conn.FailPrompt(errors.New("the worker stream dropped the frame"))
			w.deliver()
		}, after: func(w *retroWindow) {
			w.conn.FailPrompt(nil)
			if err := w.machine.Handle(w.r.ctx, aliveObservation(w.machine)); err != nil {
				w.t.Fatalf("post the sweep's observation: %v", err)
			}
		}},
		{name: "sent again after a turn that ended in a provider error", before: func(w *retroWindow) {
			w.resume()
			readyUp(w.r.ctx, w.t, w.machine, w.token)
			w.deliver()
			awaitSent(w.t, w.machine)
			takeHeldTask(w.r.ctx, w.t, w.machine, w.token)
		}, after: func(w *retroWindow) {
			taken := w.machine.Claim().Pending
			if err := w.machine.Handle(w.r.ctx, supervise.StreamTurnEnd{Claim: w.token}); err != nil {
				w.t.Fatalf("end the turn: %v", err)
			}
			if err := w.machine.Handle(w.r.ctx, supervise.StreamPromptResult{Claim: w.token, DeliveryID: taken.ID, AgentInvoked: true,
				Error: "overloaded_error", HTTPStatus: http.StatusServiceUnavailable}); err != nil {
				w.t.Fatalf("report the turn's provider error: %v", err)
			}
			w.machine.Wait()
			if fired := w.clock.fire(30 * time.Second); fired != 1 {
				w.t.Fatalf("firing the ladder's first wait ran %d timers, want 1", fired)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inRetro := retroClosedChild()
			inRetro.Phase, inRetro.Status = phase.Retro, "in_progress"
			r := newRetroOutboxRig(t, []record.Issue{retroRunningTree(), inRetro})
			clock := &heldClock{}
			r.sup.deps.Clock = clock
			r.sup.deps.PhaseHolds = (&workflowRuntime{pool: r.pool, records: r.records}).phaseHolds
			machine, token := r.claimOf(inRetro, claim.RoleImplementer, supervise.StateSuspended, supervise.SuspendedByDaemon)
			conn, _ := r.sup.deps.Conns.Conn(token)
			w := &retroWindow{t: t, r: r, issue: inRetro, machine: machine, token: token, conn: conn.(*fake.Conn), clock: clock}
			tc.before(w)

			// A person closes the child: it stands done, and its retro, running, is open after
			// the close on the entry of the task the claim holds.
			closed := retroClosedChild()
			putOutboxIssue(t, r.pool, r.records, closed)
			run := r.openRetroOnEntry(closed, token, retroPhaseEntry)
			tc.after(w)

			machine.Wait()
			if p := machine.Claim().Pending; p == nil || p.Phase != phase.Retro || p.Generation != run {
				t.Fatalf("the implementer holds %+v once the window ends, want the retro's task of run %d held: its retro after the close is open", p, run)
			}
			awaitSent(t, machine)
			takeHeldTask(r.ctx, t, machine, token)
			if status, body := r.completionRoute()(machine, retroReport()); status != http.StatusOK {
				t.Fatalf("the retro's report = %d %s, want 200", status, body)
			}
			want := intake.RetroReport{Issue: closed.Key, Claim: token, Generation: run,
				PlanRating: record.PlanRating{Rating: 4, Reason: "The plan named the tests the change needed."}}
			if got := r.facts.facts(); len(got) != 1 || got[0] != want {
				t.Fatalf("facts after the report = %+v, want the one report %+v", got, want)
			}
		})
	}
}
