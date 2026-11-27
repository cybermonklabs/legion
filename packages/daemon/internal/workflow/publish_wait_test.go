package workflow

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/intake"
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
// the withdrawal is taken out of the stored wait (ClearWaitWithdrawals): a row on its backoff is
// made due, so the withdrawal's version reaches Dispatch rather than waiting for the backoff.
func TestAWithdrawalsVersionIsDueAtOnce(t *testing.T) {
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
	backoff := rig.now.Add(2 * time.Minute)
	if _, err := rig.pool.Exec(t.Context(), "update outbox set next_at = $2 where id = $1", rows[0].id, backoff); err != nil {
		t.Fatalf("stand the publish row on its backoff: %v", err)
	}

	if result := rig.fact(intake.DefectWithdrawal{Issue: waitRoot, Defect: defect, Reason: "same as the merged fix",
		By: rig.rootArchitect()}); result.Refusal != nil {
		t.Fatalf("the withdrawal = %+v, want it taken", result.Refusal)
	}

	after := issueOutboxOf(t, rig.pool, waitRoot, string(record.OutboxKindCloseWaitPublish))
	if len(after) != 1 || after[0].nextAt.After(rig.now) {
		t.Fatalf("the publish rows after the withdrawal = %+v, want one due at once", after)
	}
}
