package store

import (
	"context"
	"testing"
)

// A queued waiting-for-slot notice is a row this release cannot run. The kind left
// record.validNoticeKinds with the notice, and the payload field `waiting_for_slot` left
// record.Notice, which record.DecodeOutboxPayload decodes with DisallowUnknownFields. So a row
// the release before this one wrote fails to decode for ever on its backoff, and while that
// backoff holds its next_at ahead of now it also fails every read of the project's state that
// passes it: record.Store.WaitingNotices lists it and daemon.pacedCatchUps returns its decode
// error, so GET /legion/v1/state answers an error for the whole project — the state the
// controller, legion state and this issue's own after_merge check read — until somebody deletes
// the row by hand.
//
// Deleting the rows a migration leaves unrunnable is what this repository's migrations do:
// 0016_ready_packet.up.sql:19,25, 0035_resident_roles.up.sql:9, 0044_ready_merge_dedup.up.sql
// and 20261105231916_production_check_role_and_wake.up.sql:42, whose own comment states the
// rule. 20261108055000_awaiting_slot_notice.up.sql:54 names this very row as a step the release
// that can no longer decode it owes, and this migration is that release's: the boot applies it
// (store.Migrate in daemon.Run, before the API serves and before any outbox lane claims a row),
// so the delete belongs in the migration and costs an operator nothing.
//
// The negative control is the notice queued beside it: a kind this release still decodes stands
// untouched after the migration, so the delete takes the unrunnable row and nothing else.
func TestTheQueuedWaitingForSlotNoticeIsGoneAfter20261109060737(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, dropSlotNoticeVersion-1)
	queue := func(issue, payload string) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ('notice', $1, $2::jsonb, 0, now(), '')`, issue, payload); err != nil {
			t.Fatalf("queue the notice of %s as the previous release wrote it: %v", issue, err)
		}
	}
	// The row the previous release wrote: admit's tellWaitingForSlot, through
	// record.NewOutboxRow, with the wait it names (record.WaitingForSlot).
	queue("LEGION-208", `{"kind":"waiting-for-slot","reason":"a person answered the question of LEGION-208; it stands 2nd of 5 in the admission line","waiting_for_slot":{"answered_at":"2026-11-08T12:00:00Z"}}`)
	// The negative control: a notice of a kind this release still decodes.
	queue("LEGION-209", `{"kind":"children-first","reason":"LEGION-209 waits for the children open under it"}`)

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the drop of the notice mark: %v", err)
	}

	var stranded int
	if err := store.Pool().QueryRow(ctx,
		`select count(*) from outbox where kind = 'notice' and payload->>'kind' = 'waiting-for-slot'`).Scan(&stranded); err != nil {
		t.Fatalf("count the queued waiting-for-slot notices after the migration: %v", err)
	}
	if stranded != 0 {
		t.Errorf("%d waiting-for-slot notice(s) survive the migration, want none: this release decodes neither the kind nor the payload's waiting_for_slot field, so each one fails for ever and fails every read of the project's state with it", stranded)
	}

	var kept int
	if err := store.Pool().QueryRow(ctx,
		`select count(*) from outbox where kind = 'notice' and payload->>'kind' = 'children-first' and issue = 'LEGION-209'`).Scan(&kept); err != nil {
		t.Fatalf("count the children-first notice after the migration: %v", err)
	}
	if kept != 1 {
		t.Errorf("the children-first notice of LEGION-209 stands %d time(s) after the migration, want 1: the delete takes the rows no daemon can run and no others", kept)
	}
}
