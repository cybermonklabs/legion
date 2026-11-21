package store

import (
	"context"
	"testing"
	"time"
)

// restoreAsksVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const restoreAsksVersion = 20261114090103

// A copy's `at` is when its take began, so this migration gives each of the two copy tables a
// column of its own for the write, which the restore weighs against a pod's first ask. A row that
// stands at the upgrade takes the database's clock at the upgrade as its write, and not its `at`,
// which on an earlier release is the shim's reading by its node's clock: no first ask is recorded
// before this migration, so every standing copy was written before every ask. A release bundle an
// earlier daemon saves after a rollback names no such column, and is stamped with the database's
// clock as it is written rather than refused: refused, every idle release that earlier daemon ran
// would fail its save and be cancelled. The negative control is the earlier daemon's save read
// before the migration, which the table takes as it always did.
func TestEachSavedCopyKeepsWhenItWasWrittenWith20261114090103(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, restoreAsksVersion-1)
	saveBundle := func(issue string, release int64, at time.Time) error {
		_, err := store.Pool().Exec(ctx, `insert into workspace_bundles (issue, release_id, source_pod_uid, bundle, size_bytes, at, head)
			values ($1, $2, 'pod-being-released', 'PACK', 4, $3, 'c1')`, issue, release, at)
		return err
	}
	bundled := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	copied := bundled.Add(time.Minute)
	if err := saveBundle("INFRA-1117", 7, bundled); err != nil {
		t.Fatalf("save a release bundle before the migration: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, `insert into workspace_running_copies (issue, source_pod_uid, bundle, size_bytes, at, head)
		values ('INFRA-1117', 'pod-still-running', 'PACK', 4, $1, 'c2')`, copied); err != nil {
		t.Fatalf("save a running copy before the migration: %v", err)
	}
	upgradeBegan := storeClock(t, store)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the copies' write times: %v", err)
	}
	upgradeEnded := storeClock(t, store)
	for _, c := range []struct {
		table string
		at    time.Time
	}{{workspaceBundlesTable, bundled}, {runningCopiesTable, copied}} {
		var written time.Time
		if err := store.Pool().QueryRow(ctx, `select written_at from `+c.table+` where issue = 'INFRA-1117'`).Scan(&written); err != nil ||
			written.Before(upgradeBegan) || written.After(upgradeEnded) {
			t.Fatalf("the row of %s standing at the upgrade, at %s, reads as written at %s (%v), want the database's clock at the upgrade, between %s and %s",
				c.table, c.at, written, err, upgradeBegan, upgradeEnded)
		}
	}

	// An earlier daemon, after a rollback, saves a release bundle naming the columns it knows.
	before := storeClock(t, store)
	if err := saveBundle("INFRA-2760", 8, bundled); err != nil {
		t.Fatalf("an earlier daemon's save of a release bundle after the migration was refused: %v", err)
	}
	var written time.Time
	if err := store.Pool().QueryRow(ctx, `select written_at from workspace_bundles where issue = 'INFRA-2760'`).Scan(&written); err != nil || written.Before(before) {
		t.Fatalf("the earlier daemon's bundle reads as written at %s (%v), want the database's clock as it was written, at %s or later", written, err, before)
	}
}
