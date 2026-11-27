package store

import (
	"context"
	"testing"
	"time"
)

// 20261124130001 records, beside a pull request's merge state, how many review requests GitHub
// holds open on it and whether a check of its head was still running when that state was read. A
// pull request written before it holds neither, and nothing in the row derives either, so the row
// takes the values that say no read wrote them - a null count and an empty rollup - beside the
// merge_state it already held, never a zero that would read as none open. The round does not end
// on `blocked` beside them, and the daemon's next read of the pull request writes both.
func TestAPullRequestRecordedBefore20261124130001HoldsNoCountAndNoRollup(t *testing.T) {
	t.Parallel()
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 20261124060500)
	if _, err := store.Pool().Exec(ctx, `insert into issues
		(key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'root', 'reviewing', 1, 'needs_review', 'A', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool().Exec(ctx, `insert into pull_requests (issue, repo, number, branch, head_sha, head_updated_at,
		failing, cancelled, fix_attempts, blocked_attempts, check_runs, generation, snapshot, head_counted, planned_red,
		review_seen, state, checked_head, required_workflows, required_workflows_head, mergeability, merge_state, base, base_moved)
		values ('LEGION-208', 'sjawhar/legion', 1208, 'legion/LEGION-208', 'h4', now(), '[]', '[]', 0, 0, '[]',
		 1, '', '', false, 0, 'open', '', '[]', '', 'MERGEABLE', 'blocked', 'main', '')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var requested *int
	var checks, state string
	var nulls int
	var since, evidence time.Time
	if err := store.Pool().QueryRow(ctx,
		`select reviews_requested, head_checks, null_mergeable_reads, null_mergeable_since, evidence_read_at, merge_state
		 from pull_requests where issue = 'LEGION-208'`).Scan(&requested, &checks, &nulls, &since, &evidence, &state); err != nil {
		t.Fatalf("read the pull request after 20261124130001: %v", err)
	}
	zero := time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)
	if requested != nil || checks != "" || nulls != 0 || !since.Equal(zero) || !evidence.Equal(zero) || state != "blocked" {
		t.Fatalf("after 20261124130001 the row holds %v review requests, rollup %q, %d null reads since %v written by a read at %v and merge state %q, want nothing read beside the blocked it held",
			requested, checks, nulls, since, evidence, state)
	}
	for _, statement := range []string{
		`update pull_requests set reviews_requested = -1 where issue = 'LEGION-208'`,
		`update pull_requests set head_checks = 'pending' where issue = 'LEGION-208'`,
		`update pull_requests set null_mergeable_reads = -1 where issue = 'LEGION-208'`,
	} {
		if _, err := store.Pool().Exec(ctx, statement); err == nil {
			t.Fatalf("%s was taken, want the column's check to refuse it", statement)
		}
	}
	if _, err := store.Pool().Exec(ctx, `update pull_requests set reviews_requested = 2, head_checks = 'running',
		null_mergeable_reads = 1, null_mergeable_since = now(), evidence_read_at = now() where issue = 'LEGION-208'`); err != nil {
		t.Fatalf("a read's count, rollup and null reads were refused: %v", err)
	}
}
