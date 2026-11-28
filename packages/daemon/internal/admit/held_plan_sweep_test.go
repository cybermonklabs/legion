package admit

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// holdMember writes key's record held from the phase it stands in, for reason, as the workflow
// writes a hold: the phase is held, and the hold names the phase it came from.
func holdMember(t *testing.T, pool *pgxpool.Pool, key string, reason record.HoldReason) {
	t.Helper()
	held := issue(t, pool, key)
	held.Phase, held.Hold = phase.Held, &record.Hold{From: held.Phase, Reason: reason}
	putIssue(t, pool, held)
}

// The two holds an owner's plan makes give the held issue no worker from the entry sweep
// (intake.EntrySweep, convergeEntries over resumeSlottedWorkers), as they give it none from any
// other path that brings work back (workflow's TestAStoppedTaskAndAHeldParentGetNoWork): an issue
// whose task its owner stopped, held from testing for task-stopped, where it keeps its slot, and
// a parent held from its retro for its children, where it keeps none. Each stands as the control
// does: its role's phases row names an entry its claim is behind, so only the hold keeps the
// sweep from sending that entry's work. The control, a slotted member in testing with the same
// rows, is sent it. A held issue has no role (phase.RoleFor), so the sweep selects neither
// (phaseWorkerOf).
func TestTheEntrySweepGivesAStoppedTaskAndAParentHeldForItsChildrenNoWorker(t *testing.T) {
	pool := migratedPool(t)
	behind := func(root, key string, stands phase.Phase, unslotted bool) convergeRow {
		return convergeRow{root: root, key: key, stands: stands, unslotted: unslotted,
			claimed: true, state: supervise.StateWorking, given: seededEntry - 1}
	}
	cases, slots := seedConvergeRows(t, pool, []convergeRow{func() convergeRow {
		row := behind("LEGION-960", "LEGION-961", phase.Testing, false)
		row.name, row.started = "the control: a slotted member in testing", true
		row.why = "a live claim behind the entry its phases row names is sent that entry's work"
		return row
	}()})
	stopped := behind("LEGION-962", "LEGION-963", phase.Testing, false)
	seedConvergeRow(t, pool, stopped)
	slots++
	holdMember(t, pool, stopped.key, record.HoldTaskStopped)
	parent := behind("LEGION-964", "LEGION-965", phase.Retro, true)
	seedConvergeRow(t, pool, parent)
	holdMember(t, pool, parent.key, record.HoldForChildren)
	cases = append(cases,
		convergeCase{key: stopped.key, why: "its owner stopped its task: no step of the task runs until a later version of its plan or a todo starts it again"},
		convergeCase{key: parent.key, why: "it is held before its retro for the issues beneath it: the retro starts when the last of them leaves (workflow's releaseHeldParents)"})

	applySweep(t, pool, quiet(t, slots), "held-plan-sweep")

	assertConverged(t, pool, cases)
}
