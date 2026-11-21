package store

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// holdsGateVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const holdsGateVersion = 20261120091044

// 20261120091044 adds issues.holds_gate, the mark that an issue holds its own design gate
// (record.Issue.HoldsGate), and backfills it from the tree the release before stored: every record
// whose key is its tree is a root of that release, or a root a hand-over made, and holds its own
// gate; every other record is a member of a tree and holds none until the chain makes it a root.
// Before the migration the issues table has no such column, which is the negative control read
// first. After it, the roots read marked and the members unmarked through the record's own read,
// a record written afterwards takes the default, and the column refuses null, which the record's
// read cannot scan.
func TestTheHoldsGateMigrationMarksEveryRootOfTheReleaseBefore(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, holdsGateVersion-1)
	for _, seed := range []struct{ key, tree string }{
		{"LEGION-208", "LEGION-208"}, // a root of the release before
		{"LEGION-209", "LEGION-208"}, // a member of its tree
		{"LEGION-300", "LEGION-300"}, // a root a hand-over made out of LEGION-208's tree
		{"LEGION-301", "LEGION-300"}, // a member of that root's tree
	} {
		if _, err := store.Pool().Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
			values ($1, $2, 'LEGION', $1, 'implementing', 1, 'in_progress', 'A', 1)`, seed.key, seed.tree); err != nil {
			t.Fatalf("record %s in the tree of %s before the migration: %v", seed.key, seed.tree, err)
		}
	}
	if _, err := store.Pool().Exec(ctx, "update issues set holds_gate = true"); err == nil || !strings.Contains(err.Error(), "holds_gate") {
		t.Fatalf("writing holds_gate before the migration = %v, want the column to be unknown: the fixture is not the old shape", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the mark that an issue holds its own gate: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-400', 'LEGION-400', 'LEGION', 'LEGION-400', 'admitted', 1, 'todo', 'B', 1)`); err != nil {
		t.Fatalf("record an issue after the migration, naming the column nowhere as the release before does: %v", err)
	}
	var recorded []record.Issue
	if err := store.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		recorded, err = record.NewStore().IssuesByKey(ctx, tx, []string{"LEGION-208", "LEGION-209", "LEGION-300", "LEGION-301", "LEGION-400"})
		return err
	}); err != nil {
		t.Fatalf("read the records after the migration: %v", err)
	}
	want := map[string]bool{"LEGION-208": true, "LEGION-209": false, "LEGION-300": true, "LEGION-301": false, "LEGION-400": false}
	if len(recorded) != len(want) {
		t.Fatalf("the record read %d issues after the migration, want the %d recorded", len(recorded), len(want))
	}
	for _, issue := range recorded {
		if issue.HoldsGate != want[issue.Key] {
			t.Errorf("%s, recorded in the tree of %s, holds its own gate = %t after the migration, want %t: a record whose key is its stored tree is a root and holds its own gate, a member holds none, and a record written after the migration takes the default",
				issue.Key, issue.Tree, issue.HoldsGate, want[issue.Key])
		}
	}

	if _, err := store.Pool().Exec(ctx, "update issues set holds_gate = null where key = 'LEGION-208'"); err == nil || !strings.Contains(err.Error(), "null value") {
		t.Fatalf("setting holds_gate to null = %v, want the not-null constraint to refuse it", err)
	}
}
