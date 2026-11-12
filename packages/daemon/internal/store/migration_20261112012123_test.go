package store

import (
	"context"
	"testing"
)

// repositoryCorrectionVersion is the version of the migration this file tests, so the test can
// migrate through everything before it whatever lands beside it.
const repositoryCorrectionVersion = 20261112012123

// A record written before this migration was never corrected, because the daemon that wrote it
// refused every correction of an issue's repository (record.(*Postgres).PutIssue's write-once
// guard, which this release removes). The migration says exactly that of such a row: repo_before
// is empty and repo_corrected_at is null, the pair that reads as "never corrected", so the
// correction the next pass applies is the row's first and has a real repository to record as the
// one the record held before it.
//
// The negative control is read before the migration: the columns do not exist yet, so the select
// that reads them is refused. The record itself stands across the migration with its repository
// unmoved, since a column added with a default changes no recorded fact.
func TestAnIssuePredating20261112012123WasNeverCorrected(t *testing.T) {
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

// The migration widens the restore attempt's own constraint to take the third outcome the
// repository refusal writes (workspace_bundles_attempt_outcome,
// store.TakeWorkspaceBundleForRestore). A row standing before the migration cannot carry the
// word — the constraint of 20261103214558 knows `sent` and `refused_older` alone — and after it
// the word is taken. The constraint is still a constraint: a word neither release wrote is
// refused, so a bundle can never be left in an outcome nothing reads.
func TestTheRestoreAttemptOf20261112012123TakesTheRepositoryRefusal(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, repositoryCorrectionVersion-1)
	if _, err := store.Pool().Exec(ctx, `insert into workspace_bundles
		(issue, bundle, size_bytes, at, head, release_id, source_pod_uid)
		values ('LEGION-208', '\x5041434b'::bytea, 4, now(), 'f2f134541b30dfa5160a5d1ce5e17b76c967c661', 11, 'pod-being-released')`); err != nil {
		t.Fatalf("save the bundle standing before the migration: %v", err)
	}

	spend := func(outcome string) error {
		_, err := store.Pool().Exec(ctx, `update workspace_bundles
			set attempt_pod_uid = 'pod-of-the-new-repository', attempt_at = now(), attempt_outcome = $1
			where issue = 'LEGION-208'`, outcome)
		return err
	}
	if err := spend("refused_repository"); err == nil {
		t.Fatal("the attempt was recorded as refused_repository before the migration, want it refused: the constraint knows two outcomes there")
	} else {
		t.Logf("before the migration the row cannot carry the repository refusal: %v", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the repository correction: %v", err)
	}

	if err := spend("refused_repository"); err != nil {
		t.Fatalf("record the attempt as refused_repository after the migration: %v", err)
	}
	var outcome string
	if err := store.Pool().QueryRow(ctx, `select attempt_outcome from workspace_bundles where issue = 'LEGION-208'`).Scan(&outcome); err != nil {
		t.Fatalf("read the attempt the migration let the row carry: %v", err)
	}
	if outcome != "refused_repository" {
		t.Fatalf("the row records the attempt as %q, want refused_repository", outcome)
	}
	if err := spend("refused_for_some_other_reason"); err == nil {
		t.Fatal("the attempt was recorded as a word no release writes, want it refused: the widened constraint is still a constraint")
	} else {
		t.Logf("after the migration a word neither release writes is still refused: %v", err)
	}
}
