package store

import (
	"context"
	"testing"
	"time"
)

// pullRequestHeadsOlderReadVersion is the migration whose trigger records a pull request as owed
// a read when a write of GitHub's answer replaces a newer row no read of GitHub wrote.
const pullRequestHeadsOlderReadVersion = 20261116182209

// TestMigrationOwesAReadWhereAnOlderAnswerReplacesANewerUnmarkedRow: after the migration, an
// update that writes a mark (github_read_at) over an unmarked row whose event_at is later than
// the update's own records the pull request as owed a read of kind failure. An update writing a
// mark over an older unmarked row, a write of the release before (no mark) over a marked row, and
// an update over a row that is marked already, record nothing.
func TestMigrationOwesAReadWhereAnOlderAnswerReplacesANewerUnmarkedRow(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.migrate(ctx, migrationsThrough(t, pullRequestHeadsOlderReadVersion)); err != nil {
		t.Fatalf("migrate to %d: %v", pullRequestHeadsOlderReadVersion, err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	owed := func(number int) string {
		t.Helper()
		var reason string
		err := store.Pool.QueryRow(ctx, `select reason from pull_request_reads_owed where repo = 'acme/widgets' and number = $1`, number).Scan(&reason)
		if err != nil {
			return ""
		}
		return reason
	}
	insert := func(number int, at time.Time, marked bool) {
		t.Helper()
		if _, err := store.Pool.Exec(ctx, `
			insert into pull_request_heads (repo, number, head_sha, state, event_at, github_read_at)
			values ('acme/widgets', $1, 'aaaa', 'open', $2, case when $3::boolean then clock_timestamp() end)
		`, number, at, marked); err != nil {
			t.Fatalf("store #%d: %v", number, err)
		}
	}
	update := func(number int, at time.Time, marked bool) {
		t.Helper()
		if _, err := store.Pool.Exec(ctx, `
			update pull_request_heads set head_sha = 'bbbb', state = 'closed', event_at = $2,
				github_read_at = case when $3::boolean then clock_timestamp() else github_read_at end
			where repo = 'acme/widgets' and number = $1
		`, number, at, marked); err != nil {
			t.Fatalf("update #%d: %v", number, err)
		}
	}

	insert(1, now, false)
	update(1, now.Add(-5*time.Minute), true)
	if got := owed(1); got != "failure" {
		t.Errorf("#1, an older marked answer over a newer unmarked row: owed %q, want failure", got)
	}
	insert(2, now, false)
	update(2, now.Add(5*time.Minute), true)
	insert(3, now, true)
	update(3, now.Add(-5*time.Minute), false)
	insert(4, now, true)
	update(4, now.Add(-5*time.Minute), true)
	for _, number := range []int{2, 3, 4} {
		if got := owed(number); got != "" {
			t.Errorf("#%d owed %q, want nothing", number, got)
		}
	}
}
