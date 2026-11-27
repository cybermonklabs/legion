package store

import (
	"context"
	"strings"
	"testing"
)

// queueTimelineReadVersion is the version of the migration this file tests, so the test can
// migrate through everything before it whatever lands beside it.
const queueTimelineReadVersion = 20261126190955

// A queue timeline read is the row that reads one pull request's merge-queue timeline for the
// queue signal (record.QueueTimelineRead). The outbox refuses a kind its constraint does not name,
// so the row could not be queued before this migration: the negative control is that refusal,
// read before the migration, on a database that already carries the merge holds' list
// (20261126110259), which this version stands above. After it the row is queued, and so is every
// kind the constraint already named, thirty-four in all, merge_holds_changed, pending_basis_seed
// and reparent_seed among them, since the migration rewrites the list whole and a kind left out
// would stop its own effect. A kind that is none is still refused, and so is a kind of the merge
// the daemon no longer submits.
func TestTheOutboxTakesTheQueueTimelineReadWith20261126190955(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, queueTimelineReadVersion-1)
	queue := func(kind string) error {
		_, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, 'LEGION-208', '{}', 0, now(), '')`, kind)
		return err
	}
	err := queue("queue_timeline_read")
	if err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a queue_timeline_read row before the migration = %v, want the kind constraint to refuse it: it is the row the migration exists for", err)
	}
	t.Logf("before the migration a queue_timeline_read row is refused: %v", err)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the queue timeline read kind: %v", err)
	}
	kinds := []string{
		"dispatch_status", "dispatch_message", "notice", "controller_notice", "supervise", "gate_seed",
		"linger_close", "workspace_remove", "issue_branch", "issue_suspend", "check_wake",
		"check_look", "check_bound", "role_message", "phase_ended", "issue_bundle", "issue_release",
		"gated_steps_publish", "claim_release", "hook_prompt", "design_gates_publish", "update_ask",
		"merge_queue_read", "merge_queue_removal", "role_holders_publish", "step_record", "plan_record",
		"conflict_confirm", "merge_forward", "design_changes", "pending_basis_seed", "reparent_seed",
		"merge_holds_changed", "queue_timeline_read",
	}
	if len(kinds) != 34 {
		t.Fatalf("the list names %d kinds, want the 34 the migration writes", len(kinds))
	}
	for _, kind := range kinds {
		if err := queue(kind); err != nil {
			t.Fatalf("queue a %s row after the migration: %v", kind, err)
		}
	}
	t.Logf("after the migration the outbox takes all %d kinds, queue_timeline_read among them", len(kinds))
	if err := queue("quiesce"); err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a row of a kind that is none = %v, want the constraint to refuse it", err)
	}
	if err := queue("ready_merge"); err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a ready_merge row = %v, want the constraint to refuse it: the merge lane's kinds went with the merge the daemon no longer submits", err)
	}
}
