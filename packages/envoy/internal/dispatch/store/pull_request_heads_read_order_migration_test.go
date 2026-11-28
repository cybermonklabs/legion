package store

import (
	"context"
	"testing"
	"time"
)

// pullRequestHeadsReadOrderVersion is the migration whose trigger orders two answers of one
// GitHub updated_at second by the place each read took in the order of reads (github_read_order),
// and not by the clock of the process that read (github_read_at).
const pullRequestHeadsReadOrderVersion = 20261117172917

// TestMigrationOrdersTheAnswersOfASecondByThePlaceOfTheirReads: after the migration, an update of
// a row by a write of the same event_at whose read took an earlier place writes nothing, whatever
// the two reads' clocks say, and owes the pull request a read where a fact GitHub answers differs.
// A write whose read took a later place applies, and owes where a fact differs, whatever its
// clock; so does one of another second, and one onto a row with no place, which a release before
// and the earlier builds of this change wrote, and a write of the release before, which leaves the
// place as it was. The control is the set through 20261117114124, which orders by the clocks: its
// case 1 (a later clock, an earlier place) applies the older answer, and its case 3 skips the newer.
func TestMigrationOrdersTheAnswersOfASecondByThePlaceOfTheirReads(t *testing.T) {
	for _, through := range []int{pullRequestHeadsReadStartVersion, pullRequestHeadsReadOrderVersion} {
		t.Run(map[int]string{pullRequestHeadsReadStartVersion: "before", pullRequestHeadsReadOrderVersion: "after"}[through], func(t *testing.T) {
			ctx := context.Background()
			store := openEmptyTestStore(t)
			if err := store.migrate(ctx, migrationsThrough(t, through)); err != nil {
				t.Fatalf("migrate to %d: %v", through, err)
			}
			after := through == pullRequestHeadsReadOrderVersion
			second := time.Now().UTC().Truncate(time.Second)
			clock := second.Add(-time.Minute)
			ahead, behind := clock.Add(time.Hour), clock.Add(-time.Hour)
			// place is the order of the stored read; earlier and later are a writer's.
			const earlier, place, later = int64(10), int64(20), int64(30)
			insert := func(number int, order *int64) {
				t.Helper()
				if after {
					_, err := store.Pool.Exec(ctx, `
						insert into pull_request_heads (repo, number, head_sha, state, event_at, github_read_at, github_read_order)
						values ('acme/widgets', $1, 'bbbb', 'open', $2, $3, $4)
					`, number, second, clock, order)
					if err != nil {
						t.Fatalf("store #%d: %v", number, err)
					}
					return
				}
				if _, err := store.Pool.Exec(ctx, `
					insert into pull_request_heads (repo, number, head_sha, state, event_at, github_read_at)
					values ('acme/widgets', $1, 'bbbb', 'open', $2, $3)
				`, number, second, clock); err != nil {
					t.Fatalf("store #%d: %v", number, err)
				}
			}
			// write writes GitHub's answer at head and at, read by a read whose clock read readAt
			// and which took the place order, as this release's upsert does; readAt nil is a write
			// of the release before, which names neither column.
			write := func(number int, head string, at time.Time, readAt *time.Time, order *int64) int64 {
				t.Helper()
				if !after {
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
				tag, err := store.Pool.Exec(ctx, `
					update pull_request_heads set head_sha = $2, event_at = $3,
						github_read_at = coalesce($4, github_read_at),
						github_read_order = case when $4::timestamptz is null then github_read_order else $5 end
					where repo = 'acme/widgets' and number = $1
				`, number, head, at, readAt, order)
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
			for _, kase := range []struct {
				number int
				why    string
				head   string
				at     time.Time
				readAt *time.Time
				order  int64
				// stored is the row's place; 0 is none. kept is whether the row keeps head B
				// after the write: after the migration, and before it.
				stored            int64
				keptAfter, keptBy bool
				owed              bool
			}{
				{1, "an earlier place and a clock an hour ahead, another head", "aaaa", second, &ahead, earlier, place, true, false, true},
				{2, "an earlier place, the same head", "bbbb", second, &ahead, earlier, place, true, false, false},
				{3, "a later place and a clock an hour behind, another head", "aaaa", second, &behind, later, place, false, true, true},
				{4, "an earlier place of a later second", "aaaa", second.Add(time.Second), &ahead, earlier, place, false, false, false},
				{5, "an earlier place onto a row with no place", "aaaa", second, &behind, earlier, 0, false, true, true},
				{6, "a write of the release before of the same second, another head", "aaaa", second, nil, 0, place, false, false, true},
			} {
				var storedOrder *int64
				if kase.stored != 0 {
					storedOrder = &kase.stored
				}
				insert(kase.number, storedOrder)
				rows := write(kase.number, kase.head, kase.at, kase.readAt, &kase.order)
				head, owed := stored(kase.number)
				t.Logf("through=%d #%d (%s): rows=%d head=%s owed=%v", through, kase.number, kase.why, rows, head, owed)
				kept := kase.keptBy
				if after {
					kept = kase.keptAfter
				}
				wantHead, wantRows := kase.head, int64(1)
				if kept {
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
