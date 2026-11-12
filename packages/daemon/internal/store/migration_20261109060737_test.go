package store

import (
	"context"
	"strings"
	"testing"
)

// dropSlotNoticeVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const dropSlotNoticeVersion = 20261109060737

// 20261109060737 takes out awaiting_slot_notice_at, the mark of a notice the daemon can no longer
// write: an issue keeps its admission slot from the moment the daemon starts it until it is done,
// leaves the workflow or waits only for its merge (record.KeepsSlot), so a root whose wait a
// person answered resumes on the row it already holds and never waits for a slot. The negative
// control is the write of the mark itself, which lands before the migration and has no column to
// land in after it.
//
// What the migration must not take with it is the shape of a wait. It drops
// issues_awaiting_shape to free the column and adds it again in its 0057 form, so the same write
// the check refused before the migration — the awaiting columns left standing on a row whose
// phase is no longer awaiting_person — is refused after it, by the same constraint.
func TestTheNoticeMarkIsGoneAfter20261109060737(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, dropSlotNoticeVersion-1)
	exec := func(statement string) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	exec(`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq,
		awaiting_ask, awaiting_since_seq, awaiting_since, awaiting_answered_at, awaiting_answered_by, awaiting_answered_detail)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'an answered root', 'awaiting_person', 1, 'in_progress', 'A', 4,
		'ask-q', 11, now(), now(), 'ask', 'ask-q')`)

	// The negative control: the mark is a column a write can name while the old migration's
	// shape stands.
	exec(`update issues set awaiting_slot_notice_at = now() where key = 'LEGION-208'`)
	t.Logf("before the migration the mark of the telling is written: awaiting_slot_notice_at stands beside the answer")

	// The wait's own shape, read before and after: the awaiting columns belong to
	// awaiting_person and to no other phase.
	const carried = `update issues set phase = 'admitted' where key = 'LEGION-208'`
	if _, err := store.Pool().Exec(ctx, carried); err == nil || !strings.Contains(err.Error(), "issues_awaiting_shape") {
		t.Fatalf("before the migration, a wait's columns left standing outside awaiting_person = %v, want issues_awaiting_shape to refuse it", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the drop of the notice mark: %v", err)
	}

	var columns int
	if err := store.Pool().QueryRow(ctx, `select count(*) from information_schema.columns
		where table_name = 'issues' and column_name = 'awaiting_slot_notice_at'`).Scan(&columns); err != nil {
		t.Fatalf("read whether issues still holds awaiting_slot_notice_at: %v", err)
	}
	if columns != 0 {
		t.Fatalf("issues holds %d awaiting_slot_notice_at column(s) after the migration, want none", columns)
	}
	if _, err := store.Pool().Exec(ctx, `update issues set awaiting_slot_notice_at = now() where key = 'LEGION-208'`); err == nil ||
		!strings.Contains(err.Error(), "awaiting_slot_notice_at") {
		t.Fatalf("writing the mark after the migration = %v, want the column to be gone", err)
	}

	if _, err := store.Pool().Exec(ctx, carried); err == nil || !strings.Contains(err.Error(), "issues_awaiting_shape") {
		t.Fatalf("after the migration, a wait's columns left standing outside awaiting_person = %v, want issues_awaiting_shape to refuse it still", err)
	}

	// The shape the check does take: the write that clears the wait whole as it leaves the phase.
	exec(`update issues set phase = 'admitted', awaiting_ask = null, awaiting_since_seq = null, awaiting_since = null,
		awaiting_answered_at = null, awaiting_answered_by = null, awaiting_answered_detail = null where key = 'LEGION-208'`)
}
