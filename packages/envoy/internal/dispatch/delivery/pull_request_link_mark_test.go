package delivery

// A write of an issue's links reaches the readers of each pull request it links or unlinks at
// the read: the statement reads the links as they stand, so no write of a head row judges an issue
// from links that another write changed.

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// writeLink does to one link of issueKey what the issue routes do (api's patchIssue): it locks
// the issue's row and adds the link or deletes it, in one transaction. It writes no head row: the
// statement reads the links at each read.
func writeLink(t *testing.T, ctx context.Context, pool *store.Pool, issueKey, url string, add bool) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `select 1 from issues where key = $1 for no key update`, issueKey); err != nil {
		t.Fatalf("lock %s: %v", issueKey, err)
	}
	statement := `delete from issue_external_links where issue_key = $1 and url = $2`
	if add {
		statement = `insert into issue_external_links (issue_key, url, kind) values ($1, $2, 'url')`
	}
	if _, err := tx.Exec(ctx, statement, issueKey, url); err != nil {
		t.Fatalf("write the link of %s to %s: %v", issueKey, url, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the link of %s to %s: %v", issueKey, url, err)
	}
}

// A link that is written while a head write of its pull request is in flight is followed at the
// next read, for each of the three writers of a head row, both where the write is the pull
// request's first (no row is stored before it) and where a row is stored. The pull request is
// merged, and its branch, title and body name no issue. The writer reads GitHub; then it is held
// inside its transaction, at the insert of the head row or at the insert of its texts, and the
// issue's link to the pull request is added (or removed, where the link gave the pull request its
// issue); then the write lands. The readers must answer with the links as they stand, with no
// pass, after one pass and after two.
func TestALinkWrittenWhileAHeadWriteIsInFlightIsFollowedByTheNextPass(t *testing.T) {
	const repo = "acme/widgets"
	writers := []struct {
		name string
		// commits is whether the writer reads GET /repos/{o}/{r}/commits/{sha}/pulls.
		commits bool
		write   func(ctx context.Context, pool *store.Pool, client *githubapp.Client, number int, head string, second time.Time) error
	}{
		{name: "the_intake", write: func(ctx context.Context, pool *store.Pool, client *githubapp.Client, number int, head string, second time.Time) error {
			envelope := prEnvelope(repo, number, "closed", head, "feature/no-key", "", second)
			envelope["merged"] = "true"
			_, err := newIntake(tuningFor(t), nil, pool, nil, client, nil, nil, unrenamed()).handlePullRequestEnvelope(ctx, nil, envelope)
			return err
		}},
		{name: "the_reconcile", write: func(ctx context.Context, pool *store.Pool, client *githubapp.Client, number int, _ string, _ time.Time) error {
			_, _, err := unrenamed().RefreshPullRequestHead(ctx, pool, client, nil, repo, number, time.Now())
			return err
		}},
		{name: "the_read_a_result_starts", commits: true, write: func(ctx context.Context, pool *store.Pool, client *githubapp.Client, _ int, head string, _ time.Time) error {
			_, _, err := unrenamed().ReadThroughCommitPulls(ctx, pool, client, repo, head, time.Now())
			return err
		}},
	}
	holds := []struct{ name, statement string }{
		{name: "held_at_the_head_row", statement: "insert into pull_request_heads"},
		{name: "held_at_the_texts", statement: "insert into pull_request_texts"},
	}
	for _, stored := range []bool{false, true} {
		for _, writer := range writers {
			for _, hold := range holds {
				for _, added := range []bool{true, false} {
					name := fmt.Sprintf("%s/%s/%s/%s", map[bool]string{false: "first_write", true: "row_stored"}[stored],
						writer.name, hold.name, map[bool]string{true: "link_added", false: "link_removed"}[added])
					t.Run(name, func(t *testing.T) {
						fastPageRetries(t)
						pool, ctx := deliveryTestPool(t)
						const number = 9411
						issue := seedHeadIssue(t, ctx, pool, number)
						head := fakeSHA("e", number)
						second := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
						url := fmt.Sprintf("https://github.com/%s/pull/%d", repo, number)
						fake := newFakeGitHub(t)
						github := serveGitHubPulls(t, fake, repo)
						github.set(repo, number, "closed", head, "feature/no-key", "", second)
						github.merge(number)
						if writer.commits {
							fake.handle("GET /repos/"+repo+"/commits/{sha}/pulls", func(w http.ResponseWriter, _ *http.Request) {
								github.mu.Lock()
								answer := maps.Clone(github.pulls[number])
								github.mu.Unlock()
								mustEncode(t, w, []any{answer})
							})
						}
						client := fake.newTestClient()
						if !added {
							if _, err := pool.Exec(ctx, `insert into issue_external_links (issue_key, url, kind) values ($1, $2, 'url')`, issue, url); err != nil {
								t.Fatal(err)
							}
						}
						if stored {
							if _, _, err := unrenamed().RefreshPullRequestHead(ctx, pool, client, nil, repo, number, time.Now()); err != nil {
								t.Fatalf("store #%d: %v", number, err)
							}
							if listed, err := unrenamed().HeadAttributedTo(ctx, pool, issue, head); err != nil || listed == added {
								t.Fatalf("before the link's write, %s lists #%d = %v, %v; want %v", issue, number, listed, err, !added)
							}
						}
						rows := func() int {
							var rows int
							if err := pool.QueryRow(ctx, `select count(*) from pull_request_heads where repo = $1 and number = $2`, repo, number).Scan(&rows); err != nil {
								t.Fatal(err)
							}
							return rows
						}
						if before, want := rows(), map[bool]int{false: 0, true: 1}[stored]; before != want {
							t.Fatalf("before the head write #%d has %d head rows, want %d", number, before, want)
						}

						arrived, release := make(chan struct{}, 1), make(chan struct{})
						traced := storetest.Traced(t, pool, pauseTracer{match: hold.statement, arrived: arrived, release: release}, nil)
						done := make(chan error, 1)
						go func() { done <- writer.write(ctx, traced, client, number, head, second) }()
						waitOn(t, arrived, "the head write's "+hold.statement)
						writeLink(t, ctx, pool, issue, url, added)
						close(release)
						if err := <-done; err != nil {
							t.Fatalf("the head write: %v", err)
						}
						if after := rows(); after != 1 {
							t.Fatalf("after the head write #%d has %d head rows, want 1", number, after)
						}
						var listed [3]bool
						for pass := range listed {
							if pass > 0 {
								NewReconcile(pool, client, events.NewBroker(), unrenamed()).runOnce(ctx)
							}
							var err error
							if listed[pass], err = unrenamed().HeadAttributedTo(ctx, pool, issue, head); err != nil {
								t.Fatal(err)
							}
						}
						t.Logf("writer=%s row_stored=%v %s link_added=%v listed_with_no_pass=%v listed_after_one_pass=%v listed_after_two_passes=%v",
							writer.name, stored, hold.name, added, listed[0], listed[1], listed[2])
						for pass, when := range []string{"with no pass", "after one pass", "after two passes"} {
							if listed[pass] != added {
								t.Errorf("%s %s lists #%d = %v, want %v: the link written while the head write was in flight is not followed", when, issue, number, listed[pass], added)
							}
						}
					})
				}
			}
		}
	}
}
