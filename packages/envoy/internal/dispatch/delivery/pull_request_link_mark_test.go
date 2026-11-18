package delivery

// A write of an issue's links reaches the readers of each pull request it links or unlinks at
// the read: the statement reads the links as they stand, so no write of a head row judges an issue
// from links that another write changed.

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// writeLink does to one link of issueKey what the issue routes do (api's patchIssue): it locks
// the issue's row, adds the link or deletes it, and marks the head rows of the pull request the
// link names (MarkHeadsLinked), in one transaction.
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
	if err := MarkHeadsLinked(ctx, tx, []string{url}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the link of %s to %s: %v", issueKey, url, err)
	}
}

// holdHeadWrite holds the first statement that writes a head row (upsertHeadRow) once armed, so
// a test can act between a head write's read of GitHub and its write.
type holdHeadWrite struct {
	armed   atomic.Bool
	reached chan struct{}
	release chan struct{}
}

func newHoldHeadWrite() *holdHeadWrite {
	hold := &holdHeadWrite{reached: make(chan struct{}), release: make(chan struct{})}
	hold.armed.Store(true)
	return hold
}

func (h *holdHeadWrite) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "insert into pull_request_heads") && h.armed.CompareAndSwap(true, false) {
		close(h.reached)
		<-h.release
	}
	return ctx
}

func (*holdHeadWrite) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// A link that is written while a head write of its pull request is in flight is followed at the
// next read, for each of the three writers of a head row. The pull request is merged, and its
// branch, title and body name no issue. The writer reads GitHub; then, before its write lands, the
// issue's link to the pull request is added (or removed, where the link gave the row its issue);
// then the write lands. The readers must answer with the links as they stand, with no pass and
// after one pass.
func TestALinkWrittenWhileAHeadWriteIsInFlightIsFollowedByTheNextPass(t *testing.T) {
	const repo = "acme/widgets"
	for _, writer := range []struct {
		name string
		// commits is whether the writer reads GET /repos/{o}/{r}/commits/{sha}/pulls.
		commits bool
		write   func(ctx context.Context, pool *store.Pool, client *githubapp.Client, number int, head string, second time.Time) error
	}{
		{name: "the_intake", write: func(ctx context.Context, pool *store.Pool, client *githubapp.Client, number int, head string, second time.Time) error {
			envelope := prEnvelope(repo, number, "closed", head, "feature/no-key", "", second)
			envelope["merged"] = "true"
			_, err := newIntake(nil, pool, nil, client, nil, nil).handlePullRequestEnvelope(ctx, nil, envelope)
			return err
		}},
		{name: "the_reconcile", write: func(ctx context.Context, pool *store.Pool, client *githubapp.Client, number int, _ string, _ time.Time) error {
			_, _, err := RefreshPullRequestHead(ctx, pool, client, nil, repo, number, time.Now())
			return err
		}},
		{name: "the_read_a_result_starts", commits: true, write: func(ctx context.Context, pool *store.Pool, client *githubapp.Client, _ int, head string, _ time.Time) error {
			_, _, err := ReadThroughCommitPulls(ctx, pool, client, repo, head, time.Now())
			return err
		}},
	} {
		for _, added := range []bool{true, false} {
			t.Run(writer.name+map[bool]string{true: "_link_added", false: "_link_removed"}[added], func(t *testing.T) {
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
				if _, _, err := RefreshPullRequestHead(ctx, pool, client, nil, repo, number, time.Now()); err != nil {
					t.Fatalf("store #%d: %v", number, err)
				}
				if listed, err := HeadAttributedTo(ctx, pool, issue, head); err != nil || listed == added {
					t.Fatalf("before the link's write, %s lists #%d = %v, %v; want %v", issue, number, listed, err, !added)
				}

				hold := newHoldHeadWrite()
				traced := storetest.Traced(t, pool, hold, nil)
				done := make(chan error, 1)
				go func() { done <- writer.write(ctx, traced, client, number, head, second) }()
				waitOn(t, hold.reached, "the head write's statement")
				writeLink(t, ctx, pool, issue, url, added)
				close(hold.release)
				if err := <-done; err != nil {
					t.Fatalf("the head write: %v", err)
				}
				now, err := HeadAttributedTo(ctx, pool, issue, head)
				if err != nil {
					t.Fatal(err)
				}
				NewReconcile(pool, client, events.NewBroker()).runOnce(ctx)
				listed, err := HeadAttributedTo(ctx, pool, issue, head)
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("writer=%s link_added=%v listed_with_no_pass=%v listed_after_one_pass=%v", writer.name, added, now, listed)
				if now != added {
					t.Errorf("with no pass %s lists #%d = %v, want %v: the link written while the head write was in flight is not read", issue, number, now, added)
				}
				if listed != added {
					t.Errorf("after one pass %s lists #%d = %v, want %v: the link written while the head write was in flight is not followed", issue, number, listed, added)
				}
			})
		}
	}
}
