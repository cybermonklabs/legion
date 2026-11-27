package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

const (
	lookEndsRoot        = "LEGION-208"
	lookEndsChild       = "LEGION-209"
	lookEndsFirstLookID = "LOOKIDFIRSTLOOK00000000001"
	lookEndsOldTask     = "Production check of LEGION-209: old look 1 task [LOOKIDFIRSTLOOK00000000001]"
)

// lookEndsRig wires a real Postgres store, workflow engine, admission, outbox runner and supervisor
// with the fake runtime and fake connections, matching the daemon's wiring.
// Note: T5 adds supervise.Deps.LookHolds (workflowRuntime.lookHolds) beside PhaseHolds here.
type lookEndsRig struct {
	t        *testing.T
	ctx      context.Context
	pool     *store.Pool
	st       *store.Store
	records  record.Store
	engine   *workflow.Engine
	admit    *admit.Admission
	handlers []intake.Handler
	answers  *workflowRuntime
	sup      *supervisor
	rt       *fake.Runtime
	conns    *fake.Conns
	runner   *outbox
	nowMu    sync.Mutex
	nowVal   time.Time
}

func newLookEndsRig(t *testing.T, base time.Time) *lookEndsRig {
	t.Helper()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	st, err := store.Open(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)

	r := &lookEndsRig{
		t:       t,
		ctx:     ctx,
		pool:    pool,
		st:      st,
		records: record.NewStore(),
		nowVal:  base,
	}
	nowFn := func() time.Time {
		r.nowMu.Lock()
		defer r.nowMu.Unlock()
		return r.nowVal
	}

	r.engine = workflow.New(r.records, workflow.Config{
		Project:              "LEGION",
		Repositories:         testRepositories,
		Steps:                config.DesignGateRootIssues.Steps(),
		Linger:               time.Hour,
		ProductionCheckRole:  claim.RoleTester,
		ProductionCheckBound: 48 * time.Hour,
		Clock:                nowFn,
	}, quietLogger())
	r.admit = admit.New(r.records, r.engine, 3, "LEGION", quietLogger())
	r.handlers = []intake.Handler{blockers.NewMirror(r.records, "LEGION"), r.engine, r.admit}
	r.answers = &workflowRuntime{pool: pool, records: r.records, handlers: r.handlers, log: quietLogger()}

	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	conns := sup.deps.Conns.(*fake.Conns)
	sup.deps.Store = st
	sup.deps.PhaseHolds = r.answers.phaseHolds
	sup.deps.LookHolds = r.answers.lookHolds
	sup.deps.RootHears = r.answers.rootHears
	sup.deps.RootTask = r.answers.rootTask
	sup.OnDurableState(r.answers.terminal)
	r.sup, r.rt, r.conns = sup, rt, conns

	r.runner = &outbox{
		log:             quietLogger(),
		pool:            pool,
		dispatchProject: "LEGION",
		records:         r.records,
		supervisor:      sup,
		trees:           st,
		tokens:          outboxTokens{},
		project:         "legion",
		stateDir:        t.TempDir(),
		githubAPI:       newBranchGitHub(t, nil, branchExists).url,
		dispatch:        &outboxDispatch{issue: dispatch.Issue{Key: lookEndsChild, Status: "done"}, statusApplied: true},
		notices:         &outboxPublisher{},
		handlers:        r.handlers,
		now:             nowFn,
		provision: func(_ context.Context, req workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/" + req.Issue}, nil
		},
		remove: func(context.Context, workspace.Workspace, sync.Locker) error { return nil },
	}
	return r
}

func (r *lookEndsRig) setNow(at time.Time) {
	r.nowMu.Lock()
	r.nowVal = at
	r.nowMu.Unlock()
}

func (r *lookEndsRig) now() time.Time {
	r.nowMu.Lock()
	defer r.nowMu.Unlock()
	return r.nowVal
}

func (r *lookEndsRig) openTree(tree string) {
	r.t.Helper()
	if _, err := r.st.OpenTreeLifecycle(r.ctx, "legion", tree, treelifecycle.AuthorityWorkflow); err != nil {
		r.t.Fatalf("open tree lifecycle %s: %v", tree, err)
	}
}

func (r *lookEndsRig) apply(id string, fact intake.Fact) {
	r.t.Helper()
	res, err := intake.ApplyFact(r.ctx, r.pool, "test", id, fact, r.handlers...)
	if err != nil || res.Refusal != nil {
		r.t.Fatalf("apply %s = %#v, %v", id, res, err)
	}
}

func (r *lookEndsRig) drain() {
	r.t.Helper()
	for range 12 {
		due, err := r.runner.listDue(r.ctx, "", nil, r.now().UTC())
		if err != nil {
			r.t.Fatalf("list due outbox rows: %v", err)
		}
		if len(due) == 0 {
			break
		}
		if err := r.runner.RunOnce(r.ctx); err != nil {
			r.t.Fatalf("run outbox: %v", err)
		}
		for _, m := range r.sup.supervised() {
			m.Wait()
		}
	}
}

// seedChildCheck creates a running root (with slot and open lifecycle), a done child LEGION-209
// with an open look 1 production check carrying lookEndsFirstLookID, and a ready tester claim on
// LEGION-209.
func (r *lookEndsRig) seedChildCheck(opened time.Time) (claim.Token, *supervise.Machine, *fake.Conn, record.ProductionCheck) {
	r.t.Helper()
	r.openTree(lookEndsRoot)
	putOutboxIssue(r.t, r.pool, r.records, record.Issue{
		Key: lookEndsRoot, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: lookEndsRoot, Title: "root", Phase: phase.Implementing,
		Generation: 1, Status: "in_progress", Rank: "U", HandedOver: true, LastDispatchSeq: 1,
	})
	holdSlot(r.t, r.pool, lookEndsRoot)
	parent := lookEndsRoot
	putOutboxIssue(r.t, r.pool, r.records, record.Issue{
		Key: lookEndsChild, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: lookEndsRoot, Parent: &parent, Title: "child", Phase: phase.Done,
		Generation: 1, Status: "done", DispatchStatus: "done", Rank: "V", LastDispatchSeq: 1,
	})
	check := record.ProductionCheck{
		Issue: lookEndsChild, Project: "LEGION", Tree: lookEndsRoot, Repo: "acme/widgets",
		Number: 42, MergeSHA: "merge-42", Role: claim.RoleTester,
		OpenedAt: opened, BoundAt: opened.Add(48 * time.Hour),
		Looks: 1, LookOpen: true, LookWakeAt: &opened,
		LastWakeKind: "merge", LastWakeDetail: "merge-42", LastWakeAt: &opened,
		LookID: lookEndsFirstLookID, LookIDLook: 1, LookIDOpenedAt: opened, OpenedGeneration: 1,
	}
	putProductionCheck(r.t, r.pool, r.records, check)
	token, machine, conn := r.readyTester(lookEndsRoot, lookEndsChild, "ses-tester-1")
	return token, machine, conn, check
}

func (r *lookEndsRig) readyTester(tree, issue, session string) (claim.Token, *supervise.Machine, *fake.Conn) {
	r.t.Helper()
	token := mustClaimToken(r.t, issue, claim.RoleTester)
	machine, _, err := r.sup.Create(r.ctx, supervise.Claim{
		Token: token, Project: "legion", Tree: tree, Issue: issue, Role: claim.RoleTester,
		State: supervise.StateQueued,
	}, "")
	if err != nil {
		r.t.Fatalf("create %s: %v", token, err)
	}
	conn := fake.NewConn()
	r.conns.Register(token, conn)
	if err := machine.Handle(r.ctx, supervise.RequestSpawn{Claim: token}); err != nil {
		r.t.Fatalf("spawn %s: %v", token, err)
	}
	readyClaimAs(r.t, r.sup, token, claim.RoleTester, session)
	machine.Wait()
	return token, machine, conn
}

// deliverUnconfirmedLook queues look 1's task on machine so that the turn match does NOT take it
// (no StreamTurnStart arrives): Pending holds lookEndsOldTask with ConfirmedAt zero.
func (r *lookEndsRig) deliverUnconfirmedLook(machine *supervise.Machine, token claim.Token, opened time.Time, task string) {
	r.t.Helper()
	if err := machine.Handle(r.ctx, supervise.RequestDeliver{
		Claim:      token,
		ID:         "outbox:look-1",
		Task:       task,
		Generation: 1,
		CheckLook:  supervise.CheckLook{Look: 1, OpenedAt: opened},
		LookID:     lookEndsFirstLookID,
	}); err != nil {
		r.t.Fatalf("deliver look 1 task to %s: %v", token, err)
	}
	machine.Wait()
	p := machine.Claim().Pending
	if p == nil || p.Task != task || !p.ConfirmedAt.IsZero() {
		r.t.Fatalf("pending after look 1 deliver = %+v, want unconfirmed %q", p, task)
	}
}

// reconnectAndReady registers a fresh fake connection for token's newly launched generation and
// drives StreamHello -> RequestRegister -> RequestReady, returning the new connection.
func (r *lookEndsRig) reconnectAndReady(machine *supervise.Machine, token claim.Token, session string) *fake.Conn {
	r.t.Helper()
	conn := fake.NewConn()
	r.conns.Register(token, conn)
	readyClaimAs(r.t, r.sup, token, claim.RoleTester, session)
	machine.Wait()
	return conn
}

// assertOldTaskGone fails if oldTask appeared in any prompt sent after the end (promptsAfter) or if
// machine's pending task is still oldTask.
func assertOldTaskGone(t *testing.T, place string, machine *supervise.Machine, oldTask string, promptsAfter []fake.Prompt) {
	t.Helper()
	for i, p := range promptsAfter {
		if strings.Contains(p.Message, oldTask) {
			t.Errorf("%s: ended look's task reached the connection after the end (prompt[%d] id=%s)", place, i, p.DeliveryID)
		}
	}
	if p := machine.Claim().Pending; p != nil && p.Task == oldTask {
		t.Errorf("%s: claim still holds the ended look's task as Pending (id=%s, look=%+v, lookID=%q)",
			place, p.ID, p.CheckLook, p.LookID)
	}
}

// TestALooksTaskEndsAtEveryPlace holds the R4 inventory of every place where a production-check
// look's task can go out or stay (except "the boot's re-issue of the same look", which needs the
// boot step of T8 and is added there). Each row sends a look's task that the turn match does not
// take, ends the look that way, and passes only when that task's text never reaches the connection
// after the end and the claim's pending task is not that task once the rows have run.
func TestALooksTaskEndsAtEveryPlace(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)

	t.Run("the send to an idle claim", func(t *testing.T) {
		r := newLookEndsRig(t, base)
		opened := base.Add(-time.Hour)
		token, machine, conn, check := r.seedChildCheck(opened)

		// Put the tester in a turn so queuing look 1's task leaves it queued, unsent and unconfirmed.
		if err := machine.Handle(r.ctx, supervise.StreamTurnStart{Claim: token}); err != nil {
			t.Fatalf("start turn: %v", err)
		}
		r.deliverUnconfirmedLook(machine, token, opened, lookEndsOldTask)
		before := len(conn.Prompts())

		// Look 1 ends in the store before the claim becomes idle and sends the queued task.
		check.LookOpen = false
		putProductionCheck(r.t, r.pool, r.records, check)

		// The turn ends, making the claim idle and triggering sendPending.
		if err := machine.Handle(r.ctx, supervise.StreamTurnEnd{Claim: token}); err != nil {
			t.Fatalf("end turn: %v", err)
		}
		machine.Wait()
		assertOldTaskGone(t, "the send to an idle claim", machine, lookEndsOldTask, conn.Prompts()[before:])
	})

	t.Run("the ready after a process death", func(t *testing.T) {
		r := newLookEndsRig(t, base)
		opened := base.Add(-time.Hour)
		token, machine, _, check := r.seedChildCheck(opened)
		r.deliverUnconfirmedLook(machine, token, opened, lookEndsOldTask)

		// Look 1 ends before the process dies and comes back ready.
		check.LookOpen = false
		putProductionCheck(r.t, r.pool, r.records, check)

		loc := *machine.Claim().Locator
		if err := machine.Handle(r.ctx, supervise.RuntimeObservation{
			Observation: runtime.Observation{Locator: loc, Kind: runtime.Gone, At: r.now()},
		}); err != nil {
			t.Fatalf("report process death: %v", err)
		}
		conn2 := r.reconnectAndReady(machine, token, "ses-tester-1")
		assertOldTaskGone(t, "the ready after a process death", machine, lookEndsOldTask, conn2.Prompts())
	})

	t.Run("the ready after fresh_session", func(t *testing.T) {
		r := newLookEndsRig(t, base)
		opened := base.Add(-time.Hour)
		token, machine, _, check := r.seedChildCheck(opened)
		r.deliverUnconfirmedLook(machine, token, opened, lookEndsOldTask)

		// Look 1 ends before the controller asks for a fresh session (the carried live case of INFRA-3084).
		check.LookOpen = false
		putProductionCheck(r.t, r.pool, r.records, check)

		tokens := api.NewBootTokens(r.st)
		grants := credential.New(nil)
		const opToken = "operator-token-for-look-ends"
		srv := api.NewServer("127.0.0.1", 0, api.Options{
			Steps:         config.DesignGateRootIssues.Steps(),
			Supervisor:    r.sup,
			BootTokens:    tokens,
			Project:       "legion",
			OperatorToken: opToken,
			Controller:    r.st,
			Grants:        grants,
			Pool:          r.pool,
			Record:        r.records,
			Releaser:      r.rt,
			Trees:         r.st,
			Handlers:      r.handlers,
			RoleFor:       workflow.RoleFor,
		})
		// Mint a controller grant and invoke POST /legion/v1/claims/fresh-session.
		grantReq := httptest.NewRequest(http.MethodPost, "/legion/v1/grants", bytes.NewBufferString("{}"))
		grantReq.Header.Set("Authorization", "Bearer "+opToken)
		grantRec := httptest.NewRecorder()
		srv.Handler.ServeHTTP(grantRec, grantReq)
		if grantRec.Code != http.StatusOK {
			t.Fatalf("mint controller grant = %d: %s", grantRec.Code, grantRec.Body)
		}
		var grant api.GrantResponse
		if err := json.Unmarshal(grantRec.Body.Bytes(), &grant); err != nil {
			t.Fatalf("decode grant: %v", err)
		}
		body, _ := json.Marshal(api.ClaimRequest{GrantID: grant.GrantID, Issue: lookEndsChild, Role: string(claim.RoleTester)})
		fsRec := httptest.NewRecorder()
		srv.Handler.ServeHTTP(fsRec, httptest.NewRequest(http.MethodPost, "/legion/v1/claims/fresh-session", bytes.NewReader(body)))
		if fsRec.Code != http.StatusOK && fsRec.Code != http.StatusAccepted {
			t.Fatalf("fresh_session = %d: %s", fsRec.Code, fsRec.Body)
		}

		conn2 := r.reconnectAndReady(machine, token, "ses-tester-fresh")
		assertOldTaskGone(t, "the ready after fresh_session", machine, lookEndsOldTask, conn2.Prompts())
	})

	t.Run("the resume after an operator's suspension", func(t *testing.T) {
		r := newLookEndsRig(t, base)
		opened := base.Add(-time.Hour)
		token, machine, _, _ := r.seedChildCheck(opened)
		r.deliverUnconfirmedLook(machine, token, opened, lookEndsOldTask)

		// Operator suspends the claim while look 1's unconfirmed task is pending.
		if err := machine.Handle(r.ctx, supervise.RequestSuspend{
			Claim: token, By: supervise.SuspendedByOperator, Reason: "operator inspection",
		}); err != nil {
			t.Fatalf("operator suspend: %v", err)
		}

		// Look 1 ends while the claim is suspended by the operator, and the outbox runs.
		r.setNow(opened.Add(workflow.CheckLookBound))
		r.apply("look-bound", intake.CheckLookEnded{
			Issue: lookEndsChild, Look: 1, OpenedAt: opened, Cause: "look bound",
		})
		r.drain()

		// Operator resumes the claim and the agent comes back ready.
		if err := machine.Handle(r.ctx, supervise.RequestResume{
			Claim: token, By: supervise.SuspendedByOperator,
		}); err != nil {
			t.Fatalf("operator resume: %v", err)
		}
		conn2 := r.reconnectAndReady(machine, token, "ses-tester-1")
		assertOldTaskGone(t, "the resume after an operator's suspension", machine, lookEndsOldTask, conn2.Prompts())
	})

	t.Run("the daemon's suspension after a look ended by its bound", func(t *testing.T) {
		r := newLookEndsRig(t, base)
		opened := base.Add(-time.Hour)
		token, machine, conn, _ := r.seedChildCheck(opened)
		r.deliverUnconfirmedLook(machine, token, opened, lookEndsOldTask)
		before := len(conn.Prompts())

		// Look 1's bound comes due: the workflow ends the look and queues the daemon's suspension
		// (and, in T6, retire_look).
		r.setNow(opened.Add(workflow.CheckLookBound))
		r.apply("look-bound", intake.CheckLookEnded{
			Issue: lookEndsChild, Look: 1, OpenedAt: opened, Cause: "look bound",
		})
		r.drain()
		if got := machine.Claim().State; got != supervise.StateSuspended {
			t.Fatalf("state after daemon suspension = %s, want suspended", got)
		}
		assertOldTaskGone(t, "the daemon's suspension after a look ended by its bound", machine, lookEndsOldTask, conn.Prompts()[before:])
	})

	t.Run("a failed claim and the next look's retry", func(t *testing.T) {
		r := newLookEndsRig(t, base)
		opened := base.Add(-time.Hour)
		token, machine, conn1, _ := r.seedChildCheck(opened)
		r.deliverUnconfirmedLook(machine, token, opened, lookEndsOldTask)
		before := len(conn1.Prompts())

		// Process dies and its relaunch fails, spending LaunchFailures (1) and failing the claim.
		// answers.terminal applies intake.ClaimFailed, ending look 1 and naming look 2's wake.
		r.rt.ScriptResume(fake.SpawnResult{Err: errors.New("pod could not start")})
		loc := *machine.Claim().Locator
		if err := machine.Handle(r.ctx, supervise.RuntimeObservation{
			Observation: runtime.Observation{Locator: loc, Kind: runtime.Gone, At: r.now()},
		}); err == nil {
			t.Fatal("death whose relaunch failed returned nil, want refusal")
		}
		if got := machine.Claim().State; got != supervise.StateFailed {
			t.Fatalf("claim state after failed relaunch = %s, want failed", got)
		}
		r.drain()

		// Advance to look 2's wake and drain the outbox so look 2 retries the failed claim.
		afterFail := readProductionCheck(t, r.pool, r.records, lookEndsChild)
		if afterFail.LookOpen || afterFail.WakeAt == nil {
			t.Fatalf("check after failed claim = %+v, want look 1 ended with WakeAt named for look 2", afterFail)
		}
		r.setNow(afterFail.WakeAt.Add(time.Second))
		r.drain()

		if got := machine.Claim().State; got != supervise.StateLaunching {
			t.Fatalf("claim state after look 2 retry = %s, want launching", got)
		}
		conn2 := r.reconnectAndReady(machine, token, "ses-tester-1")
		promptsAfter := append(append([]fake.Prompt(nil), conn1.Prompts()[before:]...), conn2.Prompts()...)
		assertOldTaskGone(t, "a failed claim and the next look's retry", machine, lookEndsOldTask, promptsAfter)
	})

	t.Run("a child's re-entry", func(t *testing.T) {
		r := newLookEndsRig(t, base)
		opened := base.Add(-time.Hour)
		token, machine, conn, _ := r.seedChildCheck(opened)
		r.deliverUnconfirmedLook(machine, token, opened, lookEndsOldTask)
		before := len(conn.Prompts())

		// Child LEGION-209 re-enters the workflow under the live root.
		r.apply("child-reenter", intake.DispatchIssue{
			Key: lookEndsChild, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
			Status: "todo", Title: "child", Parent: lookEndsRoot, Rank: "V",
		})
		r.drain()
		assertOldTaskGone(t, "a child's re-entry", machine, lookEndsOldTask, conn.Prompts()[before:])
	})

	t.Run("a root's re-admission", func(t *testing.T) {
		r := newLookEndsRig(t, base)
		opened := base.Add(-time.Hour)
		until := base.Add(time.Hour)
		r.openTree(lookEndsRoot)
		putOutboxIssue(r.t, r.pool, r.records, record.Issue{
			Key: lookEndsRoot, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Project: "LEGION", Tree: lookEndsRoot, Title: "root", Phase: phase.Done,
			Generation: 1, Status: "done", DispatchStatus: "done", Rank: "A",
			LingerUntil: &until, HandedOver: true, LastDispatchSeq: 1,
		})
		putProductionCheck(r.t, r.pool, r.records, record.ProductionCheck{
			Issue: lookEndsRoot, Project: "LEGION", Tree: lookEndsRoot, Repo: "acme/widgets",
			Number: 42, MergeSHA: "merge-42", Role: claim.RoleTester,
			OpenedAt: opened, BoundAt: opened.Add(48 * time.Hour),
			Looks: 1, LookOpen: true, LookWakeAt: &opened,
			LastWakeKind: "merge", LastWakeDetail: "merge-42", LastWakeAt: &opened,
			LookID: lookEndsFirstLookID, LookIDLook: 1, LookIDOpenedAt: opened, OpenedGeneration: 1,
		})
		token, machine, conn := r.readyTester(lookEndsRoot, lookEndsRoot, "ses-root-tester")
		r.deliverUnconfirmedLook(machine, token, opened, lookEndsOldTask)
		before := len(conn.Prompts())

		// Lingering root LEGION-208 is re-admitted via Dispatch todo.
		r.apply("root-readmit", intake.DispatchIssue{
			Key: lookEndsRoot, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
			Status: "todo", Title: "root", Rank: "A", HandedOver: true,
		})
		r.drain()
		assertOldTaskGone(t, "a root's re-admission", machine, lookEndsOldTask, conn.Prompts()[before:])
	})

	t.Run("a check closed by its bound while the issue runs again", func(t *testing.T) {
		r := newLookEndsRig(t, base)
		opened := base.Add(-time.Hour)
		r.openTree(lookEndsRoot)
		running := record.Issue{
			Key: lookEndsRoot, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Project: "LEGION", Tree: lookEndsRoot, Title: "root", Phase: phase.Implementing,
			Generation: 2, Status: "in_progress", Rank: "A", HandedOver: true, LastDispatchSeq: 2,
		}
		check := record.ProductionCheck{
			Issue: lookEndsRoot, Project: "LEGION", Tree: lookEndsRoot, Repo: "acme/widgets",
			Number: 42, MergeSHA: "merge-42", Role: claim.RoleTester,
			OpenedAt: opened, BoundAt: opened.Add(48 * time.Hour),
			Looks: 1, LookOpen: true, LookWakeAt: &opened,
			LastWakeKind: "merge", LastWakeDetail: "merge-42", LastWakeAt: &opened,
			LookID: lookEndsFirstLookID, LookIDLook: 1, LookIDOpenedAt: opened, OpenedGeneration: 1,
		}
		// Look 1's task is queued while the root is done at generation 1 and the look is open, so
		// the look holds and the send keeps it; the root then runs again at generation 2.
		done := running
		done.Phase, done.Generation, done.Status, done.DispatchStatus, done.LastDispatchSeq = phase.Done, 1, "done", "done", 1
		putOutboxIssue(r.t, r.pool, r.records, done)
		putProductionCheck(r.t, r.pool, r.records, check)
		token, machine, conn := r.readyTester(lookEndsRoot, lookEndsRoot, "ses-root-tester")
		r.deliverUnconfirmedLook(machine, token, opened, lookEndsOldTask)
		before := len(conn.Prompts())
		putOutboxIssue(r.t, r.pool, r.records, running)
		holdSlot(r.t, r.pool, lookEndsRoot)
		// Check is still open from generation 1 with look 1 already closed so CheckBound closes it.
		check.LookOpen = false
		putProductionCheck(r.t, r.pool, r.records, check)

		r.setNow(opened.Add(48 * time.Hour))
		r.apply("check-bound", intake.CheckBound{Issue: lookEndsRoot})
		r.drain()
		if check := readProductionCheck(t, r.pool, r.records, lookEndsRoot); check.Open() || check.Outcome != record.CheckNotRun {
			t.Fatalf("check after bound while running again = %+v, want closed as not_run", check)
		}
		assertOldTaskGone(t, "a check closed by its bound while the issue runs again", machine, lookEndsOldTask, conn.Prompts()[before:])
	})

	t.Run("a check replaced by a later merge", func(t *testing.T) {
		r := newLookEndsRig(t, base)
		oldOpened := base.Add(-24 * time.Hour)
		r.openTree(lookEndsRoot)
		putOutboxIssue(r.t, r.pool, r.records, record.Issue{
			Key: lookEndsRoot, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Project: "LEGION", Tree: lookEndsRoot, Title: "root", Phase: phase.Implementing,
			Generation: 1, Status: "in_progress", Rank: "U", HandedOver: true, LastDispatchSeq: 1,
		})
		holdSlot(r.t, r.pool, lookEndsRoot)
		parent := lookEndsRoot
		merging := record.Issue{
			Key: lookEndsChild, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Project: "LEGION", Tree: lookEndsRoot, Parent: &parent, Title: "child", Phase: phase.Merging,
			Generation: 2, Status: "retro", Rank: "V", LastDispatchSeq: 2,
		}
		// Look 1's task of the earlier check is queued while the child is done at generation 1 and
		// the look is open, so the look holds and the send keeps it; the child then runs again at
		// generation 2 and reaches merging.
		done := merging
		done.Phase, done.Generation, done.Status, done.DispatchStatus, done.LastDispatchSeq = phase.Done, 1, "done", "done", 1
		putOutboxIssue(r.t, r.pool, r.records, done)
		putProductionCheck(r.t, r.pool, r.records, record.ProductionCheck{
			Issue: lookEndsChild, Project: "LEGION", Tree: lookEndsRoot, Repo: "acme/widgets",
			Number: 42, MergeSHA: "merge-42", Role: claim.RoleTester,
			OpenedAt: oldOpened, BoundAt: oldOpened.Add(48 * time.Hour),
			Looks: 1, LookOpen: true, LookWakeAt: &oldOpened,
			LastWakeKind: "merge", LastWakeDetail: "merge-42", LastWakeAt: &oldOpened,
			LookID: lookEndsFirstLookID, LookIDLook: 1, LookIDOpenedAt: oldOpened, OpenedGeneration: 1,
		})
		token, machine, conn := r.readyTester(lookEndsRoot, lookEndsChild, "ses-tester-1")
		r.deliverUnconfirmedLook(machine, token, oldOpened, lookEndsOldTask)
		before := len(conn.Prompts())
		putOutboxIssue(r.t, r.pool, r.records, merging)
		if err := store.Transact(r.ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
			if err := r.records.PutPhase(ctx, tx, record.PhaseRow{
				Issue: lookEndsChild, Role: claim.RoleMerger, Claim: "child-merger", HandoffCommit: "head-44",
			}); err != nil {
				return err
			}
			return r.records.PutPullRequest(ctx, tx, record.PullRequest{
				State: record.PullRequestOpen, Issue: lookEndsChild, Repo: "acme/widgets",
				Number: 44, Branch: "legion/" + lookEndsChild, HeadSHA: "head-44", Failing: []string{},
			})
		}); err != nil {
			t.Fatalf("seed merging child PR: %v", err)
		}

		// Merging PR #44 replaces the earlier check and starts look 1 of the new check.
		r.apply("later-merge", intake.PullRequestMerged{
			Repo: "acme/widgets", Number: 44, MergeSHA: "merge-44",
		})
		r.drain()
		assertOldTaskGone(t, "a check replaced by a later merge", machine, lookEndsOldTask, conn.Prompts()[before:])
	})

	t.Run("a start row an older binary queued", func(t *testing.T) {
		r := newLookEndsRig(t, base)
		opened := base.Add(-time.Hour)
		_, machine, conn, _ := r.seedChildCheck(opened)

		// Execute a check start row with no LookID (as queued by an older binary).
		olderRow := mustOutboxRow(t, lookEndsChild, record.SuperviseRequest{
			Op:            "start",
			Tree:          lookEndsRoot,
			Role:          claim.RoleTester,
			Generation:    1,
			CheckLook:     1,
			CheckOpenedAt: opened,
			Task:          lookEndsOldTask,
		}, r.now())
		olderRow.ID = 77
		if err := r.runner.execute(r.ctx, olderRow); err != nil {
			t.Fatalf("execute older check start row: %v", err)
		}
		machine.Wait()
		assertOldTaskGone(t, "a start row an older binary queued", machine, lookEndsOldTask, conn.Prompts())
	})
}
