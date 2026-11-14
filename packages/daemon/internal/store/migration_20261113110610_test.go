package store

import (
	"context"
	"testing"
	"time"
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
// claimSelect) reads it back with its round, its reasons, its answer and its head as they were,
// and with an opening, which the reset below sets to the migration's moment.
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
		t.Fatalf("the claim after the migration = %+v, want its round, its reasons, its answer and its head as written, and an opening", c)
	}
}

// 20261113110610 also starts every pod round open at the deploy again (dispatch://INFRA-2981
// I1). The earlier release wrote update_tasked_at when it queued its task, and this release reads
// that column as the mark of its Envoy ask, so a tasked claim would never be asked. And a round
// opened longer ago than pod_update_grace_minutes would be replaced at the first sweep, before
// any ask. So every member of an open pod round gets the migration's moment as its opening, and
// loses its ask mark and its notice mark. Every member is reset, because the round's bound runs
// from the earliest opening of its claims.
//
// The negative control is the two claims the reset must not touch: one in no round and one in a
// plugin round, each written with the same old opening and the same two marks, keep all three.
// The reset also keeps what a pod round's member said before the deploy: the tester holds an
// answer and the round it left, and both read back as written, so after the reset this release
// still asks no role that answered and waits for no claim that left the round's pod.
func TestAnOpenPodRoundStartsItsClockAndItsAskAgainAt20261113110610(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, dropUpdateAnswerMarksVersion-1)
	old := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	seed := func(role, round string) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, `insert into claims (token, project, tree, issue, role, generation,
			session, session_file, state, launch_failures, prompt_failures, prompt_retires, uncertain_streak,
			plugin_contract, update_round, update_reasons, update_opened_at, update_tasked_at, update_told_at)
			values ('legion-LEGION-209-' || $1, 'legion', 'LEGION-208', 'LEGION-209', $1, 1,
			'ses_' || $1, '', 'idle', 0, 0, 0, 0, 46, $2, 'image', $3, $3, $3)`, role, round, old); err != nil {
			t.Fatalf("seed the %s in round %q: %v", role, round, err)
		}
	}
	seed("implementer", "pod:a-pod-uid")
	seed("tester", "pod:a-pod-uid")
	seed("reviewer", "")
	seed("planner", "plugin:46")
	if _, err := store.Pool().Exec(ctx, `update claims set update_signalled_at = $1, update_left_round = 'pod:a-pod-uid'
		where token = 'legion-LEGION-209-tester'`, old); err != nil {
		t.Fatalf("seed the tester's answer and the round it left: %v", err)
	}
	var before time.Time
	if err := store.Pool().QueryRow(ctx, `select now()`).Scan(&before); err != nil {
		t.Fatalf("read the database's clock before the migration: %v", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the reset of the open pod rounds: %v", err)
	}

	type marks struct {
		opened, tasked, told *time.Time
	}
	read := func(role string) marks {
		t.Helper()
		var m marks
		if err := store.Pool().QueryRow(ctx, `select update_opened_at, update_tasked_at, update_told_at from claims
			where token = 'legion-LEGION-209-' || $1`, role).Scan(&m.opened, &m.tasked, &m.told); err != nil {
			t.Fatalf("read the %s's round after the migration: %v", role, err)
		}
		return m
	}
	for _, role := range []string{"implementer", "tester"} {
		m := read(role)
		if m.opened == nil || m.opened.Before(before) {
			t.Errorf("the %s in the open pod round opened at %v after the migration, want the migration's moment, at or after %s", role, m.opened, before)
		}
		if m.tasked != nil || m.told != nil {
			t.Errorf("the %s in the open pod round keeps ask mark %v and notice mark %v, want neither", role, m.tasked, m.told)
		}
	}
	var signalled *time.Time
	var left string
	if err := store.Pool().QueryRow(ctx, `select update_signalled_at, update_left_round from claims
		where token = 'legion-LEGION-209-tester'`).Scan(&signalled, &left); err != nil {
		t.Fatalf("read the tester's answer and the round it left after the migration: %v", err)
	}
	if signalled == nil || !signalled.Equal(old) || left != "pod:a-pod-uid" {
		t.Errorf("the tester in the open pod round has answer %v and left round %q after the migration, want both kept, %s and %q", signalled, left, old, "pod:a-pod-uid")
	}
	for _, role := range []string{"reviewer", "planner"} {
		m := read(role)
		for name, at := range map[string]*time.Time{"opening": m.opened, "ask mark": m.tasked, "notice mark": m.told} {
			if at == nil || !at.Equal(old) {
				t.Errorf("the %s in no pod round has %s %v after the migration, want it untouched at %s", role, name, at, old)
			}
		}
	}
}
