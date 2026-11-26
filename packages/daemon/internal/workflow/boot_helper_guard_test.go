package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The boot pass these tests run before a fact (bootBefore) is the daemon's boot only the first
// time. After it, a pass that opens a run of an issue a fact left in a step is a gap the pass
// would hide: the fact moved the issue and counted no start, so production, whose boot ran before
// that fact, would record nothing of the run. The faults here are two such facts: a write of the
// issue into testing that counts no start, and a write of the issue's plan rows, whose walk puts
// the implementing it stands in at task a's step 1, that counts none; each one's pass fails. The
// controls are the same writes made by the test itself between two facts, as a seed of an issue
// the earlier release moved is, and the first fact after the scenario boots again (rebooted):
// none fails.
func TestTheBootPassBeforeAFactRefusesARunAFactLeftUncounted(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		write func(t *testing.T, pool *pgxpool.Pool, engine *Engine)
		// refuses is the run the pass must refuse to open, "" for a control.
		refuses string
	}{
		{name: "a fact that moves the issue into a step and counts no start", refuses: "LEGION-208 default step 3", write: func(t *testing.T, pool *pgxpool.Pool, engine *Engine) {
			factTx(t, pool, engine, moveToTestingUncounted(engine))
		}},
		{name: "a fact that writes the issue's plan rows and counts no start", refuses: "LEGION-208 a step 1", write: func(t *testing.T, pool *pgxpool.Pool, engine *Engine) {
			factTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
				seedsPlanIn(t, tx, "implementer", "tester")
				return nil
			})
		}},
		{name: "control: the test seeds the issue into the step", write: func(t *testing.T, pool *pgxpool.Pool, _ *Engine) {
			seedIssue(t, pool, testingRoot())
		}},
		{name: "control: the test seeds the issue's plan rows", write: func(t *testing.T, pool *pgxpool.Pool, _ *Engine) {
			seedStepsPlan(t, pool, "LEGION-208", 1, "implementer", "tester")
		}},
		{name: "control: the same fact, and then the daemon boots again", write: func(t *testing.T, pool *pgxpool.Pool, engine *Engine) {
			factTx(t, pool, engine, moveToTestingUncounted(engine))
			rebooted(t, pool)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			stepRoot(t, pool, phase.Implementing)
			engine := stepEngine(nil)
			// The scenario's boot, before its first fact, opens the implementer's run.
			bootBefore(t, pool, engine)
			factLeft(t, pool)
			tc.write(t, pool, engine)
			err := bootPass(t.Context(), pool, engine, bootStateOf(t, pool))
			switch {
			case tc.refuses != "" && (err == nil || !strings.Contains(err.Error(), tc.refuses)):
				t.Fatalf("the pass after a fact that left LEGION-208 in a step uncounted answered %v, want it refused naming %s", err, tc.refuses)
			case tc.refuses == "" && err != nil:
				t.Fatalf("the pass answered %v, want it to open the run of an issue the test wrote or the boot found", err)
			}
		})
	}
}

// seedsPlanIn writes LEGION-208's plan at generation 1 in tx, as seedStepsPlan writes it: task a
// of steps words, waiting at no act.
func seedsPlanIn(t *testing.T, tx pgx.Tx, words ...string) {
	t.Helper()
	v := record.PlanVersion{Issue: "LEGION-208", Generation: 1, Version: 2, ArtifactID: "plan-artifact", Source: record.PlanSourceOwner,
		GivenBy: claim.Token(treeRoleToken(t, "LEGION-208", claim.RoleArchitect)), GivenAt: stepClock}
	task := record.PlanTask{Issue: "LEGION-208", Generation: 1, Task: "a", PlanVersion: 2, BlockText: "Build the widget.", State: record.TaskWaiting}
	roles := plan.BuiltinRoles()
	steps := make([]record.PlanStep, 0, len(words))
	for i, role := range words {
		steps = append(steps, record.PlanStep{Issue: "LEGION-208", Generation: 1, Task: "a", Position: i + 1, Role: role, Type: roles[role].Type})
	}
	if err := record.NewStore().PutPlan(t.Context(), tx, v, []record.PlanTask{task}, record.NameAgents(nil, steps, nil)); err != nil {
		t.Fatalf("write the plan of LEGION-208: %v", err)
	}
}

// testingRoot is stepRoot's LEGION-208 standing in testing.
func testingRoot() record.Issue {
	return record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208",
		Project: "LEGION", Title: "root", Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U", LastDispatchSeq: 1}
}

// moveToTestingUncounted is a write of a fact that moves LEGION-208 into testing through the store
// and counts no start of it: the gap the pass's check is for.
func moveToTestingUncounted(engine *Engine) func(ctx context.Context, tx pgx.Tx) error {
	return func(ctx context.Context, tx pgx.Tx) error {
		issue, err := engine.store.Issue(ctx, tx, "LEGION-208")
		if err != nil {
			return err
		}
		issue.Phase, issue.Status = phase.Testing, "testing"
		return engine.store.PutIssue(ctx, tx, *issue)
	}
}
