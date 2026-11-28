// pull_request_former_heads_test.go holds the re-key of a pull request stored under its
// repository's old name to the heads it left while open (pull_request_former_heads): whichever
// build deletes the old row, and whatever a writer of either row does meanwhile, the heads the
// pull request left reach the canonical row while it is open, and none reach it once it is closed.
package delivery

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// foldedFormerHeadsOf is the former heads of the pull request repo#number stands for, by commit,
// as the check states settler reads them: once the records the writes of pull_request_heads
// appended under any of its names are applied (pull_request_former_heads_fold, migration
// 20261126005348), which each settle of an issue runs first for the issue's open pull requests
// (pull_request_former_heads_drain). Every name of one pull request reads its one history.
func foldedFormerHeadsOf(t *testing.T, ctx context.Context, pool *store.Pool, repo string, number int) []string {
	t.Helper()
	if _, err := pool.Exec(ctx, `select pull_request_former_heads_fold($1, $2)`, repo, number); err != nil {
		t.Fatalf("apply the records of %s#%d: %v", repo, number, err)
	}
	rows, err := pool.Query(ctx, `
		select head_sha from pull_request_former_heads where pull = pull_request_former_heads_pull_of($1, $2) order by head_sha`, repo, number)
	if err != nil {
		t.Fatalf("read the former heads of %s#%d: %v", repo, number, err)
	}
	shas, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the former heads of %s#%d: %v", repo, number, err)
	}
	return shas
}

// reKeyRig is a pull request of gadgetsRepo that arrived under both names: stored first under
// gadgetsOldRepo, where it left the head left and stands at oldHead, then read through under
// gadgetsRepo, where GitHub answers it at head, attributed to issue.
type reKeyRig struct {
	pool                  *store.Pool
	ctx                   context.Context
	issue                 string
	number                int
	left, oldHead, head   string
	refresh               func() error
	formerHeadsOf         func(repo string) []string
	canonicalStateAndHead func() (string, string)
}

func newReKeyRig(t *testing.T, number int, left, oldHead, head string) *reKeyRig {
	t.Helper()
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	issue := seedGadgetsIssue(t, ctx, pool, number)
	seedRenamedGadgets(t, ctx, pool)
	attributor := NewAttributor(renamesOf(t, ctx, pool))
	earlier := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	stale := time.Now().Add(-10 * time.Minute)
	for index, sha := range []string{left, oldHead} {
		at := earlier.Add(time.Duration(index) * time.Minute)
		if err := attributor.UpsertPullRequestHead(ctx, pool, nil, PullRequestHead{
			Repo: gadgetsOldRepo, Number: number, HeadSHA: sha,
			HeadRef: "legion/work", BaseRef: "main", State: "open", EventAt: &at, CheckedAt: &stale,
		}); err != nil {
			t.Fatalf("store the head of #%d at %s under the old name: %v", number, sha, err)
		}
	}
	fake := gadgetsFake(t, number, issue+": the work", head)
	client := fake.newTestClient()
	if written, _, err := attributor.ReadThroughCommitPulls(ctx, pool, client, nil, gadgetsRepo, head, time.Now()); err != nil || written != 1 {
		t.Fatalf("read through %s at %s = %d, %v; want one row written", gadgetsRepo, head, written, err)
	}
	rig := &reKeyRig{pool: pool, ctx: ctx, issue: issue, number: number, left: left, oldHead: oldHead, head: head}
	rig.refresh = func() error {
		return attributor.RefreshPullRequestHead(context.Background(), pool, client, nil, gadgetsOldRepo, number, time.Now())
	}
	rig.formerHeadsOf = func(repo string) []string {
		t.Helper()
		return foldedFormerHeadsOf(t, ctx, pool, repo, number)
	}
	rig.canonicalStateAndHead = func() (string, string) {
		t.Helper()
		row := readHead(t, ctx, pool, gadgetsRepo, number)
		return row.State, row.HeadSHA
	}
	return rig
}

// refreshBehind runs the re-key of the old row while holder, a transaction of another writer, holds
// that row's lock, and answers once the re-key's delete waits behind it: a re-key that never queues
// behind the writer, or answers without waiting for it, fails the wait. The caller commits holder
// and then reads the answer.
func (r *reKeyRig) refreshBehind(t *testing.T, holder pgx.Tx) <-chan error {
	t.Helper()
	answered := make(chan error, 1)
	go func() { answered <- r.refresh() }()
	storetest.WaitForLockWaiters(t, holder, 1)
	return answered
}

// wantOneHistory fails the test unless the old name reads the history want, the canonical name's:
// the two names stand for one pull request, whose history is one, under either.
func (r *reKeyRig) wantOneHistory(t *testing.T, want []string) {
	t.Helper()
	if got := r.formerHeadsOf(gadgetsOldRepo); !slices.Equal(got, want) {
		t.Fatalf("the old name %s reads the former heads %v of #%d, want %v, the canonical name's: one pull request has one history", gadgetsOldRepo, got, r.number, want)
	}
}

// TestTheReKeyCarriesTheHeadsAWriterLeftWhileItWaited: a writer holds #524's row under the old name
// and moves its head twice, from 7474… to 7575… and then to 7676…, while the reconcile re-keys the
// row. The re-key waits for that writer, and once it commits, the canonical row holds every head
// the pull request left: the one before, the two the writer left and the old row's last head, and
// the old name reads that one history too.
func TestTheReKeyCarriesTheHeadsAWriterLeftWhileItWaited(t *testing.T) {
	const left, oldHead, head = "7171717171717171717171717171717171717171", "7474747474747474747474747474747474747474", "7373737373737373737373737373737373737373"
	const moved, last = "7575757575757575757575757575757575757575", "7676767676767676767676767676767676767676"
	rig := newReKeyRig(t, 524, left, oldHead, head)
	writer, err := rig.pool.Begin(rig.ctx)
	if err != nil {
		t.Fatalf("begin the writer: %v", err)
	}
	defer writer.Rollback(context.Background())
	for _, sha := range []string{moved, last} {
		if _, err := writer.Exec(rig.ctx, `update pull_request_heads set head_sha = $3 where repo = $1 and number = $2`, gadgetsOldRepo, rig.number, sha); err != nil {
			t.Fatalf("move %s#%d to %s: %v", gadgetsOldRepo, rig.number, sha, err)
		}
	}
	answered := rig.refreshBehind(t, writer)
	if err := writer.Commit(rig.ctx); err != nil {
		t.Fatalf("commit the writer: %v", err)
	}
	if err := <-answered; err != nil {
		t.Fatalf("re-key %s#%d: %v", gadgetsOldRepo, rig.number, err)
	}
	want := []string{left, oldHead, moved, last}
	slices.Sort(want)
	if got := rig.formerHeadsOf(gadgetsRepo); !slices.Equal(got, want) {
		t.Fatalf("#%d under %s left %v after the re-key, want %v: the heads the writer left while the re-key waited are lost", rig.number, gadgetsRepo, got, want)
	}
	rig.wantOneHistory(t, want)
}

// TestTheReKeyCarriesNoHeadToAPullRequestClosedWhileItWaited: a writer holds #525's row under the
// old name while the reconcile re-keys it, and meanwhile the canonical row closes, which drops its
// former heads. Once the writer commits, the closed canonical row holds no former head: a reopen
// later must not bring back heads the pull request left before it closed.
func TestTheReKeyCarriesNoHeadToAPullRequestClosedWhileItWaited(t *testing.T) {
	const left, oldHead, head = "7171717171717171717171717171717171717171", "7272727272727272727272727272727272727272", "7373737373737373737373737373737373737373"
	rig := newReKeyRig(t, 525, left, oldHead, head)
	writer, err := rig.pool.Begin(rig.ctx)
	if err != nil {
		t.Fatalf("begin the writer: %v", err)
	}
	defer writer.Rollback(context.Background())
	if _, err := writer.Exec(rig.ctx, `update pull_request_heads set checked_at = now() where repo = $1 and number = $2`, gadgetsOldRepo, rig.number); err != nil {
		t.Fatalf("stamp %s#%d: %v", gadgetsOldRepo, rig.number, err)
	}
	answered := rig.refreshBehind(t, writer)
	if _, err := rig.pool.Exec(rig.ctx, `update pull_request_heads set state = 'closed' where repo = $1 and number = $2`, gadgetsRepo, rig.number); err != nil {
		t.Fatalf("close %s#%d: %v", gadgetsRepo, rig.number, err)
	}
	if err := writer.Commit(rig.ctx); err != nil {
		t.Fatalf("commit the writer: %v", err)
	}
	if err := <-answered; err != nil {
		t.Fatalf("re-key %s#%d: %v", gadgetsOldRepo, rig.number, err)
	}
	if state, _ := rig.canonicalStateAndHead(); state != "closed" {
		t.Fatalf("%s#%d is %s after the close, want closed", gadgetsRepo, rig.number, state)
	}
	if got := rig.formerHeadsOf(gadgetsRepo); len(got) != 0 {
		t.Fatalf("#%d under %s, closed, keeps the former heads %v the re-key carried after the close", rig.number, gadgetsRepo, got)
	}
	rig.wantOneHistory(t, nil)
}

// TestTheReKeyCarriesNothingAcrossACloseAndReopenOfThePullRequest: a writer holds the row of the
// pull request under the old name, which left 7171… and stands at 7272…, while the reconcile
// re-keys it; meanwhile the canonical row closes and reopens at 7474…. The close ends the pull
// request's lifecycle under both its names, so once the writer commits the reopened canonical row
// holds neither the head the old row left nor the old row's own head. Where the writer moves the
// old row on to 7575… after the reopen, that head is the new lifecycle's, and the re-key carries
// it.
func TestTheReKeyCarriesNothingAcrossACloseAndReopenOfThePullRequest(t *testing.T) {
	const left, oldHead, head = "7171717171717171717171717171717171717171", "7272727272727272727272727272727272727272", "7373737373737373737373737373737373737373"
	const reopened, moved = "7474747474747474747474747474747474747474", "7575757575757575757575757575757575757575"
	for _, row := range []struct {
		name   string
		number int
		move   bool
		want   []string
	}{
		{"the old row stays at the head the close left it at", 530, false, nil},
		{"the old row moves on after the reopen", 531, true, []string{moved}},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig := newReKeyRig(t, row.number, left, oldHead, head)
			writer, err := rig.pool.Begin(rig.ctx)
			if err != nil {
				t.Fatalf("begin the writer: %v", err)
			}
			defer writer.Rollback(context.Background())
			if _, err := writer.Exec(rig.ctx, `update pull_request_heads set checked_at = now() where repo = $1 and number = $2`, gadgetsOldRepo, rig.number); err != nil {
				t.Fatalf("stamp %s#%d: %v", gadgetsOldRepo, rig.number, err)
			}
			answered := rig.refreshBehind(t, writer)
			if _, err := rig.pool.Exec(rig.ctx, `update pull_request_heads set state = 'closed' where repo = $1 and number = $2`, gadgetsRepo, rig.number); err != nil {
				t.Fatalf("close %s#%d: %v", gadgetsRepo, rig.number, err)
			}
			if _, err := rig.pool.Exec(rig.ctx, `update pull_request_heads set state = 'open', head_sha = $3 where repo = $1 and number = $2`, gadgetsRepo, rig.number, reopened); err != nil {
				t.Fatalf("reopen %s#%d at %s: %v", gadgetsRepo, rig.number, reopened, err)
			}
			if row.move {
				if _, err := writer.Exec(rig.ctx, `update pull_request_heads set head_sha = $3 where repo = $1 and number = $2`, gadgetsOldRepo, rig.number, moved); err != nil {
					t.Fatalf("move %s#%d to %s: %v", gadgetsOldRepo, rig.number, moved, err)
				}
			}
			if err := writer.Commit(rig.ctx); err != nil {
				t.Fatalf("commit the writer: %v", err)
			}
			if err := <-answered; err != nil {
				t.Fatalf("re-key %s#%d: %v", gadgetsOldRepo, rig.number, err)
			}
			if state, at := rig.canonicalStateAndHead(); state != "open" || at != reopened {
				t.Fatalf("%s#%d is %s at %s after the reopen, want open at %s", gadgetsRepo, rig.number, state, at, reopened)
			}
			if got := rig.formerHeadsOf(gadgetsRepo); !slices.Equal(got, row.want) {
				t.Fatalf("#%d under %s, reopened, left %v after the re-key, want %v: a head of the lifecycle the close ended came back", rig.number, gadgetsRepo, got, row.want)
			}
			rig.wantOneHistory(t, row.want)
		})
	}
}

// TestAReKeyByTheReleaseBeforeCarriesTheHeadsItLeft: the release before this one re-keys #526 with
// a plain delete of the old row, after its write of the canonical row, and carries nothing itself.
// The canonical row holds the heads the pull request left under the old name all the same.
func TestAReKeyByTheReleaseBeforeCarriesTheHeadsItLeft(t *testing.T) {
	const left, oldHead, head = "7171717171717171717171717171717171717171", "7272727272727272727272727272727272727272", "7373737373737373737373737373737373737373"
	rig := newReKeyRig(t, 526, left, oldHead, head)
	if _, err := rig.pool.Exec(rig.ctx, `delete from pull_request_heads where repo = $1 and number = $2`, gadgetsOldRepo, rig.number); err != nil {
		t.Fatalf("delete %s#%d as the release before does: %v", gadgetsOldRepo, rig.number, err)
	}
	if got, want := rig.formerHeadsOf(gadgetsRepo), []string{left, oldHead}; !slices.Equal(got, want) {
		t.Fatalf("#%d under %s left %v after the release before deleted its old row, want %v", rig.number, gadgetsRepo, got, want)
	}
	rig.wantOneHistory(t, []string{left, oldHead})
}

// TestTheReKeyOwesTheCanonicalIssueASettle: #527 and #528 are each stored under the old name,
// unattributed there, and read through under the canonical name, attributed to the issue; the
// issue's check states are settled, so it owes nothing. Then each old row is re-keyed, #527 by the
// reconcile and #528 by the plain delete of the release before. The canonical row is written with
// the head, state and issue it already holds, so its own write marks nothing, and the old row's
// delete marks no issue, since that row has none: the re-key must itself mark the issue owed,
// in its own transaction, so the settler publishes the states at the heads it carried.
func TestTheReKeyOwesTheCanonicalIssueASettle(t *testing.T) {
	const left, oldHead, head = "7171717171717171717171717171717171717171", "7272727272727272727272727272727272727272", "7373737373737373737373737373737373737373"
	for _, reKey := range []struct {
		name   string
		number int
		run    func(rig *reKeyRig) error
	}{
		{"the reconcile", 527, func(rig *reKeyRig) error { return rig.refresh() }},
		{"the release before", 528, func(rig *reKeyRig) error {
			_, err := rig.pool.Exec(rig.ctx, `delete from pull_request_heads where repo = $1 and number = $2`, gadgetsOldRepo, rig.number)
			return err
		}},
	} {
		t.Run(reKey.name, func(t *testing.T) {
			rig := newReKeyRig(t, reKey.number, left, oldHead, head)
			owed := func() int {
				t.Helper()
				var count int
				if err := rig.pool.QueryRow(rig.ctx, `select count(*) from check_states_owed where issue_key = $1`, rig.issue).Scan(&count); err != nil {
					t.Fatalf("count the marks of %s: %v", rig.issue, err)
				}
				return count
			}
			if _, err := rig.pool.Exec(rig.ctx, `delete from check_states_owed where issue_key = $1`, rig.issue); err != nil {
				t.Fatalf("settle %s: %v", rig.issue, err)
			}
			if err := reKey.run(rig); err != nil {
				t.Fatalf("re-key %s#%d by %s: %v", gadgetsOldRepo, rig.number, reKey.name, err)
			}
			if got, want := rig.formerHeadsOf(gadgetsRepo), []string{left, oldHead}; !slices.Equal(got, want) {
				t.Fatalf("#%d under %s left %v after the re-key, want %v", rig.number, gadgetsRepo, got, want)
			}
			if marks := owed(); marks == 0 {
				t.Fatalf("%s owes no settle after %s carried the heads %v of #%d to it: nothing publishes their states", rig.issue, reKey.name, []string{left, oldHead}, rig.number)
			}
		})
	}
}

// TestAReKeyOfAPullRequestBackAtAHeadItLeftCarriesThatHead: #529 under the old name left 7171…,
// then 7272…, and is back at 7171…, which it therefore holds both as its head and as a head it
// left. The reconcile's re-key carries both heads to the canonical row, 7171… once.
func TestAReKeyOfAPullRequestBackAtAHeadItLeftCarriesThatHead(t *testing.T) {
	const left, oldHead, head = "7171717171717171717171717171717171717171", "7272727272727272727272727272727272727272", "7373737373737373737373737373737373737373"
	rig := newReKeyRig(t, 529, left, oldHead, head)
	back := time.Date(2026, 2, 1, 0, 2, 0, 0, time.UTC)
	stale := time.Now().Add(-10 * time.Minute)
	if err := unrenamed().UpsertPullRequestHead(rig.ctx, rig.pool, nil, PullRequestHead{
		Repo: gadgetsOldRepo, Number: rig.number, HeadSHA: left,
		HeadRef: "legion/work", BaseRef: "main", State: "open", EventAt: &back, CheckedAt: &stale,
	}); err != nil {
		t.Fatalf("move %s#%d back to %s: %v", gadgetsOldRepo, rig.number, left, err)
	}
	if err := rig.refresh(); err != nil {
		t.Fatalf("re-key %s#%d, back at a head it left: %v", gadgetsOldRepo, rig.number, err)
	}
	if got, want := rig.formerHeadsOf(gadgetsRepo), []string{left, oldHead}; !slices.Equal(got, want) {
		t.Fatalf("#%d under %s left %v after the re-key, want %v", rig.number, gadgetsRepo, got, want)
	}
}
