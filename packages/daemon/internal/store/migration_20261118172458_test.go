package store

import (
	"context"
	"testing"
)

// bundleSizeRefusalsVersion is the version of the migration this file tests, so the test can
// migrate through everything before it whatever lands beside it.
const bundleSizeRefusalsVersion = 20261118172458

// 20261118172458 creates workspace_bundle_refusals, which this branch's first copy of
// 20261112215432 created, so a database migrated from this branch before the table moved already
// holds it. Both kinds of database come out of the migration with the table: one that held it
// keeps the refusal it held, and one that never held it gets it empty, and each takes the row a
// refusal writes. The negative control is a plain `create table`, which fails on the database that
// already holds the table.
func TestTheBundleSizeRefusalsMigrationCreatesTheTableOnlyWhereItIsMissing(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		held bool
	}{
		{name: "a database that ran the first copy of 20261112215432", held: true},
		{name: "a database that never held the table", held: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := emptyStore(t)
			migrateThrough(t, store, bundleSizeRefusalsVersion-1)
			exec := func(statement string) {
				t.Helper()
				if _, err := store.Pool().Exec(ctx, statement); err != nil {
					t.Fatalf("%q: %v", statement, err)
				}
			}
			var present bool
			if err := store.Pool().QueryRow(ctx, `select to_regclass('workspace_bundle_refusals') is not null`).Scan(&present); err != nil {
				t.Fatalf("read whether the table stands before the migration: %v", err)
			}
			if present {
				t.Fatal("workspace_bundle_refusals stands before 20261118172458: a migration below it still creates the table")
			}
			want := 0
			if tc.held {
				// The statement the first copy of 20261112215432 ran, and a refusal written under it.
				exec(`create table workspace_bundle_refusals (issue text primary key, at timestamptz not null, head text not null)`)
				exec(`insert into workspace_bundle_refusals (issue, at, head) values ('LEGION-1', now(), 'head-one')`)
				want = 1
			}

			if _, err := store.Migrate(ctx); err != nil {
				t.Fatalf("migrate through 20261118172458: %v", err)
			}

			var rows int
			if err := store.Pool().QueryRow(ctx, `select count(*) from workspace_bundle_refusals`).Scan(&rows); err != nil {
				t.Fatalf("count the refusals after the migration: %v", err)
			}
			if rows != want {
				t.Errorf("workspace_bundle_refusals holds %d row(s) after the migration, want %d", rows, want)
			}
			exec(`insert into workspace_bundle_refusals (issue, at, head) values ('LEGION-2', now(), '')`)
		})
	}
}
