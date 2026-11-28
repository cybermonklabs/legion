//go:build basebuild

package api

// Two statements of c-tree-read-bound that TestTheTreeReadAnswersTheSkeletonWithinItsBounds holds
// beside its shapes: the read with no view of each seeded issue answers the newest of the comments
// the seeding wrote on it (requireSkeletonNewestComments), and the database time each line prints is
// the delta of pg_stat_statements' total_exec_time over every statement of the run's database and
// role, divided by the readings (skeletonDatabaseTimeOf, which
// TestTheTreeReadBoundDatabaseTimeCountsEveryStatement holds to that sum).

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// skeletonNewestShown is how many comment ids one failure of requireSkeletonNewestComments names.
const skeletonNewestShown = 5

// skeletonWrittenComment is one comment the seeding wrote, as its row holds it.
type skeletonWrittenComment struct {
	id string
	at time.Time
}

// requireSkeletonNewestComments holds the read with no view of each issue of the corpus, its pages
// followed through its cursor (walks), to the comments the seeding wrote on it, the ids its writes
// answered (skeletonIssue's written). The read answers the skeletonCommentPage newest of them, or
// every one where the issue holds no more, and no other comment. Newest is by the time each
// comment's row was written and then by id, the order of R2's comment page (refs.ReadCommentPage),
// and the times are read from those rows over conn, the test's own connection, which no reading
// measures. Each comment the read answers carries the time its row holds. Where older comments
// remain, the read's comments_before names the oldest of the newest; where none does, it is null.
// A written comment with no row fails the issue, since its newest cannot then be named.
func requireSkeletonNewestComments(t *testing.T, conn *pgx.Conn, issues []*skeletonIssue, walks map[string]map[string][]skeletonPage) {
	t.Helper()
	for _, issue := range issues {
		comments, ok := skeletonWrittenRows(t, conn, issue)
		if !ok {
			continue
		}
		slices.SortFunc(comments, func(left, right skeletonWrittenComment) int {
			return cmp.Or(right.at.Compare(left.at), strings.Compare(right.id, left.id))
		})
		newest := comments[:min(len(comments), skeletonCommentPage)]
		rank := make(map[string]int, len(newest))
		for index, comment := range newest {
			rank[comment.id] = index
		}
		what := fmt.Sprintf("the %d newest of the %d comments the seeding wrote on it", len(newest), len(comments))
		pages := walks[issue.key]["no-view"]
		answered := map[string]bool{}
		var strays, untimed []string
		for _, node := range skeletonNodes(pages) {
			if node.Kind != graphstore.KindComment {
				continue
			}
			answered[node.ID] = true
			index, held := rank[node.ID]
			switch {
			case !held:
				strays = append(strays, node.ID)
			case node.MadeAt == nil || !node.MadeAt.Equal(newest[index].at):
				untimed = append(untimed, fmt.Sprintf("%s made_at %v, its row %s", node.ID, node.MadeAt, newest[index].at.UTC().Format(time.RFC3339Nano)))
			}
		}
		var missing []string
		for index, comment := range newest {
			if !answered[comment.id] {
				missing = append(missing, fmt.Sprintf("%s (newest but %d)", comment.id, index))
			}
		}
		if len(strays) > 0 {
			t.Errorf("%s (%s), filter no-view, answers %d comments that are none of %s, first %v", issue.key, issue.label, len(strays), what,
				strays[:min(len(strays), skeletonNewestShown)])
		}
		if len(missing) > 0 {
			t.Errorf("%s (%s), filter no-view, does not answer %d of %s, first %v", issue.key, issue.label, len(missing), what,
				missing[:min(len(missing), skeletonNewestShown)])
		}
		if len(untimed) > 0 {
			t.Errorf("%s (%s), filter no-view, answers %d of %s at another time than its row holds, first %v", issue.key, issue.label, len(untimed), what,
				untimed[:min(len(untimed), skeletonNewestShown)])
		}
		if len(pages) == 0 {
			continue
		}
		want := "null"
		if len(comments) > len(newest) {
			oldest := newest[len(newest)-1]
			encoded, err := json.Marshal((refs.CommentPosition{MadeAt: oldest.at, ID: oldest.id}).String())
			if err != nil {
				t.Fatalf("encode the position of %s: %v", oldest.id, err)
			}
			want = string(encoded)
		}
		if got := strings.TrimSpace(string(pages[0].CommentsBefore)); cmp.Or(got, "null") != want {
			t.Errorf("%s (%s), filter no-view, answers comments_before %s, want %s, the oldest of %s", issue.key, issue.label, got, want, what)
		}
		t.Logf("c-tree-read-bound: %s (%s), filter no-view, answers %d comments over %d pages; %s are %d of them, comments_before %s",
			issue.key, issue.label, len(answered), len(pages), what, len(newest)-len(missing), want)
	}
}

// skeletonWrittenRows answers the row of each comment the seeding wrote on issue, its id and the
// time its row holds, and false, having failed the test, where a written comment has no row.
func skeletonWrittenRows(t *testing.T, conn *pgx.Conn, issue *skeletonIssue) ([]skeletonWrittenComment, bool) {
	t.Helper()
	written := make(map[string]bool, len(issue.written))
	for _, id := range issue.written {
		written[id] = true
	}
	rows, err := conn.Query(context.Background(), `select id::text, created_at from comments where issue_key = $1`, issue.key)
	if err != nil {
		t.Fatalf("read the comments rows of %s: %v", issue.key, err)
	}
	defer rows.Close()
	var comments []skeletonWrittenComment
	others := 0
	for rows.Next() {
		var comment skeletonWrittenComment
		if err := rows.Scan(&comment.id, &comment.at); err != nil {
			t.Fatalf("read the comments rows of %s: %v", issue.key, err)
		}
		if written[comment.id] {
			comments = append(comments, comment)
		} else {
			others++
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the comments rows of %s: %v", issue.key, err)
	}
	if len(written) != issue.comments || len(comments) != issue.comments {
		t.Errorf("%s (%s) holds the rows of %d of the %d comments the seeding wrote on it (%d distinct ids), and %d rows of other comments",
			issue.key, issue.label, len(comments), issue.comments, len(written), others)
		return nil, false
	}
	return comments, true
}

// TestTheTreeReadBoundDatabaseTimeCountsEveryStatement holds skeletonDatabaseTimeOf to the words of
// c-tree-read-bound: the database time is the delta of total_exec_time over every statement of the
// run's database and role, divided by the readings, whatever the number of times each ran. Over 30
// readings, one statement ran 60 times in 60 ms, one 7 times in 21 ms, one not at all, one was
// evicted and written again, so its 3 calls in 6 ms count from zero, and one was first seen with 2
// calls in 3 ms: 90 ms and 72 calls, 3 ms and 2.4 statements per reading, of which the statement
// that ran at least once per reading holds 2 ms and the three that ran fewer times 1 ms.
func TestTheTreeReadBoundDatabaseTimeCountsEveryStatement(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, time.November, 28, 12, 0, 0, 0, time.UTC)
	before := skeletonSnapshot{at: at, evicted: 2, statements: map[int64]skeletonStatementTotal{
		1: {query: "select every reading", calls: 100, ms: 1_000},
		2: {query: "select some readings", calls: 4, ms: 40},
		3: {query: "select no reading", calls: 9, ms: 90},
		4: {query: "select evicted and written again", calls: 50, ms: 500},
	}}
	after := skeletonSnapshot{at: at.Add(3 * time.Second), evicted: 3, statements: map[int64]skeletonStatementTotal{
		1: {query: "select every reading", calls: 160, ms: 1_060},
		2: {query: "select some readings", calls: 11, ms: 61},
		3: {query: "select no reading", calls: 9, ms: 90},
		4: {query: "select evicted and written again", calls: 3, ms: 6},
		5: {query: "select first seen", calls: 2, ms: 3},
	}}
	spent := skeletonDatabaseTimeOf(before, after, 30)
	for _, held := range []struct {
		what      string
		got, want float64
	}{
		{"database time per reading (ms)", spent.ms, 3},
		{"statements per reading (calls)", spent.calls, 2.4},
		{"time per reading of the statements that ran at least once per reading (onceMS)", spent.onceMS, 2},
		{"time per reading of the statements that ran fewer times (otherMS)", spent.otherMS, 1},
		{"statements that ran fewer times (others)", float64(spent.others), 3},
		{"entries evicted (evicted)", float64(spent.evicted), 1},
	} {
		if math.Abs(held.got-held.want) > 1e-9 {
			t.Errorf("c-tree-read-bound: skeletonDatabaseTimeOf answers %s %v, want %v", held.what, held.got, held.want)
		}
	}
	if spent.length != 3*time.Second {
		t.Errorf("c-tree-read-bound: skeletonDatabaseTimeOf answers an interval of %s, want 3s", spent.length)
	}
	if _, held := spent.byText[skeletonNormalized("select no reading")]; held {
		t.Errorf("c-tree-read-bound: skeletonDatabaseTimeOf counts the calls per reading of a statement that did not run: %v", spent.byText)
	}
}
