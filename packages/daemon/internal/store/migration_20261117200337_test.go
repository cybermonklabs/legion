package store

import (
	"context"
	"testing"
	"time"
)

// updateAskMarksAtDeliveryVersion is the version of the migration this file tests, so the test
// can migrate through everything before it whatever lands beside it.
const updateAskMarksAtDeliveryVersion = 20261117200337

// 20261117200337 clears the ask mark the earlier release wrote when it queued an update ask, on
// every claim whose ask of its round still waits in the outbox. This release marks a claim when
// the ask's publish reached a session, and its row publishes nothing to a claim already marked, so
// an earlier release's mark over a queued ask would drop that ask unsent.
//
// The implementer is that case: marked, in a pod round, with an update_ask row of its issue, its
// role and that round queued. Its mark goes, and its round, its opening and its row stay. The
// controls are every mark the migration must keep: the tester's, whose ask has no queued row (its
// row finished, and nothing says whether it was delivered); the reviewer's, whose queued row asks
// another round than the one the claim records; and the planner's, whose queued row is for another
// issue. The negative control is the implementer's mark itself, read before the migration.
func TestAQueuedUpdateAskOfTheEarlierReleaseLosesItsMarkWith20261117200337(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, updateAskMarksAtDeliveryVersion-1)
	marked := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	seed := func(role, issue, round string) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, `insert into claims (token, project, tree, issue, role, generation,
			session, session_file, state, launch_failures, prompt_failures, prompt_retires, uncertain_streak,
			plugin_contract, update_round, update_reasons, update_opened_at, update_tasked_at)
			values ('legion-' || $2 || '-' || $1, 'legion', 'LEGION-208', $2, $1, 1,
			'ses_' || $1, '', 'ready', 0, 0, 0, 0, 46, $3, 'image', $4, $4)`, role, issue, round, marked); err != nil {
			t.Fatalf("seed the %s of %s in round %q: %v", role, issue, round, err)
		}
	}
	queue := func(role, issue, round string) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ('update_ask', $2, jsonb_build_object('role', $1::text, 'round', $3::text, 'pod', 'a-pod-uid', 'text', 'Push your work.'), 0, now(), '')`,
			role, issue, round); err != nil {
			t.Fatalf("queue the %s's ask of %s for %s: %v", role, round, issue, err)
		}
	}
	seed("implementer", "LEGION-209", "pod:a-pod-uid")
	queue("implementer", "LEGION-209", "pod:a-pod-uid")
	seed("tester", "LEGION-209", "pod:a-pod-uid")
	seed("reviewer", "LEGION-209", "pod:a-pod-uid")
	queue("reviewer", "LEGION-209", "pod:an-older-pod-uid")
	seed("planner", "LEGION-209", "pod:a-pod-uid")
	queue("planner", "LEGION-210", "pod:a-pod-uid")

	tasked := func(role string) *time.Time {
		t.Helper()
		var at *time.Time
		if err := store.Pool().QueryRow(ctx, `select update_tasked_at from claims where token = 'legion-LEGION-209-' || $1`,
			role).Scan(&at); err != nil {
			t.Fatalf("read the %s's ask mark: %v", role, err)
		}
		return at
	}
	if at := tasked("implementer"); at == nil {
		t.Fatalf("the implementer holds no ask mark before the migration, want the earlier release's mark: it is the state the migration exists for")
	}
	t.Logf("before the migration the implementer is marked asked with its ask still queued")

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the cutover of the update ask's mark: %v", err)
	}

	if at := tasked("implementer"); at != nil {
		t.Fatalf("the implementer keeps ask mark %v after the migration, want it cleared: its ask is still queued", at)
	}
	for _, role := range []string{"tester", "reviewer", "planner"} {
		if at := tasked(role); at == nil || !at.Equal(marked) {
			t.Errorf("the %s holds ask mark %v after the migration, want %s kept: no ask of its own round waits for it", role, at, marked)
		}
	}
	claims, err := store.Claims(ctx)
	if err != nil {
		t.Fatalf("read the claims with this release's claim read after the migration: %v", err)
	}
	for _, c := range claims {
		if c.UpdateRound != "pod:a-pod-uid" || c.UpdateOpenedAt == nil || !c.UpdateOpenedAt.Equal(marked) {
			t.Errorf("the %s after the migration is in round %q opened at %v, want its round and its opening as written", c.Role, c.UpdateRound, c.UpdateOpenedAt)
		}
	}
	var queued int
	if err := store.Pool().QueryRow(ctx, `select count(*) from outbox where kind = 'update_ask'`).Scan(&queued); err != nil {
		t.Fatalf("count the queued asks after the migration: %v", err)
	}
	if queued != 3 {
		t.Errorf("the outbox holds %d asks after the migration, want the three queued: the migration touches no row of it", queued)
	}
}
