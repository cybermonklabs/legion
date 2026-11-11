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
	// account of what a jump displaced outlives the issue record, so a key no issue row carries
	// is recorded here.
	const record = `insert into merge_jump_records (project, repository, issue, number, head, at, reason,
		entries, entries_total, entries_unknown, refused, refusal)
		values ('LEGION', 'acme/widgets', 'LEGION-DELETED', 42, 'c0de', now(), 'the release waits on it',
		'[{"number": 7, "position": 0, "jump": false, "state": "QUEUED", "enqueuedAt": "2026-11-10T00:00:00Z"}]',
		1, false, false, '')`
	exec(record)
	if _, err := store.Pool().Exec(ctx, record); err == nil || !strings.Contains(err.Error(), "merge_jump_records_pkey") {
		t.Fatalf("a second jump record of one repository = %v, want the primary key to refuse it", err)
	}
	exec(`insert into merge_jump_records (project, repository, issue, number, head, at, reason,
		entries, entries_total, entries_unknown, refused, refusal)
		values ('LEGION', 'acme/other', 'LEGION-208', 43, 'beef', now(), 'another repository of the project',
		'[]', 0, true, true, 'GitHub refused the jump')`)
	t.Logf("after the migration each of the three tables takes a row and holds its own key")
}
