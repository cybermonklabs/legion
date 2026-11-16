package store

import (
	"context"
	"testing"
)

// dropDefaultBranchCommitsVersion drops default_branch_commits, the per-repository commit cache
// 20261107234701_branch_commits replaced with one keyed by the branch as well (INFRA-1700).
const dropDefaultBranchCommitsVersion = 20261110004829

// TestMigrationDropsTheDefaultBranchCommits runs the shipped files: the replaced cache's table is
// gone after the migration, and branch_commits, the table that took its work, keeps its rows.
func TestMigrationDropsTheDefaultBranchCommits(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.migrate(ctx, migrationsThrough(t, dropDefaultBranchCommitsVersion-1)); err != nil {
		t.Fatalf("migrate through %d: %v", dropDefaultBranchCommitsVersion-1, err)
	}

	// The negative control of the check below, inside this one run: the table is here at the
	// schema the release before this one carried, so its absence after the migration is the
	// migration's doing and not a table that never existed.
	var presentBefore bool
	if err := store.Pool.QueryRow(ctx, `select to_regclass('default_branch_commits') is not null`).Scan(&presentBefore); err != nil {
		t.Fatalf("read the catalog at %d: %v", dropDefaultBranchCommitsVersion-1, err)
	}
	if !presentBefore {
		t.Fatalf("to_regclass('default_branch_commits') is null at %d, want the table that %d drops to exist before it runs", dropDefaultBranchCommitsVersion-1, dropDefaultBranchCommitsVersion)
	}

	if _, err := store.Pool.Exec(ctx, `insert into default_branch_commits (repo, sha, on_default) values ('acme/widgets', 'f00dcafe', true)`); err != nil {
		t.Fatalf("seed a default-branch answer: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `insert into branch_commits (repo, branch, sha, on_branch) values ('acme/widgets', 'infra-1700', 'f00dcafe', true)`); err != nil {
		t.Fatalf("seed a branch answer: %v", err)
	}

	if err := store.migrate(ctx, migrationsThrough(t, dropDefaultBranchCommitsVersion)); err != nil {
		t.Fatalf("migrate to %d: %v", dropDefaultBranchCommitsVersion, err)
	}

	var droppedExists bool
	if err := store.Pool.QueryRow(ctx, `select to_regclass('default_branch_commits') is not null`).Scan(&droppedExists); err != nil {
		t.Fatalf("read the catalog at %d: %v", dropDefaultBranchCommitsVersion, err)
	}
	if droppedExists {
		t.Errorf("to_regclass('default_branch_commits') is not null after %d, want the table gone", dropDefaultBranchCommitsVersion)
	}

	var keptExists bool
	if err := store.Pool.QueryRow(ctx, `select to_regclass('branch_commits') is not null`).Scan(&keptExists); err != nil {
		t.Fatalf("read the catalog for branch_commits: %v", err)
	}
	if !keptExists {
		t.Errorf("to_regclass('branch_commits') is null after %d, want the table that took the cache's work left alone", dropDefaultBranchCommitsVersion)
	}

	var repo, branch, sha string
	var onBranch bool
	if err := store.Pool.QueryRow(ctx, `select repo, branch, sha, on_branch from branch_commits`).Scan(&repo, &branch, &sha, &onBranch); err != nil {
		t.Errorf("read the seeded branch_commits row after %d: %v", dropDefaultBranchCommitsVersion, err)
	} else if repo != "acme/widgets" || branch != "infra-1700" || sha != "f00dcafe" || !onBranch {
		t.Errorf("branch_commits row = (%q, %q, %q, %t), want (%q, %q, %q, %t)", repo, branch, sha, onBranch, "acme/widgets", "infra-1700", "f00dcafe", true)
	}
}
