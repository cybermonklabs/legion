package legionfacts

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
)

// TestAPruneBetweenTwoPagesOfACatchUpReadsTheCursorAsLost: a consumer that stood still while
// changes arrived catches them up a full page at a time, and between two pages of that pass a
// prune takes the change the cursor rests on and the unread change after it, a pull request's
// merge on an issue with Legion phase work. The change log's retention has passed since the last
// page that read the log to its end: the full page before the prune carried that page's ReadAt
// forward, and the page after the prune judges it at its own reading of the clock. So that page
// reads the cursor as lost, the pass computes the project whole, and the issue's row holds the
// merge no page read. Stamped with the pass's moment, the full page would have made the cursor
// read a moment ago, the page after the prune would read on past the gap, and the row would keep
// its stale merge until its next input.
//
// It runs twice: with the retention already passed when the pass starts (a consumer down for 25
// hours), and with the pass starting 23 hours after the last read of the whole log and running two
// hours before the page after the prune, which a check at the pass's start would judge 23 hours
// old.
func TestAPruneBetweenTwoPagesOfACatchUpReadsTheCursorAsLost(t *testing.T) {
	for _, catchUp := range []struct {
		name          string
		down, running time.Duration
	}{
		{"down past the retention", 25 * time.Hour, 0},
		{"crossing the retention mid-pass", 23 * time.Hour, 2 * time.Hour},
	} {
		t.Run(catchUp.name, func(t *testing.T) {
			run := newConsumer(t, 2)
			run.pass(t)
			run.at = run.at.Add(time.Minute)
			run.pass(t)
			_, closed, _ := issueRows(t, run.history)
			issue := ""
			for _, key := range run.fixture.PhaseWork {
				if closed[key] != nil {
					issue = key
					break
				}
			}
			before, stored := run.rows(t)[issue]
			if !stored {
				t.Fatalf("no closed issue with Legion phase work holds a stored row (%q), so this test examined nothing", issue)
			}
			merged := closed[issue].Add(-time.Hour)
			if sameTime(before.MergedAt, &merged) {
				t.Fatalf("%s's row already holds the merge %s, so this test examined nothing", issue, merged.UTC())
			}
			readToEnd := run.cursors(t)[changesCursor]

			// The backlog the consumer missed, recorded a day ago by the database's clock: a change
			// the first page reads, then the merge, then a change recorded now that the prune keeps.
			var first, merge int64
			if err := run.database.Pool.QueryRow(t.Context(), `
				insert into delivery_changes (kind, repo, ref) values ('session_group', null, 'backlog') returning seq
			`).Scan(&first); err != nil {
				t.Fatalf("log the backlog's first change: %v", err)
			}
			run.exec(t, "seed the merge of "+issue, `
				insert into delivery_pull_requests (repo, number, title, url, author, created_at, merged_at, issue_key)
				values ('cybermonklabs/legion', 1, 'A pull request', 'https://example.test/pull/1', 'alice', $1, $2, $3)
			`, merged.Add(-time.Hour), merged, issue)
			if err := run.database.Pool.QueryRow(t.Context(), `select max(seq) from delivery_changes where kind = 'pull_request'`).Scan(&merge); err != nil || merge <= first {
				t.Fatalf("the merge logged the change %d after the backlog's first %d (%v), want one after it", merge, first, err)
			}
			run.exec(t, "record the backlog a day ago", `update delivery_changes set at = now() - interval '25 hours' where seq between $1 and $2`, first, merge)
			run.exec(t, "log a change the prune keeps", `insert into delivery_changes (kind, repo, ref) values ('session_group', null, 'kept')`)

			run.at = readToEnd.updatedAt.Add(catchUp.down)
			passAt := run.at
			if _, err := run.deps.Horizon.Await(t.Context()); err != nil {
				t.Fatal(err)
			}
			safe, err := run.deps.Horizon.SafeOrAwait(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			narrow := run.sched
			narrow.page = 1
			_, next, err := passPage(t.Context(), run.deps, narrow, safe, passAt, true)
			if err != nil {
				t.Fatal(err)
			}
			if at := run.cursors(t)[changesCursor]; !next.more || at.position != first {
				t.Fatalf("the first page of the catch-up left the change cursor at %d (more %t), want a full page resting on %d, so this test examined nothing", at.position, next.more, first)
			}
			if at := run.cursors(t)[changesCursor]; !at.updatedAt.Equal(readToEnd.updatedAt) {
				t.Errorf("the full page stamped the change cursor %s, want the last read of the whole log, %s, carried forward", at.updatedAt.UTC(), readToEnd.updatedAt.UTC())
			}

			if _, err := delivery.PruneChanges(t.Context(), run.database.Pool); err != nil {
				t.Fatalf("prune the change log: %v", err)
			}
			var left int
			if err := run.database.Pool.QueryRow(t.Context(), `select count(*) from delivery_changes where seq between $1 and $2`, first, merge).Scan(&left); err != nil || left != 0 {
				t.Fatalf("the prune left %d of the backlog's changes %d to %d (%v), so this test examined nothing", left, first, merge, err)
			}
			run.at = run.at.Add(catchUp.running)
			for page := next; page.more; {
				if _, page, err = passPage(t.Context(), run.deps, narrow, safe, passAt, page.unfed); err != nil {
					t.Fatal(err)
				}
			}

			after := run.rows(t)[issue]
			if !sameTime(after.MergedAt, &merged) {
				t.Errorf("after the catch-up %s's row holds the merge %v, want %s, the merge the prune took before a page read it", issue, utc(after.MergedAt), merged.UTC())
			}
			if whole := computedWholeAt(t, run)[run.fixture.Project]; !whole.Equal(passAt) {
				t.Errorf("%s was last computed whole as of %s, want the catch-up's moment %s: the pass read past the prune", run.fixture.Project, whole.UTC(), passAt.UTC())
			}
			if at := run.cursors(t)[changesCursor]; !at.updatedAt.Equal(run.at) {
				t.Errorf("after the move to the log's head the change cursor is stamped %s, want that page's reading of the clock, %s", at.updatedAt.UTC(), run.at.UTC())
			}
			t.Logf("%s: the last read of the whole log at %s, the pass from %s, the page after the prune at %s; %s's merge %v before, %v after",
				catchUp.name, readToEnd.updatedAt.UTC(), passAt.UTC(), run.at.UTC(), issue, utc(before.MergedAt), utc(after.MergedAt))
		})
	}
}
