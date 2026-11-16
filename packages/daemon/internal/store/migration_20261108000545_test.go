package store

import (
	"context"
	"strings"
	"testing"
)

// claimReleaseVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const claimReleaseVersion = 20261108000545

// A claim-release row releases the Dispatch claim of an issue the daemon took out of the workflow
// without closing it (record.ClaimRelease). The outbox refuses a kind its constraint does not
// name, so the row could not be queued before this migration, and the transaction that parks the
// issue would fail with it: the negative control is that refusal, read before the migration. After
// the whole set the row is queued, and so is every kind this release writes, since each migration
// of the constraint rewrites the list whole and a kind left out would stop its own effect. The
// three merge-lane kinds this release retired are refused instead, because 20261108152000, which
// merged after this one, is the last word of both lists and names the kinds the merged daemon
// writes. A kind that is none is still refused.
func TestTheOutboxTakesTheClaimReleaseKindWith20261108000545(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, claimReleaseVersion-1)
	queue := func(kind string) error {
		_, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, 'LEGION-208', '{}', 0, now(), '')`, kind)
		return err
	}
	// This branch's own 20261107095842 runs below this version and admits the kind too, since a
	// database main migrated meets that list after main's own (20261108152000's comment), so on
	// this branch's path the row is taken before this migration rather than refused. What the
	// upgrade from main does with such a row is
	// TestADatabaseMainMigratedUpgradesWithEveryRowItHolds.
	if err := queue("claim_release"); err != nil {
		t.Fatalf("queueing a claim_release row on this branch's path = %v, want it taken: 20261107095842 admits the kind below this version", err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the claim-release kind: %v", err)
	}
	for _, kind := range []string{
		"dispatch_status", "dispatch_message", "notice", "controller_notice", "supervise", "gate_seed",
		"linger_close", "workspace_remove", "issue_branch", "issue_suspend", "check_wake",
		"check_look", "check_bound", "role_message", "phase_ended", "issue_bundle", "issue_release",
		"gated_steps_publish", "claim_release",
	} {
		if err := queue(kind); err != nil {
			t.Fatalf("queue a %s row after the migration: %v", kind, err)
		}
	}
	t.Logf("after the whole set the outbox takes every kind this release writes, claim_release among them")
	for _, retired := range []string{"merge_queue_publish", "auto_merge_disable", "ready_merge"} {
		if err := queue(retired); err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
			t.Fatalf("queueing a %s row = %v, want the constraint to refuse it: this release writes no merge-lane row", retired, err)
		}
	}
	if err := queue("quiesce"); err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a row of a kind that is none = %v, want the constraint to refuse it", err)
	}
}
