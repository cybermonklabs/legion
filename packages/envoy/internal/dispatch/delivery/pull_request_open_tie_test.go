package delivery

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// An open row is left to the reconcile, whatever issue it credits (HeadStoredAt), and its issue is
// the statement's at each read, from the facts the row's last read stored, with Dispatch's links,
// issues, parents and mappings as they stand then. So a tie to an issue made after the open row's
// last event is followed at once, with no event and no results read, for each of the four ties no
// event of the pull request carries:
//
//   - a link of the issue to a GitHub issue the pull request's body cites (rank 2);
//   - a parent, with a repository of its own, given to the issue the title names, which before it
//     resolved to another repository;
//   - a rename of the repository the issue's project maps, to the repository the pull request is
//     in;
//   - a cited GitHub issue's link moved from one issue to another, with the row credited to the
//     first.
//
// The row is read 6 minutes after its last read, past PullRequestHeadStaleAfter, as the pass
// finds a row whose event came 6 minutes before. A pass takes PullRequestHeadBatch rows, the due
// open rows read longest ago first (ListStalePullRequestHeads), so where 200 open rows due since
// 10 minutes stand before it, the next pass does not read it. The issue lists it after the first
// pass all the same: the statement needs no read of the row.
func TestATieMadeAfterAnOpenRowsLastEventIsFollowedByThePassThatReachesIt(t *testing.T) {
	const repo = "acme/widgets"
	const number = 9431
	key := func(project string, n int) string { return project + "-" + strconv.Itoa(n) }
	for _, kase := range []struct {
		name string
		// title and body are GitHub's answer for the pull request; before makes the state its
		// first read meets, and tie makes the tie and answers the issue the row must credit after
		// the pass that reaches it. due is how many open rows read longer ago are due before it.
		title, body string
		due         int
		before      func(t *testing.T, ctx context.Context, pool *store.Pool)
		tie         func(t *testing.T, ctx context.Context, pool *store.Pool) string
	}{
		{
			name: "a_link_to_a_github_issue_its_body_cites", title: "A change", body: "Fixes #78.",
			tie: func(t *testing.T, ctx context.Context, pool *store.Pool) string {
				issue := seedHeadIssue(t, ctx, pool, number)
				mustExec(t, ctx, pool, `insert into issue_external_links (issue_key, url, kind) values ($1, 'https://github.com/acme/widgets/issues/78', 'github_issue')`, issue)
				return issue
			},
		},
		{
			name: "a_link_to_a_github_issue_its_body_cites_behind_200_due_open_rows", title: "A change", body: "Fixes #78.",
			due: PullRequestHeadBatch,
			tie: func(t *testing.T, ctx context.Context, pool *store.Pool) string {
				issue := seedHeadIssue(t, ctx, pool, number)
				mustExec(t, ctx, pool, `insert into issue_external_links (issue_key, url, kind) values ($1, 'https://github.com/acme/widgets/issues/78', 'github_issue')`, issue)
				return issue
			},
		},
		{
			name: "a_parent_with_the_repository_of_the_pull_request", title: key("GDGT", number) + " the work",
			before: func(t *testing.T, ctx context.Context, pool *store.Pool) {
				seedGadgetsIssue(t, ctx, pool, number)
				// The parent names the pull request's repository as its own, which its project maps.
				parent := seedHeadIssue(t, ctx, pool, number+1)
				mustExec(t, ctx, pool, `update issues set repo = 'acme/widgets' where key = $1`, parent)
			},
			tie: func(t *testing.T, ctx context.Context, pool *store.Pool) string {
				mustExec(t, ctx, pool, `update issues set parent_key = $2 where key = $1`, key("GDGT", number), key("HEAD", number+1))
				return key("GDGT", number)
			},
		},
		{
			name: "a_rename_of_the_repository_the_issues_project_maps", title: key("MAPX", number) + " the work",
			before: func(t *testing.T, ctx context.Context, pool *store.Pool) {
				seedProjectRepository(t, ctx, pool, "MAPX", "Mapped", "oldco/widgets")
				mustExec(t, ctx, pool, `
					insert into issues (key, project_key, number, title, status, created_by, rank)
					values ($1, 'MAPX', $2, $1, 'todo', '{"kind":"system","id":"test"}'::jsonb, $1)
				`, key("MAPX", number), number)
			},
			tie: func(t *testing.T, ctx context.Context, pool *store.Pool) string {
				mustExec(t, ctx, pool, `update repo_projects set renamed_to = 'acme/widgets' where repo = 'oldco/widgets'`)
				return key("MAPX", number)
			},
		},
		{
			name: "a_cited_github_issues_link_moved_to_another_issue", title: "A change", body: "Fixes #79.",
			before: func(t *testing.T, ctx context.Context, pool *store.Pool) {
				first := seedHeadIssue(t, ctx, pool, number+1)
				mustExec(t, ctx, pool, `insert into issue_external_links (issue_key, url, kind) values ($1, 'https://github.com/acme/widgets/issues/79', 'github_issue')`, first)
			},
			tie: func(t *testing.T, ctx context.Context, pool *store.Pool) string {
				issue := seedHeadIssue(t, ctx, pool, number)
				mustExec(t, ctx, pool, `update issue_external_links set issue_key = $1 where url = 'https://github.com/acme/widgets/issues/79'`, issue)
				return issue
			},
		},
	} {
		t.Run(kase.name, func(t *testing.T) {
			fastPageRetries(t)
			pool, ctx := deliveryTestPool(t)
			seedDeliveryProject(t, ctx, pool)
			if kase.before != nil {
				kase.before(t, ctx, pool)
			}
			head := fakeSHA("c", number)
			second := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
			fake := newFakeGitHub(t)
			github := serveGitHubPulls(t, fake, repo)
			github.set(repo, number, "open", head, "feature/no-key", kase.body, second)
			github.mu.Lock()
			github.pulls[number]["title"] = kase.title
			github.mu.Unlock()
			client := fake.newTestClient()
			if _, _, err := RefreshPullRequestHead(ctx, pool, client, nil, repo, number, time.Now()); err != nil {
				t.Fatal(err)
			}
			before := readHead(t, ctx, pool, repo, number)
			issue := kase.tie(t, ctx, pool)
			if before.IssueKey != nil && *before.IssueKey == issue {
				t.Fatalf("the row credited %s before the tie: the case did not happen", issue)
			}
			mustExec(t, ctx, pool, `update pull_request_heads set checked_at = now() - interval '6 minutes' where repo = $1 and number = $2`, repo, number)
			for open := 1; open <= kase.due; open++ {
				github.set(repo, open, "open", fakeSHA("o", open), "feature/open", "", second)
			}
			if kase.due > 0 {
				mustExec(t, ctx, pool, `
					insert into pull_request_heads (repo, number, head_sha, head_ref, base_ref, state, url, event_at, checked_at, attribution_source)
					select $1, n, 'o' || n || repeat('0', 39 - length(n::text)), 'feature/open', 'main', 'open',
						'https://github.com/' || $1 || '/pull/' || n, $2, now() - interval '10 minutes', 'none'
					from generate_series(1, $3::int) n
				`, repo, second, kase.due)
			}
			want := 1
			reconcile := NewReconcile(pool, client, events.NewBroker())
			reached := 0
			for pass := 1; pass <= want+1 && reached == 0; pass++ {
				reads := github.reads.Load()
				reconcile.runOnce(ctx)
				listed, err := HeadAttributedTo(ctx, pool, issue, head)
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("tie=%s credited_before=%v due_before_it=%d pass=%d listed=%v pass_reads=%d", kase.name, before.IssueKey, kase.due, pass, listed, github.reads.Load()-reads)
				if listed {
					reached = pass
				}
			}
			if reached != want {
				t.Errorf("%s lists #%d at %.12s after pass %d, want pass %d, the pass that reaches the row with %d due open rows read longer ago before it: the tie made after the open row's last event is not followed by that pass",
					issue, number, head, reached, want, kase.due)
			}
		})
	}
}

// mustExec runs one statement of a test's setup and fails the test on its error.
func mustExec(t *testing.T, ctx context.Context, pool *store.Pool, statement string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, statement, args...); err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
}
