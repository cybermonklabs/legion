package store

import (
	"context"
	"strings"
	"testing"
)

// basisSeedsVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const basisSeedsVersion = 20261114064235

// A pending-basis seed reads the ancestor's request a root's step waits on (record.PendingBasisSeed)
// and a re-parent seed the new parent of a root that ran (record.ReparentSeed). The outbox refuses
// a kind its constraint does not name, so neither row could be queued before this migration: the
// negative control is that refusal, read before the migration. After it both rows are queued, and
// so is every kind the constraint already named, since the migration rewrites the list whole and a
// kind left out would stop its own effect. A kind that is none is still refused, and so is a
// merge-lane kind, which went with the merge the daemon no longer submits.
func TestTheOutboxTakesTheBasisSeedKindsWith20261114064235(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, basisSeedsVersion-1)
	queue := func(kind string) error {
		_, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, 'LEGION-208', '{}', 0, now(), '')`, kind)
		return err
	}
	for _, kind := range []string{"pending_basis_seed", "reparent_seed"} {
		err := queue(kind)
		if err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
			t.Fatalf("queueing a %s row before the migration = %v, want the kind constraint to refuse it: it is a row the migration exists for", kind, err)
		}
		t.Logf("before the migration the %s row is refused: %v", kind, err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the basis seed kinds: %v", err)
	}
	for _, kind := range []string{
		"dispatch_status", "dispatch_message", "notice", "controller_notice", "supervise", "gate_seed",
		"linger_close", "workspace_remove", "issue_branch", "issue_suspend", "check_wake",
		"check_look", "check_bound", "role_message", "phase_ended", "issue_bundle", "issue_release",
		"gated_steps_publish", "claim_release", "hook_prompt", "design_gates_publish", "update_ask",
		"pending_basis_seed", "reparent_seed",
	} {
		if err := queue(kind); err != nil {
			t.Fatalf("queue a %s row after the migration: %v", kind, err)
		}
	}
	t.Logf("after the migration the outbox takes every kind, the two basis seeds among them")
	if err := queue("quiesce"); err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a row of a kind that is none = %v, want the constraint to refuse it", err)
	}
	if err := queue("ready_merge"); err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a ready_merge row = %v, want the constraint to refuse it: the merge lane's kinds went with the merge the daemon no longer submits", err)
	}
}
