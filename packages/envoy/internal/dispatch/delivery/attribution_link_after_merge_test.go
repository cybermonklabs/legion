package delivery

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
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
	if _, _, err := RefreshPullRequestHead(ctx, pool, client, nil, "acme/widgets", number, time.Now()); err != nil {
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

// dispatchChange is one change in Dispatch that gives a stored pull request another issue, with
// no event of the pull request. The pull request is merged, and its title, body and branch name
// no held issue before the change.
type dispatchChange struct {
	name, repo, branch, body string
	number                   int
	issue                    string // the issue that the change gives the pull request
	setup, change, reverse   func(t *testing.T, ctx context.Context, pool *store.Pool)
}

// dispatchChanges are the changes of attribution_dispatch_change_test.go. The first three and
// the moved link are on acme/widgets, the new parent on acme/prnt-side, and the mapping on
// acme/mapped.
func dispatchChanges() []dispatchChange {
	link := func(number int) string { return fmt.Sprintf("https://github.com/acme/widgets/pull/%d", number) }
	return []dispatchChange{
		{
			name: "pull_request_link", repo: "acme/widgets", number: 311, branch: "test/no-key", body: "No key here.", issue: "HEAD-9311",
			setup: func(t *testing.T, ctx context.Context, pool *store.Pool) { seedHeadIssue(t, ctx, pool, 9311) },
			change: func(t *testing.T, ctx context.Context, pool *store.Pool) {
				changeIssueLink(t, ctx, pool, "HEAD-9311", link(311), "url", true)
			},
			reverse: func(t *testing.T, ctx context.Context, pool *store.Pool) {
				changeIssueLink(t, ctx, pool, "HEAD-9311", link(311), "url", false)
			},
		},
		{
			name: "github_issue_link", repo: "acme/widgets", number: 312, branch: "test/no-key", body: "Closes https://github.com/acme/widgets/issues/77.", issue: "HEAD-9312",
			setup: func(t *testing.T, ctx context.Context, pool *store.Pool) { seedHeadIssue(t, ctx, pool, 9312) },
			change: func(t *testing.T, ctx context.Context, pool *store.Pool) {
				changeIssueLink(t, ctx, pool, "HEAD-9312", "https://github.com/acme/widgets/issues/77", "github_issue", true)
			},
			reverse: func(t *testing.T, ctx context.Context, pool *store.Pool) {
				changeIssueLink(t, ctx, pool, "HEAD-9312", "https://github.com/acme/widgets/issues/77", "github_issue", false)
			},
		},
		{
			name: "issue_created", repo: "acme/widgets", number: 313, branch: "legion/HEAD-9313", body: "No key here.", issue: "HEAD-9313",
			setup:  func(t *testing.T, ctx context.Context, pool *store.Pool) { seedDeliveryProject(t, ctx, pool) },
			change: func(t *testing.T, ctx context.Context, pool *store.Pool) { seedHeadIssue(t, ctx, pool, 9313) },
			// No route deletes an issue. So the reverse change is the state before, which SQL makes.
			reverse: func(t *testing.T, ctx context.Context, pool *store.Pool) {
				execDispatchChange(t, ctx, pool, "delete HEAD-9313", `delete from issues where key = 'HEAD-9313'`)
			},
		},
		{
			name: "new_parent", repo: "acme/prnt-side", number: 314, branch: "legion/PRNT-314", body: "No key here.", issue: "PRNT-314",
			setup: func(t *testing.T, ctx context.Context, pool *store.Pool) {
				seedProjectRepository(t, ctx, pool, "PRNT", "PRNT", "acme/prnt-main")
				mapRepository(t, ctx, pool, "acme/prnt-side", "PRNT", false, nil)
				insertProjectIssue(t, ctx, pool, "PRNT", "PRNT-300", 300)
				insertProjectIssue(t, ctx, pool, "PRNT", "PRNT-314", 314)
				execDispatchChange(t, ctx, pool, "give PRNT-300 its own repository", `update issues set repo = 'acme/prnt-side' where key = 'PRNT-300'`)
			},
			change: func(t *testing.T, ctx context.Context, pool *store.Pool) {
				execDispatchChange(t, ctx, pool, "move PRNT-314 under PRNT-300", `update issues set parent_key = 'PRNT-300' where key = 'PRNT-314'`)
			},
			reverse: func(t *testing.T, ctx context.Context, pool *store.Pool) {
				execDispatchChange(t, ctx, pool, "move PRNT-314 back", `update issues set parent_key = null where key = 'PRNT-314'`)
			},
		},
		{
			name: "repository_mapping", repo: "acme/mapped", number: 315, branch: "legion/MAPD-315", body: "No key here.", issue: "MAPD-315",
			setup: func(t *testing.T, ctx context.Context, pool *store.Pool) {
				seedProjectRepository(t, ctx, pool, "MAPD", "MAPD", "acme/elsewhere")
				insertProjectIssue(t, ctx, pool, "MAPD", "MAPD-315", 315)
			},
			change: func(t *testing.T, ctx context.Context, pool *store.Pool) {
				mapRepository(t, ctx, pool, "acme/mapped", "MAPD", true, nil)
			},
			reverse: func(t *testing.T, ctx context.Context, pool *store.Pool) {
				execDispatchChange(t, ctx, pool, "delete the mapping of acme/mapped", `delete from repo_projects where repo = 'acme/mapped'`)
				mapRepository(t, ctx, pool, "acme/elsewhere", "MAPD", true, nil)
			},
		},
		{
			name: "link_moved", repo: "acme/widgets", number: 316, branch: "test/no-key", body: "No key here.", issue: "HEAD-9317",
			setup: func(t *testing.T, ctx context.Context, pool *store.Pool) {
				seedHeadIssue(t, ctx, pool, 9316)
				seedHeadIssue(t, ctx, pool, 9317)
				changeIssueLink(t, ctx, pool, "HEAD-9316", link(316), "url", true)
			},
			change: func(t *testing.T, ctx context.Context, pool *store.Pool) {
				changeIssueLink(t, ctx, pool, "HEAD-9316", link(316), "url", false)
				changeIssueLink(t, ctx, pool, "HEAD-9317", link(316), "url", true)
			},
			reverse: func(t *testing.T, ctx context.Context, pool *store.Pool) {
				changeIssueLink(t, ctx, pool, "HEAD-9317", link(316), "url", false)
				changeIssueLink(t, ctx, pool, "HEAD-9316", link(316), "url", true)
			},
		},
	}
}

// Each change of attribution_dispatch_change_test.go, on a merged row that the reconcile's own
// writer stores from GitHub's answer. After the change and one pass, the issue lists the pull
// request, and the pass reads nothing of it from GitHub. After the reverse change and one pass,
// the issue lists nothing. Control in each subtest: before the change, the issue lists nothing.
func TestAChangeInDispatchReachesAMergedRowWithinOnePass(t *testing.T) {
	for _, c := range dispatchChanges() {
		t.Run(c.name, func(t *testing.T) {
			fastPageRetries(t)
			pool, ctx := deliveryTestPool(t)
			c.setup(t, ctx, pool)
			fake := newFakeGitHub(t)
			github := serveGitHubPulls(t, fake, c.repo)
			head := fmt.Sprintf("%040d", c.number)
			github.set(c.repo, c.number, "closed", head, c.branch, c.body, time.Now().Add(-time.Hour).UTC().Truncate(time.Second))
			github.mu.Lock()
			github.pulls[c.number]["merged"] = true
			github.mu.Unlock()
			client := fake.newTestClient()
			// A writer of GitHub's answer stores the row, never SQL, as in the test above.
			if _, _, err := RefreshPullRequestHead(ctx, pool, client, nil, c.repo, c.number, time.Now()); err != nil {
				t.Fatalf("write #%d through the refresh: %v", c.number, err)
			}
			listed := func() []int {
				t.Helper()
				heads, err := HeadsAttributedTo(ctx, pool, c.issue)
				if err != nil {
					t.Fatalf("HeadsAttributedTo(%s): %v", c.issue, err)
				}
				numbers := []int{}
				for _, attributed := range heads {
					numbers = append(numbers, attributed.Number)
				}
				return numbers
			}
			pass := func() (int32, int32) {
				reads, commitReads := github.reads.Load(), github.commitReads.Load()
				NewReconcile(pool, client, nil).runOnce(ctx)
				return github.reads.Load() - reads, github.commitReads.Load() - commitReads
			}
			if before := listed(); len(before) != 0 {
				t.Fatalf("%s lists %v before the change, want nothing (the control)", c.issue, before)
			}

			c.change(t, ctx, pool)
			reads, commitReads := pass()
			after := listed()
			t.Logf("after the change and one pass %s lists %v; the pass read #%d %d times and its commits %d times", c.issue, after, c.number, reads, commitReads)
			if fmt.Sprint(after) != fmt.Sprint([]int{c.number}) {
				t.Errorf("%s after the change and one pass lists %v, want [%d]: a change in Dispatch does not reach a merged row", c.issue, after, c.number)
			}
			if reads != 0 || commitReads != 0 {
				t.Errorf("the pass after the change read #%d %d times and its commits %d times, want 0: a change in Dispatch needs no read of GitHub", c.number, reads, commitReads)
			}

			c.reverse(t, ctx, pool)
			reads, commitReads = pass()
			back := listed()
			t.Logf("after the reverse change and one pass %s lists %v; the pass read #%d %d times and its commits %d times", c.issue, back, c.number, reads, commitReads)
			if len(back) != 0 {
				t.Errorf("%s after the reverse change and one pass lists %v, want nothing", c.issue, back)
			}
			if reads != 0 || commitReads != 0 {
				t.Errorf("the pass after the reverse change read #%d %d times and its commits %d times, want 0", c.number, reads, commitReads)
			}
		})
	}
}
