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
// it the row is queued, and so is every kind the constraint already named, since the migration
// rewrites the list whole and a kind left out would stop its own effect. A kind that is none is
// still refused.
func TestTheOutboxTakesTheClaimReleaseKindWith20261108000545(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, claimReleaseVersion-1)
	queue := func(kind string) error {
		_, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, 'LEGION-208', '{}', 0, now(), '')`, kind)
		return err
	}
	err := queue("claim_release")
	if err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a claim_release row before the migration = %v, want the kind constraint to refuse it: it is the row the migration exists for", err)
	}
	t.Logf("before the migration the claim-release row is refused: %v", err)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the claim-release kind: %v", err)
	}
	for _, kind := range []string{
		"dispatch_status", "dispatch_message", "notice", "controller_notice", "supervise", "gate_seed",
		"linger_close", "workspace_remove", "merge_queue_publish", "issue_branch", "issue_suspend",
		"auto_merge_disable", "ready_merge", "check_wake", "check_look", "check_bound",
		"phase_ended", "issue_bundle", "issue_release", "gated_steps_publish", "claim_release",
	} {
		if err := queue(kind); err != nil {
			t.Fatalf("queue a %s row after the migration: %v", kind, err)
		}
	}
	t.Logf("after the migration the outbox takes every kind, claim_release among them")
	if err := queue("quiesce"); err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a row of a kind that is none = %v, want the constraint to refuse it", err)
	}
}
