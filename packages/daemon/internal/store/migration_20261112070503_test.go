package store

import (
	"context"
	"testing"
	"time"
)

// 20261112070503 records when the daemon last read a pull request and got an answer. A pull request
// written before it carries no read of its own: no release before this one stamped a read at all,
// and the row holds nothing to derive one from. The column takes the zero time beside whatever
// merge_state the row already held, which reads as a state with no read time on record
// (record.PullRequest.StateReadAt), rather than the migration's own moment, which would date every
// upgraded row to the deploy and call a merge state read a day ago fresh. The daemon's next read of
// the pull request fills it.
func TestAPullRequestRecordedBefore20261112070503HasNoStateReadAt(t *testing.T) {
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 20261111181500)
	if _, err := store.Pool().Exec(ctx, `insert into issues
		(key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'root', 'reviewing', 1, 'needs_review', 'A', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool().Exec(ctx, `insert into pull_requests (issue, repo, number, branch, head_sha, head_updated_at,
		failing, cancelled, fix_attempts, blocked_attempts, check_runs, generation, snapshot, head_counted, planned_red,
		review_seen, state, checked_head, required_workflows, required_workflows_head, mergeability, merge_state, base, base_moved)
		values ('LEGION-208', 'sjawhar/legion', 1208, 'legion/LEGION-208', 'h4', now(), '[]', '[]', 0, 0, '[]',
		 1, '', '', false, 0, 'open', '', '[]', '', 'MERGEABLE', 'clean', 'main', '')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var read time.Time
	if err := store.Pool().QueryRow(ctx,
		`select state_read_at from pull_requests where issue = 'LEGION-208'`).Scan(&read); err != nil {
		t.Fatalf("read the pull request after 20261112070503: %v", err)
	}
	if !read.Equal(time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("state_read_at after 20261112070503 = %s, want the zero time for a pull request recorded before it",
			read.UTC().Format(time.RFC3339))
	}
}
