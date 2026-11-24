package daemon

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/wait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// A workflow close that reserved its tree's cleanup keeps driving it through the outbox after a
// re-admission ends the linger it closed. The reservation is taken, then its cleanup waits for a
// claim still running (or a delete fails transiently, the fake runtime's FailCleanupTree), then
// the root goes back to todo: the re-admission bumps the root's generation, clears its linger,
// and cannot open the tree's next epoch until the cleanup confirms. Every remaining or retried
// close row of the tree must still retire its claim and drive the cleanup to confirmation before
// the stale-close fences (the row's generation, the ended linger) apply; finished without acting,
// they would leave the claims running, the reservation unconfirmed, and the tree waiting for
// admission forever.
func TestAReservedTreeCloseFinishesItsCleanupAfterReadmission(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		withChild      bool
		deleteFailures int
	}{
		{name: "cleanup waits for a running claim", withChild: true},
		{name: "cleanup delete fails transiently", deleteFailures: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			st, err := store.Open(ctx, pool.Config().ConnString())
			if err != nil {
				t.Fatalf("open the store: %v", err)
			}
			t.Cleanup(st.Close)
			records := record.NewStore()
			root := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-208", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"}
			putOutboxIssue(t, pool, records, root)
			parent := root.Key
			child := record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: root.Key, Parent: &parent, Title: "child", Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "V"}
			if tc.withChild {
				putOutboxIssue(t, pool, records, child)
			}
			if _, err := st.OpenTreeLifecycle(ctx, "legion", root.Key, treelifecycle.AuthorityWorkflow); err != nil {
				t.Fatalf("admit the tree: %v", err)
			}

			rt := fake.NewRuntime()
			if tc.deleteFailures > 0 {
				rt.FailCleanupTree(errors.New("transient Sandbox delete failure"))
			}
			sup := newSupervisor(ctx, nil, "legion", t.TempDir(), quietLogger())
			sup.deps = supervise.Deps{
				Runtime: rt, Conns: fake.NewConns(), Store: st, Specs: outboxSpecs{}, Clock: stillClock{}, Log: quietLogger(),
				Limits:         supervise.Limits{LaunchFailures: 2, PromptFailures: 2, PromptRetires: 2},
				Timeouts:       supervise.Timeouts{Boot: time.Second, RegistrationIntervals: 2, RPC: time.Second, Probe: time.Second, Stop: time.Second, LaunchBackoff: time.Millisecond},
				PluginContract: 24,
				RefreshGate:    supervise.NewRefreshGate(0),
			}
			t.Cleanup(sup.stop)
			runner := &outbox{log: quietLogger(),
				dispatchProject: "LEGION",
				pool:            pool, records: records, supervisor: sup, trees: st, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
				provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
					return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
				},
			}
			start := func(issue record.Issue, role claim.Role, task string) *supervise.Machine {
				t.Helper()
				row := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: role, Task: task, Generation: issue.Generation}, time.Now())
				if err := runner.execute(ctx, row); err != nil {
					t.Fatalf("start the %s of %s: %v", role, issue.Key, err)
				}
				token, err := claim.NewToken("legion", issue.Key, role)
				if err != nil {
					t.Fatal(err)
				}
				machine, found := sup.Machine(token)
				if !found {
					t.Fatalf("no claim for the %s of %s", role, issue.Key)
				}
				return machine
			}
			closeOf := func(issue record.Issue, role claim.Role) record.OutboxRow {
				return mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "tree_close", Tree: root.Key, Role: role, Generation: 1, Linger: 1}, time.Now())
			}
			architect := start(root, claim.RoleArchitect, "")
			var tester *supervise.Machine
			if tc.withChild {
				tester = start(child, claim.RoleTester, "Test it.")
			}

			// The tree lingers at generation 1 and its linger expires: the architect's close reserves
			// the cleanup and retires the architect, but the cleanup does not finish yet.
			until := time.Now().Add(-time.Minute)
			root.Phase, root.Status, root.LingerUntil = phase.Done, "done", &until
			putOutboxIssue(t, pool, records, root)
			if err := runner.execute(ctx, closeOf(root, claim.RoleArchitect)); err == nil {
				t.Fatal("the first close finished its cleanup; want it to wait for the running claim or fail its delete")
			}
			if got := architect.Claim().State; got != supervise.StateRetired {
				t.Fatalf("architect after the reserving close = %s, want retired", got)
			}

			// The root goes back to todo and is re-admitted: generation 2, no linger. The tree's next
			// epoch waits for the reserved cleanup.
			root.Generation, root.Phase, root.Status, root.LingerUntil = 2, phase.Planning, "in_progress", nil
			putOutboxIssue(t, pool, records, root)
			if opened, err := st.OpenTreeLifecycle(ctx, "legion", root.Key, treelifecycle.AuthorityWorkflow); !errors.Is(err, treelifecycle.ErrCleanupReserved) {
				t.Fatalf("re-admission before the cleanup confirmed = %+v, %v; want the reservation's wait", opened, err)
			}

			// The remaining close, and the retried one, still act under the reservation. The delete
			// that failed transiently above succeeds on retry.
			rt.FailCleanupTree(nil)
			if tc.withChild {
				if err := runner.execute(ctx, closeOf(child, claim.RoleTester)); err != nil {
					t.Fatalf("the tester's close after re-admission: %v", err)
				}
				if got := tester.Claim().State; got != supervise.StateRetired {
					t.Fatalf("tester after its close under the reservation = %s, want retired", got)
				}
			}
			if err := runner.execute(ctx, closeOf(root, claim.RoleArchitect)); err != nil {
				t.Fatalf("the architect's retried close after re-admission: %v", err)
			}
			opened, err := st.OpenTreeLifecycle(ctx, "legion", root.Key, treelifecycle.AuthorityWorkflow)
			if err != nil || opened.Epoch != 2 {
				t.Fatalf("re-admission after the closes = %+v, %v; want the tree's epoch 2 opened", opened, err)
			}
			claims, err := st.Claims(ctx)
			if err != nil {
				t.Fatalf("read claims: %v", err)
			}
			for _, c := range claims {
				if c.Tree == root.Key && c.State != supervise.StateRetired {
					t.Fatalf("claim %s of the closed tree still %s, want retired", c.Token, c.State)
				}
			}
		})
	}
}

// A child of a lingering tree set back to todo is re-admitted as a root of its own (an orphan),
// while its roles' claims, suspended by the old tree's close, still name the old tree. The old
// tree's cleanup does not wait on those claims, which run nothing and are no longer its, and the
// orphan's start re-points its claim to the orphan's own tree before it starts it, so the claim
// binds the new tree's lifecycle rather than the old tree's confirmed one. The claim keeps its
// session and resumes it under every runtime: under tmux it is on the host; under the Sandbox
// runtime, which provisions each issue's workspace in a pod, it is in the session database the
// deployment's session_store names, which every tree's pods read.
func TestAnOrphansClaimsLeaveTheirOldTreeAndStartInTheirOwn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		inPod bool
	}{{"tmux", false}, {"a runtime that provisions each issue's workspace in its pod", true}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			st := outboxTreeStore(t, pool, "LEGION-208")
			records := record.NewStore()
			until := time.Now().Add(-time.Minute)
			root := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-208", Title: "root", Phase: phase.Done, Generation: 1, Status: "done", Rank: "U", LingerUntil: &until}
			putOutboxIssue(t, pool, records, root)
			parent := root.Key
			child := record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: root.Key, Parent: &parent, Title: "child", Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "V"}
			putOutboxIssue(t, pool, records, child)

			rt := fake.NewRuntime()
			rt.InPod = tc.inPod
			sup := newSupervisor(ctx, st, "legion", t.TempDir(), quietLogger())
			sup.deps = supervise.Deps{
				Runtime: rt, Conns: fake.NewConns(), Store: st, Specs: outboxSpecs{}, Clock: stillClock{}, Log: quietLogger(),
				Limits:         supervise.Limits{LaunchFailures: 2, PromptFailures: 2, PromptRetires: 2},
				Timeouts:       supervise.Timeouts{Boot: time.Second, RegistrationIntervals: 2, RPC: time.Second, Probe: time.Second, Stop: time.Second, LaunchBackoff: time.Millisecond},
				PluginContract: 24,
				RefreshGate:    supervise.NewRefreshGate(0),
			}
			t.Cleanup(sup.stop)
			token, err := claim.NewToken("legion", child.Key, claim.RolePlanner)
			if err != nil {
				t.Fatal(err)
			}
			machine, _, err := sup.Create(ctx, supervise.Claim{Token: token, Project: "legion", Tree: root.Key, TreeEpoch: 1, Issue: child.Key, Role: claim.RolePlanner,
				Generation: 1, State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByDaemon, Session: "ses-planner", SessionFile: "/legion/sessions/planner.jsonl"}, "")
			if err != nil {
				t.Fatal(err)
			}

			// The old tree's linger expired and reserved its cleanup; the child was re-admitted as a root.
			lifecycle, reserved, err := st.ReserveWorkflowTreeCleanup(ctx, "legion", root.Key, root.Generation)
			if err != nil || !reserved {
				t.Fatalf("reserve the old tree's cleanup = %v, %v", reserved, err)
			}
			child.Tree, child.Parent, child.Generation = child.Key, nil, 2
			putOutboxIssue(t, pool, records, child)
			if _, err := st.OpenTreeLifecycle(ctx, "legion", child.Key, treelifecycle.AuthorityWorkflow); err != nil {
				t.Fatalf("admit the orphan's tree: %v", err)
			}
			if err := st.CleanupReservedTree(ctx, "legion", root.Key, lifecycle.Epoch, rt); err != nil {
				t.Fatalf("the old tree's cleanup with the orphan's suspended claim = %v, want it confirmed", err)
			}

			runner := &outbox{log: quietLogger(), dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup, trees: st, tokens: outboxTokens{},
				project: "legion", stateDir: t.TempDir(), now: time.Now,
				provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
					return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-209"}, nil
				},
			}
			start := mustOutboxRow(t, child.Key, record.SuperviseRequest{Op: "start", Tree: child.Key, Role: claim.RolePlanner, Generation: 2, Phase: phase.Planning, Task: "Plan it."}, time.Now())
			if err := runner.execute(ctx, start); err != nil {
				t.Fatalf("the orphan's planner start: %v", err)
			}
			got := machine.Claim()
			if got.Tree != child.Key || got.TreeEpoch != 1 || got.State != supervise.StateLaunching {
				t.Fatalf("orphan's planner = tree %s epoch %d %s, want tree %s epoch 1 launching", got.Tree, got.TreeEpoch, got.State, child.Key)
			}
			resumes, spawns := rt.CallsOf("Resume"), rt.CallsOf("Spawn")
			if len(spawns) != 0 || len(resumes) != 1 || resumes[0].Spec.Tree != child.Key || resumes[0].Spec.ResumeSessionFile != "/legion/sessions/planner.jsonl" {
				t.Fatalf("resumes = %+v, spawns = %+v; want the kept session resumed once in tree %s and no fresh spawn", resumes, spawns, child.Key)
			}
			if got.SessionFile != "/legion/sessions/planner.jsonl" {
				t.Fatalf("orphan's planner = %+v, want its session kept", got)
			}
		})
	}
}

// A root that leaves admission's waiting line before a slot opens its tree (labelled and todo, then
// set to backlog) lingers like any root. Its tree never had a lifecycle, so no claim of it was ever
// stored, and its linger's expiry queues one close row naming no claim for each role (the close
// reaches the claims workflow's claimTargets names, and a claim that comes up after the expiry is
// reached by its role's row). A close row of such a tree — those, or one an earlier release wrote
// for a role — reserves nothing and finishes at the ordinary fences, with no claim to close.
// Before, the reservation refused the absent lifecycle, and every close row retried on its backoff
// forever, logged as a failure each time.
func TestANeverOpenedTreesLingerClosesFinishWithoutRetrying(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	root := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-208", Title: "waiting", Phase: phase.Admitted, Generation: 1,
		Status: "todo", DispatchStatus: "todo", HandedOver: true, Rank: "U", LastDispatchSeq: 5}
	putOutboxIssue(t, pool, records, root)
	st := outboxTreeStore(t, pool) // no slot ever opened the tree
	sup := newSupervisor(ctx, nil, "legion", t.TempDir(), quietLogger())
	sup.deps = supervise.Deps{
		Runtime: fake.NewRuntime(), Conns: fake.NewConns(), Store: st, Specs: outboxSpecs{}, Clock: stillClock{}, Log: quietLogger(),
		Limits:         supervise.Limits{LaunchFailures: 2, PromptFailures: 2, PromptRetires: 2},
		Timeouts:       supervise.Timeouts{Boot: time.Second, RegistrationIntervals: 2, RPC: time.Second, Probe: time.Second, Stop: time.Second, LaunchBackoff: time.Millisecond},
		PluginContract: 24,
		RefreshGate:    supervise.NewRefreshGate(0),
	}
	t.Cleanup(sup.stop)
	engine := workflow.New(records, workflow.Config{Project: "legion", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, quietLogger())
	admission := admit.New(records, engine, 1, "legion", quietLogger())
	var logs bytes.Buffer
	runner := &outbox{
		dispatchProject: "LEGION",
		pool:            pool, records: records, supervisor: sup, trees: st, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
		dispatch: &outboxDispatch{issue: dispatch.Issue{Key: root.Key, Status: "backlog"}}, handlers: []intake.Handler{engine, admission},
		now: func() time.Time { return time.Now().Add(time.Hour) }, log: slog.New(slog.NewJSONHandler(&logs, nil)),
		remove: func(context.Context, workspace.Workspace, sync.Locker) error { return nil },
	}

	// A person moves the waiting root to backlog: it leaves the line and lingers.
	if _, err := intake.ApplyFact(ctx, pool, "dispatch", "backlog", intake.DispatchIssue{Key: root.Key, ResolvedRepo: "acme/widgets", Seq: 6, Type: "issue.updated", Status: "backlog", Title: root.Title, Rank: root.Rank, HandedOver: true}, engine, admission); err != nil {
		t.Fatalf("apply the backlog: %v", err)
	}
	rows := func() (closes, all int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `select count(*) filter (where kind = 'supervise' and payload->>'op' = 'tree_close'), count(*) from outbox`).Scan(&closes, &all); err != nil {
			t.Fatalf("read the outbox: %v", err)
		}
		return closes, all
	}
	// The first run expires the linger, which queues close rows that name no claim: the tree holds
	// none.
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("run the linger: %v", err)
	}
	var named int
	if err := pool.QueryRow(ctx, `select count(*) from outbox where kind = 'supervise' and payload->>'op' = 'tree_close'
		and coalesce(payload->>'claim', '') <> ''`).Scan(&named); err != nil {
		t.Fatal(err)
	}
	if named != 0 {
		t.Fatalf("the linger's expiry queued %d close rows naming a claim, want none: the never-opened tree holds no claim", named)
	}
	queued, _ := rows()
	closeRow := mustOutboxRow(t, root.Key, record.SuperviseRequest{Op: "tree_close", Tree: root.Key, Role: claim.RoleArchitect, Generation: 1, Linger: 1}, time.Now())
	if _, err := pool.Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error, lease_token)
		values ($1, $2, $3, 0, now(), '', '')`, string(closeRow.Kind), closeRow.Issue, string(closeRow.Payload)); err != nil {
		t.Fatalf("queue a close row an earlier release wrote: %v", err)
	}
	if closes, _ := rows(); closes != queued+1 {
		t.Fatalf("the outbox holds %d close rows, want the %d the expiry queued and the one queued after", closes, queued)
	}
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("run the close rows: %v", err)
	}
	if closes, all := rows(); closes != 0 || all != 0 {
		t.Errorf("the outbox holds %d close rows and %d rows in all after the closes ran, want none", closes, all)
	}
	if failed := strings.Count(logs.String(), `"msg":"outbox row failed"`); failed != 0 {
		t.Errorf("%d close attempts were logged as failed, want none", failed)
	}
}

type carryFixture struct {
	ctx       context.Context
	pool      *store.Pool
	records   record.Store
	trees     *store.Store
	sup       *supervisor
	rt        *fake.Runtime
	engine    *workflow.Engine
	admission *admit.Admission
	runner    *outbox
	clock     time.Time
}

func newCarryFixture(t *testing.T, now time.Time, root, child record.Issue, roots ...string) *carryFixture {
	t.Helper()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	putOutboxIssue(t, pool, records, root)
	putOutboxIssue(t, pool, records, child)
	trees := outboxTreeStore(t, pool, roots...)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.Store = trees
	engine := workflow.New(records, workflow.Config{Project: "legion", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour}, quietLogger())
	admission := admit.New(records, engine, 2, "legion", quietLogger())
	fixture := &carryFixture{ctx: ctx, pool: pool, records: records, trees: trees, sup: sup, rt: rt, engine: engine, admission: admission, clock: now}
	fixture.runner = &outbox{
		dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup, trees: trees,
		tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
		githubAPI: newBranchGitHub(t, nil, branchExists).url, notices: &outboxPublisher{},
		dispatch: &outboxDispatch{issue: dispatch.Issue{Key: child.Key, Status: "todo", HandedOver: true, Labels: []string{"legion"}}},
		handlers: []intake.Handler{engine, admission}, now: func() time.Time { return fixture.clock }, log: quietLogger(),
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/" + child.Key}, nil
		},
		remove: func(context.Context, workspace.Workspace, sync.Locker) error { return nil },
	}
	return fixture
}

func (f *carryFixture) run(t *testing.T, what string, attempts int) {
	t.Helper()
	for range attempts {
		if err := f.runner.RunOnce(f.ctx); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		f.clock = f.clock.Add(5 * time.Minute)
	}
}

// A done root's linger suspends every claim of its tree, and the hand-over at the linger's expiry
// carries an unfinished child into a tree of its own at its next generation, which every row of
// the old tree then serves an earlier one of. The child's own suspend can run only after the
// hand-over has moved it, and a child its new tree holds (on a blocker, behind the admission cap)
// is not started there, so nothing re-points its role, which still runs in the old tree. The old
// tree's cleanup waits on every claim still running in its tree, and the old root's next run waits
// on that cleanup. So the old tree's stop rows still stop the role while it stands in the old
// tree, as the daemon and keeping its session, as the new tree's start does. Neither the child's
// row nor its new tree's start releases anything of the old tree on the child's lane: each makes
// the old tree's waiting close due at once, and that close confirms the cleanup on its own issue's
// lane at the same moment, not at its backoff's retry. A role the new tree has re-pointed and
// started is that tree's, and the old tree's rows leave it running.
func TestTheOldTreesStopsReachACarriedRoleItsLingerSuspendMissed(t *testing.T) {
	t.Parallel()
	const rootKey, childKey = "LEGION-207", "LEGION-208"
	for _, tc := range []struct {
		name string
		// op is the row of the child's planner that runs after the hand-over: the linger's
		// suspend, or the close at the linger's expiry, of the old tree, or the new tree's start.
		op record.SuperviseOp
		// repointed is a planner the new tree has already re-pointed and started.
		repointed bool
	}{
		{name: "the linger's suspend", op: "suspend"},
		{name: "the close at the linger's expiry", op: "tree_close"},
		{name: "the new tree's start", op: "start"},
		{name: "a role its new tree re-pointed", op: "tree_close", repointed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := time.Now().UTC().Truncate(time.Second)
			until := clock.Add(-time.Minute)
			root := record.Issue{Key: rootKey, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
				Tree: rootKey, Title: "root", Phase: phase.Done, Generation: 1, Status: "done", Rank: "A", LingerUntil: &until}
			// The hand-over carried the child: a tree of its own at generation 2, held at admitted,
			// or, where its new tree starts it, planning.
			carried := record.Issue{Key: childKey, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
				Tree: childKey, Title: "child", Phase: phase.Admitted, Generation: 2, Status: "todo", Rank: "B", HandedOver: true}
			if tc.op == "start" {
				carried.Phase, carried.Status = phase.Planning, "in_progress"
			}
			fixture := newCarryFixture(t, clock, root, carried, rootKey, childKey)
			ctx, pool, st, records, sup, rt, runner := fixture.ctx, fixture.pool, fixture.trees, fixture.records, fixture.sup, fixture.rt, fixture.runner
			architect := launchedClaim(t, sup, rootKey, rootKey, claim.RoleArchitect)
			plannerTree := rootKey
			if tc.repointed {
				plannerTree = childKey
			}
			planner := launchedClaim(t, sup, plannerTree, childKey, claim.RolePlanner)
			cleaned := func() bool {
				t.Helper()
				live, err := st.TreeLive(ctx, "legion", rootKey)
				if err != nil {
					t.Fatalf("read the lifecycle of %s: %v", rootKey, err)
				}
				return !live
			}
			releases := func() int { return len(rt.CallsOf("CleanupTree")) }
			stopRow := func(issue string, op record.SuperviseOp, role claim.Role, generation uint64) record.OutboxRow {
				payload := record.SuperviseRequest{Op: op, Tree: rootKey, Role: role, Generation: generation}
				switch op {
				case "tree_close":
					payload.Linger = 1
				case "start":
					payload.Tree, payload.Generation, payload.Phase, payload.Task = childKey, 2, phase.Planning, "Plan it."
				default:
					payload.Reason = "the tree of " + rootKey + " lingers"
				}
				return mustOutboxRow(t, issue, payload, clock)
			}

			// The root's architect closes first, which reserves the old tree's cleanup and retires it.
			enqueueOutbox(t, pool, records, stopRow(rootKey, "tree_close", claim.RoleArchitect, 1))
			if err := runner.RunOnce(ctx); err != nil {
				t.Fatalf("run the root's close: %v", err)
			}
			if machine, _ := sup.Machine(architect); machine.Claim().State != supervise.StateRetired {
				t.Fatalf("the root's architect after its close = %s, want retired", machine.Claim().State)
			}
			var waiting string
			if err := pool.QueryRow(ctx, `select coalesce(max(last_error), '') from outbox where issue = $1`, rootKey).Scan(&waiting); err != nil {
				t.Fatalf("read the root's close: %v", err)
			}
			if tc.repointed {
				if waiting != "" || !cleaned() || releases() != 1 {
					t.Fatalf("the root's close with the planner in its new tree waits on %q, confirmed %t after %d releases; want the cleanup confirmed",
						waiting, cleaned(), releases())
				}
			} else if !strings.Contains(waiting, string(planner)+":ready") || cleaned() || releases() != 0 {
				t.Fatalf("the root's close waits on %q, confirmed %t after %d releases; want the cleanup waiting on the carried planner, still ready in %s",
					waiting, cleaned(), releases(), rootKey)
			}

			// The child's own row of the old tree, or its new tree's start, runs after the hand-over
			// moved it.
			if err := runner.execute(ctx, stopRow(childKey, tc.op, claim.RolePlanner, 1)); err != nil {
				t.Fatalf("the %s of the carried planner: %v", tc.op, err)
			}
			machine, _ := sup.Machine(planner)
			got := machine.Claim()
			suspends := 0
			for _, call := range rt.CallsOf("Suspend") {
				if call.Locator.Claim == planner {
					suspends++
				}
			}
			switch {
			case tc.repointed:
				if got.State != supervise.StateReady || got.Tree != childKey || suspends != 0 {
					t.Fatalf("the re-pointed planner after the old tree's close = %s in %s, suspended %d times; want it running in %s, untouched",
						got.State, got.Tree, suspends, childKey)
				}
				return
			case tc.op == "start":
				// The start stopped the planner in the old tree, re-pointed it and resumed its session
				// in the new one.
				resumes := resumesOf(rt, planner)
				if suspends != 1 || got.Tree != childKey || len(resumes) != 1 || resumes[0].Spec.Tree != childKey ||
					resumes[0].Spec.ResumeSessionFile != "/tmp/planner.jsonl" {
					t.Fatalf("the carried planner after its new tree's start = %s in %s, suspended %d times, resumed %+v; "+
						"want it suspended once in %s, then its session resumed once in %s", got.State, got.Tree, suspends, resumes, rootKey, childKey)
				}
			default:
				if got.State != supervise.StateSuspended || got.SuspendedBy != supervise.SuspendedByDaemon || got.Tree != rootKey ||
					got.Session != "ses-planner" || got.SessionFile != "/tmp/planner.jsonl" || suspends != 1 {
					t.Fatalf("the carried planner after the old tree's %s = %s by %q in %s, session %q at %q, suspended %d times; "+
						"want it suspended once by the daemon in %s, its session kept", tc.op, got.State, got.SuspendedBy, got.Tree, got.Session,
						got.SessionFile, suspends, rootKey)
				}
			}
			if cleaned() || releases() != 0 {
				t.Fatalf("the child's %s confirmed the old tree's cleanup %t after %d releases; want the old tree's resources left to its own close",
					tc.op, cleaned(), releases())
			}
			// The root's close is due again now, though its backoff named a later retry, and confirms
			// the cleanup in the same moment the planner stopped.
			if err := runner.RunOnce(ctx); err != nil {
				t.Fatalf("run the root's close once the carried planner stopped: %v", err)
			}
			if !cleaned() || releases() != 1 {
				t.Fatalf("the root's close once the carried planner stopped: confirmed %t after %d releases, the clock unmoved; "+
					"want the cleanup confirmed once, without waiting out the close's backoff", cleaned(), releases())
			}
		})
	}
}

func TestAStartThatFindsAnAlreadySuspendedCarriedClaimDoesNotWakeAnOldTreeClose(t *testing.T) {
	t.Parallel()
	const rootKey, childKey = "LEGION-207", "LEGION-208"
	now := time.Now().UTC().Truncate(time.Microsecond)
	until := now.Add(-time.Minute)
	root := record.Issue{Key: rootKey, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: rootKey, Title: "root", Phase: phase.Done, Generation: 1, Status: "done", Rank: "A", LingerUntil: &until}
	child := record.Issue{Key: childKey, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: childKey, Title: "child", Phase: phase.Planning, Generation: 2, Status: "in_progress", Rank: "B", HandedOver: true}
	fixture := newCarryFixture(t, now, root, child, rootKey, childKey)
	planner := launchedClaim(t, fixture.sup, rootKey, childKey, claim.RolePlanner)
	machine, _ := fixture.sup.Machine(planner)
	if err := machine.Handle(fixture.ctx, supervise.RequestSuspend{Claim: planner, By: supervise.SuspendedByDaemon}); err != nil {
		t.Fatalf("suspend the old-tree planner: %v", err)
	}
	future := now.Add(time.Hour)
	enqueueOutbox(t, fixture.pool, fixture.records, mustOutboxRow(t, rootKey, record.SuperviseRequest{Op: "tree_close", Tree: rootKey,
		Role: claim.RoleArchitect, Generation: 1, Linger: 1}, future))
	start := mustOutboxRow(t, childKey, record.SuperviseRequest{Op: "start", Tree: childKey, Role: claim.RolePlanner,
		Generation: 2, Phase: phase.Planning, Task: "Plan it."}, now)
	if err := fixture.runner.execute(fixture.ctx, start); err != nil {
		t.Fatalf("start the child with its planner already suspended in the old tree: %v", err)
	}
	var nextAt time.Time
	if err := fixture.pool.QueryRow(fixture.ctx, `select next_at from outbox where issue = $1 and kind = 'supervise'`, rootKey).Scan(&nextAt); err != nil {
		t.Fatalf("read the future close: %v", err)
	}
	if !nextAt.Equal(future) {
		t.Fatalf("the start moved the future old-tree close from %s to %s, want it left at %s", future, nextAt, future)
	}
}

// A left-tree stop waits only when its claim still may run in that tree. A stored claim with no
// machine can belong to another daemon during a rolling restart, a machine can already stand in a
// third tree, and an old close with no claim has nothing to stop.
func TestALeftTreeStopWaitsForTheMachineOfAStoredClaimInItsTree(t *testing.T) {
	t.Parallel()
	const rootKey, childKey, thirdTree = "LEGION-207", "LEGION-208", "LEGION-999"
	for _, tc := range []struct {
		name string
		// tree and state are the stored claim's; machineTree launches its claim here instead.
		tree, state, machineTree string
		op                       record.SuperviseOp
		waits, closeUnmoved      bool
	}{
		{name: "a claim that may still run in the old tree", tree: rootKey, state: "ready", op: "suspend", waits: true},
		{name: "a claim that runs nothing in the old tree", tree: rootKey, state: "suspended", op: "suspend"},
		{name: "a claim its new tree re-pointed", tree: childKey, state: "ready", op: "suspend"},
		{name: "a machine in a third tree", machineTree: thirdTree, op: "suspend"},
		{name: "a machine already suspended in the old tree", machineTree: rootKey, op: "suspend", closeUnmoved: true},
		{name: "a close with no claim", op: "tree_close"},
		{name: "no claim", op: "suspend"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC().Truncate(time.Microsecond)
			until := now.Add(-time.Minute)
			root := record.Issue{Key: rootKey, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
				Tree: rootKey, Title: "root", Phase: phase.Done, Generation: 1, Status: "done", Rank: "A", LingerUntil: &until}
			child := record.Issue{Key: childKey, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
				Tree: childKey, Title: "child", Phase: phase.Admitted, Generation: 2, Status: "todo", Rank: "B", HandedOver: true}
			fixture := newCarryFixture(t, now, root, child, rootKey, childKey, thirdTree)
			ctx, pool, records, sup, rt, runner := fixture.ctx, fixture.pool, fixture.records, fixture.sup, fixture.rt, fixture.runner
			planner := mustClaimToken(t, childKey, claim.RolePlanner)
			if tc.tree != "" {
				// Stored, as another daemon of a rolling restart stored it, with no machine here.
				if _, err := pool.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file,
					state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
					values ($1, 'legion', $2, $3, 'planner', 1, 'ses-planner', '/tmp/planner.jsonl', $4, 0, 0, 0, 0)`,
					string(planner), tc.tree, childKey, tc.state); err != nil {
					t.Fatalf("store the planner's claim: %v", err)
				}
			}
			if tc.machineTree == "" {
				if _, found := sup.Machine(planner); found {
					t.Fatalf("the supervisor holds a machine for %s; want none", planner)
				}
			} else {
				planner = launchedClaim(t, sup, tc.machineTree, childKey, claim.RolePlanner)
				if tc.closeUnmoved {
					machine, _ := sup.Machine(planner)
					if err := machine.Handle(ctx, supervise.RequestSuspend{Claim: planner, By: supervise.SuspendedByDaemon}); err != nil {
						t.Fatalf("suspend the old-tree planner before its stale stop: %v", err)
					}
				}
			}
			payload := record.SuperviseRequest{Op: tc.op, Tree: rootKey, Role: claim.RolePlanner, Generation: 1,
				Reason: childKey + " left tree " + rootKey}
			if tc.op == "tree_close" {
				payload.Linger = 1
			}
			var futureClose time.Time
			if tc.closeUnmoved {
				futureClose = now.Add(time.Hour)
				enqueueOutbox(t, pool, records, mustOutboxRow(t, rootKey, record.SuperviseRequest{Op: "tree_close", Tree: rootKey,
					Role: claim.RoleArchitect, Generation: 1, Linger: 1}, futureClose))
			}
			err := runner.execute(ctx, mustOutboxRow(t, childKey, payload, time.Now()))
			if tc.waits {
				if !errors.Is(err, wait.ErrWaiting) || !strings.Contains(err.Error(), "supervising machine of its stored claim "+string(planner)) {
					t.Fatalf("the old tree's suspend with no machine for the stored %s claim in %s = %v; want a wait for its machine", tc.state, tc.tree, err)
				}
			} else if err != nil {
				t.Fatalf("the old tree's %s with the stored claim %q in %q = %v; want it finished, with nothing to stop", tc.op, tc.state, tc.tree, err)
			}
			if tc.machineTree == thirdTree {
				machine, found := sup.Machine(planner)
				if !found {
					t.Fatalf("the supervisor holds no machine for the third-tree planner")
				}
				if got := machine.Claim(); got.Tree != thirdTree || got.State != supervise.StateReady {
					t.Fatalf("the third-tree planner after the old tree's stop = %+v; want it ready in %s", got, thirdTree)
				}
				for _, call := range rt.CallsOf("Suspend") {
					if call.Locator.Claim == planner {
						t.Fatalf("the old tree's stop suspended the third-tree planner")
					}
				}
			}
			if tc.closeUnmoved {
				var nextAt time.Time
				if err := pool.QueryRow(ctx, `select next_at from outbox where issue = $1 and kind = 'supervise'`, rootKey).Scan(&nextAt); err != nil {
					t.Fatalf("read the future close: %v", err)
				}
				if !nextAt.Equal(futureClose) {
					t.Fatalf("the stale stop moved the future close from %s to %s, want it left at %s", futureClose, nextAt, futureClose)
				}
			}
		})
	}
}

// A role of a lingering tree's member can still run in that tree with no stop row of the tree left
// to reach it. The linger's suspend of it can be superseded by a newer start, as a production
// check's look start is, the one start a lingering tree runs, and finish without acting; or that
// look's start, already past its fences when the move commits, can bring the claim up only after
// the move has read the claims. A person sets the merged child back to todo while its look runs,
// and admission carries it into a tree of its own (an orphan), held there on an unfinished blocker,
// so its new tree starts nothing that would stop the role. When the old root's linger expires, the
// old tree's cleanup waits on the role still running in it. The move itself queues the old tree's
// stop of every role of the issue, so the role is stopped, its session kept, and the old tree's
// cleanup confirms while the child still waits on its blocker.
func TestAMoveStopsTheCarriedRoleNoStopRowOfItsOldTreeStillReaches(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// afterMove is a claim the look's in-flight start brings up only once the move has read the
		// claims; otherwise the claim runs before the move, its linger suspend superseded.
		afterMove bool
	}{
		{name: "the linger's suspend superseded by the look's start"},
		{name: "the look's in-flight start brings the claim up after the move", afterMove: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			movedCarriedRole(t, tc.afterMove)
		})
	}
}

// movedCarriedRole runs one case of TestAMoveStopsTheCarriedRoleNoStopRowOfItsOldTreeStillReaches.
func movedCarriedRole(t *testing.T, afterMove bool) {
	const rootKey, childKey, blockerKey = "LEGION-207", "LEGION-208", "LEGION-300"
	start := time.Now().UTC().Truncate(time.Second)
	until := start.Add(time.Hour)
	parent := rootKey
	root := record.Issue{Key: rootKey, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: rootKey, Project: "LEGION",
		Title: "root", Phase: phase.Done, Generation: 1, Status: "done", Rank: "A", LingerUntil: &until, LastDispatchSeq: 1}
	child := record.Issue{Key: childKey, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: rootKey, Project: "LEGION",
		Title: "child", Parent: &parent, Phase: phase.Done, Generation: 1, Status: "done", Rank: "B", LastDispatchSeq: 1}
	fixture := newCarryFixture(t, start, root, child, rootKey)
	ctx, pool, records, trees, sup, rt, engine, admission := fixture.ctx, fixture.pool, fixture.records, fixture.trees, fixture.sup, fixture.rt, fixture.engine, fixture.admission
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		for _, put := range []error{
			records.PutDispatchIssue(ctx, tx, record.DispatchReading{Key: childKey, Project: "LEGION", Seq: 1,
				Status: record.Carry("done"), Parent: record.Carry(""), Blockers: record.Carry([]string{blockerKey})}),
			records.PutDispatchIssue(ctx, tx, record.DispatchReading{Key: blockerKey, Project: "LEGION", Seq: 1,
				Status: record.Carry("todo"), Parent: record.Carry(""), Blockers: record.Carry[[]string](nil)}),
		} {
			if put != nil {
				return put
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the child's blocker: %v", err)
	}
	run := func(what string) { fixture.run(t, what, 4) }

	var machine *supervise.Machine
	if !afterMove {
		// The child's tester runs in the old tree from a start newer than the linger's suspend of it.
		tester := launchedClaim(t, sup, rootKey, childKey, claim.RoleTester)
		machine, _ = sup.Machine(tester)
		enqueueOutbox(t, pool, records, mustOutboxRow(t, childKey, record.SuperviseRequest{Op: "suspend", Tree: rootKey, Role: claim.RoleTester,
			Generation: 1, Reason: "the tree of " + rootKey + " lingers"}, fixture.clock))
		var suspendRow int64
		if err := pool.QueryRow(ctx, `select id from outbox where issue = $1 and kind = 'supervise'`, childKey).Scan(&suspendRow); err != nil {
			t.Fatalf("read the linger's suspend row: %v", err)
		}
		// The look's start is the row right after the linger's suspend. Its id is taken from the
		// outbox's sequence, as the start's own row takes it, so no later row reuses it: the move's
		// stop rows come after the start, as they do in a deployment.
		if _, err := pool.Exec(ctx, `select setval(pg_get_serial_sequence('outbox', 'id'), $1)`, suspendRow+1); err != nil {
			t.Fatalf("take the look's start id: %v", err)
		}
		if err := machine.StartedBy(ctx, suspendRow+1, nil); err != nil {
			t.Fatalf("record the look's start: %v", err)
		}
		run("the linger's suspend, superseded by the look's start")
		if got := machine.Claim(); got.State != supervise.StateReady || got.Tree != rootKey {
			t.Fatalf("the tester after its superseded suspend = %s in %s; want it still ready in %s", got.State, got.Tree, rootKey)
		}
	}

	// A person sets the merged child back to todo: an orphan, a tree of its own held on its blocker.
	if _, err := intake.ApplyFact(ctx, pool, "dispatch", "person-sets-the-child-todo", intake.DispatchIssue{Key: childKey, ResolvedRepo: "acme/widgets",
		Seq: 2, Type: "issue.updated", Status: "todo", Title: "child", Parent: rootKey, Rank: "B", HandedOver: true, Blockers: []string{blockerKey}},
		engine, admission); err != nil {
		t.Fatalf("apply the child's todo: %v", err)
	}
	var carried *record.Issue
	if err := store.TransactWith(ctx, pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		carried, err = records.Issue(ctx, tx, childKey)
		return err
	}); err != nil || carried == nil || carried.Tree != childKey || carried.Generation != 2 || carried.Phase != phase.Admitted {
		t.Fatalf("the carried child = %+v, %v; want its own tree at generation 2, admitted", carried, err)
	}
	if afterMove {
		// The look's start, past its fences before the move, brings its claim up in the old tree now.
		tester := launchedClaim(t, sup, rootKey, childKey, claim.RoleTester)
		machine, _ = sup.Machine(tester)
	}
	run("the carry's rows while the child waits on its blocker")

	// The old root's linger expires, and its close reserves the old tree's cleanup.
	fixture.clock = until.Add(time.Minute)
	if _, err := intake.ApplyFact(ctx, pool, "outbox", "linger-expired", intake.LingerExpired{Issue: rootKey, Generation: 1}, engine, admission); err != nil {
		t.Fatalf("expire the old root's linger: %v", err)
	}
	run("the old tree's close")

	got := machine.Claim()
	if got.State != supervise.StateSuspended || got.SuspendedBy != supervise.SuspendedByDaemon || got.Tree != rootKey ||
		got.Session != "ses-tester" || got.SessionFile != "/tmp/tester.jsonl" {
		t.Errorf("the carried tester = %s by %q in %s, session %q at %q; want it suspended by the daemon in %s, its session kept",
			got.State, got.SuspendedBy, got.Tree, got.Session, got.SessionFile, rootKey)
	}
	if live, err := trees.TreeLive(ctx, "legion", rootKey); err != nil || live {
		var lastError string
		_ = pool.QueryRow(ctx, `select coalesce(max(last_error), '') from outbox where issue = $1 and payload->>'op' = 'tree_close'`, rootKey).Scan(&lastError)
		t.Errorf("the old tree's cleanup once its root's linger expired: live %t, %v, its close last waiting on %q; want it confirmed", live, err, lastError)
	}
	for _, call := range append(rt.CallsOf("Spawn"), rt.CallsOf("Resume")...) {
		if call.Spec.Tree == childKey {
			t.Errorf("the child's new tree launched %s while the child waits on %s; want nothing launched", call.Spec.Claim, blockerKey)
		}
	}
}
