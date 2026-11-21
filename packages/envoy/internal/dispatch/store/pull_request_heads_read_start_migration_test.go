package store

import (
	"context"
	"testing"
	"time"
)

// pullRequestHeadsReadStartVersion is the migration whose trigger keeps, of two answers of one
// GitHub updated_at second, the one whose read of GitHub began later (github_read_at).
const pullRequestHeadsReadStartVersion = 20261117114124

// TestMigrationKeepsTheAnswerOfItsSecondWhoseReadBeganLater: after the migration, an update of a
// marked row by a marked write of the same event_at whose read began before the row's read writes
// nothing, and owes the pull request a read of kind failure where a fact GitHub answers differs
// (acceptance of cybermonklabs/legion#4209 at 3a21edb2c9, D2). An update whose read began later
// applies, and owes where a fact differs, as before; so does one of another second, whatever its
// read's start, and a write of the release before, which leaves the mark as it was. The control is
// the set through 20261116182209, where the earlier read's answer applies.
func TestMigrationKeepsTheAnswerOfItsSecondWhoseReadBeganLater(t *testing.T) {
	for _, through := range []int{pullRequestHeadsOlderReadVersion, pullRequestHeadsReadStartVersion} {
		t.Run(map[int]string{pullRequestHeadsOlderReadVersion: "before", pullRequestHeadsReadStartVersion: "after"}[through], func(t *testing.T) {
			ctx := context.Background()
			store := openEmptyTestStore(t)
			if err := store.migrate(ctx, migrationsThrough(t, through)); err != nil {
				t.Fatalf("migrate to %d: %v", through, err)
			}
			second := time.Now().UTC().Truncate(time.Second)
			began := second.Add(-time.Minute)
			insert := func(number int, readAt time.Time) {
				t.Helper()
				if _, err := store.Pool.Exec(ctx, `
					insert into pull_request_heads (repo, number, head_sha, state, event_at, github_read_at)
					values ('acme/widgets', $1, 'bbbb', 'open', $2, $3)
				`, number, second, readAt); err != nil {
					t.Fatalf("store #%d: %v", number, err)
				}
			}
			// write writes GitHub's answer at head and at, read by a read that began at readAt, as
			// this release's upsert does; readAt nil is a write of the release before, which names
			// no github_read_at.
			write := func(number int, head string, at time.Time, readAt *time.Time) int64 {
				t.Helper()
				tag, err := store.Pool.Exec(ctx, `
					update pull_request_heads set head_sha = $2, event_at = $3,
						github_read_at = coalesce($4, github_read_at)
					where repo = 'acme/widgets' and number = $1
				`, number, head, at, readAt)
				if err != nil {
					t.Fatalf("write #%d: %v", number, err)
				}
				return tag.RowsAffected()
			}
			stored := func(number int) (head string, owed bool) {
				t.Helper()
				if err := store.Pool.QueryRow(ctx, `
					select head_sha, exists (select from pull_request_reads_owed o where o.repo = h.repo and o.number = h.number)
					from pull_request_heads h where repo = 'acme/widgets' and number = $1
				`, number).Scan(&head, &owed); err != nil {
					t.Fatalf("read #%d: %v", number, err)
				}
				return head, owed
			}
			earlier, later := began.Add(-time.Second), began.Add(time.Second)
			after := through == pullRequestHeadsReadStartVersion
			for _, kase := range []struct {
				number int
				why    string
				head   string
				at     time.Time
				readAt *time.Time
				// kept is whether the row keeps head B after the write, after the migration.
				kept, owed bool
			}{
				{1, "an answer of the second whose read began earlier, with another head", "aaaa", second, &earlier, true, true},
				{2, "an answer of the second whose read began earlier, with the same head", "bbbb", second, &earlier, true, false},
				{3, "an answer of the second whose read began later, with another head", "aaaa", second, &later, false, true},
				{4, "an answer of a later second whose read began earlier", "aaaa", second.Add(time.Second), &earlier, false, false},
				{5, "a write of the release before of the same second, with another head", "aaaa", second, nil, false, true},
			} {
				insert(kase.number, began)
				rows := write(kase.number, kase.head, kase.at, kase.readAt)
				head, owed := stored(kase.number)
				t.Logf("through=%d #%d (%s): rows=%d head=%s owed=%v", through, kase.number, kase.why, rows, head, owed)
				wantHead, wantRows := kase.head, int64(1)
				if after && kase.kept {
					wantHead, wantRows = "bbbb", 0
				}
				if head != wantHead || rows != wantRows || owed != kase.owed {
					t.Errorf("through %d, #%d, %s: %d rows written, head %s, owed %v; want %d rows, head %s, owed %v",
						through, kase.number, kase.why, rows, head, owed, wantRows, wantHead, kase.owed)
				}
			}
		})
	}
}
