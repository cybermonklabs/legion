package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// releasedForWaitVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const releasedForWaitVersion = 20261111214351

// An issue the daemon has started and that now waits on another issue gives its admission slot
// back, and released_for_wait_at is the mark of that: the moment the row was taken away. Before
// this migration the issues table has no such column, which is the negative control read first —
// the daemon could record the release of a started issue's row nowhere, so the waiting line could
// not tell that issue from one that never started.
//
// After the migration the column is nullable with no backfill: an issue recorded before the
// deploy reads null, which is the truth about it, since no daemon could give a slot back for a
// wait yet. A mark written reads back as the moment it was written, and clearing it reads null
// again, which is what the grant of a slot and the ordinary release of a row both write.
func TestAnIssueRecordsItsReleaseForAWaitWith20261111214351(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, releasedForWaitVersion-1)
	seed := func(key string) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
			values ($1, $1, 'LEGION', 'root', 'implementing', 1, 'in_progress', 'A', 1)`, key); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}

	seed("LEGION-208")
	var before *time.Time
	err := store.Pool().QueryRow(ctx, `select released_for_wait_at from issues where key = 'LEGION-208'`).Scan(&before)
	if err == nil || !strings.Contains(err.Error(), "released_for_wait_at") {
		t.Fatalf("reading an issue's release for a wait before the migration = %v, want the column to be missing: it is what the migration exists for", err)
	}
	t.Logf("before the migration the issue records no release for a wait: %v", err)

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the issue's release for a wait: %v", err)
	}

	if err := store.Pool().QueryRow(ctx, `select released_for_wait_at from issues where key = 'LEGION-208'`).Scan(&before); err != nil {
		t.Fatalf("read the issue the migration found standing: %v", err)
	}
	if before != nil {
		t.Fatalf("the issue standing across the migration = released for a wait at %v, want null: nothing backfills a release no daemon could make", before)
	}
	t.Logf("after the migration the issue standing across it reads null, as an issue holding its slot does")

	at := time.Date(2026, 11, 11, 21, 43, 51, 0, time.UTC)
	if _, err := store.Pool().Exec(ctx, `update issues set released_for_wait_at = $1 where key = 'LEGION-208'`, at); err != nil {
		t.Fatalf("mark the issue released for a wait: %v", err)
	}
	var marked *time.Time
	if err := store.Pool().QueryRow(ctx, `select released_for_wait_at from issues where key = 'LEGION-208'`).Scan(&marked); err != nil {
		t.Fatalf("read the mark: %v", err)
	}
	if marked == nil || !marked.Equal(at) {
		t.Fatalf("the issue released for a wait = %v, want %v", marked, at)
	}

	if _, err := store.Pool().Exec(ctx, `update issues set released_for_wait_at = null where key = 'LEGION-208'`); err != nil {
		t.Fatalf("clear the mark: %v", err)
	}
	if err := store.Pool().QueryRow(ctx, `select released_for_wait_at from issues where key = 'LEGION-208'`).Scan(&marked); err != nil {
		t.Fatalf("read the cleared mark: %v", err)
	}
	if marked != nil {
		t.Fatalf("the issue whose mark was cleared = released for a wait at %v, want null", marked)
	}
	t.Logf("after the migration the mark is written and cleared, which is the slot given back and taken again")
}
