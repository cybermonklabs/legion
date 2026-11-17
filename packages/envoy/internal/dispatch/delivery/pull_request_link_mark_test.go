package delivery

// A write of an issue's links marks the stored head row of each pull request it links or unlinks
// (MarkHeadsLinked), and the reconcile's next pass judges each marked row again: a link to a
// pull request's own URL is an input of its attribution that no event of the pull request
// carries.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

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
// a test can act between a head write's read of the links (its attribution) and its write.
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

// A link that is written while a head write of its pull request is in flight is followed by the
// reconcile's next pass, for each of the three writers of a head row. The pull request is merged,
// and its branch, title and body name no issue. The writer reads GitHub and judges the row's issue
// from the links; then, before its write lands, the issue's link to the pull request is added (or
// removed, where the row was attributed by that link) and the row is marked; then the write lands.
// The write judged from links that are no longer the issue's links, so the row must stay unread,
// and one pass must attribute it with the links as they stand.
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
			_, err := RefreshPullRequestHead(ctx, pool, client, nil, nil, repo, number, time.Now())
			return err
		}},
		{name: "the_read_a_result_starts", commits: true, write: func(ctx context.Context, pool *store.Pool, client *githubapp.Client, _ int, head string, _ time.Time) error {
			_, _, err := ReadThroughCommitPulls(ctx, pool, client, nil, repo, head, time.Now())
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
				if _, err := RefreshPullRequestHead(ctx, pool, client, nil, nil, repo, number, time.Now()); err != nil {
					t.Fatalf("store #%d: %v", number, err)
				}
				if listed, err := HeadAttributedTo(ctx, pool, issue, head); err != nil || listed == added {
					t.Fatalf("before the link's write, %s lists #%d = %v, %v; want %v", issue, number, listed, err, !added)
				}

				hold := newHoldHeadWrite()
				traced := storetest.Traced(t, pool, hold, nil)
				done := make(chan error, 1)
				go func() { done <- writer.write(ctx, traced, client, number, head, second) }()
				waitOn(t, hold.reached, "the head write's statement, after its attribution read the links")
				writeLink(t, ctx, pool, issue, url, added)
				close(hold.release)
				if err := <-done; err != nil {
					t.Fatalf("the head write: %v", err)
				}
				written := readHead(t, ctx, pool, repo, number)
				NewReconcile(pool, client, events.NewBroker()).runOnce(ctx)
				listed, err := HeadAttributedTo(ctx, pool, issue, head)
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("writer=%s link_added=%v checked_at_after_the_write=%v listed_after_one_pass=%v", writer.name, added, written.CheckedAt, listed)
				if written.CheckedAt != nil {
					t.Errorf("the head write that judged #%d from the links before the link's write stamped the row read at %s: the mark is lost, and a merged row stamped by the current rule is read by no pass",
						number, written.CheckedAt.Format(time.RFC3339Nano))
				}
				if listed != added {
					t.Errorf("after one pass %s lists #%d = %v, want %v: the link written while the head write was in flight is not followed", issue, number, listed, added)
				}
			})
		}
	}
}

// A marked row is read by the pass that reaches it in the first list's order: the unread rows
// first (checked_at null), each by (repository, number), then the open rows read longest ago
// (ListStalePullRequestHeads), PullRequestHeadBatch rows a pass. With 200 open rows due that a
// read has reached, the marked merged row is read by the next pass. With 200 open rows no read has
// reached, all of them before it by number, the next pass reads those 200 and the pass after it
// the marked row: a marked row that k unread rows stand before is read by pass ⌈(k + 1) / 200⌉,
// where each pass reads its rows.
func TestAMarkedMergedRowIsReadByThePassThatReachesItInTheListsOrder(t *testing.T) {
	const repo = "acme/widgets"
	for _, kase := range []struct {
		name string
		// unread is whether the 200 open rows are rows no read has reached.
		unread bool
		passes int
	}{
		{"200_open_rows_due_that_a_read_has_reached", false, 1},
		{"200_open_rows_no_read_has_reached_before_it", true, 2},
	} {
		t.Run(kase.name, func(t *testing.T) {
			fastPageRetries(t)
			pool, ctx := deliveryTestPool(t)
			const number = 9500
			issue := seedHeadIssue(t, ctx, pool, number)
			head := fakeSHA("f", number)
			second := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
			fake := newFakeGitHub(t)
			github := serveGitHubPulls(t, fake, repo)
			github.set(repo, number, "closed", head, "feature/no-key", "", second)
			github.merge(number)
			for open := 1; open <= PullRequestHeadBatch; open++ {
				github.set(repo, open, "open", fakeSHA("o", open), "feature/open", "", second)
			}
			client := fake.newTestClient()
			if _, err := RefreshPullRequestHead(ctx, pool, client, nil, nil, repo, number, time.Now()); err != nil {
				t.Fatal(err)
			}
			checked := any(time.Now().Add(-2 * PullRequestHeadStaleAfter))
			if kase.unread {
				checked = nil
			}
			if _, err := pool.Exec(ctx, `
				insert into pull_request_heads (repo, number, head_sha, head_ref, base_ref, state, url, event_at, checked_at, attribution_source)
				select $1, n, 'o' || n || repeat('0', 39 - length(n::text)), 'feature/open', 'main', 'open',
					'https://github.com/' || $1 || '/pull/' || n, $2, $3, 'none'
				from generate_series(1, $4::int) n
			`, repo, second, checked, PullRequestHeadBatch); err != nil {
				t.Fatalf("store the open rows: %v", err)
			}
			writeLink(t, ctx, pool, issue, fmt.Sprintf("https://github.com/%s/pull/%d", repo, number), true)
			reconcile := NewReconcile(pool, client, events.NewBroker())
			read := 0
			for pass := 1; pass <= 3 && read == 0; pass++ {
				reconcile.runOnce(ctx)
				listed, err := HeadAttributedTo(ctx, pool, issue, head)
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("pass %d: %s lists #%d = %v", pass, issue, number, listed)
				if listed {
					read = pass
				}
			}
			if read != kase.passes {
				t.Errorf("the marked row was attributed by pass %d, want pass %d", read, kase.passes)
			}
		})
	}
}

// A write of an issue's links and a head write that moves the row of a pull request it links
// take their rows in one order: the issue's row, then the head row. The write of the links holds
// the issue's row, as api's patchIssue does from its first lock to its commit, while the reconcile
// reads GitHub's answer, which now names that issue, and writes the row, which moves the row to
// the issue and so appends an event to it. Then the write of the links marks the head row. The
// head write takes the issue rows its move can append to before the head row, so it waits for the
// write of the links, which then commits, and the head write lands after it; in the other order
// each would wait for the other's row, and PostgreSQL would end one of them with a deadlock
// (SQLSTATE 40P01).
func TestALinkWriteAndAHeadWriteThatMovesTheRowTakeTheirRowsInOneOrder(t *testing.T) {
	const repo = "acme/widgets"
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	const number = 9421
	issue := seedHeadIssue(t, ctx, pool, number)
	head := fakeSHA("d", number)
	second := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	url := fmt.Sprintf("https://github.com/%s/pull/%d", repo, number)
	fake := newFakeGitHub(t)
	github := serveGitHubPulls(t, fake, repo)
	github.set(repo, number, "closed", head, "feature/no-key", "", second)
	github.merge(number)
	client := fake.newTestClient()
	if _, err := RefreshPullRequestHead(ctx, pool, client, nil, nil, repo, number, time.Now()); err != nil {
		t.Fatal(err)
	}
	// GitHub's answer now names the issue, so the next head write moves the row to it.
	github.mu.Lock()
	github.pulls[number]["body"] = "Dispatch: " + issue
	github.mu.Unlock()

	links, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = links.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := links.Exec(ctx, `select 1 from issues where key = $1 for no key update`, issue); err != nil {
		t.Fatal(err)
	}
	if _, err := links.Exec(ctx, `insert into issue_external_links (issue_key, url, kind) values ($1, $2, 'url')`, issue, url); err != nil {
		t.Fatal(err)
	}
	written := make(chan error, 1)
	go func() {
		_, err := RefreshPullRequestHead(ctx, pool, client, events.NewBroker(), nil, repo, number, time.Now())
		written <- err
	}()
	// The head write is waiting on a lock the write of the links holds.
	waitFor(t, time.Minute, func() bool {
		var waiting int
		if err := pool.QueryRow(ctx, `
			select count(*) from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock' and pid <> pg_backend_pid()
		`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		return waiting > 0
	}, func() string { return "the head write never waited on a lock of the write of the links" })
	marked := make(chan error, 1)
	go func() {
		err := MarkHeadsLinked(ctx, links, []string{url})
		if err == nil {
			err = links.Commit(ctx)
		}
		marked <- err
	}()
	var markErr, writeErr error
	select {
	case markErr = <-marked:
	case <-time.After(time.Minute):
		t.Fatal("the mark of the write of the links did not end in a minute")
	}
	select {
	case writeErr = <-written:
	case <-time.After(time.Minute):
		t.Fatal("the head write did not end in a minute")
	}
	stored := readHead(t, ctx, pool, repo, number)
	t.Logf("mark_error=%v head_write_error=%v issue_after=%v checked_at_after=%v", markErr, writeErr, stored.IssueKey, stored.CheckedAt)
	for what, err := range map[string]error{"the write of the links": markErr, "the head write": writeErr} {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "40P01" {
			t.Errorf("%s ended with a deadlock: %v", what, err)
		} else if err != nil {
			t.Errorf("%s: %v", what, err)
		}
	}
	if stored.IssueKey == nil || *stored.IssueKey != issue {
		t.Errorf("#%d is attributed to %v after both writes, want %s", number, stored.IssueKey, issue)
	}
}
