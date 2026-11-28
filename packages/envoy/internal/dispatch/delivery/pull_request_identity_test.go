// pull_request_identity_test.go holds the record of a pull request's former heads to one identity
// of the pull request under every name its repository goes by: a record a writer appended under a
// name, before or after Dispatch learned that its repository was renamed, is applied to the same
// history as every other record of that pull request, and a close under any name ends that one
// history's lifecycle, whatever order the records commit in.
package delivery

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// formerHeadsRecordsOf is how many records the writes of pull_request_heads appended for pull
// request number, under any name, that no settle has applied yet.
func formerHeadsRecordsOf(t *testing.T, ctx context.Context, pool *store.Pool, number int) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `select count(*) from pull_request_former_heads_moves where number = $1`, number).Scan(&count); err != nil {
		t.Fatalf("count the records of #%d: %v", number, err)
	}
	return count
}

// TestRecordsWrittenBeforeARenameIsKnownReachThePullRequestsHistory: #940 is stored under the
// repository's old name, open at H1, and moves to H2 there; both writes append their records, and
// no settle applies them yet. The canonical row is then written at H3 and the old row deleted, as
// the re-key does, and only then are the records applied. Whether Dispatch knew of the rename
// before the writes or learned it between them and their application, the records of both names
// are of one pull request: its history holds H1 and H2, the heads it left, under the canonical
// name. Where each name's records were applied apart before the rename was known, the re-key joins
// both histories: H1, H2 and H3.
func TestRecordsWrittenBeforeARenameIsKnownReachThePullRequestsHistory(t *testing.T) {
	const h1, h2, h3 = "9191919191919191919191919191919191919191", "9292929292929292929292929292929292929292", "9393939393939393939393939393939393939393"
	const h4 = "9494949494949494949494949494949494949494"
	for _, row := range []struct {
		name          string
		number        int
		known, folded bool
		want          []string
	}{
		{"the rename is known before the writes", 940, true, false, []string{h1, h2}},
		{"the rename is learned between the writes and their application", 941, false, false, []string{h1, h2}},
		{"each name's records were applied apart before the rename was learned", 942, false, true, []string{h1, h2, h3}},
	} {
		t.Run(row.name, func(t *testing.T) {
			pool, ctx := deliveryTestPool(t)
			issue := seedGadgetsIssue(t, ctx, pool, row.number)
			if row.known {
				seedRenamedGadgets(t, ctx, pool)
			}
			exec := func(statement string, args ...any) {
				t.Helper()
				if _, err := pool.Exec(ctx, statement, args...); err != nil {
					t.Fatalf("%s: %v", statement, err)
				}
			}
			exec(`insert into pull_request_heads (repo, number, head_sha, state, issue_key) values ($1, $2, $3, 'open', $4)`, gadgetsOldRepo, row.number, h1, issue)
			exec(`update pull_request_heads set head_sha = $3 where repo = $1 and number = $2`, gadgetsOldRepo, row.number, h2)
			var queued int
			if err := pool.QueryRow(ctx, `select count(*) from pull_request_former_heads_moves where number = $1`, row.number).Scan(&queued); err != nil {
				t.Fatalf("count the records of #%d: %v", row.number, err)
			}
			if queued < 2 {
				t.Fatalf("the two writes of #%d under %s appended %d records, want two: this test holds nothing queued", row.number, gadgetsOldRepo, queued)
			}
			canonicalHead := h3
			if row.folded {
				foldedFormerHeadsOf(t, ctx, pool, gadgetsOldRepo, row.number)
				exec(`insert into pull_request_heads (repo, number, head_sha, state, issue_key) values ($1, $2, $3, 'open', $4)`, gadgetsRepo, row.number, h3, issue)
				exec(`update pull_request_heads set head_sha = $3 where repo = $1 and number = $2`, gadgetsRepo, row.number, h4)
				foldedFormerHeadsOf(t, ctx, pool, gadgetsRepo, row.number)
				canonicalHead = h4
			}
			if !row.known {
				seedRenamedGadgets(t, ctx, pool)
			}
			if !row.folded {
				exec(`insert into pull_request_heads (repo, number, head_sha, state, issue_key) values ($1, $2, $3, 'open', $4)`, gadgetsRepo, row.number, canonicalHead, issue)
			}
			exec(`delete from pull_request_heads where repo = $1 and number = $2`, gadgetsOldRepo, row.number)
			foldedFormerHeadsOf(t, ctx, pool, gadgetsRepo, row.number)
			foldedFormerHeadsOf(t, ctx, pool, gadgetsOldRepo, row.number)
			got := foldedFormerHeadsOf(t, ctx, pool, gadgetsRepo, row.number)
			if !slices.Equal(got, row.want) {
				t.Fatalf("#%d under %s holds the former heads %v once every record is applied, want %v: a record written under %s is of the same pull request", row.number, gadgetsRepo, got, row.want, gadgetsOldRepo)
			}
			if left := formerHeadsRecordsOf(t, ctx, pool, row.number); left != 0 {
				t.Fatalf("%d records of #%d are left after both names were applied, want none", left, row.number)
			}
		})
	}
}

// TestACloseUnderTheOldNameThatCommitsLateEndsTheLifecycleItClosed: #947 is stored under the old
// name, where it left 7171… and stands at 7272…, and read through under the canonical name at
// 7373…; both names' records are applied. The old row's close is then written and held open, and
// meanwhile the canonical row moves to 7474… and on to 7575…, both committed and applied: 7474… is
// a former head then. Once the close commits and is applied, it ends the lifecycle it closed
// under every name, by the place it was written at, so the heads reached before it go and 7474…,
// reached after it, stays; no record is left.
func TestACloseUnderTheOldNameThatCommitsLateEndsTheLifecycleItClosed(t *testing.T) {
	const h4, h5 = "7474747474747474747474747474747474747474", "7575757575757575757575757575757575757575"
	rig := newReKeyRig(t, 947, "7171717171717171717171717171717171717171", "7272727272727272727272727272727272727272", "7373737373737373737373737373737373737373")
	if queued := formerHeadsRecordsOf(t, rig.ctx, rig.pool, rig.number); queued == 0 {
		t.Fatalf("the writes of #%d appended no record: this test holds nothing queued", rig.number)
	}
	rig.formerHeadsOf(gadgetsRepo)
	closing, err := rig.pool.Begin(rig.ctx)
	if err != nil {
		t.Fatalf("begin the close: %v", err)
	}
	defer closing.Rollback(context.Background())
	if _, err := closing.Exec(rig.ctx, `update pull_request_heads set state = 'closed' where repo = $1 and number = $2`, gadgetsOldRepo, rig.number); err != nil {
		t.Fatalf("close %s#%d: %v", gadgetsOldRepo, rig.number, err)
	}
	for _, head := range []string{h4, h5} {
		if _, err := rig.pool.Exec(rig.ctx, `update pull_request_heads set head_sha = $3 where repo = $1 and number = $2`, gadgetsRepo, rig.number, head); err != nil {
			t.Fatalf("move %s#%d to %s: %v", gadgetsRepo, rig.number, head, err)
		}
	}
	if before := rig.formerHeadsOf(gadgetsRepo); !slices.Contains(before, h4) {
		t.Fatalf("#%d holds the former heads %v while the close is open, want 7474… among them: this test holds no head of the later lifecycle", rig.number, before)
	}
	if err := closing.Commit(rig.ctx); err != nil {
		t.Fatalf("commit the close: %v", err)
	}
	if after := rig.formerHeadsOf(gadgetsRepo); !slices.Equal(after, []string{h4}) {
		t.Fatalf("#%d holds the former heads %v once the earlier close committed, want 7474… alone: the close ends what was reached before it and nothing after", rig.number, after)
	}
	if left := formerHeadsRecordsOf(t, rig.ctx, rig.pool, rig.number); left != 0 {
		t.Fatalf("%d records of #%d are left after the close was applied, want none", left, rig.number)
	}
}

// TestAPullRequestFirstSeenClosedUnderItsCanonicalNameEndsTheLifecycleOfItsOldName: a pull request
// is stored under the repository's old name, open, and visits 8181… and then 8282…, so 8181… is its
// former head. The canonical-name writer (upsertHeadUnderCanonicalName) then writes the canonical
// row and deletes the old one, as the re-key does, and the canonical row reopens at 8484…. Where
// Dispatch first sees the canonical row closed or merged, that write is the close: it ends the
// lifecycle the old name stood for, so the reopened pull request holds no head of it, 8181… and
// 8282… included, and its generation moves on. The neighbour writes the canonical row open first
// and then closes it by the writer, an update of an open row, with the same outcome.
func TestAPullRequestFirstSeenClosedUnderItsCanonicalNameEndsTheLifecycleOfItsOldName(t *testing.T) {
	const h1, h2 = "8181818181818181818181818181818181818181", "8282828282828282828282828282828282828282"
	const h3, h4 = "8383838383838383838383838383838383838383", "8484848484848484848484848484848484848484"
	for _, row := range []struct {
		name, state string
		existing    bool
		number      int
	}{
		{"the canonical row is open and then closes", "closed", true, 961},
		{"the canonical row is first seen closed", "closed", false, 962},
		{"the canonical row is first seen merged", "merged", false, 963},
	} {
		t.Run(row.name, func(t *testing.T) {
			pool, ctx := deliveryTestPool(t)
			issue := seedGadgetsIssue(t, ctx, pool, row.number)
			seedRenamedGadgets(t, ctx, pool)
			attributor := NewAttributor(renamesOf(t, ctx, pool))
			put := func(repo, head, state string) {
				t.Helper()
				if err := attributor.UpsertPullRequestHead(ctx, pool, nil, PullRequestHead{
					Repo: repo, Number: row.number, HeadSHA: head, State: state, IssueKey: &issue, AttributionComplete: true,
				}); err != nil {
					t.Fatalf("write %s#%d at %s, %s: %v", repo, row.number, head, state, err)
				}
			}
			put(gadgetsOldRepo, h1, "open")
			put(gadgetsOldRepo, h2, "open")
			if before := foldedFormerHeadsOf(t, ctx, pool, gadgetsOldRepo, row.number); !slices.Equal(before, []string{h1}) {
				t.Fatalf("#%d holds the former heads %v under the old name, want 8181… alone: this test holds no history to end", row.number, before)
			}
			if row.existing {
				put(gadgetsRepo, h3, "open")
			}
			if err := attributor.upsertHeadUnderCanonicalName(ctx, pool, nil, PullRequestHead{
				Repo: gadgetsRepo, Number: row.number, HeadSHA: h3, State: row.state, IssueKey: &issue, AttributionComplete: true,
			}, gadgetsOldRepo); err != nil {
				t.Fatalf("write %s#%d %s under the canonical name: %v", gadgetsRepo, row.number, row.state, err)
			}
			put(gadgetsRepo, h4, "open")
			after := foldedFormerHeadsOf(t, ctx, pool, gadgetsRepo, row.number)
			var ended *int64
			var generation int
			if err := pool.QueryRow(ctx, `
				select ended, generation from pull_request_former_heads_pulls where pull = pull_request_former_heads_pull_of($1, $2)`,
				gadgetsRepo, row.number).Scan(&ended, &generation); err != nil {
				t.Fatalf("read #%d's lifecycle: %v", row.number, err)
			}
			if len(after) != 0 || ended == nil || generation < 2 {
				t.Fatalf("after the canonical row %s and reopened, #%d holds the former heads %v with ended=%v generation=%d, want none, an end and a later generation: the close ends the lifecycle it closes under every name",
					row.state, row.number, after, ended, generation)
			}
		})
	}
}

// TestTheCanonicalNameWriterWritesTheCanonicalRowBeforeItDeletesTheOldOne: #964 is stored under
// the repository's old name; it opens at 8181…, closes, reopens at 8282… and moves on to 8383…, so
// a close of it is known and 8282… is a former head of the lifecycle the reopen began. The
// canonical-name writer (upsertHeadUnderCanonicalName) then reads it closed at 8484… under the
// canonical name: it writes the canonical row closed and deletes the old name's open row. The
// records of the two writes are in that order, the write first, and the close they show ends the
// lifecycle: once the canonical row reopens at 8585…, #964 holds no former head, 8282… and 8383…
// included. The settler counts a row written closed while a close of the pull request is known
// only where a row of it is open then (pull_request_former_heads_apply), so a delete of the open
// row before the write would leave no row open, and the close would end nothing.
func TestTheCanonicalNameWriterWritesTheCanonicalRowBeforeItDeletesTheOldOne(t *testing.T) {
	const h1, h2, h3 = "8181818181818181818181818181818181818181", "8282828282828282828282828282828282828282", "8383838383838383838383838383838383838383"
	const h4, h5 = "8484848484848484848484848484848484848484", "8585858585858585858585858585858585858585"
	const number = 964
	pool, ctx := deliveryTestPool(t)
	issue := seedGadgetsIssue(t, ctx, pool, number)
	seedRenamedGadgets(t, ctx, pool)
	attributor := NewAttributor(renamesOf(t, ctx, pool))
	put := func(repo, head, state string) {
		t.Helper()
		if err := attributor.UpsertPullRequestHead(ctx, pool, nil, PullRequestHead{
			Repo: repo, Number: number, HeadSHA: head, State: state, IssueKey: &issue, AttributionComplete: true,
		}); err != nil {
			t.Fatalf("write %s#%d at %s, %s: %v", repo, number, head, state, err)
		}
	}
	lifecycle := func() (*int64, int) {
		t.Helper()
		var ended *int64
		var generation int
		if err := pool.QueryRow(ctx, `
			select ended, generation from pull_request_former_heads_pulls where pull = pull_request_former_heads_pull_of($1, $2)`,
			gadgetsRepo, number).Scan(&ended, &generation); err != nil {
			t.Fatalf("read #%d's lifecycle: %v", number, err)
		}
		return ended, generation
	}
	put(gadgetsOldRepo, h1, "open")
	put(gadgetsOldRepo, h1, "closed")
	put(gadgetsOldRepo, h2, "open")
	put(gadgetsOldRepo, h3, "open")
	if before := foldedFormerHeadsOf(t, ctx, pool, gadgetsOldRepo, number); !slices.Equal(before, []string{h2}) {
		t.Fatalf("#%d holds the former heads %v under the old name, want 8282… alone: this test holds no history to end", number, before)
	}
	ended, reopened := lifecycle()
	if ended == nil {
		t.Fatalf("#%d holds no close after it closed and reopened under the old name: this test needs a close the settler knows", number)
	}

	if err := attributor.upsertHeadUnderCanonicalName(ctx, pool, nil, PullRequestHead{
		Repo: gadgetsRepo, Number: number, HeadSHA: h4, State: "closed", IssueKey: &issue, AttributionComplete: true,
	}, gadgetsOldRepo); err != nil {
		t.Fatalf("write %s#%d closed under the canonical name: %v", gadgetsRepo, number, err)
	}
	rows, err := pool.Query(ctx, `
		select op || ' ' || repo || '#' || number || case when coalesce(new_open, old_open) then ' open' else ' closed' end
		from pull_request_former_heads_moves where number = $1 order by seq`, number)
	if err != nil {
		t.Fatalf("read the records of #%d: %v", number, err)
	}
	records, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the records of #%d: %v", number, err)
	}
	if want := []string{"INSERT acme/gadgets#964 closed", "DELETE oldco/gadgets#964 open"}; !slices.Equal(records, want) {
		t.Errorf("upsertHeadUnderCanonicalName's writes are recorded as %v, want %v: the canonical row is written before the old name's open row is deleted, so the close is written while a row of the pull request is open", records, want)
	}

	put(gadgetsRepo, h5, "open")
	after := foldedFormerHeadsOf(t, ctx, pool, gadgetsRepo, number)
	if _, generation := lifecycle(); len(after) != 0 || generation <= reopened {
		t.Fatalf("after the canonical row was written closed and reopened, #%d holds the former heads %v at generation %d (%d before), want none at a later generation: the close the canonical-name writer wrote ended nothing",
			number, after, generation, reopened)
	}
}
