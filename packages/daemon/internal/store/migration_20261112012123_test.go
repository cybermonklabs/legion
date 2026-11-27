package store

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/store/migrations"
)

// repositoryCorrectionVersion is the version of the migration this file tests, so the test can
// migrate through everything before it whatever lands beside it.
const repositoryCorrectionVersion = 20261112012123

// A record written before this migration was never corrected, because the daemon that wrote it
// refused every correction of an issue's repository (its record.(*Postgres).PutIssue held the
// repository write-once). The migration says exactly that of such a row: repo_before
// is empty and repo_corrected_at is null, the pair that reads as "never corrected", so the
// correction the next pass applies is the row's first and has a real repository to record as the
// one the record held before it.
//
// The negative control is read before the migration: the columns do not exist yet, so the select
// that reads them is refused. The record itself stands across the migration with its repository
// unmoved, since a column added with a default changes no recorded fact.
func TestAnIssuePredating20261112012123WasNeverCorrected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, repositoryCorrectionVersion-1)
	if _, err := store.Pool().Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, repo, default_branch, github_repo)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'root', 'planning', 1, 'in_progress', 'A', 1, 'acme/widgets', 'main', 'Acme/Widgets')`); err != nil {
		t.Fatalf("record the issue standing before the migration: %v", err)
	}

	var before string
	err := store.Pool().QueryRow(ctx, `select repo_before from issues where key = 'LEGION-208'`).Scan(&before)
	if err == nil {
		t.Fatalf("reading repo_before before the migration = %q, want it refused: the column is what the migration adds", before)
	}
	t.Logf("before the migration the record carries no repository it was corrected from: %v", err)

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the repository correction: %v", err)
	}

	var repo, pending, reason string
	var correctedAt *string
	if err := store.Pool().QueryRow(ctx, `select repo, repo_before, repo_corrected_at, repo_change_pending, repo_change_reason
		from issues where key = 'LEGION-208'`).Scan(&repo, &before, &correctedAt, &pending, &reason); err != nil {
		t.Fatalf("read the record the migration found standing: %v", err)
	}
	if repo != "acme/widgets" {
		t.Fatalf("the record across the migration is on %q, want acme/widgets: a column with a default changes no recorded fact", repo)
	}
	if before != "" || correctedAt != nil {
		t.Fatalf("the record reads repo_before=%q repo_corrected_at=%v, want the empty repository and the null moment: nothing corrected it", before, correctedAt)
	}
	if pending != "" || reason != "" {
		t.Fatalf("the record reads repo_change_pending=%q repo_change_reason=%q, want both empty: no change waits on it", pending, reason)
	}
	t.Logf("after the migration the record reads repo_before=%q and repo_corrected_at=%v, which is never corrected", before, correctedAt)
}

// The migration stands below versions a deployed database has already recorded, among them the
// release that keeps no saved workspace (20261123050533), and the store applies a missing lower
// version late, so on such a database it runs after every higher version; on a fresh database it
// runs before them. It alters issues and claims alone, so it applies in either order, and the
// record it finds reads as never corrected in both. The fresh order is
// TestAnIssuePredating20261112012123WasNeverCorrected's, and this is the other: every embedded
// migration but this one first, then the upgrade.
func TestTheRepositoryCorrectionAppliesAfterEveryHigherVersion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	for _, migration := range all {
		if migration.Version == repositoryCorrectionVersion {
			continue
		}
		if _, err := store.apply(ctx, migration); err != nil {
			t.Fatalf("apply %s before the repository correction: %v", migration.Name, err)
		}
	}
	if _, err := store.Pool().Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, repo, default_branch, github_repo)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'root', 'planning', 1, 'in_progress', 'A', 1, 'acme/widgets', 'main', 'Acme/Widgets')`); err != nil {
		t.Fatalf("record the issue standing before the migration: %v", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("apply the repository correction after every higher version: %v", err)
	}

	var repo, before string
	var correctedAt *string
	if err := store.Pool().QueryRow(ctx, `select repo, repo_before, repo_corrected_at from issues where key = 'LEGION-208'`).
		Scan(&repo, &before, &correctedAt); err != nil {
		t.Fatalf("read the record the late migration found standing: %v", err)
	}
	if repo != "acme/widgets" || before != "" || correctedAt != nil {
		t.Fatalf("the record reads repo=%q repo_before=%q repo_corrected_at=%v, want acme/widgets, never corrected", repo, before, correctedAt)
	}
	var columns int
	if err := store.Pool().QueryRow(ctx, `select count(*) from information_schema.columns
		where table_schema = 'public' and table_name = 'claims'
		and column_name in ('update_repository', 'update_repository_round', 'working_repository', 'working_repository_incarnation')`).
		Scan(&columns); err != nil {
		t.Fatalf("read the claims columns the late migration added: %v", err)
	}
	if columns != 4 {
		t.Fatalf("claims holds %d of the four columns the migration adds, want all four", columns)
	}
}
