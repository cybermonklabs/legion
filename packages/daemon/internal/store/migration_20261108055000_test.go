package store

import (
	"context"
	"strings"
	"testing"
)

// 20261108055000 adds awaiting_slot_notice_at, the mark that says the daemon wrote a root's
// architect the waiting-for-slot notice for the wait the row holds, and names it in
// issues_awaiting_shape. The mark is the wait's own: it stands only in awaiting_person, and only
// beside an answer, since nothing of a wait still with a person is told to wait for a slot.
func TestTheAwaitingShapeConstraintNamesTheNoticeMark(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
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

	// The shape the mark is written in: an answered wait whose architect the daemon has told.
	exec(`update issues set awaiting_slot_notice_at = now() where key = 'LEGION-208'`)
	var told *string
	if err := store.Pool().QueryRow(ctx, `select awaiting_slot_notice_at::text from issues where key = 'LEGION-208'`).Scan(&told); err != nil ||
		told == nil {
		t.Fatalf("the mark of the told root = %v, %v; want the moment the notice was written", told, err)
	}

	for _, refused := range []struct{ why, statement string }{
		// Leaving the phase clears the wait, so a mark outside awaiting_person is a wait's mark
		// left behind.
		{"the mark in another phase", `update issues set phase = 'admitted', awaiting_ask = null, awaiting_since_seq = null,
			awaiting_since = null, awaiting_answered_at = null, awaiting_answered_by = null, awaiting_answered_detail = null
			where key = 'LEGION-208'`},
		// A root still waiting on a person wants no slot, so no notice of one is written for it.
		{"the mark on an unanswered wait", `update issues set awaiting_answered_at = null, awaiting_answered_by = null,
			awaiting_answered_detail = null where key = 'LEGION-208'`},
	} {
		_, err := store.Pool().Exec(ctx, refused.statement)
		if err == nil || !strings.Contains(err.Error(), "issues_awaiting_shape") {
			t.Fatalf("%s = %v, want issues_awaiting_shape to refuse it", refused.why, err)
		}
	}

	// The mark goes with the wait: the same write that clears the wait and leaves the phase is
	// taken once the mark goes with it.
	exec(`update issues set phase = 'admitted', awaiting_ask = null, awaiting_since_seq = null, awaiting_since = null,
		awaiting_answered_at = null, awaiting_answered_by = null, awaiting_answered_detail = null, awaiting_slot_notice_at = null
		where key = 'LEGION-208'`)
}
