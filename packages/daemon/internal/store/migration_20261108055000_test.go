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

	// Each refused write states a mark the row did not hold, which is what the check is left to
	// refuse. A mark a write leaves unchanged while it takes the row out of its answered wait is
	// cleared by the trigger before the check reads the row, whichever release wrote it
	// (TestAnEarlierDaemonsWriteOfAToldRootClearsTheMarkItCarries).
	const stated = `awaiting_slot_notice_at = timestamptz '2026-11-08 09:00:00+00'`
	for _, refused := range []struct{ why, statement string }{
		// Leaving the phase clears the wait, so a mark outside awaiting_person is a wait's mark
		// left behind.
		{"the mark in another phase", `update issues set phase = 'admitted', awaiting_ask = null, awaiting_since_seq = null,
			awaiting_since = null, awaiting_answered_at = null, awaiting_answered_by = null, awaiting_answered_detail = null,
			` + stated + ` where key = 'LEGION-208'`},
		// A root still waiting on a person wants no slot, so no notice of one is written for it.
		{"the mark on an unanswered wait", `update issues set awaiting_answered_at = null, awaiting_answered_by = null,
			awaiting_answered_detail = null, ` + stated + ` where key = 'LEGION-208'`},
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

// An earlier daemon's PutIssue names no awaiting_slot_notice_at, so on conflict a marked row
// keeps the mark and that daemon's next write of the root carries it into a row the check above
// refuses: the resume that leaves the phase, the linger or re-admission that clears the wait,
// and the new wait it starts over the old one each write every awaiting column but this one.
// Admission runs inside every fact's transaction, so one such row would fail every fact of its
// project. issues_carried_slot_notice_cleared takes the carried mark out before the check reads
// the row, so each of those writes lands with the mark null, and a write that leaves the root
// standing in its answered wait keeps the mark the telling wrote.
func TestAnEarlierDaemonsWriteOfAToldRootClearsTheMarkItCarries(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	// told is the root as the telling leaves it: an answered wait with the mark beside the
	// answer, written in two statements as the daemon writes it.
	told := func(key string) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank,
			last_dispatch_seq, awaiting_ask, awaiting_since_seq, awaiting_since, awaiting_answered_at, awaiting_answered_by,
			awaiting_answered_detail, awaiting_slot_notice_at)
			values ($1, $1, 'LEGION', 'an answered root its architect was told of', 'awaiting_person', 1, 'in_progress', 'A', 4,
			'ask-q', 11, now(), now(), 'ask', 'ask-q', now())`, key); err != nil {
			t.Fatalf("seed the told root %s: %v", key, err)
		}
	}
	mark := func(key string) *string {
		t.Helper()
		var at *string
		if err := store.Pool().QueryRow(ctx, `select awaiting_slot_notice_at::text from issues where key = $1`, key).Scan(&at); err != nil {
			t.Fatalf("read the mark of %s: %v", key, err)
		}
		return at
	}

	for _, write := range []struct{ key, what, statement string }{
		// The resume: the root leaves awaiting_person for admitted and its wait goes whole.
		{"LEGION-301", "the resume of an answered root", `update issues set phase = 'admitted', awaiting_ask = null,
			awaiting_since_seq = null, awaiting_since = null, awaiting_answered_at = null, awaiting_answered_by = null,
			awaiting_answered_detail = null where key = $1`},
		// The linger: the root is closed with its tree, which clears the wait as the resume does.
		{"LEGION-302", "the linger of a told root", `update issues set phase = 'done', status = 'done',
			linger_until = now() + interval '1 hour', awaiting_ask = null, awaiting_since_seq = null, awaiting_since = null,
			awaiting_answered_at = null, awaiting_answered_by = null, awaiting_answered_detail = null where key = $1`},
		// A new wait over the old one: awaiting_person again, nobody having answered it yet.
		{"LEGION-303", "a new wait over the told one", `update issues set awaiting_ask = 'ask-r', awaiting_since_seq = 21,
			awaiting_since = now(), awaiting_answered_at = null, awaiting_answered_by = null, awaiting_answered_detail = null
			where key = $1`},
	} {
		key := write.key
		told(key)
		if _, err := store.Pool().Exec(ctx, write.statement, key); err != nil {
			t.Fatalf("%s, as an earlier daemon writes it: %v; want the carried mark cleared and the write taken", write.what, err)
		}
		if at := mark(key); at != nil {
			t.Fatalf("the mark of %s after %s = %q, want it cleared with the wait it belonged to", key, write.what, *at)
		}
	}

	// The other half: a write that leaves the root standing in its answered wait names no mark
	// either, and the telling's mark stays. This release's own telling writes the mark on such a
	// row (admit's tellWaitingForSlot), and nothing here may take it back.
	key := "LEGION-304"
	told(key)
	if _, err := store.Pool().Exec(ctx, `update issues set title = 'the same answered root, retitled' where key = $1`, key); err != nil {
		t.Fatalf("write a told root that is still waiting: %v", err)
	}
	if at := mark(key); at == nil {
		t.Fatalf("the mark of %s after a write that leaves its wait standing = null, want the moment the notice was written", key)
	}
}
