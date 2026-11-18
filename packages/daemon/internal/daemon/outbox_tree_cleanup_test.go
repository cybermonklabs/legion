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
				project: "legion", stateDir: t.TempDir(),
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
// set to backlog) lingers like any root, and its linger's expiry queues a close row per role. Its
// tree never had a lifecycle, so it holds nothing a cleanup releases: each close reserves nothing
// and finishes at the ordinary fences, with no claim to close. Before, the reservation refused the
// absent lifecycle, and every close row retried on its backoff forever, logged as a failure each
// time.
func TestANeverOpenedTreesLingerClosesFinishWithoutRetrying(t *testing.T) {
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
	// The first run expires the linger, which queues the tree's close rows; the second runs them.
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("run the linger: %v", err)
	}
	if closes, _ := rows(); closes != len(claim.Roles) {
		t.Fatalf("the linger's expiry queued %d close rows, want one per role (%d)", closes, len(claim.Roles))
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

// A done root's linger suspends every claim of its tree, and the hand-over at the linger's expiry
// carries an unfinished child into a tree of its own at its next generation, which every row of
// the old tree then serves an earlier one of. The child's own suspend can run only after the
// hand-over has moved it, and a child its new tree holds (on a blocker, behind the admission cap)
// is not started there, so nothing re-points its role, which still runs in the old tree. The old
// tree's cleanup waits on every claim still running in its tree, and the old root's next run waits
// on that cleanup. So the old tree's stop rows still stop the role while it stands in the old
// tree, as the daemon and keeping its session, as the new tree's start would, and the close then
// confirms the old tree's cleanup itself. A role the new tree has re-pointed and started is that
// tree's, and the old tree's rows leave it running.
func TestTheOldTreesStopsReachACarriedRoleItsLingerSuspendMissed(t *testing.T) {
	const rootKey, childKey = "LEGION-207", "LEGION-208"
	for _, tc := range []struct {
		name string
		// op is the old tree's row of the child's planner that runs after the hand-over: the
		// linger's suspend, or the close at the linger's expiry.
		op record.SuperviseOp
		// repointed is a planner the new tree has already re-pointed and started.
		repointed bool
	}{
		{name: "the linger's suspend", op: "suspend"},
		{name: "the close at the linger's expiry", op: "tree_close"},
		{name: "a role its new tree re-pointed", op: "tree_close", repointed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			st := outboxTreeStore(t, pool, rootKey, childKey)
			records := record.NewStore()
			until := time.Now().Add(-time.Minute)
			putOutboxIssue(t, pool, records, record.Issue{Key: rootKey, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
				Tree: rootKey, Title: "root", Phase: phase.Done, Generation: 1, Status: "done", Rank: "A", LingerUntil: &until})
			// The hand-over carried the child: a tree of its own at generation 2, held at admitted.
			putOutboxIssue(t, pool, records, record.Issue{Key: childKey, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
				Tree: childKey, Title: "child", Phase: phase.Admitted, Generation: 2, Status: "todo", Rank: "B", HandedOver: true})
			sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
			sup.deps.Store = st
			architect := launchedClaim(t, sup, rootKey, rootKey, claim.RoleArchitect)
			plannerTree := rootKey
			if tc.repointed {
				plannerTree = childKey
			}
			planner := launchedClaim(t, sup, plannerTree, childKey, claim.RolePlanner)
			runner := &outbox{log: quietLogger(), dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup, trees: st,
				tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), now: time.Now}
			cleaned := func() bool {
				t.Helper()
				live, err := st.TreeLive(ctx, "legion", rootKey)
				if err != nil {
					t.Fatalf("read the lifecycle of %s: %v", rootKey, err)
				}
				return !live
			}
			stopRow := func(issue string, op record.SuperviseOp, role claim.Role, generation uint64) record.OutboxRow {
				payload := record.SuperviseRequest{Op: op, Tree: rootKey, Role: role, Generation: generation}
				if op == "tree_close" {
					payload.Linger = 1
				} else {
					payload.Reason = "the tree of " + rootKey + " lingers"
				}
				return mustOutboxRow(t, issue, payload, time.Now())
			}

			// The root's architect closes first, which reserves the old tree's cleanup and retires it.
			err := runner.execute(ctx, stopRow(rootKey, "tree_close", claim.RoleArchitect, 1))
			if machine, _ := sup.Machine(architect); machine.Claim().State != supervise.StateRetired {
				t.Fatalf("the root's architect after its close = %s, want retired", machine.Claim().State)
			}
			if tc.repointed {
				if err != nil || !cleaned() {
					t.Fatalf("the root's close with the planner in its new tree = %v, confirmed %t; want the cleanup confirmed", err, cleaned())
				}
			} else if !errors.Is(err, wait.ErrWaiting) || !strings.Contains(err.Error(), string(planner)+":ready") || cleaned() {
				t.Fatalf("the root's close = %v, confirmed %t; want the cleanup waiting on the carried planner, still ready in %s",
					err, cleaned(), rootKey)
			}

			// The child's own row of the old tree runs after the hand-over moved it.
			if err := runner.execute(ctx, stopRow(childKey, tc.op, claim.RolePlanner, 1)); err != nil {
				t.Fatalf("the old tree's %s of the carried planner: %v", tc.op, err)
			}
			machine, _ := sup.Machine(planner)
			got := machine.Claim()
			suspends := 0
			for _, call := range rt.CallsOf("Suspend") {
				if call.Locator.Claim == planner {
					suspends++
				}
			}
			if tc.repointed {
				if got.State != supervise.StateReady || got.Tree != childKey || suspends != 0 {
					t.Fatalf("the re-pointed planner after the old tree's close = %s in %s, suspended %d times; want it running in %s, untouched",
						got.State, got.Tree, suspends, childKey)
				}
				return
			}
			if got.State != supervise.StateSuspended || got.SuspendedBy != supervise.SuspendedByDaemon || got.Tree != rootKey ||
				got.Session != "ses-planner" || got.SessionFile != "/tmp/planner.jsonl" || suspends != 1 {
				t.Fatalf("the carried planner after the old tree's %s = %s by %q in %s, session %q at %q, suspended %d times; "+
					"want it suspended once by the daemon in %s, its session kept", tc.op, got.State, got.SuspendedBy, got.Tree, got.Session,
					got.SessionFile, suspends, rootKey)
			}
			if tc.op == "tree_close" && !cleaned() {
				t.Fatalf("the old tree's close of the carried planner stopped it but left the cleanup of %s unconfirmed", rootKey)
			}
			// A suspend drives no cleanup: the close that waited confirms it at its next attempt.
			if err := runner.execute(ctx, stopRow(rootKey, "tree_close", claim.RoleArchitect, 1)); err != nil || !cleaned() {
				t.Fatalf("the root's close once the carried planner stopped = %v, confirmed %t; want the cleanup confirmed", err, cleaned())
			}
		})
	}
}
