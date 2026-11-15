package store

import (
	"context"
	"strings"
	"testing"
)

// mergeQueueRemovalVersion is the version of the migration this file tests, so the test can
// migrate through everything before it whatever lands beside it.
const mergeQueueRemovalVersion = 20261114031356

// A merge-queue read and a merge-queue removal are the two rows that tell an issue's merger its
// pull request left the merge queue (record.MergeQueueRead, record.MergeQueueRemoval). The outbox
// refuses a kind its constraint does not name, so neither row could be queued before this
// migration, and the transaction that applied GitHub's `dequeued` would fail with it: the negative
// control is that refusal, read before the migration, on a database that already carries the
// update ask's list (20261113082214), which this version stands above. After it both rows are
// queued, and so is every kind the constraint already named, update_ask among them, since the
// migration rewrites the list whole and a kind left out would stop its own effect. A kind that is
// none is still refused, and so is a kind of the merge the daemon no longer submits.
func TestTheOutboxTakesTheMergeQueueKindsWith20261114031356(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, mergeQueueRemovalVersion-1)
	queue := func(kind string) error {
		_, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, 'LEGION-208', '{}', 0, now(), '')`, kind)
		return err
	}
	added := []string{"merge_queue_read", "merge_queue_removal"}
	for _, kind := range added {
		err := queue(kind)
		if err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
			t.Fatalf("queueing a %s row before the migration = %v, want the kind constraint to refuse it: it is a row the migration exists for", kind, err)
		}
		t.Logf("before the migration a %s row is refused: %v", kind, err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the merge-queue kinds: %v", err)
	}
	for _, kind := range append([]string{
		"dispatch_status", "dispatch_message", "notice", "controller_notice", "supervise", "gate_seed",
		"linger_close", "workspace_remove", "issue_branch", "issue_suspend", "check_wake",
		"check_look", "check_bound", "role_message", "phase_ended", "issue_bundle", "issue_release",
		"gated_steps_publish", "claim_release", "hook_prompt", "design_gates_publish", "update_ask",
	}, added...) {
		if err := queue(kind); err != nil {
			t.Fatalf("queue a %s row after the migration: %v", kind, err)
		}
	}
	t.Logf("after the migration the outbox takes every kind, merge_queue_read and merge_queue_removal among them")
	if err := queue("quiesce"); err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a row of a kind that is none = %v, want the constraint to refuse it", err)
	}
	if err := queue("ready_merge"); err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a ready_merge row = %v, want the constraint to refuse it: the merge lane's kinds went with the merge the daemon no longer submits", err)
	}
}
