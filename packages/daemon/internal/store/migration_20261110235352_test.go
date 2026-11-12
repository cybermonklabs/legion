package store

import (
	"context"
	"strings"
	"testing"
)

// mergeMarksVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const mergeMarksVersion = 20261110235352

// The controller's two merge marks are durable because the submission they govern is made later,
// by an outbox row (record.MergeJump, record.MergeHold): 20261110235352 gives them their tables.
// The negative control is the read of each of the three before the migration, which answers an
// undefined-table error. After it each takes a row and holds its own key: one jump mark per issue,
// one hold per place in the project's list, one jump record per repository of the project.
//
// The jump mark's row is bound to the issue's by `references issues (key) on delete cascade`,
// which is the one schema rule the API reasons from: it refuses a mark on an issue the daemon
// does not record because such a mark could govern no submission and the database would refuse
// the write with nothing a controller could act on (api's recordedMarkIssue). So the issue is
// deleted at the end, with the hold and the jump record already seeded, and the mark is gone
// while those two stand: neither references issues, the jump record deliberately so.
func TestTheMergeMarkTablesArriveWith20261110235352(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, mergeMarksVersion-1)

	for _, table := range []string{"merge_jumps", "merge_holds", "merge_jump_records"} {
		_, err := store.Pool().Exec(ctx, "select 1 from "+table)
		if err == nil || !strings.Contains(err.Error(), "does not exist") {
			t.Fatalf("reading %s before the migration = %v, want the table not to exist: it is what the migration adds", table, err)
		}
		t.Logf("before the migration %s is no table: %v", table, err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the merge marks: %v", err)
	}

	exec := func(statement string, args ...any) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, statement, args...); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	exec(`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'a jumping merge', 'awaiting_merge', 1, 'in_progress', 'A', 4)`)

	// The jump mark: one row per issue, and a second of the same issue is refused — the daemon
	// replaces it rather than writing a second (record.Store.SetMergeJump).
	const jump = `insert into merge_jumps (issue, reason, session, set_at)
		values ('LEGION-208', 'the release waits on it', 'controller-1', now())`
	exec(jump)
	if _, err := store.Pool().Exec(ctx, jump); err == nil || !strings.Contains(err.Error(), "merge_jumps_pkey") {
		t.Fatalf("a second jump mark of one issue = %v, want the primary key to refuse it", err)
	}

	// The hold list: a row is identified by its place in its project's list, so one project
	// cannot hold two rows at one ordinal, and another project's list numbers from 0 of its own.
	const hold = `insert into merge_holds (project, ordinal, paths, reason, ends_at, session, set_at)
		values ('LEGION', 0, array['packages/daemon/'], 'the release is going out', now() + interval '1 hour', 'controller-1', now())`
	exec(hold)
	if _, err := store.Pool().Exec(ctx, hold); err == nil || !strings.Contains(err.Error(), "merge_holds_pkey") {
		t.Fatalf("a second hold at one ordinal of one project = %v, want the primary key to refuse it", err)
	}
	exec(`insert into merge_holds (project, ordinal, paths, repository, reason, ends_at, session, set_at)
		values ('INFRA', 0, array['infra/'], 'acme/infra', 'the same place in another project''s list', now() + interval '1 hour', 'controller-1', now())`)

	// The jump record: the newest jump of each repository of the project, so one repository holds
	// one record and a second repository holds its own. Its issue carries no foreign key: the
	// account of what a jump asked to go ahead of outlives the issue record, so a key no issue row carries
	// is recorded here.
	const record = `insert into merge_jump_records (project, repository, issue, number, head, at, reason,
		entries, entries_total, entries_unknown, outcome, answer)
		values ('LEGION', 'acme/widgets', 'LEGION-DELETED', 42, 'c0de', now(), 'the release waits on it',
		'[{"number": 7, "position": 0, "jump": false, "state": "QUEUED", "enqueuedAt": "2026-11-10T00:00:00Z"}]',
		1, false, 'asked', '')`
	exec(record)
	if _, err := store.Pool().Exec(ctx, record); err == nil || !strings.Contains(err.Error(), "merge_jump_records_pkey") {
		t.Fatalf("a second jump record of one repository = %v, want the primary key to refuse it", err)
	}
	exec(`insert into merge_jump_records (project, repository, issue, number, head, at, reason,
		entries, entries_total, entries_unknown, outcome, answer)
		values ('LEGION', 'acme/other', 'LEGION-208', 43, 'beef', now(), 'another repository of the project',
		'[]', 0, true, 'refused', 'GitHub refused the jump')`)

	exec(`delete from issues where key = 'LEGION-208'`)
	var marks, holds, jumped int
	if err := store.Pool().QueryRow(ctx, `select
		(select count(*) from merge_jumps),
		(select count(*) from merge_holds),
		(select count(*) from merge_jump_records)`).Scan(&marks, &holds, &jumped); err != nil {
		t.Fatalf("count the marks after the issue was deleted: %v", err)
	}
	if marks != 0 {
		t.Fatalf("jump marks after LEGION-208 was deleted = %d, want none: the mark cascades with the issue's record", marks)
	}
	if holds != 2 || jumped != 2 {
		t.Fatalf("holds %d and jump records %d after LEGION-208 was deleted, want 2 and 2: neither table references issues", holds, jumped)
	}
	t.Logf("after the migration each of the three tables takes a row and holds its own key")
}
