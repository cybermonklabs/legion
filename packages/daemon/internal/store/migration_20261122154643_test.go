package store

import (
	"context"
	"strings"
	"testing"
)

// basisSeedsVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const basisSeedsVersion = 20261122154643

// A pending-basis seed reads the ancestor's request a root's step waits on (record.PendingBasisSeed)
// and a re-parent seed the new parent of a root that ran (record.ReparentSeed). The outbox refuses
// a kind its constraint does not name, so on main's database neither row could be queued before
// this migration: the negative control is that refusal, read before the migration, on a database
// main migrated (migrateMainsThrough), which already carries the design news's kind list
// (20261119203047), which this version stands above. On this stack's own order the two rows are
// queued before this migration already, and that expectation is the one that moved: the task
// copies' 20261121165047 and the pull requests by task's 20261122122826, numbered below this
// version, name both seeds, since the last migration to write the list owns every value of it,
// main's included, and on a database main migrated those two run late, after this one, as the last
// word on the check. After the migration both rows are queued, and so is every kind the constraint
// already named, the plan record, the conflict's second read, the merge forward, the reconcile of a
// tree's news and the phase-ended message among them, since the migration rewrites the list whole
// and a kind left out would stop its own effect. A kind that is none is still refused, and so is a
// merge-lane kind, which went with the merge the daemon no longer submits.
func TestTheOutboxTakesTheBasisSeedKindsWith20261122154643(t *testing.T) {
	ctx := context.Background()
	seeds := []string{"pending_basis_seed", "reparent_seed"}
	ours := emptyStore(t)
	migrateThrough(t, ours, basisSeedsVersion-1)
	for _, kind := range seeds {
		if err := queueRowOfKind(ours, kind); err != nil {
			t.Fatalf("queueing a %s row on this stack's database before the migration = %v, want it queued: 20261121165047 and 20261122122826, numbered below it, name both seeds, since the last migration to write the list owns every value of it, main's included", kind, err)
		}
	}
	t.Logf("on this stack's database before the migration both seeds are queued, by the lists of 20261121165047 and 20261122122826")
	store := emptyStore(t)
	migrateMainsThrough(t, store, basisSeedsVersion-1)
	for _, kind := range seeds {
		err := queueRowOfKind(store, kind)
		if err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
			t.Fatalf("queueing a %s row on main's database before the migration = %v, want the kind constraint to refuse it: it is a row the migration exists for", kind, err)
		}
		t.Logf("on main's database before the migration the %s row is refused: %v", kind, err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the basis seed kinds: %v", err)
	}
	for _, kind := range []string{
		"dispatch_status", "dispatch_message", "notice", "controller_notice", "supervise", "gate_seed",
		"linger_close", "workspace_remove", "issue_branch", "issue_suspend", "check_wake",
		"check_look", "check_bound", "role_message", "phase_ended", "issue_bundle", "issue_release",
		"gated_steps_publish", "claim_release", "hook_prompt", "design_gates_publish", "update_ask",
		"merge_queue_read", "merge_queue_removal", "role_holders_publish", "step_record", "plan_record",
		"conflict_confirm", "merge_forward", "design_changes", "pending_basis_seed", "reparent_seed",
	} {
		if err := queueRowOfKind(store, kind); err != nil {
			t.Fatalf("queue a %s row after the migration: %v", kind, err)
		}
	}
	t.Logf("after the migration the outbox takes every kind, the two basis seeds among them")
	if err := queueRowOfKind(store, "quiesce"); err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a row of a kind that is none = %v, want the constraint to refuse it", err)
	}
	if err := queueRowOfKind(store, "ready_merge"); err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a ready_merge row = %v, want the constraint to refuse it: the merge lane's kinds went with the merge the daemon no longer submits", err)
	}
}
