package daemon

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/checkstatetest"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The daemon's own engine reads every check at the testing move, those a CI job runs among them:
// engineConfig wires TestingLeavesCI false, since until the end of a task holds the checks a CI
// job runs, a task whose steps after testing hold no review and no acceptance would otherwise end
// with no gate on them; the change that builds that hold at the end of a task turns the setting
// on, and changes this test then. ACME-1 is in testing on the daemon's engine; k1 and k2 read yes from the
// tester at its head, and k3, a check a CI job runs, reads no for its job's failure, or no for no
// result while its job goes on. The tester's completion sends the issue back for the failure and
// waits for the job that goes on. An engine wired to leave those checks out would send ACME-1 to
// review in both cases, and this test fails then.
func TestTheDaemonWiresTheTestingMoveToReadEveryCheck(t *testing.T) {
	for _, tc := range []struct {
		name string
		k3   intake.DispatchCheckState
		want phase.Phase
	}{
		{"k3's job failed", intake.DispatchCheckState{Check: "k3", State: "no", Reason: "fail", CICase: "failure", Roles: []string{"ci"}}, phase.Implementing},
		{"k3's job goes on", intake.DispatchCheckState{Check: "k3", State: "no", Reason: "no_result", CICase: "not_completed"}, phase.Testing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, w := checkStatesRig(t, &stateLogs{})
			engineCfg := engineConfig(config.Config{Project: "ACME"}, "", nil)
			// The rig's own repositories and clock, and no design gate on the root, beside the
			// daemon's wiring of the testing move.
			engineCfg.Repositories, engineCfg.Clock, engineCfg.Steps = testRepositories, catchUpClock, nil
			engine := workflow.New(record.NewStore(), engineCfg, quietLogger())
			w.engine, w.handlers = engine, []intake.Handler{engine}
			seedCatchUp(t, pool, func(ctx context.Context, tx pgx.Tx, records *record.Postgres) error {
				if err := records.PutIssue(ctx, tx, acmeIssue("ACME-1", phase.Testing, "in_progress")); err != nil {
					return err
				}
				return openAt(ctx, tx, records, "ACME-1", 70, statesHeadOne)
			})
			checkstatetest.Apply(t, pool, "ACME-1", "acme/widgets", statesHeadOne, 1, []intake.DispatchCheckState{
				{Check: "k1", State: "yes", Roles: []string{"tester"}}, {Check: "k2", State: "yes", Roles: []string{"tester"}}, tc.k3,
			}, w.handlers...)
			testerCompletes(t, w, "ACME-1", statesHeadOne, 3)
			if got := readIssue(t, pool, "ACME-1").Phase; got != tc.want {
				t.Fatalf("ACME-1 stands in %s after its tester's completion, want %s: the daemon's testing move reads k3, a check a CI job runs", got, tc.want)
			}
		})
	}
}
