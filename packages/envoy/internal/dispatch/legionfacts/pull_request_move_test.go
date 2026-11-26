package legionfacts

import (
	"fmt"
	"testing"
	"time"
)

// TestAPullRequestMovedToAnotherIssueRewritesTheMergeOfTheIssueItLeft: the attribution re-resolves
// a stored pull request and rewrites its issue_key (delivery.AttributePullRequests), and the change
// that logs names the pull request and its merge times, not the issue it left. A stored row's
// merged_at is the earliest merge of its issue's pull requests (metrics), so the issue that held
// the moved one keeps a merge no pull request backs unless the pass names it too.
//
// Two merged pull requests move between closed issues in one pass: the earlier of one issue's
// two, to an issue whose own merged later, and the only one of another issue, to an issue with
// none. After the pass the first loser holds its other pull request's merge, the second none, and
// each gainer the merge of the pull request it took, each row as a computation of its issue
// answers it.
func TestAPullRequestMovedToAnotherIssueRewritesTheMergeOfTheIssueItLeft(t *testing.T) {
	run := newConsumer(t, 3)
	_, closed, _ := issueRows(t, run.history)
	kept, gained := run.fixture.Keys[1], run.fixture.Keys[2]
	emptied, filled := run.fixture.Keys[7], run.fixture.Keys[8]
	for _, key := range []string{kept, gained, emptied, filled} {
		if closed[key] == nil {
			t.Fatalf("%s is open, and this check moves pull requests between closed issues", key)
		}
	}
	const repo = "cybermonklabs/legion"
	merged := map[int]time.Time{}
	merge := func(number int, issue string, at time.Time) {
		t.Helper()
		merged[number] = at
		run.exec(t, fmt.Sprintf("seed %s#%d, merged, on %s", repo, number, issue), `
			insert into delivery_pull_requests (repo, number, title, url, author, created_at, merged_at, issue_key)
			values ($1, $2, 'A pull request', $3, 'alice', $4, $5, $6)
		`, repo, number, fmt.Sprintf("https://example.test/pull/%d", number), at.Add(-time.Hour), at, issue)
	}
	merge(1, kept, closed[kept].Add(-2*time.Hour))
	merge(2, kept, closed[kept].Add(-time.Hour))
	merge(3, gained, closed[gained].Add(-time.Hour))
	merge(4, emptied, closed[emptied].Add(-90*time.Minute))
	run.pass(t)
	before := run.rows(t)
	for key, want := range map[string]*time.Time{kept: new(merged[1]), gained: new(merged[3]), emptied: new(merged[4]), filled: nil} {
		if !sameTime(before[key].MergedAt, want) {
			t.Fatalf("%s's row holds the merge %v before any move, want %v: this test examined nothing", key, utc(before[key].MergedAt), utc(want))
		}
	}
	if !merged[1].Before(merged[3]) {
		t.Fatalf("%s#1 merged at %s, not before %s#3 at %s, so %s would not take its merge", repo, merged[1].UTC(), repo, merged[3].UTC(), gained)
	}

	run.at = run.at.Add(time.Minute)
	move := func(number int, to string) {
		t.Helper()
		run.exec(t, fmt.Sprintf("attribute %s#%d to %s", repo, number, to), `
			update delivery_pull_requests set issue_key = $3 where repo = $1 and number = $2
		`, repo, number, to)
	}
	move(1, gained)
	move(4, filled)
	changed := run.pass(t)
	after := run.rows(t)

	for key, want := range map[string]*time.Time{kept: new(merged[2]), gained: new(merged[1]), emptied: nil, filled: new(merged[4])} {
		if !sameTime(after[key].MergedAt, want) {
			t.Errorf("%s's row holds the merge %v after the moves, want %v", key, utc(after[key].MergedAt), utc(want))
		}
		if difference := rowDifference(after[key], computedPlacement(t, run, key)); difference != "" {
			t.Errorf("%s's stored row stands apart from its computation: %s", key, difference)
		}
	}
	t.Logf("%s#1 moved from %s to %s and %s#4 from %s to %s: the pass changed %v; merges %s %v to %v, %s %v to %v, %s %v to %v, %s %v to %v",
		repo, kept, gained, repo, emptied, filled, changed,
		kept, utc(before[kept].MergedAt), utc(after[kept].MergedAt), gained, utc(before[gained].MergedAt), utc(after[gained].MergedAt),
		emptied, utc(before[emptied].MergedAt), utc(after[emptied].MergedAt), filled, utc(before[filled].MergedAt), utc(after[filled].MergedAt))
}
