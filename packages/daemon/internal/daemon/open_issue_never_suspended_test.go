package daemon

import (
	"context"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// An open issue's claims are stopped by nothing but the exceptions
// TestOpenIssueSuspensionInvariantKeepsTheExceptions enumerates, and an issue whose pod is still
// working takes none of them. This drives the durable workflow and outbox through phase changes,
// an awaiting-person hand-back and answer, two child re-entries, boot/tick reconciliation, and a
// pass of the idle tick with release on: no suspend, no issue-suspend, no step stop and no idle
// release may appear at any layer. The release (INFRA-945) and the owner's stop of a task are the
// two exceptions that act on an issue still in the workflow, so they are the ones this case has to
// keep out, and each is told from every other stop by its own kind and op: an `issue_release` row
// and `supervise` rows whose op is `release`, and a `supervise` row whose op is `step_stop`,
// against the `issue_suspend` row and the `suspend` op of a stop that ends a claim's phase.
func TestAnOpenIssueThatIsWorkingHasNoClaimStopped(t *testing.T) {
	t.Parallel()
	const (
		root     = "LEGION-800"
		forward  = "LEGION-801"
		backward = "LEGION-802"
		running  = "LEGION-803"
		checking = "LEGION-804"
	)
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()

	rootIssue := record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: root,
		Title: "root", Phase: phase.Admitted, Generation: 1, Status: "in_progress", Rank: "A", HandedOver: true}
	parent := root
	issues := []record.Issue{
		rootIssue,
		{Key: forward, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: root, Parent: &parent,
			Title: "forward", Phase: phase.Admitted, Generation: 1, Status: "todo", Rank: "B"},
		{Key: backward, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: root, Parent: &parent,
			Title: "backward", Phase: phase.Admitted, Generation: 1, Status: "todo", Rank: "C"},
		{Key: running, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: root, Parent: &parent,
			Title: "running", Phase: phase.Admitted, Generation: 1, Status: "todo", Rank: "D"},
		{Key: checking, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: root, Parent: &parent,
			Title: "checking", Phase: phase.Done, Generation: 1, Status: "done", Rank: "E"},
	}
	for _, issue := range issues {
		putOutboxIssue(t, pool, records, issue)
	}
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := records.PutSlot(ctx, tx, record.Slot{Issue: root, Index: 0, AdmittedAt: time.Now()}); err != nil {
			return err
		}
		for _, row := range []record.PhaseRow{
			{Issue: forward, Role: claim.RolePlanner, Claim: "forward-planner"},
			{Issue: backward, Role: claim.RoleImplementer, Claim: "backward-implementer"},
		} {
			if err := records.PutPhase(ctx, tx, row); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed open tree: %v", err)
	}

	trees := outboxTreeStore(t, pool, root)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.PhaseHolds = (&workflowRuntime{pool: pool, records: records}).phaseHolds
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, quietLogger())
	admission := admit.New(records, engine, 1, "LEGION", quietLogger())
	runner := &outbox{
		dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup, trees: trees,
		tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
		githubAPI: newBranchGitHub(t, nil, branchExists).url,
		dispatch:  &outboxDispatch{issue: dispatch.Issue{Key: root, Status: "in_progress", Labels: []string{"legion"}}},
		notices:   &outboxPublisher{}, handlers: []intake.Handler{engine, admission}, log: quietLogger(), now: time.Now,
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/" + root}, nil
		},
		remove: func(context.Context, workspace.Workspace, sync.Locker) error { return nil },
	}

	claims := make(map[claim.Token]struct{})
	launch := func(issue string, role claim.Role) claim.Token {
		t.Helper()
		// The root's roles keep the session named for their role, which the ask below names as its
		// author; a child's claim of the same role registers a session of its own, since the
		// daemon holds each session id to the first claim that recorded it.
		session := "ses-" + string(role)
		if issue != root {
			session = "ses-" + issue + "-" + string(role)
		}
		token := launchedClaimAs(t, sup, root, issue, role, session)
		claims[token] = struct{}{}
		return token
	}
	architect := launch(root, claim.RoleArchitect)
	for _, role := range []claim.Role{claim.RolePlanner, claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer, claim.RoleMerger} {
		launch(root, role)
	}
	tester := launch(running, claim.RoleTester)
	implementer := launch(checking, claim.RoleImplementer)
	for _, token := range []claim.Token{architect, tester, implementer} {
		machine, ok := sup.Machine(token)
		if !ok {
			t.Fatalf("no machine for %s", token)
		}
		if err := trees.PutClaim(ctx, machine.Claim()); err != nil {
			t.Fatalf("persist %s: %v", token, err)
		}
	}

	working := func(token claim.Token, id string, p phase.Phase) {
		t.Helper()
		machine, _ := sup.Machine(token)
		if err := machine.Handle(ctx, supervise.RequestDeliver{Claim: token, ID: id, Task: "Continue.", Phase: p, Generation: 1}); err != nil {
			t.Fatalf("deliver %s: %v", token, err)
		}
		machine.Wait()
		pending := machine.Claim().Pending
		if pending == nil {
			t.Fatalf("%s has no pending task", token)
		}
		if err := machine.Handle(ctx, supervise.StreamTurnStart{Claim: token, DeliveryID: pending.ID}); err != nil {
			t.Fatalf("start %s turn: %v", token, err)
		}
	}

	apply := func(id string, fact intake.Fact) {
		t.Helper()
		if result, err := intake.ApplyFact(ctx, pool, "test", id, fact, engine, admission); err != nil || result.Refusal != nil {
			t.Fatalf("apply %s = %#v, %v", id, result, err)
		}
	}
	assertOpen := func(stage string) {
		t.Helper()
		if got := len(rt.CallsOf("Suspend")); got != 0 {
			t.Fatalf("%s: runtime suspend calls = %d, want 0", stage, got)
		}
		for token := range claims {
			machine, _ := sup.Machine(token)
			got := machine.Claim()
			byRule := got.SuspensionHeldBy.ByRule() || got.SuspensionHeldBy == supervise.SuspendedByReleased
			if got.State == supervise.StateSuspended || got.SuspensionHeld && byRule {
				t.Fatalf("%s: %s = %+v, want live without a suspension by any daemon rule", stage, token, got)
			}
		}
		var stopping int
		if err := pool.QueryRow(ctx, `select count(*) from outbox where issue = any($1)
			and (kind in ('issue_suspend', 'issue_release') or (kind = 'supervise' and payload->>'op' in ('suspend', 'step_stop', 'release')))`,
			[]string{root, forward, backward, running, checking}).Scan(&stopping); err != nil {
			t.Fatalf("%s: count stopping rows: %v", stage, err)
		}
		if stopping != 0 {
			t.Fatalf("%s: stopping rows = %d, want none for open issues that are working", stage, stopping)
		}
		var marked int
		if err := pool.QueryRow(ctx, `select count(*) from issue_releases where issue = any($1)`,
			[]string{root, forward, backward, running, checking}).Scan(&marked); err != nil {
			t.Fatalf("%s: count release marks: %v", stage, err)
		}
		if marked != 0 {
			t.Fatalf("%s: release marks = %d, want none: no pod of this tree is idle", stage, marked)
		}
	}

	apply("ask", intake.DispatchAskOpened{Key: root, Ask: "ask-person", Kind: record.AskQuestion, AuthorSession: "ses-architect", Mark: record.EventMark{Seq: 10}})
	apply("hand-back", intake.DispatchAskReply{Key: root, Ask: "ask-person", WaitingOn: "human", Actor: dispatch.Actor{Kind: "session", ID: "ses-architect"}, Mark: record.EventMark{Seq: 11}})
	apply("awaiting-tick", intake.ControllerTick{})
	var rootAfterWait *record.Issue
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rootAfterWait, err = records.Issue(ctx, tx, root)
		return err
	}); err != nil || rootAfterWait == nil || !rootAfterWait.AwaitingPerson() {
		t.Fatalf("root after hand-back = %#v, %v; want awaiting_person", rootAfterWait, err)
	}
	assertOpen("awaiting person")
	apply("answer", intake.DispatchAskClosed{Key: root, Ask: "ask-person", Kind: record.AskQuestion, State: "answered", Actor: dispatch.Actor{Kind: "user", ID: "person"}, Mark: record.EventMark{Seq: 12}})
	apply("answer-tick", intake.ControllerTick{})
	assertOpen("answer and re-admission")
	forwardIssue := issues[1]
	forwardIssue.Phase, forwardIssue.Status = phase.Planning, "in_progress"
	putOutboxIssue(t, pool, records, forwardIssue)
	backwardIssue := issues[2]
	backwardIssue.Phase, backwardIssue.Status = phase.Retro, "retro"
	putOutboxIssue(t, pool, records, backwardIssue)
	runningIssue := issues[3]
	runningIssue.Phase, runningIssue.Status = phase.Testing, "testing"
	putOutboxIssue(t, pool, records, runningIssue)
	working(tester, "running-task", phase.Testing)
	working(implementer, "check-task", "")
	check := openCheck(issues[4], "merged")
	putProductionCheck(t, pool, records, check)

	apply("forward", intake.HandoffComplete{Issue: forward, Role: claim.RolePlanner, Claim: "forward-planner", Generation: 1, Summary: "planned", HandoffRecord: "forward-plan", HandoffVersion: 1})
	apply("backward", intake.BackwardMove{Issue: backward, Requester: claim.RoleImplementer, To: phase.Implementing, Reason: "rework"})
	assertOpen("ordinary forward and backward transitions")

	apply("running-reentry", intake.DispatchIssue{Key: running, ResolvedRepo: "acme/widgets", Seq: 20, Type: "issue.updated", Status: "todo", Title: "running", Parent: root, Rank: "D"})
	apply("check-reentry", intake.DispatchIssue{Key: checking, ResolvedRepo: "acme/widgets", Seq: 21, Type: "issue.updated", Status: "todo", Title: "checking", Parent: root, Rank: "E"})
	if got := readProductionCheck(t, pool, records, checking); got.Open() || got.Outcome != record.CheckNotRun {
		t.Fatalf("production check after re-entry = %+v, want closed not_run", got)
	}
	assertOpen("child re-entry")

	apply("boot", intake.DaemonBooted{Boot: 1})
	apply("reconcile-tick", intake.ControllerTick{})
	for range 32 {
		if err := runner.RunOnce(ctx); err != nil {
			t.Fatalf("drain outbox: %v", err)
		}
	}
	assertOpen("boot, tick, and outbox drain")

	// The idle release is the one exception that acts on an issue still in the workflow, so a
	// pass of the tick with release on has to leave this tree as it found it: every pod here took
	// a turn a moment ago, so none of them is idle. The inputs the rule reads outside the issue
	// record are all given to it — the Envoy listener reporting itself ready, the pods on the
	// configured image, and every claim on the daemon's plugin contract having reported what it
	// is doing since its last turn — so what keeps these pods is the rule itself, not an input it
	// could not read.
	if _, err := pool.Exec(ctx, `insert into listener_reports (project, ready_at) values ($1, $2)
		on conflict (project) do update set ready_at = excluded.ready_at`, "legion", time.Now().UTC()); err != nil {
		t.Fatalf("seed the listener's report: %v", err)
	}
	if _, err := pool.Exec(ctx, `update claims set plugin_contract = $1, activity_at = now(),
		activity_background = false, activity_kinds = '', activity_next_reminder_at = null`, api.DaemonAPIVersion); err != nil {
		t.Fatalf("report what the pods are doing: %v", err)
	}
	idle := &workflowRuntime{pool: pool, projectID: "legion", log: quietLogger(), idleStore: trees,
		idleRuntime: &releaseRuntime{Runtime: fake.NewRuntime(), onImage: true}, idlePeriod: time.Minute}
	if !idle.listenerLive(ctx) {
		t.Fatal("the listener's report is not live, so the tick below would decide no pod at all")
	}
	pods, podsErr := trees.IdlePods(ctx, "legion")
	if podsErr != nil || len(pods) == 0 {
		t.Fatalf("the pods the tick decides = %v, %v; want this tree's live claims", pods, podsErr)
	}
	idle.releaseIdlePods(ctx)
	assertOpen("a pass of the idle tick over working pods")
}

// TestOpenIssueSuspensionInvariantKeepsTheExceptions enumerates every stop an open issue's claims
// may take: the tree move that carries a child, the three statuses that take an issue out of the
// workflow, the owner's stop of a task, and the idle release (INFRA-945). The stop of a task and
// the release leave the issue in the workflow: the release brings its pod back at the next wake
// for one of its roles, and the stopped task's issue stays held until a later version of its plan
// starts the task again. Each is told from the others by the kind and op it writes.
// TestTheStoreRecordsWhoSuspendedAClaim covers the operator's real
// POST /legion/v1/operator/claims/{token}/suspend route, and
// TestAnOpenIssueThatIsWorkingHasNoClaimStopped that an open issue whose pod is working takes
// none of these.
func TestOpenIssueSuspensionInvariantKeepsTheExceptions(t *testing.T) {
	t.Parallel()

	t.Run("tree move", func(t *testing.T) {
		carriedChildStops(t, "LEGION-811", "LEGION-812", func(apply func(string, intake.Fact)) {
			apply("carry", intake.DispatchIssue{Key: "LEGION-812", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "todo", Title: "child", Parent: "LEGION-811", Rank: "B", HandedOver: true})
		}, false, turnEnds)
	})

	for _, status := range []string{"done", "backlog", "icebox"} {
		t.Run("leave "+status, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			root := record.Issue{Key: "LEGION-820", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-820", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "A", HandedOver: true}
			parent := root.Key
			child := record.Issue{Key: "LEGION-821", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: root.Key, Parent: &parent, Title: "child", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "B"}
			putOutboxIssue(t, pool, records, root)
			putOutboxIssue(t, pool, records, child)
			if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
				return records.PutSlot(ctx, tx, record.Slot{Issue: root.Key, Index: 0, AdmittedAt: time.Now()})
			}); err != nil {
				t.Fatal(err)
			}
			engine := workflow.New(records, workflow.Config{Project: "legion", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, quietLogger())
			if _, err := intake.ApplyFact(ctx, pool, "test", "leave-"+status, intake.DispatchIssue{Key: child.Key, ResolvedRepo: child.Repo, Seq: 2, Type: "issue.updated", Status: status, Title: child.Title, Parent: root.Key, Rank: child.Rank}, engine); err != nil {
				t.Fatalf("leave %s: %v", status, err)
			}
			var rows int
			op := "suspend"
			if status == "done" {
				op = "issue_close"
			}
			if err := pool.QueryRow(ctx, `select count(*) from outbox where issue = $1 and kind = 'supervise' and payload->>'op' = $2`, child.Key, op).Scan(&rows); err != nil || rows == 0 {
				t.Fatalf("leave %s rows = %d, %v; want %s", status, rows, err, op)
			}
		})
	}

	// The owner's stop of a task stops the process of the step it ran and keeps the session, in
	// one op of its own, step_stop, so it is told from the suspend of an issue the workflow takes
	// out: the issue stays in the workflow, held from the step's phase for the stopped task. An
	// issue with no plan rows has no task to stop, so the same stop writes nothing.
	for _, planned := range []bool{true, false} {
		name := "a stopped task"
		if !planned {
			name = "a stop of an issue with no plan rows"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			root := record.Issue{Key: "LEGION-830", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-830", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "A", HandedOver: true}
			putOutboxIssue(t, pool, records, root)
			architect, err := claim.NewToken("legion", root.Key, claim.RoleArchitect)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
				if err := records.PutSlot(ctx, tx, record.Slot{Issue: root.Key, Index: 0, AdmittedAt: time.Now()}); err != nil || !planned {
					return err
				}
				v := record.PlanVersion{Issue: root.Key, Generation: root.Generation, Version: 2, ArtifactID: "plan-artifact", Source: record.PlanSourceOwner, GivenBy: architect, GivenAt: time.Now()}
				task := record.PlanTask{Issue: root.Key, Generation: root.Generation, Task: "a", PlanVersion: 2, BlockText: "Build the widget.",
					State: record.TaskRunning, Place: record.TaskPlace{Kind: record.PlaceStep, Position: 1}}
				steps := []record.PlanStep{
					{Issue: root.Key, Generation: root.Generation, Task: "a", Position: 1, Role: "implementer", Type: "implementation"},
					{Issue: root.Key, Generation: root.Generation, Task: "a", Position: 2, Role: "reviewer", Type: "review"},
				}
				return records.PutPlan(ctx, tx, v, []record.PlanTask{task}, steps)
			}); err != nil {
				t.Fatal(err)
			}
			engine := workflow.New(records, workflow.Config{Project: "legion", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, quietLogger())
			result, err := intake.ApplyFact(ctx, pool, "test", "stop-task", intake.TaskStopped{Issue: root.Key, Task: "a", By: architect, Generation: root.Generation}, engine)
			if err != nil {
				t.Fatalf("stop task a: %v", err)
			}
			if planned == (result.Refusal != nil) {
				t.Fatalf("the stop's refusal = %+v, want one only where the issue has no plan rows", result.Refusal)
			}
			ops := map[string]int{}
			rows, err := pool.Query(ctx, `select case when kind = 'supervise' then 'supervise:' || (payload->>'op') else kind end from outbox where issue = $1
				and (kind in ('issue_suspend', 'issue_release') or kind = 'supervise')`, root.Key)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var op string
				if err := rows.Scan(&op); err != nil {
					t.Fatal(err)
				}
				ops[op]++
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			want := map[string]int{}
			if planned {
				want["supervise:step_stop"] = 1
			}
			if !maps.Equal(ops, want) {
				t.Fatalf("the stop's stopping rows = %v, want %v: a step_stop of the step's role where a task stopped, and no suspend, issue_close or release", ops, want)
			}
			var status string
			if err := pool.QueryRow(ctx, `select status from issues where key = $1`, root.Key).Scan(&status); err != nil {
				t.Fatalf("read the stopped issue: %v", err)
			}
			if record.OutOfWorkflow(status) {
				t.Fatalf("the stopped issue is %q, want the stop to have left it in the workflow", status)
			}
		})
	}

	// The idle release, the other exception that acts on an issue still in the workflow: its pod has
	// had no turn in progress and no turn due for the idle period, so every live claim of it is
	// suspended with the source `released` and its session kept. It writes neither the `suspend`
	// op of a stop that ends a phase nor the `issue_close` of an issue that left the workflow,
	// but its own: one `release` supervise row per live role, then the `issue_release` teardown.
	t.Run("idle release", func(t *testing.T) {
		ctx := context.Background()
		rig := newIdleRig(t)
		if record.OutOfWorkflow(rig.issue.Status) {
			t.Fatalf("the rig's issue is %q, want one still in the workflow", rig.issue.Status)
		}

		queued := rig.queuedBy(t, func() { rig.w.releaseIdlePods(ctx) })
		if want := []string{"issue_bundle", "supervise:release", "issue_release"}; !slices.Equal(queued, want) {
			t.Fatalf("queued rows = %v, want the save, the role stop and the teardown and nothing else", queued)
		}
		rig.drain(t)
		if got := rig.machine.Claim(); got.State != supervise.StateSuspended || got.SuspendedBy != supervise.SuspendedByReleased {
			t.Fatalf("the released claim = %s by %s, want suspended by released", got.State, got.SuspendedBy)
		}
		var status string
		if err := rig.pool.QueryRow(ctx, `select status from issues where key = $1`, rig.issue.Key).Scan(&status); err != nil {
			t.Fatalf("read the released issue: %v", err)
		}
		if record.OutOfWorkflow(status) {
			t.Fatalf("the released issue is %q, want the release to have left it in the workflow", status)
		}
	})
}
