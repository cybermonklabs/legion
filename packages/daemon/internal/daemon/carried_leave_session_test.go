package daemon

import (
	"context"
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
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// A child runs the project's default steps after a change of owner, and its tester's step names
// ses-key-k, the session of key k's agent that the claim of the name tester records, as the
// session to leave (record.PlanStep.LeaveSession). No writer sets that field now, so the test
// writes the step's row with it.
// Before the tester's first start runs, a person's label carries the child into a tree of its own
// with its run (admission's orphan carry), which copies its plan to the new generation
// (record.Store.CarryPlan), and the promotion starts the tester there. That start reads the session
// to leave from the carried step, so it carries it, and the outbox's start brings the claim back in
// a session of its own: the runtime spawns a fresh agent, resumes none, and the claim records
// ses-key-k as abandoned. No agent but the key resumes the key's session. The control is the same
// carry of a step that leaves no session, whose start resumes the claim's session.
func TestAStartAfterACarryBeforeTheFirstStartLeavesTheKeysSession(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		leave string
	}{
		{"the step leaves key k's session", "ses-key-k"},
		{"the control: the step leaves no session", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			const rootKey, childKey = "LEGION-207", "LEGION-208"
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			start := time.Now().UTC().Truncate(time.Second)
			until := start.Add(time.Hour)
			parent := rootKey
			version := record.PlanVersion{Issue: childKey, Generation: 1, Source: record.PlanSourceDefault, GivenAt: start.Add(-time.Hour)}
			task := record.PlanTask{Issue: childKey, Generation: 1, Task: "default", BlockText: "The project's default steps.", StepsByDefault: true,
				State: record.TaskRunning, Place: record.TaskPlace{Kind: record.PlaceStep, Position: 2}}
			steps := []record.PlanStep{
				{Issue: childKey, Generation: 1, Task: "default", Position: 1, Role: "implementer", Type: "implementation", Agent: "implementer"},
				{Issue: childKey, Generation: 1, Task: "default", Position: 2, Role: "tester", Type: "testing", Agent: "tester", LeaveSession: tc.leave},
				{Issue: childKey, Generation: 1, Task: "default", Position: 3, Role: "reviewer", Type: "review", Agent: "reviewer"},
			}
			if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
				for _, put := range []error{
					records.PutIssue(ctx, tx, record.Issue{Key: rootKey, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: rootKey, Project: "LEGION", Title: "root",
						Phase: phase.Done, Generation: 1, Status: "done", Rank: "A", LingerUntil: &until, LastDispatchSeq: 1}),
					records.PutIssue(ctx, tx, record.Issue{Key: childKey, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: rootKey, Project: "LEGION", Title: "child",
						Parent: &parent, Phase: phase.Testing, Generation: 1, Status: "in_progress", Rank: "B", LastDispatchSeq: 1}),
					records.PutHandover(ctx, tx, record.Handover{Issue: childKey, Tree: rootKey, Generation: 1}),
					records.PutPlan(ctx, tx, version, []record.PlanTask{task}, steps),
				} {
					if put != nil {
						return put
					}
				}
				return nil
			}); err != nil {
				t.Fatalf("seed the finished root and its child: %v", err)
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

			// The claim of the name tester ran key k's agent in the old tree, and is ready there.
			tester := launchedClaimAs(t, sup, rootKey, childKey, claim.RoleTester, "ses-key-k")
			machine, _ := sup.Machine(tester)
			launches := func(method string) int {
				n := 0
				for _, call := range rt.CallsOf(method) {
					if call.Spec.Claim == tester || call.Locator.Claim == tester {
						n++
					}
				}
				return n
			}
			spawns, resumes := launches("Spawn"), launches("Resume")

			if _, err := intake.ApplyFact(ctx, pool, "dispatch", "person-labels-the-child", intake.DispatchIssue{Key: childKey, ResolvedRepo: "acme/widgets", Seq: 2,
				Type: "issue.updated", Status: "todo", Title: "child", Parent: rootKey, Rank: "B", HandedOver: true}, engine, admission); err != nil {
				t.Fatalf("carry the child: %v", err)
			}
			var leaves []string
			rows, err := pool.Query(ctx, `select coalesce(payload->>'leaveSession', '') from outbox where issue = $1 and kind = 'supervise'
				and payload->>'op' = 'start' and payload->>'role' = 'tester' and payload->>'generation' = '2' and coalesce(payload->>'task', '') <> ''`, childKey)
			if err == nil {
				leaves, err = pgx.CollectRows(rows, pgx.RowTo[string])
			}
			if err != nil || len(leaves) != 1 || leaves[0] != tc.leave {
				t.Errorf("the promotion's starts of the tester at generation 2 leave %q (%v), want one leaving %q: the carried step lost the session to leave", leaves, err, tc.leave)
			}

			for range 6 {
				if err := runner.RunOnce(ctx); err != nil {
					t.Fatalf("run the carry's rows: %v", err)
				}
				clock = clock.Add(5 * time.Minute)
			}

			spawned, resumed := launches("Spawn")-spawns, launches("Resume")-resumes
			got := machine.Claim()
			if tc.leave == "" {
				if spawned != 0 || resumed != 1 || got.Session != "ses-key-k" {
					t.Fatalf("the tester's claim after a carried step that leaves nothing spawned %d and resumed %d, and records %q; want its session ses-key-k resumed once",
						spawned, resumed, got.Session)
				}
				return
			}
			if spawned != 1 || resumed != 0 {
				t.Fatalf("the tester's claim after the carry spawned %d and resumed %d; want a fresh agent and key k's session resumed by nobody", spawned, resumed)
			}
			if got.Session == "ses-key-k" || got.Abandoned.Session != "ses-key-k" || got.Tree != childKey {
				t.Fatalf("the tester's claim records %q, abandoned %q, in tree %s; want ses-key-k abandoned, in tree %s", got.Session, got.Abandoned.Session, got.Tree, childKey)
			}
		})
	}
}
