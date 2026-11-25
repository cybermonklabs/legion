package daemon

import (
	"bytes"
	"context"
	"encoding/json"
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

// The session the retro's agent kept from the run, which every relaunch of it must resume.
const (
	retroAgentSession     = "ses_implementer"
	retroAgentSessionFile = "/sessions/implementer.jsonl"
)

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
	token := mustClaimToken(r.t, issue.Key, role)
	machine, _, err := r.sup.Create(r.ctx, supervise.Claim{Token: token, Project: "legion", Tree: issue.Tree, TreeEpoch: 1, Issue: issue.Key, Role: role,
		Generation: 1, State: state, SuspendedBy: by, Session: retroAgentSession, SessionFile: retroAgentSessionFile}, "")
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
	run := issue.Generation
	started := time.Now().Add(-time.Minute).UTC().Truncate(time.Microsecond)
	if err := store.Transact(r.ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := r.records.MarkRunStarted(ctx, tx, issue.Key, run, issue.Tree, started.Add(-24*time.Hour)); err != nil {
			return err
		}
		if err := r.records.MarkRetroStarted(ctx, tx, issue.Key, run, issue.Tree, started); err != nil {
			return err
		}
		_, err := r.records.OpenRetroAfterClose(ctx, tx, issue.Key, run, issue.Tree, started, started.Add(workflow.RetroAfterCloseBound), agent, 0)
		return err
	}); err != nil {
		r.t.Fatalf("open the retro after the close of %s: %v", issue.Key, err)
	}
	return run
}

// endRetroAfterClose ends issue's open retro of run with outcome, by cause.
func (r *retroOutboxRig) endRetroAfterClose(issue record.Issue, run uint64, outcome record.RetroOutcome, cause record.RetroEndCause) {
	r.t.Helper()
	if err := store.Transact(r.ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		_, ended, err := r.records.EndRetroAfterClose(ctx, tx, issue.Key, run, time.Now().UTC().Truncate(time.Microsecond), outcome, cause)
		if err == nil && !ended {
			return fmt.Errorf("the retro of run %d was not open", run)
		}
		return err
	}); err != nil {
		r.t.Fatalf("end the retro after the close of %s: %v", issue.Key, err)
	}
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
// does. It answers that delivery.
func heldRetroTask(t *testing.T, machine *supervise.Machine, id int64, issue record.Issue, run uint64) supervise.Delivery {
	t.Helper()
	p := machine.Claim().Pending
	if p == nil || p.ID != fmt.Sprintf("outbox:%d", id) || p.Phase != "" || p.Generation != issue.Generation ||
		p.RetroRun != supervise.RetroRun(run) || !strings.HasPrefix(p.Task, retroTaskLine(issue)) {
		t.Fatalf("the agent's pending delivery = %+v, want the retro's task under outbox:%d with no phase at generation %d, carrying run %d",
			p, id, issue.Generation, run)
	}
	return *p
}

// relaunchedInItsSession fails unless the runtime launched the claim of token exactly once, by
// resuming the session file it kept, and never spawned it afresh.
func relaunchedInItsSession(t *testing.T, rt *fake.Runtime, token claim.Token) {
	t.Helper()
	var resumes, spawns int
	for _, call := range rt.Calls() {
		switch {
		case call.Method == "Resume" && call.Spec.Claim == token:
			resumes++
			if call.Spec.ResumeSessionFile != retroAgentSessionFile {
				t.Fatalf("the runtime resumed %s from %q, want the session file it kept, %q", token, call.Spec.ResumeSessionFile, retroAgentSessionFile)
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
	want := intake.RetroReport{Issue: closed.Key, Claim: token, Generation: run, Summary: "the retro of the closed run",
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
// makes is routed to the handler of the task that turn is on — the look's to the check, the
// retro's to the retro — whichever start came first.
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
			lookReport := api.HandoffCompleteRequest{Summary: "production carries the merge", Verdict: "pass"}
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
// implementer in the session it kept. At the retro's end, its report, the tree closes again: the
// agent's claim is retired and the reopened epoch's cleanup is confirmed.
func TestARetroAfterTheLingerReopensAndClosesTheTree(t *testing.T) {
	t.Parallel()
	until := time.Now().Add(-time.Minute)
	root := checkRoot()
	root.Status, root.LingerUntil = "backlog", &until
	engine := workflow.New(record.NewStore(), workflow.Config{Project: "legion", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour}, quietLogger())
	r := newRetroOutboxRig(t, []record.Issue{root}, engine)
	machine, token := r.claimOf(root, claim.RoleImplementer, supervise.StateSuspended, supervise.SuspendedByDaemon)

	// The linger ends: its close retires the implementer, removes the workspace and confirms the
	// tree's cleanup.
	applyFact(t, r.pool, "outbox", "linger:LEGION-208:1", intake.LingerExpired{Issue: root.Key, Generation: root.Generation}, engine)
	for range 2 {
		if err := r.runner.RunOnce(r.ctx); err != nil {
			t.Fatalf("run the linger's end: %v", err)
		}
	}
	if got := machine.Claim().State; got != supervise.StateRetired {
		t.Fatalf("the implementer after the linger's end = %s, want retired", got)
	}
	if lc := r.lifecycle(root.Tree); !lc.CleanupStarted || lc.CleanupConfirmedAt.IsZero() || lc.Epoch != 1 {
		t.Fatalf("the lifecycle after the linger's end = %+v, want epoch 1's cleanup confirmed", lc)
	}
	removedAtLinger := r.removed.Load()

	// A person sets the parked root done, and the close owes its run the retro.
	root.Status = "done"
	putOutboxIssue(t, r.pool, r.records, root)
	run := r.openRetroAfterClose(root, token)
	if err := r.runner.execute(r.ctx, retroStart(t, root, claim.RoleImplementer, run, 80)); err != nil {
		t.Fatalf("the retro start in the tree past its linger = %v, want the lifecycle reopened and the implementer relaunched", err)
	}
	if lc := r.lifecycle(root.Tree); !lc.OpenUnderWorkflow() || lc.Epoch != 2 {
		t.Fatalf("the lifecycle after the retro's start = %+v, want epoch 2 open under the workflow", lc)
	}
	if got := machine.Claim(); got.State == supervise.StateRetired || got.TreeEpoch != 2 || got.LastStartRow != 80 {
		t.Fatalf("the implementer after the retro's start = %s in epoch %d with last start %d, want relaunched by row 80 in epoch 2",
			got.State, got.TreeEpoch, got.LastStartRow)
	}
	if got := r.provisioned.Load(); got != 1 {
		t.Fatalf("the retro's start provisioned %d workspaces, want the one the linger removed, again", got)
	}
	relaunchedInItsSession(t, r.rt, token)
	heldRetroTask(t, machine, 80, root, run)

	// The retro's report ends it, and the tree, past its linger, closes again.
	applyFact(t, r.pool, "api", fmt.Sprintf("retro-after-close:%s:%d", root.Key, run), intake.RetroReport{Issue: root.Key, Claim: token,
		Generation: run, Summary: "the retro of the closed run", PlanRating: record.PlanRating{Rating: 4, Reason: "The plan fit the work."}}, engine)
	for range 2 {
		if err := r.runner.RunOnce(r.ctx); err != nil {
			t.Fatalf("run the retro's end: %v", err)
		}
	}
	if got := machine.Claim().State; got != supervise.StateRetired {
		t.Fatalf("the implementer after the retro's end = %s, want retired: the tree closed again", got)
	}
	if lc := r.lifecycle(root.Tree); !lc.CleanupStarted || lc.CleanupConfirmedAt.IsZero() || lc.Epoch != 2 {
		t.Fatalf("the lifecycle after the retro's end = %+v, want epoch 2's cleanup confirmed", lc)
	}
	if got := r.removed.Load(); got <= removedAtLinger {
		t.Fatalf("the retro's end removed no workspace (%d removals, %d at the linger's end), want the one the retro ran in removed", got, removedAtLinger)
	}
}

// The agent the close keeps for the retro is mid-turn in another phase (INFRA-5602's P3, s20): a
// person sets a root done while its implementer's turn of implementing runs. The close keeps that
// claim working, so its turn finishes, and the retro's start, which the outbox runs behind the
// running turn, queues the retro's task on the claim rather than stopping or replacing the turn.
// Once the turn ends the retro's task is sent, and the next turn is on it. The issue's run starts
// through the workflow's own transition out of planning, so the close reads that run as one with
// no retro.
func TestARetroTaskFollowsTheRunningTurnOfTheSparedAgent(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	ctx := context.Background()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-208",
		Title: "Workflow", Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 5}
	putOutboxIssue(t, pool, records, issue)
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
		!strings.HasPrefix(p.Task, "Continue Workflow. Issue: LEGION-208. Phase: retro.") {
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
