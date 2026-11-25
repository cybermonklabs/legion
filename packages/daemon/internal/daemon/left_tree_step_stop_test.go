package daemon

import (
	"context"
	"fmt"
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
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The owner stops a child's task while the step of its implementer runs, and the child leaves its
// tree before the outbox runs that stop: the root finishes, and its hand-over at the linger's expiry
// carries the child into a tree of its own at its next generation. The role's process stops with its
// session kept, the stopped step's run has its end in the step record, and the role does not run
// again while its task stays stopped.
//
// The stop's row (step_stop) names the tree and the generation the child has left, so the outbox
// finishes it without acting (the generation fence), and the role's process stops through the
// suspend of the old tree that the move queues for every role (workflow.Engine.StopLeftTree), which
// the outbox runs for a claim still standing there (stopInLeftTree). The hand-over's promotion then
// queues a start of every role of the held member that has run, the stopped step's among them
// (admit's startMidPhaseMembers), and that start finishes at the outbox, the one place every start
// row of a role runs (workflow.StoppedStepRole).
func TestAStoppedStepsRoleStaysStoppedWhenItsIssueLeavesItsTree(t *testing.T) {
	t.Parallel()
	const rootKey, childKey = "LEGION-207", "LEGION-208"
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	start := time.Now().UTC().Truncate(time.Second)
	parent := rootKey
	root := record.Issue{Key: rootKey, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: rootKey, Project: "LEGION",
		Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "A", LastDispatchSeq: 1}
	child := record.Issue{Key: childKey, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: rootKey, Project: "LEGION",
		Title: "child", Parent: &parent, Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "B", LastDispatchSeq: 1}
	owner := mustClaimToken(t, rootKey, claim.RoleArchitect)
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		version := record.PlanVersion{Issue: childKey, Generation: 1, Version: 2, ArtifactID: "plan-artifact", Source: record.PlanSourceOwner,
			GivenBy: owner, GivenAt: start.Add(-time.Hour)}
		task := record.PlanTask{Issue: childKey, Generation: 1, Task: "a", PlanVersion: 2, BlockText: "Build the widget.",
			State: record.TaskRunning, Place: record.TaskPlace{Kind: record.PlaceStep, Position: 1}}
		steps := record.NameAgents(nil, nil, nil, []record.PlanStep{
			{Issue: childKey, Generation: 1, Task: "a", Position: 1, Role: "implementer", Type: "implementation"},
			{Issue: childKey, Generation: 1, Task: "a", Position: 2, Role: "reviewer", Type: "review"},
		})
		for _, put := range []error{
			records.PutIssue(ctx, tx, root),
			records.PutIssue(ctx, tx, child),
			records.PutPlan(ctx, tx, version, []record.PlanTask{task}, steps),
		} {
			if put != nil {
				return put
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the root and its child with its plan: %v", err)
	}
	trees := outboxTreeStore(t, pool, rootKey)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.Store = trees
	engine := workflow.New(records, workflow.Config{Project: "legion", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour}, quietLogger())
	admission := admit.New(records, engine, 2, "legion", quietLogger())
	clock := start
	runner := &outbox{
		dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup, trees: trees,
		tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
		githubAPI: newBranchGitHub(t, nil, branchExists).url, notices: &outboxPublisher{},
		dispatch: &outboxDispatch{issue: dispatch.Issue{Key: childKey, Status: "todo", HandedOver: true, Labels: []string{"legion"}}},
		handlers: []intake.Handler{engine, admission}, now: func() time.Time { return clock },
		log: quietLogger(),
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/" + childKey}, nil
		},
	}
	apply := func(id string, fact intake.Fact) {
		t.Helper()
		if result, err := intake.ApplyFact(ctx, pool, "dispatch", id, fact, engine, admission); err != nil || result.Refusal != nil {
			t.Fatalf("apply %s = %+v, %v; want it taken", id, result.Refusal, err)
		}
	}

	// The child's implementer runs the first step of task a, in the root's tree.
	implementer := launchedClaim(t, sup, rootKey, childKey, claim.RoleImplementer)
	machine, _ := sup.Machine(implementer)
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		ctx = engine.OpenFact(ctx)
		if err := engine.StartStepRecord(ctx, tx, child); err != nil {
			return fmt.Errorf("start the implementer's step: %w", err)
		}
		if err := engine.SettleFact(ctx, tx); err != nil {
			return fmt.Errorf("settle the step's start: %w", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	stepRun := func() string {
		t.Helper()
		var run string
		if err := pool.QueryRow(ctx, `select run from step_starts where issue = $1 and task = 'a' and kind = 'step' and place = 1`, childKey).Scan(&run); err != nil {
			t.Fatalf("read the implementer's step run: %v", err)
		}
		return run
	}
	if run := stepRun(); run != string(record.RunOpen) {
		t.Fatalf("the implementer's step run = %q; want it open", run)
	}

	// The owner stops task a while the tree runs.
	apply("stop-task", intake.TaskStopped{Issue: childKey, Task: "a", By: owner, Generation: 1})
	var stops int
	if err := pool.QueryRow(ctx, `select count(*) from outbox where issue = $1 and kind = 'supervise' and payload->>'op' = 'step_stop'
		and payload->>'tree' = $2 and payload->>'role' = 'implementer' and (payload->>'generation')::int = 1`, childKey, rootKey).Scan(&stops); err != nil || stops != 1 {
		t.Fatalf("the stop queued %d step stops of the implementer in tree %s at generation 1, %v; want one", stops, rootKey, err)
	}
	if run := stepRun(); run != string(record.RunEnded) {
		t.Fatalf("the stopped step's run = %q; want it ended", run)
	}
	var ended int
	if err := pool.QueryRow(ctx, `select count(*) from outbox where issue = $1 and kind = 'step_record' and payload->>'task' = 'a'
		and (payload->>'step')::int = 1 and (payload->>'generation')::int = 1 and payload->>'result' = $2 and payload->>'ended_at' is not null`,
		childKey, string(workflow.ResultStopped)).Scan(&ended); err != nil || ended != 1 {
		t.Fatalf("the step records queued %d ends of task a's step 1, stopped, %v; want one", ended, err)
	}

	// Before the outbox runs the stop, the root finishes and lingers, and its hand-over at the
	// linger's expiry carries the child into a tree of its own.
	until := start.Add(time.Hour)
	root.Phase, root.Status, root.LingerUntil = phase.Done, "done", &until
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := records.PutIssue(ctx, tx, root); err != nil {
			return err
		}
		return records.PutHandover(ctx, tx, record.Handover{Issue: childKey, Tree: rootKey, Generation: 1})
	}); err != nil {
		t.Fatalf("finish the root and fence the child's hand-over: %v", err)
	}
	apply("handover:"+childKey, intake.Handover{Issue: childKey, Tree: rootKey, Generation: 1})
	var carried *record.Issue
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		carried, err = records.Issue(ctx, tx, childKey)
		return err
	}); err != nil || carried == nil || carried.Tree != childKey || carried.Generation != 2 || workflow.StoppedStepRole(*carried) != claim.RoleImplementer {
		t.Fatalf("the handed-over child = %+v, %v; want its own tree at generation 2, held for its stopped task", carried, err)
	}
	// The promotion brings back the held member's roles that have run, the stopped step's among them.
	var restarts int
	if err := pool.QueryRow(ctx, `select count(*) from outbox where issue = $1 and kind = 'supervise' and payload->>'op' = 'start'
		and payload->>'role' = 'implementer'`, childKey).Scan(&restarts); err != nil || restarts != 1 {
		t.Fatalf("the hand-over queued %d starts of the stopped step's implementer, %v; want one: this test is of that start", restarts, err)
	}

	for range 4 {
		if err := runner.RunOnce(ctx); err != nil {
			t.Fatalf("run the outbox: %v", err)
		}
		clock = clock.Add(5 * time.Minute)
	}
	architect := mustClaimToken(t, childKey, claim.RoleArchitect)
	var launched bool
	for _, call := range rt.CallsOf("Spawn") {
		launched = launched || (call.Spec.Claim == architect && call.Spec.Tree == childKey)
	}
	if !launched {
		t.Fatalf("the new tree's architect never launched: the outbox did not run the hand-over's promotion, so this proves nothing of it")
	}
	if got := machine.Claim(); got.State != supervise.StateSuspended || got.SuspendedBy != supervise.SuspendedByDaemon ||
		got.Session != "ses-implementer" || got.SessionFile != "/tmp/implementer.jsonl" {
		t.Fatalf("the implementer after the outbox ran = %s by %q on %q (%q); want its process stopped, suspended by the daemon with its session kept",
			got.State, got.SuspendedBy, got.Session, got.SessionFile)
	}
	if resumes := resumesOf(rt, implementer); len(resumes) != 0 {
		t.Fatalf("the implementer of the stopped task was resumed %d times (%+v); want none: its task stays stopped", len(resumes), resumes)
	}
}
