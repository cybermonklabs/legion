package daemon

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A wake brings a released pod back and tells the role why it came back; it hands the architect
// no phase entry's work, so its start carries no entry (workflow's wakeRow) and the claim's two
// entry numbers stand where the entry's own start left them, the turn that takes the wake's task
// included (supervise.Claim.GivenEntry and ServedEntry). The architect's phases row stands with
// them: the record still names the entry it was handed and the work of it.
//
// The resume of a cancelled idle release is the other start of this shape (releaseResume), which
// carries no task at all and so delivers nothing to move either number.
func TestAWakeStartConfirmedByTheArchitectMovesNeitherEntryNumber(t *testing.T) {
	r := newMainPathRig(t, true)
	r.put(r.waitingRoot())
	r.admitPass()
	r.runStarts()
	r.bringUp()
	// The architect takes the work of the entry its admission raised, which is the premise: both
	// numbers stand on that entry.
	r.takeTheTask()
	entry := rolePhaseRow(t, r.pool, mainPathRoot, claim.RoleArchitect)
	if entry.Entry != 1 || entry.EntryTask == "" {
		t.Fatalf("the architect's row after its admission = entry %d carrying %q, want entry 1 carrying that entry's work",
			entry.Entry, entry.EntryTask)
	}
	if held := r.machine().Claim(); held.GivenEntry != 1 || held.ServedEntry != 1 {
		t.Fatalf("the architect's claim after it took entry 1's task was given entry %d and served entry %d, want both 1",
			held.GivenEntry, held.ServedEntry)
	}

	wake := workflow.WakeTask(pods.WakeBackstop, "nothing else asked for it")
	r.queueArchitectStart(record.SuperviseRequest{Op: "start", Tree: mainPathRoot, Role: claim.RoleArchitect,
		Generation: r.issue().Generation, Task: wake})
	r.runStarts()

	if pending := r.machine().Claim().Pending; pending == nil || pending.Task != wake || pending.Entry != 0 {
		t.Fatalf("the architect's pending task after the wake = %#v, want the wake's own task of no entry", pending)
	}
	r.takeTheTask()

	if held := r.machine().Claim(); held.GivenEntry != 1 || held.ServedEntry != 1 {
		t.Fatalf("the architect's claim after the wake's task was given entry %d and served entry %d, want both 1: a delivery of no entry moves neither",
			held.GivenEntry, held.ServedEntry)
	}
	after := rolePhaseRow(t, r.pool, mainPathRoot, claim.RoleArchitect)
	if after.Entry != entry.Entry || after.EntryTask != entry.EntryTask || !after.EntryRaisedAt.Equal(entry.EntryRaisedAt) {
		t.Fatalf("the architect's row after the wake = entry %d, task %q, raised at %s; want the entry %d, task %q and moment %s it stood at",
			after.Entry, after.EntryTask, after.EntryRaisedAt, entry.Entry, entry.EntryTask, entry.EntryRaisedAt)
	}
}

// queueArchitectStart writes one architect start row of the rig's root, which runStarts then runs
// as the supervise lane does.
func (r *mainPathRig) queueArchitectStart(request record.SuperviseRequest) {
	r.t.Helper()
	row, err := record.NewOutboxRow(r.key, request, time.Now().UTC())
	if err != nil {
		r.t.Fatalf("compose the architect start: %v", err)
	}
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		return r.records.Enqueue(r.ctx, tx, row)
	}); err != nil {
		r.t.Fatalf("queue the architect start: %v", err)
	}
}
