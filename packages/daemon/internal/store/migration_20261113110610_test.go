package store

import (
	"context"
	"testing"
)

// dropUpdateAnswerMarksVersion is the version of the migration this file tests, so the test can
// migrate through everything before it whatever lands beside it.
const dropUpdateAnswerMarksVersion = 20261113110610

// 20261113110610 takes out the two marks of how a claim answered its update round: whether the
// answer was the end of a turn, and whether the round had asked the claim again for a push check
// the pod could not run. No rule of this release reads either: a turn's end is no answer, no turn
// takes an answer back, and the round ends at its bound. The negative control is the two columns
// themselves, which stand before the migration and take a write there.
//
// What the migration must not take with it is the claim. A claim in an open round, answered, is
// written before the migration, and after it the release's own claim read (Store.Claims,
// claimSelect) reads it back with its round, its answer and its head as they were.
func TestTheClaimsUpdateAnswerMarksAreGoneAfter20261113110610(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, dropUpdateAnswerMarksVersion-1)
	marks := func() int {
		t.Helper()
		var n int
		if err := store.Pool().QueryRow(ctx, `select count(*) from information_schema.columns
			where table_name = 'claims'
			and column_name in ('update_answered_by_turn_end', 'update_reasked_for_push_check')`).Scan(&n); err != nil {
			t.Fatalf("read which of the two marks claims holds: %v", err)
		}
		return n
	}

	if _, err := store.Pool().Exec(ctx, `insert into claims (token, project, tree, issue, role, generation,
		session, session_file, state, launch_failures, prompt_failures, prompt_retires, uncertain_streak,
		plugin_contract, update_round, update_reasons, update_opened_at, update_signalled_at, update_head,
		update_answered_by_turn_end, update_reasked_for_push_check)
		values ('legion-LEGION-209-implementer', 'legion', 'LEGION-208', 'LEGION-209', 'implementer', 1,
		'ses_implementer', '', 'idle', 0, 0, 0, 0, 46, 'pod:a-pod-uid', 'image', now(), now(), 'c0ffee', true, true)`); err != nil {
		t.Fatalf("seed a claim that holds both marks: %v", err)
	}
	if n := marks(); n != 2 {
		t.Fatalf("claims holds %d of the two marks before the migration, want both: this is the shape the migration exists to change", n)
	}
	t.Logf("before the migration the claim is written with both marks set")

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the drop of the update answer marks: %v", err)
	}

	if n := marks(); n != 0 {
		t.Fatalf("claims holds %d of the two marks after the migration, want none", n)
	}
	claims, err := store.Claims(ctx)
	if err != nil {
		t.Fatalf("read the claims with this release's claim read after the migration: %v", err)
	}
	if len(claims) != 1 {
		t.Fatalf("the claim read answers %d claims after the migration, want the one seeded", len(claims))
	}
	c := claims[0]
	if c.Token != "legion-LEGION-209-implementer" || c.UpdateRound != "pod:a-pod-uid" || c.UpdateReasons != "image" ||
		c.UpdateOpenedAt == nil || c.UpdateSignalledAt == nil || c.UpdateHead != "c0ffee" {
		t.Fatalf("the claim after the migration = %+v, want its round, its reasons, its opening, its answer and its head as written", c)
	}
}
