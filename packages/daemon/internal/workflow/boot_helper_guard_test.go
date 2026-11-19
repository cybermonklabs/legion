package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The boot pass these tests run before a fact (bootBefore) is the daemon's boot only the first
// time. After it, a pass that opens a run of an issue a fact left in a step is a gap the pass
// would hide: the fact moved the issue and counted no start, so production, whose boot ran before
// that fact, would record nothing of the run. The fault here is such a fact, a write of the issue
// into testing that counts no start; its pass fails. The controls are the same move written by
// the test itself between two facts, as a seed of an issue the earlier release moved is, and the
// same fact after the scenario boots again (rebooted): neither fails.
func TestTheBootPassBeforeAFactRefusesARunAFactLeftUncounted(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(t *testing.T, pool *pgxpool.Pool, engine *Engine)
		fails bool
	}{
		{name: "a fact that moves the issue into a step and counts no start", fails: true, write: func(t *testing.T, pool *pgxpool.Pool, engine *Engine) {
			factTx(t, pool, engine, moveToTestingUncounted(engine))
		}},
		{name: "control: the test seeds the issue into the step", write: func(t *testing.T, pool *pgxpool.Pool, _ *Engine) {
			seedIssue(t, pool, testingRoot())
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
			case tc.fails && (err == nil || !strings.Contains(err.Error(), "LEGION-208 default step 3")):
				t.Fatalf("the pass after a fact that left LEGION-208 in testing uncounted answered %v, want it refused naming default step 3", err)
			case !tc.fails && err != nil:
				t.Fatalf("the pass answered %v, want it to open the run of an issue the test wrote or the boot found", err)
			}
		})
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
