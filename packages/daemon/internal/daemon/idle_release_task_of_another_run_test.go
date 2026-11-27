package daemon

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The first of dispatch://INFRA-2746 R1's two exceptions at the release of a pod (check
// c-idle-keeps-nothing-for-a-claim-that-owes-nothing, holdsUndeliveredEntryTask): a released
// claim behind its entry keeps its backstop only for an undelivered task of the issue's run and
// phase that it still works (supervise.Delivery.StillWorked, which admission's owed-entry repair
// reads too). The second, a task of news alone (holdsUndeliveredNewsTask), is the fifth control
// of TestIdleOfAClaimThatOwesNothingStillGetsABackstopOnItsReleaseForAnUndeliveredTaskOfItsEntry,
// whose rows hold a news task of another run or of another phase as well. A claim behind its
// entry that holds an unconfirmed task of another phase or of no run is not passed over by that
// repair, which sends it its row's entry's task, and that send is what brings its pod back: its
// release arms no backstop. The control is the same claim holding a task of the issue's run
// and phase, which the repair passes over and which keeps its backstop. The two rows of another
// phase or of no run fail if the exception reads the task's confirmation alone
// (p.ConfirmedAt.IsZero()) in place of StillWorked, which no row of
// TestIdleOfAClaimThatOwesNothingStillGetsABackstopOnItsReleaseForAnUndeliveredTaskOfItsEntry
// tells apart.
func TestIdleOfAClaimThatOwesNothingGetsNoBackstopForATaskOfAnotherRunOrPhase(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// generation and at are the run and the phase of the task the worker holds unconfirmed.
		generation uint64
		at         phase.Phase
		backstop   bool
	}{
		{name: "the control: it holds a task of the issue's run and phase", generation: 1, at: phase.Implementing, backstop: true},
		{name: "it holds a task of another phase", generation: 1, at: phase.Retro},
		{name: "it holds a task of no run (generation 0)", generation: 0, at: phase.Implementing},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newPauseRig(t)
			if _, err := r.pool.Exec(r.ctx, `update issues set phase = $2 where key = $1`, pauseIssue, string(phase.Implementing)); err != nil {
				t.Fatalf("move %s into implementing: %v", pauseIssue, err)
			}
			// The worker took entry 1 and holds a task of entry 2 unconfirmed, while its row stands
			// on entry 3: it is behind its entry.
			worker := r.claim(claim.RoleImplementer, supervise.StateSuspended)
			task := undeliveredTask(c.at, 2)
			task.Generation = c.generation
			worker.ServedEntry, worker.GivenEntry, worker.Pending = 1, 2, task
			r.putClaim(worker)
			r.putPhaseRow(claim.RoleImplementer, 3, false)

			runner := &outbox{pool: r.pool, records: r.pauses.records, trees: r.st, project: "legion", backstop: time.Hour,
				now: time.Now, log: quietLogger()}
			if err := runner.armBackstops(r.ctx, record.Issue{Key: pauseIssue, Tree: pauseIssue, Phase: phase.Implementing, Generation: 1}); err != nil {
				t.Fatalf("arm the backstops of %s's release: %v", pauseIssue, err)
			}
			armed := r.count(`select count(*) from pod_wake_backstops where issue = $1 and role = $2 and not fired`,
				pauseIssue, string(claim.RoleImplementer))
			want := 0
			if c.backstop {
				want = 1
			}
			if armed != want {
				t.Errorf("the release of %s armed %d open backstops for its implementer, want %d", pauseIssue, armed, want)
			}
		})
	}
}
