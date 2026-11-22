package store

import (
	"context"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// 20261121083932 keeps the check runs of each pull request's checked head that the listener
// excluded. A pull request written before it was settled by a release that kept no exclusion, so
// it carries none: the column takes the empty list, which the store reads as a checked head with
// no excluded run (record.PullRequest.ExcludedRuns), and what the release before wrote of the
// settlement - the checked head, its check runs and the failing names - reads back as it was
// written. A pull request written after it keeps the excluded runs it is given, and reads them
// back.
func TestAPullRequestRecordedBefore20261121083932HasNoExcludedRuns(t *testing.T) {
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 20261120055717)
	if _, err := store.Pool().Exec(ctx, `insert into issues
		(key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('WIDGETS-1208', 'WIDGETS-1208', 'WIDGETS', 'root', 'reviewing', 1, 'needs_review', 'A', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool().Exec(ctx, `insert into pull_requests (issue, repo, number, branch, head_sha, head_updated_at,
		failing, cancelled, fix_attempts, blocked_attempts, check_runs, generation, snapshot, head_counted, planned_red,
		review_seen, state, checked_head, required_workflows, required_workflows_head, mergeability, merge_state, base, base_moved)
		values ('WIDGETS-1208', 'acme/widgets', 1208, 'legion/WIDGETS-1208', 'h4', now(), '["unit"]', '[]', 0, 0,
		 '[{"name":"unit","id":7,"head":"h4","conclusion":"failed"}]', 3, 'h4-3', '', false, 0, 'open', 'h4', '[]', '',
		 'MERGEABLE', 'clean', 'main', '')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var column string
	if err := store.Pool().QueryRow(ctx,
		`select excluded_runs::text from pull_requests where issue = 'WIDGETS-1208'`).Scan(&column); err != nil {
		t.Fatalf("read the pull request's excluded runs after 20261121083932: %v", err)
	}
	if column != "[]" {
		t.Fatalf("excluded_runs after 20261121083932 = %s, want [] for a pull request recorded before it", column)
	}
	read := func() *record.PullRequest {
		t.Helper()
		var pr *record.PullRequest
		if err := pgx.BeginFunc(ctx, store.Pool(), func(tx pgx.Tx) error {
			var err error
			pr, err = record.NewStore().PullRequest(ctx, tx, "WIDGETS-1208")
			return err
		}); err != nil || pr == nil {
			t.Fatalf("read the pull request after 20261121083932: %+v, %v", pr, err)
		}
		return pr
	}
	pr := read()
	wantRuns := []record.AttemptRun{{Name: "unit", ID: 7, Head: "h4", Conclusion: record.RunFailed}}
	if len(pr.ExcludedRuns) != 0 || pr.CheckedHead != "h4" || !reflect.DeepEqual(pr.CheckRuns, wantRuns) ||
		!reflect.DeepEqual(pr.Failing, []string{"unit"}) || pr.Generation != 3 || pr.Snapshot != "h4-3" {
		t.Fatalf("the pull request after 20261121083932 = excluded %+v, checked head %q, runs %+v, failing %v, generation %d, snapshot %q; "+
			"want no excluded run beside the settlement of h4 the release before wrote", pr.ExcludedRuns, pr.CheckedHead, pr.CheckRuns,
			pr.Failing, pr.Generation, pr.Snapshot)
	}

	excluded := []record.AttemptRun{{Name: "ci-ok", ID: 9}}
	pr.ExcludedRuns = excluded
	if err := pgx.BeginFunc(ctx, store.Pool(), func(tx pgx.Tx) error {
		return record.NewStore().PutPullRequest(ctx, tx, *pr)
	}); err != nil {
		t.Fatalf("write the pull request with an excluded run: %v", err)
	}
	if got := read().ExcludedRuns; !reflect.DeepEqual(got, excluded) {
		t.Fatalf("the pull request reads back excluded runs %+v, want %+v", got, excluded)
	}
}
