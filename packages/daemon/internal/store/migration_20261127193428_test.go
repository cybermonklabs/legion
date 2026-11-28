package store

import (
	"context"
	"reflect"
	"testing"
)

// claimLeftVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const claimLeftVersion = 20261127193428

// claimLeftRelease names the release whose way back the migration's header carries, as a failure
// to read that block names it.
const claimLeftRelease = "the release that releases a left session's claim"

// 20261127193428 adds issues.claim_left, the entry of the newest claim release that left the
// issue's Dispatch claim (record.Issue.ClaimLeft), null on every issue the table holds: no release
// before this one recorded one. The negative control is the store before the migration, whose
// table holds no such column. Its way back, the last paragraph of the migration's header
// (migrations.WayBack), deletes each queued claim_release row that names a session, which this
// release queues (store's putClaim) and the release before refuses (DisallowUnknownFields), and
// keeps a move's row, which names none, and a row of another kind that carries the same key. It
// drops the column and deletes the migration's version row, so the roll-forward's Migrate applies
// the file again and adds the column again, null on every issue.
func TestTheClaimLeftComesNullAndItsWayBackDeletesTheRowsTheReleaseBeforeRefuses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, claimLeftVersion-1)
	exec := func(statement string, args ...any) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, statement, args...); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	for _, key := range []string{"LEGION-208", "LEGION-209"} {
		exec(`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
			values ($1, $1, 'LEGION', 'backlog', 'admitted', 1, 'backlog', 'A', 1)`, key)
	}
	const column = `select count(*)::text from information_schema.columns where table_name = 'issues' and column_name = 'claim_left'`
	if got := rowsAsText(t, store, column); !reflect.DeepEqual(got, []string{"0"}) {
		t.Fatalf("the column before the migration = %v, want none", got)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the claim left: %v", err)
	}
	const entries = `select key || ' ' || coalesce(claim_left::text, 'null') from issues order by key`
	none := []string{"LEGION-208 null", "LEGION-209 null"}
	if got := rowsAsText(t, store, entries); !reflect.DeepEqual(got, none) {
		t.Fatalf("the entries after the migration = %v, want %v: every issue it found with none", got, none)
	}
	exec(`update issues set claim_left = '{"holder": "user:alice", "reason": "person", "at": "2026-11-27T09:00:00Z"}' where key = 'LEGION-208'`)

	queue := func(kind, issue, payload string) {
		t.Helper()
		exec(`insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, $2, $3::jsonb, 0, now(), '')`, kind, issue, payload)
	}
	queue("claim_release", "LEGION-208", `{}`)
	queue("claim_release", "LEGION-208", `{"session": "ses-1"}`)
	queue("claim_release", "LEGION-209", `{"session": "ses-2"}`)
	queue("supervise", "LEGION-209", `{"op": "start", "session": "ses-3"}`)

	exec(wayBackOf(t, claimLeftVersion, claimLeftRelease))
	const version = `select count(*)::text from schema_version where version = 20261127193428`
	if got := append(rowsAsText(t, store, column), rowsAsText(t, store, version)...); !reflect.DeepEqual(got, []string{"0", "0"}) {
		t.Fatalf("after the way back the column and the version row = %v, want neither", got)
	}
	const queued = `select kind || ' ' || issue || ' ' || payload::text from outbox order by id`
	if got, want := rowsAsText(t, store, queued), []string{
		`claim_release LEGION-208 {}`,
		`supervise LEGION-209 {"op": "start", "session": "ses-3"}`,
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the queued rows after the way back = %q, want %q: each claim release that names a session gone, the move's row and every other kind's row kept", got, want)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("roll forward after the way back: %v", err)
	}
	if got := rowsAsText(t, store, entries); !reflect.DeepEqual(got, none) {
		t.Fatalf("the entries after the roll-forward = %v, want %v: the column again, null on every issue", got, none)
	}
}
