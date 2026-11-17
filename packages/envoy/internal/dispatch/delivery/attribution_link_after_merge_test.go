package delivery

import (
	"fmt"
	"testing"
	"time"
)

// A merged pull request whose branch, title and body name no issue is stored credited to
// nobody, through the reconcile's own writer. Then an issue links it. The rule ranks a link
// first, so within one pass the results read compares the issue's checks with the pull
// request's head, and that needs no read of GitHub: the link is a fact of Dispatch, and the pull
// request's own facts are stored. Control in the same test: before the link, the issue lists
// nothing.
func TestALinkAfterAMergedRowReachesItWithinOnePass(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	issue := seedHeadIssue(t, ctx, pool, 9303)
	fake := newFakeGitHub(t)
	github := serveGitHubPulls(t, fake, "acme/widgets")
	const number, head = 303, "4303430343034303430343034303430343034303"
	github.set("acme/widgets", number, "closed", head, "test/no-key", "No key here.", time.Now().Add(-time.Hour).UTC().Truncate(time.Second))
	github.mu.Lock()
	github.pulls[number]["merged"] = true
	github.mu.Unlock()
	client := fake.newTestClient()

	// The row is written by a writer of GitHub's answer, never by SQL: a row written by SQL
	// holds no facts of a read, so no rule over stored facts could give it to the link.
	if _, err := RefreshPullRequestHead(ctx, pool, client, nil, nil, "acme/widgets", number, time.Now()); err != nil {
		t.Fatalf("write #%d through the refresh: %v", number, err)
	}
	listed := func() []int {
		t.Helper()
		heads, err := HeadsAttributedTo(ctx, pool, issue)
		if err != nil {
			t.Fatalf("HeadsAttributedTo(%s): %v", issue, err)
		}
		numbers := []int{}
		for _, attributed := range heads {
			numbers = append(numbers, attributed.Number)
		}
		return numbers
	}
	if before := listed(); len(before) != 0 {
		t.Fatalf("%s lists %v before any link, want nothing (the control)", issue, before)
	}

	if _, err := pool.Exec(ctx, `insert into issue_external_links (issue_key, url, kind) values ($1, $2, 'url')`,
		issue, fmt.Sprintf("https://github.com/acme/widgets/pull/%d", number)); err != nil {
		t.Fatalf("link #%d to %s: %v", number, issue, err)
	}
	reads, commitReads := github.reads.Load(), github.commitReads.Load()
	NewReconcile(pool, client, nil).runOnce(ctx)
	reads, commitReads = github.reads.Load()-reads, github.commitReads.Load()-commitReads
	after := listed()
	t.Logf("after the link and one pass %s lists %v; the pass read #%d %d times and its commits %d times", issue, after, number, reads, commitReads)
	if fmt.Sprint(after) != fmt.Sprint([]int{number}) {
		t.Errorf("%s after the link and one pass lists %v, want [%d]: a new link does not reach a merged row", issue, after, number)
	}
	if reads != 0 || commitReads != 0 {
		t.Errorf("the pass read #%d %d times and its commits %d times, want 0: a new link needs no read of GitHub", number, reads, commitReads)
	}
}
