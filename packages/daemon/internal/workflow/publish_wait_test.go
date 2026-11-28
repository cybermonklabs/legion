package workflow

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// D11 says close-wait.md carries a version at each change of what the close waits for. The row
// that writes it renders the record when it runs, so one waiting row stands for every change
// before it runs — but only where that row is due. A row waiting on a backoff is due later, and a
// change that commits under it reached Dispatch only when that backoff came up: a reader saw a
// wait the record no longer held, for as long as the backoff lasted. So every change of the wait
// makes the row that writes it due at once (publishWait, record.Store.ExpediteOutbox).
func TestEachChangeOfTheWaitIsDueAtOnce(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, "")
	rig.seedRoot(phase.Merging, "retro")
	rig.mergeRoot()

	rows := issueOutboxOf(t, rig.pool, waitRoot, string(record.OutboxKindCloseWaitPublish))
	if len(rows) != 1 {
		t.Fatalf("the close_wait_publish rows after the merge = %+v, want the entry's one", rows)
	}
	// The row waits on the backoff a failed attempt leaves, as the executor's own failure writes
	// it (daemon's outbox backoff): it is due two minutes out.
	backoff := rig.now.Add(2 * time.Minute)
	if _, err := rig.pool.Exec(t.Context(), "update outbox set next_at = $2 where id = $1", rows[0].id, backoff); err != nil {
		t.Fatalf("stand the publish row on its backoff: %v", err)
	}

	// A change of what the close waits for: the first read of the checks, which stores a wait
	// naming the check and the production assessment in place of the unread one the entry stored.
	rig.read(waitRoot, answerOf(waitRoot, nil, failing("c-after", waitRootMerge)))

	after := issueOutboxOf(t, rig.pool, waitRoot, string(record.OutboxKindCloseWaitPublish))
	if len(after) != 1 {
		t.Fatalf("the close_wait_publish rows after the read = %+v, want one: a waiting row stands for every change", after)
	}
	if after[0].nextAt.After(rig.now) {
		t.Fatalf("the publish row is due at %s, after the change at %s; want it due at once, or the version Dispatch holds is one the record no longer does",
			after[0].nextAt, rig.now)
	}
}

// The same rule where the change is a withdrawal, whose version close-wait.md must show before
// the withdrawal is taken out of the stored wait (ClearWaitWithdrawals). The close still waits for
// a check with no result, so the withdrawal writes a new wait that holds it as withdrawn, and the
// row on its backoff is made due: the withdrawal's version reaches Dispatch at once, not when the
// backoff runs out.
func TestAWithdrawalsVersionIsDueAtOnce(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, "")
	rig.seedRoot(phase.Merging, "retro")
	owedMerger(t, rig)
	rig.mergeRoot()
	defect := rig.seedDefect(waitRoot, "The page is blank after a save.")
	rig.read(waitRoot, answerOf(waitRoot, nil, noResult("c-after", waitRootMerge)))
	rows := issueOutboxOf(t, rig.pool, waitRoot, string(record.OutboxKindCloseWaitPublish))
	if len(rows) != 1 {
		t.Fatalf("the close_wait_publish rows = %+v, want one", rows)
	}
	rig.dueAt(rows[0].id, rig.now.Add(2*time.Minute))

	rig.withdraw(waitRoot, defect, "same as the merged fix")

	rig.stands(waitRoot, phase.Verifying, "testing")
	if withdrawn := rig.stored(waitRoot).Withdrawn; len(withdrawn) != 1 || withdrawn[0].ID != defect {
		t.Fatalf("the stored wait's withdrawals after the withdrawal = %+v, want defect %d's alone", withdrawn, defect)
	}
	after := issueOutboxOf(t, rig.pool, waitRoot, string(record.OutboxKindCloseWaitPublish))
	if len(after) != 1 {
		t.Fatalf("the close_wait_publish rows after the withdrawal = %+v, want one: a waiting row stands for every change", after)
	}
	if after[0].nextAt.After(rig.now) {
		t.Fatalf("the publish row is due at %s, after the withdrawal at %s; want it due at once, or Dispatch shows the withdrawal only when the backoff runs out",
			after[0].nextAt, rig.now)
	}
}

// A close on the checks writes its version through the row that waits too. The status write of
// done waits behind every older publish row of its issue (record's outboxInOrder), so a second row
// queued beside one on its backoff let the closed version out at once while the older row's
// backoff held the done write: Dispatch showed the issue open after its close until the backoff
// ran out. So the close makes the waiting row due and queues no second (publishWait), and that
// row stays older than the done write, so the closed version is written while the issue is open.
func TestACloseMakesTheWaitingRowDueAndQueuesNoSecond(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, "")
	rig.seedRoot(phase.Merging, "retro")
	owedMerger(t, rig)
	rig.mergeRoot()
	defect := rig.seedDefect(waitRoot, "The page is blank after a save.")
	rig.read(waitRoot, answerOf(waitRoot, nil, passing("c-after", waitRootMerge)))
	rows := issueOutboxOf(t, rig.pool, waitRoot, string(record.OutboxKindCloseWaitPublish))
	if len(rows) != 1 {
		t.Fatalf("the close_wait_publish rows = %+v, want one", rows)
	}
	rig.dueAt(rows[0].id, rig.now.Add(2*time.Minute))

	// The withdrawal of the one defect leaves the close waiting for nothing: the issue closes.
	rig.withdraw(waitRoot, defect, "same as the merged fix")

	rig.stands(waitRoot, phase.Done, "done")
	after := issueOutboxOf(t, rig.pool, waitRoot, string(record.OutboxKindCloseWaitPublish))
	if len(after) != 1 {
		t.Fatalf("the close_wait_publish rows after the close = %+v, want the one that waited: a second row leaves the older one's backoff holding the done write", after)
	}
	if after[0].nextAt.After(rig.now) {
		t.Fatalf("the publish row is due at %s, after the close at %s; want it due at once, or the done write waits behind it for the backoff",
			after[0].nextAt, rig.now)
	}
	if done := rig.firstID(waitRoot, "dispatch_status", "status", "done"); done == 0 || after[0].id > done {
		t.Fatalf("the done write is row %d and the publish row %d; want the publish row older, so the closed version is written while the issue is open", done, after[0].id)
	}
}
