package store

import (
	"context"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// 20261117112334 keeps the commit statuses of each pull request's checked head. A pull request
// written before it was settled by a release that kept no status, so it carries none: the column
// takes the empty list, which the store reads as a checked head the daemon holds no status for
// (record.PullRequest.Statuses), and no text names one until the next settlement of that head
// names its own. What the release before wrote of the settlement - the checked head and its check
// runs - reads back as it was written.
func TestAPullRequestRecordedBefore20261117112334HasNoStatuses(t *testing.T) {
	t.Parallel()
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 20261114061900)
	if _, err := store.Pool().Exec(ctx, `insert into issues
		(key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'root', 'reviewing', 1, 'needs_review', 'A', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool().Exec(ctx, `insert into pull_requests (issue, repo, number, branch, head_sha, head_updated_at,
		failing, cancelled, fix_attempts, blocked_attempts, check_runs, generation, snapshot, head_counted, planned_red,
		review_seen, state, checked_head, required_workflows, required_workflows_head, mergeability, merge_state, base, base_moved)
		values ('LEGION-208', 'acme/widgets', 1208, 'legion/LEGION-208', 'h4', now(), '["unit"]', '[]', 0, 0,
		 '[{"name":"unit","id":7,"head":"h4","conclusion":"failed"}]', 3, 'h4-3', '', false, 0, 'open', 'h4', '[]', '',
		 'MERGEABLE', 'clean', 'main', '')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var column string
	if err := store.Pool().QueryRow(ctx,
		`select statuses::text from pull_requests where issue = 'LEGION-208'`).Scan(&column); err != nil {
		t.Fatalf("read the pull request's statuses after 20261117112334: %v", err)
	}
	if column != "[]" {
		t.Fatalf("statuses after 20261117112334 = %s, want [] for a pull request recorded before it", column)
	}
	var pr *record.PullRequest
	if err := Transact(ctx, store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		pr, err = record.NewStore().PullRequest(ctx, tx, "LEGION-208")
		return err
	}); err != nil || pr == nil {
		t.Fatalf("read the pull request after 20261117112334: %+v, %v", pr, err)
	}
	wantRuns := []record.AttemptRun{{Name: "unit", ID: 7, Head: "h4", Conclusion: record.RunFailed}}
	if len(pr.Statuses) != 0 || pr.CheckedHead != "h4" || !reflect.DeepEqual(pr.CheckRuns, wantRuns) ||
		!reflect.DeepEqual(pr.Failing, []string{"unit"}) || pr.Generation != 3 || pr.Snapshot != "h4-3" {
		t.Fatalf("the pull request after 20261117112334 = statuses %+v, checked head %q, runs %+v, failing %v, generation %d, snapshot %q; "+
			"want no status beside the settlement of h4 the release before wrote", pr.Statuses, pr.CheckedHead, pr.CheckRuns,
			pr.Failing, pr.Generation, pr.Snapshot)
	}
}
