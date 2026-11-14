package store

import (
	"context"
	"strings"
	"testing"
)

// waitDueVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const waitDueVersion = 20261114080300

// A wait_due row brings a phase worker's wait to its time (record.WaitDue), queued by the
// declaration's own transaction. The outbox refuses a kind its constraint does not name, so the
// row could not be queued before this migration, and every declaration would fail with it: the
// negative control is that refusal, read before the migration. After it the row is queued, and so
// is every kind the constraint already named, since the migration rewrites the list whole and a
// kind left out would stop its own effect. A kind that is none is still refused, and so is a
// merge-lane kind, which went with the merge the daemon no longer submits.
func TestTheOutboxTakesTheWaitDueKindWith20261114080300(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, waitDueVersion-1)
	queue := func(kind string) error {
		_, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, 'LEGION-208', '{}', 0, now(), '')`, kind)
		return err
	}
	err := queue("wait_due")
	if err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a wait_due row before the migration = %v, want the kind constraint to refuse it: it is the row the migration exists for", err)
	}
	t.Logf("before the migration the wait's time is refused: %v", err)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the wait_due kind: %v", err)
	}
	for _, kind := range []string{
		"dispatch_status", "dispatch_message", "notice", "controller_notice", "supervise", "gate_seed",
		"linger_close", "workspace_remove", "issue_branch", "issue_suspend", "check_wake",
		"check_look", "check_bound", "role_message", "phase_ended", "issue_bundle", "issue_release",
		"gated_steps_publish", "claim_release", "hook_prompt", "design_gates_publish", "update_ask",
		"wait_due",
	} {
		if err := queue(kind); err != nil {
			t.Fatalf("queue a %s row after the migration: %v", kind, err)
		}
	}
	t.Logf("after the migration the outbox takes every kind, wait_due among them")
	if err := queue("wait_ends"); err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a row of a kind that is none = %v, want the constraint to refuse it", err)
	}
	if err := queue("ready_merge"); err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a ready_merge row = %v, want the constraint to refuse it: the merge lane's kinds went with the merge the daemon no longer submits", err)
	}
}
